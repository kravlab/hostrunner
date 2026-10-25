package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
)

// defaultAskTimeout is how long an approval request waits for an answer
// when the client sets no limit.
const defaultAskTimeout = 10 * time.Minute

// maxAskTimeout is the longest limit a client may set on the wait.
const maxAskTimeout = time.Hour

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
	stoppingMessage   = "not approved: the hostrunner daemon is stopping"
)

// Errors for the log, beside the messages above.
var (
	errRefused     = errors.New("refused by the human")
	errNoSession   = errors.New("no approval session serves the daemon")
	errUnanswered  = errors.New("nobody answered the approval request")
	errTooLarge    = errors.New("the approval request exceeds a frame")
	errSessionLeft = errors.New("the approval session left")
	errStopping    = errors.New("the daemon stops")
)

// WithApprovals makes the Server take approval sessions on l, its approval
// socket, and report rulesFile, the rules file it loaded, in its hello.
// Serve accepts on l alongside the container's socket and closes l when it
// stops. Without it, nothing is approved.
func WithApprovals(l net.Listener, rulesFile string) Option {
	return func(s *Server) {
		s.approvals = &approvals{listener: l, rulesFile: rulesFile, waiting: map[uint64]*approvalRequest{}, telling: map[uint64]endReason{}}
	}
}

// approvals is the daemon's side of its approval sessions: the session that
// serves it, if any, and the approval requests waiting for its answer.
type approvals struct {
	listener  net.Listener
	rulesFile string

	// takeovers makes take-overs one at a time: the last wins.
	takeovers sync.Mutex

	mu      sync.Mutex
	serving *sessionConn // nil while no session serves the daemon
	lastID  uint64
	waiting map[uint64]*approvalRequest
	// telling holds the requests that ended while the session is told so.
	telling map[uint64]endReason
	// standing holds the standing approvals, oldest first; they live as
	// long as the daemon, with a session or without.
	standing     []standingApproval
	lastStanding uint64
}

// standingApproval is a standing approval, and the path failures it
// passes as given: those the human saw when granting it.
type standingApproval struct {
	protocol.Standing
	passes pathPasses
}

// sessionConn is a connected approval session: its connection, where its
// frames go (each write bounded by sessionWriteTimeout), and its pid,
// which names it in the log.
type sessionConn struct {
	conn net.Conn
	out  *frameWriter
	pid  int
}

// approvalRequest is a request waiting for its outcome, which arrives once
// on done: the answer of session, the session it waits for (one that
// takes the daemon over gets it), or why it ended unanswered. Whatever
// ends it first is its outcome; the rest find it no longer waiting.
type approvalRequest struct {
	session  *sessionConn
	done     chan outcome
	msg      protocol.ApprovalRequest // as sent, for a session that takes over
	deadline time.Time                // when its limit runs out
}

// outcome is how an approval request ended: the human's answer, or, when
// ended is set, why it got none.
type outcome struct {
	answer protocol.Answer
	ended  endReason
}

// endReason is why an approval request ended unanswered.
type endReason string

// Reasons an approval request ends unanswered; the daemon log gives them.
const (
	endedWithdrawn   endReason = protocol.WithdrawnByClient // the client went away
	endedExpired     endReason = protocol.WithdrawnExpired  // its limit ran out
	endedSessionLeft endReason = "session left"             // its session left
	endedStopping    endReason = "daemon stopping"          // the daemon stops or steps aside
)

// told reports whether the session is told of the end (FrameWithdrawn): it
// still shows the request, while a session that left or a stopping daemon
// takes it along.
func (r endReason) told() bool { return r == endedWithdrawn || r == endedExpired }

// refusal is what the client of a request that ended for r is told, or nil
// when it went away.
func (r endReason) refusal() *refusal {
	switch r {
	case endedExpired:
		return &refusal{protocol.ExitRejected, unansweredMessage, errUnanswered}
	case endedSessionLeft:
		return &refusal{protocol.ExitRejected, refusedMessage, errSessionLeft}
	case endedStopping:
		return &refusal{protocol.ExitRejected, stoppingMessage, errStopping}
	}
	return nil
}

