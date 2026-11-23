// Package shared implements the shared process, `hostrunner shared`: one
// host process that holds the daemons of several devcontainers, one
// process per build of hostrunner (see docs/adr/0010-shared-process.md).
//
// The process listens on its control socket in the runtime root, where
// `hostrunner up` asks it, with Add, to add a daemon; Serve runs the
// daemons it is asked for, each under the process's context, and returns
// once the last of them ended, or once its first add did not come in
// time. How a daemon is made from a request is the caller's (see
// Prepare), so this package knows nothing of serve.
//
// A daemon gets from its request its arguments and environment only: the
// attributes of the process not carried in the request, such as its
// umask, resource limits and session, are those of the process, which it
// has from the `hostrunner up` that started it, whichever up added the
// daemon.
package shared

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/contain"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// requestTimeout bounds one exchange on the control socket, connecting
// included, on either side.
const requestTimeout = 5 * time.Second

// idLength is how many hex digits of the executable's digest name the
// files of its shared process.
const idLength = 16

// ErrNotListening means nothing accepts connections on the control socket:
// no shared process of this build runs, and one has to be started.
var ErrNotListening = errors.New("no shared process is listening")

// ErrEnding means the shared process asked to add a daemon is ending and
// added nothing: it refused the add once its end was decided, or closed
// the connection without an answer, as when it closed its control socket
// with the request still waiting to be accepted. A new process has to be
// started, and the daemon added to it.
var ErrEnding = errors.New("the shared process is ending")

// Files returns the control socket and the log of the shared process of
// the hostrunner executable exe, in the runtime root root: shared-<id>.sock
// and shared-<id>.log, id the first 16 hex digits of the sha256 of exe's
// content, so each build has a process of its own. Neither name ends in
// protocol.ApprovalSuffix, so neither is taken for a daemon's approval
// socket. An executable that cannot be read is an error.
func Files(root, exe string) (socket, logPath string, err error) {
	f, err := os.Open(exe)
	if err != nil {
		return "", "", fmt.Errorf("digest of %s: %w", hostterm.Display(exe), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", fmt.Errorf("digest of %s: %w", hostterm.Display(exe), err)
	}
	name := "shared-" + hex.EncodeToString(h.Sum(nil))[:idLength]
	return filepath.Join(root, name+".sock"), filepath.Join(root, name+".log"), nil
}

// LockFile returns the lock file of the shared process whose control
// socket, as Files returns it, is socket: shared-<id>.lock beside it, in
// the runtime root. `hostrunner up` holds a flock on it while it adds a
// daemon to that process, starting the process when none listens, so
// that concurrent ups of one build start one process. It is derived from
// the socket rather than returned by Files, so that the callers that need
// only the socket and the log digest the executable once and name no lock
// they do not take. Its name does not end in protocol.ApprovalSuffix
// either. The file is never removed, on purpose: unlinking a file that
// others flock breaks their mutual exclusion, as an up waiting for the
// lock on the old inode and one opening the path anew, on a new inode,
// would both hold "the" lock.
func LockFile(socket string) string {
	return strings.TrimSuffix(socket, ".sock") + ".lock"
}

// Daemon is a daemon the process was asked to add, ready to run.
type Daemon struct {
	// Socket is the daemon's socket, which the process log names it by.
	Socket string
	// Log is the daemon's own log, which Serve logs to. Once Serve
	// returned, the process writes there the error it returned, or the
	// panic it ended with, with its stack (see serveDaemon), as
	// "hostrunner: <error>", as main prints the error of a daemon of its
	// own process to its log, then closes it; any other panic of the
	// daemon while it is open goes there too, likewise (see start).
	Log io.WriteCloser
	// Serve runs the daemon until it ends, or until ctx is cancelled.
	Serve func(ctx context.Context) error
}

// Prepare makes the Daemon an AddDaemon request asks for from its Args and
// Env (see protocol.AddDaemon), without starting it; an error, refused
// arguments or a log that cannot be opened, is the reason the request is
// refused with.
type Prepare func(args, env []string) (Daemon, error)

// Serve runs the shared process on l, its control socket: it takes the
// AddDaemon requests of connections from its own user (others are
// refused with FrameError), makes each daemon with prepare and runs it in
// a goroutine under a context derived from ctx, answering
// FrameDaemonAdded once it started, FrameEnding when it refused the add
// as its end was decided (see ErrEnding), or FrameError with any other
// reason. It logs its start, each daemon added and ended, and its exit to
// log. Once the last daemon it added has ended, or an add was refused
// while no daemon runs, or no daemon started within firstAdd of its start
// (an add still in flight then is refused), or ctx is cancelled, which
// ends every daemon, it closes l, waits for the daemons and the
// connections it accepted and returns; nil unless l fails otherwise. A
// connection whose request is not yet read then is not waited for (see
// handle). Accept errors of a momentary resource shortage (see
// transport.IsTemporary) are retried, with a backoff, rather than ending
// the process. A daemon that panics ends alone (see serveDaemon). Serve
// closes l before it returns, whatever ends it.
func Serve(ctx context.Context, l net.Listener, prepare Prepare, firstAdd time.Duration, log *slog.Logger) error {
	defer l.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &process{prepare: prepare, log: log, cancel: cancel}
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	timer := time.AfterFunc(firstAdd, func() { p.endIfNoneAdded(ctx, firstAdd) })
	defer timer.Stop()
	log.Info("shared process started", "socket", l.Addr().String(), "pid", os.Getpid())
	var (
		err     error
		backoff time.Duration
	)
	for {
		c, acceptErr := l.Accept()
		if acceptErr != nil {
			if transport.IsTemporary(acceptErr) && ctx.Err() == nil {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				log.Warn("accept failed, retrying", "error", acceptErr, "backoff", backoff)
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
				}
			}
			if ctx.Err() == nil {
				err = acceptErr
			}
			break
		}
		backoff = 0
		p.wg.Go(func() { p.handle(ctx, c) })
	}
	cancel()
	p.wg.Wait()
	log.Info("shared process exiting", "error", err)
	return err
}

