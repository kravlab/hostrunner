package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
)

// defaultAskTimeout is how long an approval request waits for an answer
// when the client sets no limit.
const defaultAskTimeout = 10 * time.Minute

// sessionWriteTimeout bounds a write to an approval session. A session
// that does not take a frame within it (suspended, its terminal stopped)
// is taken for gone, so it holds up neither requests nor other sessions.
const sessionWriteTimeout = 2 * time.Second

// Messages a refused approval request ends with; the client prints each
// after "hostrun: ".
const (
	refusedMessage    = "refused on the host"
	noSessionMessage  = "not approved: no approval session serves this devcontainer"
	unansweredMessage = "not approved: nobody answered on the host"
	tooLargeMessage   = "not approved: the request is too large to show"
)

// Errors for the log, beside the messages above.
var (
	errRefused    = errors.New("refused by the human")
	errNoSession  = errors.New("no approval session serves the daemon")
	errUnanswered = errors.New("nobody answered the approval request")
	errTooLarge   = errors.New("the approval request exceeds a frame")
)

// WithApprovals makes the Server take approval sessions on l, its approval
// socket, and report rulesFile, the rules file it loaded, in its hello.
// Serve accepts on l alongside the container's socket and closes l when it
// stops. Without it, nothing is approved.
func WithApprovals(l net.Listener, rulesFile string) Option {
	return func(s *Server) {
		s.approvals = &approvals{listener: l, rulesFile: rulesFile, waiting: map[uint64]*approvalRequest{}}
	}
}

// approvals is the daemon's side of its approval sessions: the session that
// serves it, if any, and the approval requests waiting for its answer.
type approvals struct {
	listener  net.Listener
	rulesFile string

	mu      sync.Mutex
	serving *sessionConn // nil while no session serves the daemon
	lastID  uint64
	waiting map[uint64]*approvalRequest
}

// sessionConn is a connected approval session: its connection, where its
// frames go (each write bounded by sessionWriteTimeout), and its pid,
// which names it in the log.
type sessionConn struct {
	conn net.Conn
	out  *frameWriter
	pid  int
}

// approvalRequest is a request waiting for an answer, which arrives once on
// answers.
type approvalRequest struct {
	answers chan protocol.Answer
}

// enqueue registers msg as a new approval request and sends it to the
// serving session. It returns the request's id and where its answer
// arrives, or errNoSession when no session serves the daemon or the
// session does not take the request (it is then taken for gone and its
// connection closed), or errTooLarge when the request exceeds a frame.
// The write happens outside the lock, so a slow session holds up only the
// request it is sent.
func (a *approvals) enqueue(msg protocol.ApprovalRequest) (uint64, <-chan protocol.Answer, error) {
	a.mu.Lock()
	c := a.serving
	if c == nil {
		a.mu.Unlock()
		return 0, nil, errNoSession
	}
	a.lastID++
	msg.ID = a.lastID
	r := &approvalRequest{answers: make(chan protocol.Answer, 1)}
	a.waiting[msg.ID] = r
	a.mu.Unlock()
	if err := c.out.writeJSON(protocol.FrameApprovalRequest, msg); err != nil {
		a.withdraw(msg.ID)
		if errors.Is(err, protocol.ErrPayloadTooLarge) {
			return 0, nil, errTooLarge
		}
		// Its handleSession then sees the connection end, and leaves.
		c.conn.Close()
		return 0, nil, errNoSession
	}
	return msg.ID, r.answers, nil
}

// withdraw drops request id: it takes no answer any more.
func (a *approvals) withdraw(id uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.waiting, id)
}

// answer delivers ans from session c to its request and returns why it was
// not taken, or "".
func (a *approvals) answer(c *sessionConn, ans protocol.Answer) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.serving != c {
		return "this session does not serve the daemon"
	}
	if ans.Scope != "" && ans.Scope != protocol.ScopeOnce {
		return fmt.Sprintf("unknown scope %q", ans.Scope)
	}
	r, ok := a.waiting[ans.ID]
	if !ok {
		return fmt.Sprintf("request %d is not waiting any more", ans.ID)
	}
	delete(a.waiting, ans.ID)
	r.answers <- ans
	return ""
}

// serve makes c the session that serves the daemon and returns why not, or
// "".
func (a *approvals) serve(c *sessionConn) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.serving != nil && a.serving != c {
		return "another approval session serves this daemon"
	}
	a.serving = c
	return ""
}