// enqueue registers msg as a new approval request and sends it to the
// serving session. It returns the request's id and the request, whose
// outcome arrives on its done channel, or errNoSession when no session
// serves the daemon or the session does not take the request (it is then
// taken for gone and its connection closed), or errTooLarge when the
// request exceeds a frame. The write happens outside the lock, so a slow
// session holds up only the request it is sent.
func (a *approvals) enqueue(msg protocol.ApprovalRequest) (uint64, *approvalRequest, error) {
	a.mu.Lock()
	c := a.serving
	if c == nil {
		a.mu.Unlock()
		return 0, nil, errNoSession
	}
	a.lastID++
	msg.ID = a.lastID
	r := &approvalRequest{session: c, done: make(chan outcome, 1), msg: msg, deadline: time.Now().Add(time.Duration(msg.TimeoutMs) * time.Millisecond)}
	a.waiting[msg.ID] = r
	a.mu.Unlock()
	if err := c.out.writeJSON(protocol.FrameApprovalRequest, msg); err != nil {
		a.mu.Lock()
		// A take-over may have passed the request on as c went: it waits
		// for the session that took over.
		moved := r.session != c
		if !moved {
			delete(a.waiting, msg.ID)
		}
		a.mu.Unlock()
		if moved {
			return msg.ID, r, nil
		}
		if errors.Is(err, protocol.ErrPayloadTooLarge) {
			return 0, nil, errTooLarge
		}
		// Its handleSession then sees the connection end, and leaves.
		c.conn.Close()
		return 0, nil, errNoSession
	}
	return msg.ID, r, nil
}

// end ends request id, r, unanswered for reason, unless something else
// ended it first, and returns its outcome. A session still serving is told
// when the reason says so. Until it is told, an answer it sends for the
// request learns the reason from answer, so the session reports the end
// whichever frame reaches it first.
func (a *approvals) end(id uint64, r *approvalRequest, reason endReason) outcome {
	a.mu.Lock()
	_, waiting := a.waiting[id]
	if waiting {
		delete(a.waiting, id)
		r.done <- outcome{ended: reason}
	}
	// A take-over may move the request to another session: the one told is
	// the one it has under the lock.
	session := r.session
	tell := waiting && reason.told() && a.serving == session
	if tell {
		a.telling[id] = reason
	}
	a.mu.Unlock()
	if tell {
		if err := session.out.writeJSON(protocol.FrameWithdrawn, protocol.Withdrawn{ID: id, Reason: string(reason)}); err != nil {
			// As in enqueue: a session that does not take it is gone.
			session.conn.Close()
		}
		a.mu.Lock()
		delete(a.telling, id)
		a.mu.Unlock()
	}
	return <-r.done
}

// answer delivers ans from session c to its request and returns why it was
// not taken, or "", and, for a request that ended unanswered while the
// session is being told so, the reason (see end).
func (a *approvals) answer(c *sessionConn, ans protocol.Answer) (string, endReason) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if why := a.checkServing(c); why != "" {
		return why, ""
	}
	if ans.Scope != "" && ans.Scope != protocol.ScopeOnce && ans.Scope != protocol.ScopeLifetime && ans.Scope != protocol.ScopeRule {
		return fmt.Sprintf("unknown scope %q", ans.Scope), ""
	}
	r, ok := a.waiting[ans.ID]
	if !ok {
		return fmt.Sprintf("request %d is not waiting any more", ans.ID), a.telling[ans.ID]
	}
	delete(a.waiting, ans.ID)
	r.done <- outcome{answer: ans}
	return "", ""
}