// process is the state of a running Serve.
type process struct {
	prepare Prepare
	log     *slog.Logger
	cancel  context.CancelFunc // ends the process: Serve's context
	wg      sync.WaitGroup     // the connections and the daemons

	mu      sync.Mutex
	daemons int  // the daemons running
	ending  bool // the end is decided: no daemon starts any more
}

// handle answers the one request of the control connection c, then
// closes it. A connection from another user is answered FrameError once
// its request was read, unlooked at, and logged refused, but not as a
// refused add. Once the end is decided, as ctx is done, the request is
// not waited for any more: a connection that sent no byte of it then is
// answered FrameEnding, with no add logged refused, and one cut in the
// middle of it is refused with ErrEnding. A connection closed before any
// byte of a request is a probe, neither logged nor answered.
func (p *process) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	uid, pid, err := transport.PeerCred(c)
	if err != nil {
		p.log.Warn("control socket connection refused", "uid", uid, "pid", pid, "error", err)
		return
	}
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	// Once the end is decided, the request is not waited for any more, so
	// that a connection that sends nothing does not keep the process
	// alive: its read is cut at once.
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()) })
	defer stop()
	if uid != os.Getuid() {
		p.log.Warn("control socket connection refused", "uid", uid, "pid", pid, "reason", "the peer's uid is not this user's")
		// The request is read, unlooked at, before the answer, so that
		// the peer's write of it does not fail on a connection already
		// closed, which Add would take for ErrEnding.
		_, _ = protocol.ReadFrame(c)
		_ = protocol.WriteJSON(c, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "refused: the control socket serves its own user only"})
		return
	}
	request := &countingReader{r: c}
	f, err := protocol.ReadFrame(request)
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
			// A connection closed before any byte of a request, as from
			// up's check that the socket listens, is a probe: no add was
			// refused, so nothing is logged or answered and the process
			// goes on.
		case ctx.Err() != nil && errors.Is(err, os.ErrDeadlineExceeded) && request.n == 0:
			// Cut before any byte of a request, the connection asked for
			// nothing, so no add is logged refused, but it is answered,
			// if it can still take it, that the process is ending.
			_ = protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameEnding})
		case ctx.Err() != nil && errors.Is(err, os.ErrDeadlineExceeded):
			// Cut in the middle of a request, the add is refused as the
			// process is ending.
			p.refuse(c, pid, fmt.Errorf("read the request: %w", ErrEnding))
		default:
			// A partial or malformed request is refused.
			p.refuse(c, pid, fmt.Errorf("read the request: %w", err))
		}
		return
	}
	req, err := decodeAdd(f)
	if err == nil {
		err = p.addContained(ctx, req, pid)
	}
	if err != nil {
		p.refuse(c, pid, err, requestSocket(req.Args)...)
		p.endIfIdle()
		return
	}
	_ = protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameDaemonAdded})
}