// leave drops c, if it served the daemon, and reports whether it did.
func (a *approvals) leave(c *sessionConn) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.serving != c {
		return false
	}
	a.serving = nil
	return true
}

// served reports whether a session serves the daemon.
func (a *approvals) served() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serving != nil
}

// serveApprovals accepts approval sessions on the approval socket until it
// is closed, handling each in wg. An accept error it cannot retry ends
// approvals, not the daemon.
func (s *Server) serveApprovals(ctx context.Context, wg *sync.WaitGroup) {
	for {
		conn, err := s.approvals.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if isTemporary(err) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			s.log.Warn("approval socket failed", "error", err)
			return
		}
		wg.Go(func() { s.handleSession(ctx, conn) })
	}
}

// handleSession serves one connection to the approval socket. Only a
// process of the daemon's own uid may use it; it opens with FrameHello and
// then takes FrameServe and FrameAnswer. Any other frame ends it.
func (s *Server) handleSession(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	uid, pid, err := transport.PeerCred(conn)
	if err != nil || uid != os.Getuid() {
		s.log.Warn("approval socket connection refused", "uid", uid, "pid", pid, "error", err)
		return
	}
	out := &frameWriter{conn: conn, timeout: sessionWriteTimeout}
	c := &sessionConn{conn: conn, out: out, pid: pid}
	// A bug triggered by one session must not take the daemon down.
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn("approval session failed", "pid", pid, "error", fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
			_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "internal error in the hostrunner daemon"})
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(s.requestTimeout))
	f, err := protocol.ReadFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	var hello protocol.Hello
	if err == nil && f.Type != protocol.FrameHello {
		err = fmt.Errorf("expected a hello on the approval socket, got frame type %d", f.Type)
	}
	if err == nil {
		err = protocol.DecodeJSON(f, &hello)
	}
	if err != nil {
		s.sessionRefused(out, pid, err)
		return
	}
	host, container := s.mapper.Roots()
	_ = out.writeJSON(protocol.FrameDaemonInfo, protocol.DaemonInfo{
		Version: protocol.ApprovalVersion, Workspace: host, ContainerWorkspace: container,
		RulesFile: s.approvals.rulesFile, Served: s.approvals.served(),
	})
	defer func() {
		if s.approvals.leave(c) {
			s.log.Info("approval session left", "pid", pid)
		}
	}()
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		switch f.Type {
		case protocol.FrameServe:
			if hello.Version != protocol.ApprovalVersion {
				s.sessionRefused(out, pid, fmt.Errorf("approval protocol version mismatch: session %d, daemon %d", hello.Version, protocol.ApprovalVersion))
				return
			}
			if why := s.approvals.serve(c); why != "" {
				_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: protocol.ExitRejected, Message: why})
				continue
			}
			s.log.Info("approval session serving", "pid", pid)
			_ = out.write(protocol.Frame{Type: protocol.FrameServing})
		case protocol.FrameAnswer:
			var ans protocol.Answer
			if err := protocol.DecodeJSON(f, &ans); err != nil {
				s.sessionRefused(out, pid, err)
				return
			}
			_ = out.writeJSON(protocol.FrameAnswered, protocol.Answered{ID: ans.ID, Error: s.approvals.answer(c, ans)})
		default:
			s.sessionRefused(out, pid, fmt.Errorf("frame type %d does not belong on the approval socket", f.Type))
			return
		}
	}
}

// sessionRefused reports err to the session at pid, in the log too.
func (s *Server) sessionRefused(out *frameWriter, pid int, err error) {
	s.log.Warn("approval socket connection refused", "pid", pid, "error", err)
	_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: err.Error()})
}