// serve makes c the session that serves the daemon and returns why not, or
// "". With takeOver it replaces the session serving it, which it returns
// with the requests waiting for its answer, now waiting for c's: the caller
// sends them to c and tells the old session (see serveSession).
func (a *approvals) serve(c *sessionConn, takeOver bool) (string, *sessionConn, []*approvalRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.serving
	if old == c {
		return "", nil, nil
	}
	if old != nil && !takeOver {
		return protocol.ServedRefusal, nil, nil
	}
	a.serving = c
	var moved []*approvalRequest
	for _, r := range a.waiting {
		if r.session == old {
			r.session = c
			moved = append(moved, r)
		}
	}
	slices.SortFunc(moved, func(x, y *approvalRequest) int { return cmp.Compare(x.msg.ID, y.msg.ID) })
	return "", old, moved
}

// leave drops c, if it served the daemon, and reports whether it did. Every
// request still waiting for c's answer ends unanswered for reason
// (endedSessionLeft, or endedStopping when c leaves because the daemon
// stops): nobody else would see it.
func (a *approvals) leave(c *sessionConn, reason endReason) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, r := range a.waiting {
		if r.session == c {
			delete(a.waiting, id)
			r.done <- outcome{ended: reason}
		}
	}
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

// grant grants a standing approval for req's argv from its container
// directory, guarded or not, passing as given the path failures passes
// does, and returns its number. One the daemon holds for the same argv
// and directory already takes the new approval's terms and keeps its
// number, so one revoke ends it.
func (a *approvals) grant(req protocol.Request, guarded bool, passes pathPasses) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := a.standingIndex(req); i >= 0 {
		a.standing[i].Guarded = guarded
		a.standing[i].passes = passes
		return a.standing[i].N
	}
	a.lastStanding++
	a.standing = append(a.standing, standingApproval{
		Standing: protocol.Standing{N: a.lastStanding, Argv: slices.Clone(req.Argv), Cwd: req.Cwd, Guarded: guarded},
		passes:   passes,
	})
	return a.lastStanding
}

// standingIndex returns the index in a.standing of the standing approval
// for req, its argv byte for byte from the same container directory, or
// -1. The caller holds a.mu.
func (a *approvals) standingIndex(req protocol.Request) int {
	return slices.IndexFunc(a.standing, func(sa standingApproval) bool {
		return sa.Cwd == req.Cwd && slices.Equal(sa.Argv, req.Argv)
	})
}

// notServing is why a session that does not serve the daemon is not
// heard.
const notServing = "this session does not serve the daemon"

// checkServing returns notServing unless c serves the daemon, or "". The
// caller holds a.mu.
func (a *approvals) checkServing(c *sessionConn) string {
	if a.serving != c {
		return notServing
	}
	return ""
}

// heard is checkServing for a caller that does not hold a.mu.
func (a *approvals) heard(c *sessionConn) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.checkServing(c)
}

// standingFor returns the standing approval that lets req through, its
// argv byte for byte from the same container directory, or nil. A Server
// without approvals has none.
func (a *approvals) standingFor(req protocol.Request) *standingApproval {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.standingIndex(req)
	if i < 0 {
		return nil
	}
	sa := a.standing[i]
	return &sa
}

// list returns the standing approvals for session c, or why c may not see
// them.
func (a *approvals) list(c *sessionConn) ([]protocol.Standing, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if why := a.checkServing(c); why != "" {
		return nil, why
	}
	list := make([]protocol.Standing, len(a.standing))
	for i, sa := range a.standing {
		list[i] = sa.Standing
	}
	return list, ""
}