// refuse logs that the request on c, of the process pid, is refused for
// err, with the attributes attrs naming the daemon it asked for, if any,
// and answers it: with FrameEnding when err is ErrEnding, so that up
// starts a new process, otherwise with err in a FrameError.
func (p *process) refuse(c net.Conn, pid int, err error, attrs ...any) {
	p.log.Warn("add refused", append(append([]any{"pid", pid}, attrs...), "error", err)...)
	if errors.Is(err, ErrEnding) {
		_ = protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameEnding})
		return
	}
	_ = protocol.WriteJSON(c, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: err.Error()})
}

// requestSocket returns the log attributes of the socket that args, the
// arguments of serve in an AddDaemon request, give with --socket, and of
// its runtime directory; none when they give none. It parses only that
// flag, to name the daemon of a request refused, maybe for its args.
func requestSocket(args []string) []any {
	for i, arg := range args {
		socket, ok := strings.CutPrefix(arg, "--socket=")
		if !ok && arg == "--socket" && i+1 < len(args) {
			socket, ok = args[i+1], true
		}
		if ok {
			return socketAttrs(socket)
		}
	}
	return nil
}

// socketAttrs returns the log attributes of the daemon socket socket and of
// its runtime directory.
func socketAttrs(socket string) []any {
	return []any{"socket", socket, "runtime_dir", filepath.Dir(socket)}
}

// endIfIdle ends the process when it runs no daemon, as after a refused
// add nothing else would.
func (p *process) endIfIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.daemons == 0 {
		p.endLocked()
	}
}

// endLocked decides the end of the process: no daemon starts any more, and
// its context is cancelled. It requires p.mu held.
func (p *process) endLocked() {
	p.ending = true
	p.cancel()
}

// endIfNoneAdded ends the process, logging so, when no daemon started
// within firstAdd, the time it was given for its first add: nothing else
// would end it. A process that started a daemon ends with its last, and
// one whose end is already decided, or whose ctx is done, ends anyway: it
// is left as it is.
func (p *process) endIfNoneAdded(ctx context.Context, firstAdd time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ending || p.daemons > 0 || ctx.Err() != nil {
		return
	}
	p.log.Info("no daemon added", "timeout", firstAdd)
	p.endLocked()
}

// decodeAdd returns the AddDaemon request f carries, an error when f is
// not one of this version.
func decodeAdd(f protocol.Frame) (protocol.AddDaemon, error) {
	var req protocol.AddDaemon
	if f.Type != protocol.FrameAddDaemon {
		return req, fmt.Errorf("unexpected frame type %d on the control socket", f.Type)
	}
	if err := protocol.DecodeJSON(f, &req); err != nil {
		return req, err
	}
	if req.Version != protocol.SharedVersion {
		return req, fmt.Errorf("control protocol version %d, want %d", req.Version, protocol.SharedVersion)
	}
	return req, nil
}

// add prepares the daemon req, of the process pid, asks for and starts it
// (see start); an error is the reason it refuses the request.
func (p *process) add(ctx context.Context, req protocol.AddDaemon, pid int) error {
	d, err := p.prepare(req.Args, req.Env)
	if err != nil {
		return err
	}
	return p.start(ctx, d, pid)
}

// addContained is add within a containment of its own (see package
// contain), so that a panic of the preparation of the daemon, as of its
// arguments or its log, refuses req alone, not the process and its
// daemons: the panic is logged, with its stack, to the process log, and
// the error returned, the reason req is refused with, says only that the
// add failed.
func (p *process) addContained(ctx context.Context, req protocol.AddDaemon, pid int) error {
	// The daemon runs under ctx, not under the containment's context,
	// which ends with this call.
	cctx, cancel := contain.Within(ctx, nil)
	defer cancel()
	err := contain.Call(cctx, func() error { return p.add(ctx, req, pid) })
	var pe *contain.PanicError
	if errors.As(err, &pe) {
		p.log.Error("add panicked", "pid", pid, "error", pe)
		return errors.New("the shared process failed to add the daemon: internal error")
	}
	return err
}

// start runs d, asked for by the process pid, in a goroutine under ctx,
// counted among the daemons, and logs it added as it starts to run; when its
// Serve returns, it logs that it ended and, for the last daemon, ends the
// process. A panic of d other than the one it ends with (see serveDaemon),
// a later one or one after its end, is logged, with its stack, to the
// process log and to d's log while it is open. Once the end is decided,
// or ctx is done, it refuses d, closing its log, with ErrEnding: d could
// not run.
func (p *process) start(ctx context.Context, d Daemon, pid int) error {
	p.mu.Lock()
	if p.ending || ctx.Err() != nil {
		p.mu.Unlock()
		d.Log.Close()
		return ErrEnding
	}
	p.daemons++
	p.mu.Unlock()
	log := &daemonLog{w: d.Log}
	others := func(pe *contain.PanicError) {
		p.log.Error("daemon panicked again", "socket", d.Socket, "error", pe)
		fmt.Fprintf(log, "hostrunner: %v\n", pe)
	}
	p.wg.Go(func() {
		p.log.Info("daemon added", append(socketAttrs(d.Socket), "pid", pid)...)
		err := serveDaemon(ctx, d, others)
		if err != nil {
			fmt.Fprintf(log, "hostrunner: %v\n", err)
		}
		log.Close()
		p.log.Info("daemon ended", "socket", d.Socket, "error", err)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.daemons--; p.daemons == 0 {
			p.endLocked()
		}
	})
	return nil
}