// ask handles FrameAsk: a command the rules allow runs as for FrameRequest;
// one they refuse but a human may approve waits for the serving session's
// answer and runs if approved. Approvals cover commands no rule matches,
// which run as with no rule: in the mirrored directory, with nothing
// injected. Any other refusal is the one a plain request gets.
func (s *Server) ask(ctx context.Context, cancel context.CancelFunc, conn net.Conn, out *frameWriter, f protocol.Frame) {
	var ask protocol.Ask
	if err := protocol.DecodeJSON(f, &ask); err != nil {
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	req := ask.Request
	if len(req.Argv) == 0 {
		err := errors.New("empty command")
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	_, err := s.policy.Check(req.Argv)
	var d *rules.Denial
	if err == nil || !errors.As(err, &d) || !d.Approvable() || d.Rule != "" || d.Guarded() {
		s.run(ctx, cancel, conn, out, req)
		return
	}
	approved := rules.Allowed{} // no rule: the mirrored directory, nothing injected
	// What would refuse the command anyway is reported before a human is
	// asked; starting it checks all of it again.
	p, r := s.prepareAllowed(req, approved, true)
	if r == nil {
		r = lookupStart(p, req)
	}
	if r != nil {
		s.reject(out, r.code, r.message, r.err)
		return
	}
	hostDir := p.hostPath
	p.close()
	timeout := defaultAskTimeout
	if ask.TimeoutMs > 0 {
		timeout = time.Duration(ask.TimeoutMs) * time.Millisecond
	}
	reasons := reasonTexts(d)
	var (
		id      uint64
		answers <-chan protocol.Answer
	)
	err = errNoSession
	if s.approvals != nil {
		id, answers, err = s.approvals.enqueue(protocol.ApprovalRequest{Argv: req.Argv, Cwd: req.Cwd, Reasons: reasons, TimeoutMs: timeout.Milliseconds()})
	}
	if err != nil {
		reason, message := "no session", noSessionMessage
		if errors.Is(err, errTooLarge) {
			reason, message = "too large", tooLargeMessage
		}
		s.log.Info("approval not answered", "reason", reason, "argv", req.Argv, "dir", hostDir)
		s.reject(out, protocol.ExitRejected, message, err)
		return
	}
	defer s.approvals.withdraw(id)
	s.log.Info("approval requested", "approval_id", id, "argv", req.Argv, "dir", hostDir, "reasons", reasons, "guarded", false, "timeout", timeout)
	_ = out.writeJSON(protocol.FrameWaiting, protocol.Waiting{Reasons: reasons})

	frames := readFrames(ctx, conn)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var ans protocol.Answer
	select {
	case ans = <-answers:
	case <-frames:
		// The client sends nothing before its command starts: it went away.
		s.log.Info("approval not answered", "approval_id", id, "reason", "withdrawn")
		return
	case <-timer.C:
		s.log.Info("approval not answered", "approval_id", id, "reason", "expired")
		s.reject(out, protocol.ExitRejected, unansweredMessage, errUnanswered)
		return
	case <-ctx.Done():
		return
	}
	if ans.Scope == "" {
		s.log.Info("approval answered", "approval_id", id, "scope", "refused", "note", ans.Note)
		message := refusedMessage
		if ans.Note != "" {
			message += ": " + ans.Note
		}
		s.reject(out, protocol.ExitRejected, message, errRefused)
		return
	}
	s.log.Info("approval answered", "approval_id", id, "scope", ans.Scope)
	_ = out.writeJSON(protocol.FrameApproved, protocol.Approved{Scope: ans.Scope})
	if p, r = s.prepareAllowed(req, approved, false); r != nil {
		s.reject(out, r.code, r.message, r.err)
		return
	}
	s.start(ctx, cancel, conn, out, req, p, frames.next, "approved", ans.Scope, "approval_id", id)
}

// reasonTexts returns the texts of every reason of d.
func reasonTexts(d *rules.Denial) []string {
	texts := make([]string, len(d.Reasons))
	for i, r := range d.Reasons {
		texts[i] = r.Text
	}
	return texts
}

// clientFrames carries what the client sends, read once, so that waiting
// for an answer and then running the command share one reader of the
// connection.
type clientFrames chan frameRead

// frameRead is one read of the client's connection.
type frameRead struct {
	f   protocol.Frame
	err error
}

// readFrames reads the client's frames on conn into the returned channel
// until a read fails, which it delivers last, or ctx ends; then it closes
// the channel.
func readFrames(ctx context.Context, conn net.Conn) clientFrames {
	ch := make(clientFrames)
	go func() {
		defer close(ch)
		for {
			f, err := protocol.ReadFrame(conn)
			select {
			case ch <- frameRead{f, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// next is a frameSource over the frames read; once they end it reports
// io.EOF.
func (ch clientFrames) next() (protocol.Frame, error) {
	r, ok := <-ch
	if !ok {
		return protocol.Frame{}, io.EOF
	}
	return r.f, r.err
}