// revoke revokes standing approval n for session c and returns why not, or
// "". A command it let through keeps running.
func (a *approvals) revoke(c *sessionConn, n uint64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if why := a.checkServing(c); why != "" {
		return why
	}
	i := slices.IndexFunc(a.standing, func(sa standingApproval) bool { return sa.N == n })
	if i < 0 {
		return fmt.Sprintf("no standing approval %d", n)
	}
	a.standing = slices.Delete(a.standing, i, i+1)
	return ""
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
// then takes FrameServe, FrameAnswer, FrameStanding, FrameRevoke and
// FrameReread. Any other frame ends it.
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
		RulesFile: s.approvals.rulesFile, RulesDigest: s.RulesDigest(), Served: s.approvals.served(),
	})
	defer func() {
		// The daemon stopping closes the connection too: its requests end
		// for that, not for the session leaving.
		reason := endedSessionLeft
		if ctx.Err() != nil {
			reason = endedStopping
		}
		if s.approvals.leave(c, reason) {
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
			var sv protocol.Serve
			if len(f.Payload) > 0 {
				if err := protocol.DecodeJSON(f, &sv); err != nil {
					s.sessionRefused(out, pid, err)
					return
				}
			}
			if why := s.serveSession(c, sv.TakeOver); why != "" {
				_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: protocol.ExitRejected, Message: why})
				continue
			}
		case protocol.FrameAnswer:
			var ans protocol.Answer
			if err := protocol.DecodeJSON(f, &ans); err != nil {
				s.sessionRefused(out, pid, err)
				return
			}
			why, ended := s.approvals.answer(c, ans)
			_ = out.writeJSON(protocol.FrameAnswered, protocol.Answered{ID: ans.ID, Error: why, Ended: string(ended)})
		case protocol.FrameStanding:
			list, why := s.approvals.list(c)
			_ = out.writeJSON(protocol.FrameStandingList, protocol.StandingList{Approvals: list, Error: why})
		case protocol.FrameRevoke:
			var rv protocol.Revoke
			if err := protocol.DecodeJSON(f, &rv); err != nil {
				s.sessionRefused(out, pid, err)
				return
			}
			why := s.approvals.revoke(c, rv.N)
			if why == "" {
				s.log.Info("standing approval revoked", "standing", rv.N, "pid", pid)
			}
			_ = out.writeJSON(protocol.FrameRevoked, protocol.Revoked{N: rv.N, Error: why})
		case protocol.FrameReread:
			var rr protocol.Reread
			if err := protocol.DecodeJSON(f, &rr); err != nil {
				s.sessionRefused(out, pid, err)
				return
			}
			_ = out.writeJSON(protocol.FrameRereadResult, s.reread(c, rr))
		default:
			s.sessionRefused(out, pid, fmt.Errorf("frame type %d does not belong on the approval socket", f.Type))
			return
		}
	}
}

// reread rereads the rules file for session c, which wrote rr.Rule into
// it, or, with no rule, found it changed by hand before proposing one,
// and logs both; a failed reread keeps the old rules and is logged as a
// warning.
func (s *Server) reread(c *sessionConn, rr protocol.Reread) protocol.RereadResult {
	if why := s.approvals.heard(c); why != "" {
		return protocol.RereadResult{Error: why}
	}
	if rr.Rule != "" {
		s.log.Info("rule added", "file", s.approvals.rulesFile, "rule", rr.Rule, "edited", rr.Edited, "pid", c.pid)
	} else {
		// A file changed by hand, reread before a rule is proposed.
		s.log.Info("hand-edited rules reread", "file", s.approvals.rulesFile, "pid", c.pid)
	}
	digest, err := s.rereadRules(rr.Digest)
	if err != nil {
		s.log.Warn("rules reread failed: the old rules stay", "file", s.approvals.rulesFile, "error", err)
		return protocol.RereadResult{Error: err.Error()}
	}
	s.log.Info("rules reread", "file", s.approvals.rulesFile, "digest", digest)
	res := protocol.RereadResult{Digest: digest}
	if msg, ok := s.approvals.waitingMsg(rr.ID); ok {
		// As the request will be run once answered (see ask): its checks
		// under the new rules, as a dry run.
		p, r := s.prepare(protocol.Request{Version: protocol.Version, Argv: msg.Argv, Cwd: msg.Cwd}, true)
		if r != nil {
			res.RunsOnce = r.message
		} else {
			p.close()
		}
	}
	return res
}

// waitingMsg returns the request id as sent to the session, while it
// waits for an answer.
func (a *approvals) waitingMsg(id uint64) (protocol.ApprovalRequest, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.waiting[id]
	if !ok {
		return protocol.ApprovalRequest{}, false
	}
	return r.msg, true
}