// serveDaemon runs d's Serve under a context derived from ctx, within a
// containment of its own (see package contain), and returns its error. A
// panic of Serve, or of code d runs under that context's containment, as
// in a goroutine it starts with contain.Go, ends d alone, not the process
// and its other daemons: the first one cancels d's context, as a stop
// with --kill would, and once Serve returned it is returned as the error,
// with the stack, so it reaches d's log and the process log as any error
// d ends with. Every other panic, a later one or one after Serve
// returned, is passed to others.
func serveDaemon(ctx context.Context, d Daemon, others func(*contain.PanicError)) error {
	ctx, cancel := contain.Within(ctx, others)
	defer cancel() // when Serve panicked
	return contain.Call(ctx, func() error {
		err := d.Serve(ctx)
		// Cancelled here, before Call looks for the panic kept: one
		// contained from now on goes to others, not kept unread.
		cancel()
		return err
	})
}

// daemonLog is a daemon's log that Writes after Close drop, safe for
// concurrent use, as a panic of the daemon may be logged there from any
// goroutine, even once it ended.
type daemonLog struct {
	mu     sync.Mutex
	w      io.WriteCloser
	closed bool
}

// Write writes p to l.w, or, once l is closed, drops it and returns
// os.ErrClosed.
func (l *daemonLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	return l.w.Write(p)
}

// Close closes l.w; Writes after it are dropped.
func (l *daemonLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return l.w.Close()
}

// Add asks the shared process on its control socket, socket, to add the
// daemon req describes, setting req.Version. It returns once the process
// answered that it added it, with an error carrying the process's reason
// when it refused; an error wrapping ErrNotListening when nothing accepts
// connections on socket; an error wrapping ErrEnding when the process
// answered that it is ending (FrameEnding), or closed or reset the
// connection before any byte of an answer, as one ending with the request
// still in its listener's backlog does, whether Add was still writing the
// request then (the write fails with EPIPE or ECONNRESET) or already
// reading the answer (see closedByPeer). A connection closed in the middle
// of an answer is an ordinary error: the process answered.
func Add(ctx context.Context, socket string, req protocol.AddDaemon) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	c, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotListening, err)
	}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	name := hostterm.Display(socket)
	unanswered := fmt.Errorf("shared process on %s closed the connection unanswered: %w", name, ErrEnding)
	req.Version = protocol.SharedVersion
	if err := protocol.WriteJSON(c, protocol.FrameAddDaemon, req); err != nil {
		if closedByPeer(err) {
			return unanswered
		}
		return fmt.Errorf("shared process on %s did not accept the add request: %w", name, err)
	}
	answer := &countingReader{r: c}
	f, err := protocol.ReadFrame(answer)
	if answer.n == 0 && closedByPeer(err) {
		return unanswered
	}
	if err != nil {
		return fmt.Errorf("shared process on %s did not answer the add request: %w", name, err)
	}
	switch f.Type {
	case protocol.FrameDaemonAdded:
		return nil
	case protocol.FrameEnding:
		return fmt.Errorf("shared process on %s did not add the daemon: %w", name, ErrEnding)
	case protocol.FrameError:
		var e protocol.Error
		_ = protocol.DecodeJSON(f, &e)
		return fmt.Errorf("shared process on %s refused to add the daemon: %s", name, e.Message)
	default:
		return fmt.Errorf("shared process on %s answered the add request with frame type %d", name, f.Type)
	}
}

// closedByPeer reports whether err, of a read or a write on a control
// connection, says that the peer closed or reset it: io.EOF, EPIPE or
// ECONNRESET. Add takes it for ErrEnding while no byte of an answer came.
func closedByPeer(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// countingReader is a reader that counts the bytes read through it, so
// that Add tells a connection closed before any byte of an answer from
// one closed in the middle of it, which a reset does not.
type countingReader struct {
	r io.Reader
	n int64
}

// Read reads from r.r, adding the bytes read to r.n.
func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}