// serveSession makes c the session that serves the daemon, taking it over
// from the one serving it with takeOver, and returns why not, or "". It
// confirms with FrameServing and sends c the requests a take-over passed
// to it, each with the rest of its limit, holding c's writer from before
// c serves: no request or end of one reaches c before them. It then tells
// the session it replaced, which it drops. Take-overs run one at a time.
func (s *Server) serveSession(c *sessionConn, takeOver bool) string {
	s.approvals.takeovers.Lock()
	defer s.approvals.takeovers.Unlock()
	var (
		why   string
		old   *sessionConn
		moved []*approvalRequest
	)
	err := c.out.batch(func(send func(protocol.FrameType, any) error) error {
		if why, old, moved = s.approvals.serve(c, takeOver); why != "" {
			return nil
		}
		if err := send(protocol.FrameServing, nil); err != nil {
			return err
		}
		for _, r := range moved {
			msg := r.msg
			msg.TimeoutMs = max(time.Until(r.deadline).Milliseconds(), 1)
			if err := send(protocol.FrameApprovalRequest, msg); err != nil {
				return err
			}
		}
		return nil
	})
	if why != "" {
		return why
	}
	if err != nil {
		// As in enqueue: a session that does not take its frames is gone.
		c.conn.Close()
	}
	if old != nil {
		s.log.Info("approval session taken over", "pid", old.pid, "by", c.pid)
		_ = old.out.write(protocol.Frame{Type: protocol.FrameTakenOver})
		// Its handleSession sees the connection end; it no longer serves.
		old.conn.Close()
	}
	s.log.Info("approval session serving", "pid", c.pid)
	return ""
}

// sessionRefused reports err to the session at pid, in the log too.
func (s *Server) sessionRefused(out *frameWriter, pid int, err error) {
	s.log.Warn("approval socket connection refused", "pid", pid, "error", err)
	_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: err.Error()})
}

// ask handles FrameAsk: a command the rules allow, and whose path checks
// pass, runs as for FrameRequest. One the rules refuse, or whose path
// checks fail, waits for the serving session's answer and runs if
// approved, as the rule that refused it runs its allowed commands (in the
// mirrored directory with nothing injected when no rule matched). A
// refusal that cannot be approved is the one a plain request gets. A
// command a standing approval lets through runs at once, as for a plain
// request; the answer "for the daemon's lifetime" grants one, once the
// approved command has passed its checks.
//
// A waiting request ends unanswered when its client goes away, its limit
// runs out, its session leaves or the daemon stops; but for the first,
// the client is told why. To tell it the daemon stops, ask closes the
// connection on ctx's end itself, once nothing waits (handle does not).
func (s *Server) ask(ctx context.Context, cancel context.CancelFunc, conn net.Conn, out *frameWriter, f protocol.Frame) {
	// The connection closes when the daemon stops, as handle closes it for
	// other requests, but while a request waits: ask then tells its client
	// that the daemon stops first.
	closeOnStop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() { closeOnStop() }()
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
	timeout := defaultAskTimeout
	if ask.TimeoutMs > 0 {
		timeout = time.Duration(ask.TimeoutMs) * time.Millisecond
	}
	if timeout > maxAskTimeout {
		err := fmt.Errorf("--ask limit %s exceeds %s", timeout, maxAskTimeout)
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	need, r := s.assessApproval(req)
	if r != nil {
		s.reject(out, r.code, r.message, r.err)
		return
	}
	if need == nil {
		// It runs as a plain request does, but a refusal (a file changed
		// since the assessment) does not hint at --ask: it was asked with it.
		p, r := s.prepare(req, false)
		if r != nil {
			s.reject(out, r.code, r.message, r.err)
			return
		}
		s.start(ctx, cancel, conn, out, req, p, func() (protocol.Frame, error) { return protocol.ReadFrame(conn) })
		return
	}
	// From here on the daemon stopping does not close the connection: a
	// waiting request tells its client first (see the wait below). Had it
	// closed it already, nothing is asked.
	if !closeOnStop() {
		return
	}
	var (
		id      uint64
		request *approvalRequest
	)
	err := errNoSession
	if s.approvals != nil {
		id, request, err = s.approvals.enqueue(protocol.ApprovalRequest{
			Argv: req.Argv, Cwd: req.Cwd, Reasons: need.text.sessionReasons, Guards: need.text.guards, TimeoutMs: timeout.Milliseconds(),
		})
	}
	if err != nil {
		reason, message := "no session", noSessionMessage
		if errors.Is(err, errTooLarge) {
			reason, message = "too large", tooLargeMessage
		}
		s.log.Info("approval not answered", "reason", reason, "argv", req.Argv, "dir", need.hostDir)
		s.reject(out, protocol.ExitRejected, message, err)
		return
	}
	guarded := need.text.guarded()
	s.log.Info("approval requested", "approval_id", id, "argv", req.Argv, "dir", need.hostDir, "reasons", need.text.agentReasons, "guarded", guarded, "timeout", timeout)
	_ = out.writeJSON(protocol.FrameWaiting, protocol.Waiting{Reasons: need.text.agentReasons, Guarded: guarded})

	frames := readFrames(ctx, conn)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var o outcome
	select {
	case o = <-request.done:
	case <-frames:
		// The client sends nothing before its command starts: it went away.
		o = s.approvals.end(id, request, endedWithdrawn)
	case <-timer.C:
		o = s.approvals.end(id, request, endedExpired)
	case <-ctx.Done():
		o = s.approvals.end(id, request, endedStopping)
	}
	if o.ended != "" {
		s.log.Info("approval not answered", "approval_id", id, "reason", string(o.ended))
		if r := o.ended.refusal(); r != nil {
			s.reject(out, r.code, r.message, r.err)
		}
		return
	}
	closeOnStop = context.AfterFunc(ctx, func() { conn.Close() })
	ans := o.answer
	if ans.Scope == "" {
		s.log.Info("approval answered", "approval_id", id, "scope", "refused", "note", ans.Note)
		message := refusedMessage
		if ans.Note != "" {
			message += ": " + ans.Note
		}
		s.reject(out, protocol.ExitRejected, message, errRefused)
		return
	}
	scope := ans.Scope
	var p *prepared
	if scope == protocol.ScopeRule {
		// The session added a rule and the daemon reread it: the command
		// runs under it. The session answers once when the reread found
		// the daemon's checks refusing it still (see reread); should they
		// refuse it now all the same (the host changed meanwhile), it runs
		// as approved once, and the agent and the log are told so.
		var r *refusal
		if p, r = s.prepare(req, false); r != nil {
			scope = protocol.ScopeOnce
		}
	}
	s.log.Info("approval answered", "approval_id", id, "scope", scope)
	_ = out.writeJSON(protocol.FrameApproved, protocol.Approved{Scope: scope})
	// Only the path failures the human saw are passed as given, for the
	// reason they saw: one that fails its check only now, or otherwise (a
	// file swapped for a symlink meanwhile), refuses the command.
	seen := seenPaths(need.pathFailures)
	if p == nil {
		var r *refusal
		if p, r = s.prepareAllowed(req, need.approved, false, seen); r != nil {
			s.reject(out, r.code, r.message, r.err)
			return
		}
	}
	if scope == protocol.ScopeLifetime {
		// Granted once the approved command has passed its checks, so a
		// request refused at its start leaves nothing standing.
		n := s.approvals.grant(req, guarded, seen)
		s.log.Info("standing approval granted", "standing", n, "argv", req.Argv, "dir", need.hostDir, "approval_id", id)
	}
	s.start(ctx, cancel, conn, out, req, p, frames.next, "approved", scope, "approval_id", id)
}

// approvalNeed is what --ask would ask a human about a request: what the
// command would run with, the path failures that would be passed as
// given, the host directory it would run from (for the log), and how the
// request reads.
type approvalNeed struct {
	approved     rules.Allowed
	pathFailures []pathFailure
	hostDir      string
	text         requestText
}

// assessApproval runs the checks --ask runs on req before asking a human:
// the rules, then, as a dry run, every check an approval cannot lift
// (directories, secrets' sources, the program), with the path checks
// collected rather than refusing. It returns nil and nil when req would run
// without asking (the rules, or a standing approval, let it through), a
// refusal when it would be refused without asking, or what the human
// would be asked. A command whose standing approval no longer lets it
// through (a path check failing for a reason the human did not see) is
// assessed as if it had none, so the human is asked again.
func (s *Server) assessApproval(req protocol.Request) (*approvalNeed, *refusal) {
	if s.approvals.standingFor(req) != nil {
		p, r := s.prepare(req, true)
		if r == nil {
			r = lookupStart(p, req)
		}
		if r == nil {
			p.close()
			return nil, nil
		}
	}
	approved, err := s.rules().Check(req.Argv)
	var d *rules.Denial
	if err != nil {
		var ok bool
		if d, ok = rules.ApprovableDenial(err); !ok {
			return nil, &refusal{protocol.ExitRejected, err.Error(), err}
		}
		approved = d.Approved
	}
	// Starting the command checks all of it again. Every path check may
	// fail here: its failure is asked about.
	p, r := s.prepareAllowed(req, approved, true, pathPasses{all: true})
	if r == nil {
		r = lookupStart(p, req)
	}
	if r != nil {
		return nil, r
	}
	p.close()
	if d == nil && len(p.pathFailures) == 0 {
		return nil, nil
	}
	return &approvalNeed{
		approved:     approved,
		pathFailures: p.pathFailures,
		hostDir:      p.hostPath,
		text:         describeRequest(d, approved, p.pathFailures),
	}, nil
}

// withAskHint returns r, the refusal of a plain request or dry run of req,
// with a hint at --ask added to its message when a session serves the
// daemon and --ask would ask a human about req; the hint says whether
// approving it is guarded, judged over every reason. Otherwise it returns
// r.
func (s *Server) withAskHint(req protocol.Request, r *refusal) *refusal {
	if s.approvals == nil || !s.approvals.served() {
		return r
	}
	// A refusal --ask would get comes without what it would ask.
	need, _ := s.assessApproval(req)
	if need == nil {
		return r
	}
	hint := "ask for an approval: hostrun --ask …"
	if need.text.guarded() {
		hint += "; guarded: needs a second confirmation"
	}
	return &refusal{r.code, r.message + " (" + hint + ")", r.err}
}

// requestText is how an approval request reads: its reasons for the
// client and the log, the first naming the rule that refused it, as a
// refusal does; the same reasons as the session shows them, each also
// naming a deny list that gave it; and its guarded reasons, one line each.
type requestText struct {
	agentReasons   []string
	sessionReasons []string
	guards         []string
}

// guarded reports whether approving the request needs a warning and a
// second confirmation: it has a guard.
func (t requestText) guarded() bool { return len(t.guards) > 0 }

// describeRequest describes a request the rules refused as d says, or,
// with d nil, one they allowed, and whose arguments failed their path
// checks as pathFailures say; approved is what the command would run
// with.
func describeRequest(d *rules.Denial, approved rules.Allowed, pathFailures []pathFailure) requestText {
	var text requestText
	if d != nil {
		for i, reason := range d.Reasons {
			agent := reason.Text
			if i == 0 {
				agent = d.Describe(agent)
			}
			session := agent
			if reason.Kind == rules.ReasonDenied {
				session += " (deny list)"
			}
			text.agentReasons = append(text.agentReasons, agent)
			text.sessionReasons = append(text.sessionReasons, session)
			if guard := reason.Guard(); guard != "" {
				text.guards = append(text.guards, guard)
			}
		}
	}
	for _, failure := range pathFailures {
		text.agentReasons = append(text.agentReasons, failure.message)
		text.sessionReasons = append(text.sessionReasons, failure.message)
		text.guards = append(text.guards, "path check: "+failure.message)
	}
	if names := approved.SecretNames(); len(names) > 0 {
		text.guards = append(text.guards, "injects secrets: "+strings.Join(names, ", "))
	}
	return text
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
