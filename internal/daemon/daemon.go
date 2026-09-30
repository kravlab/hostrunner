// Package daemon runs host commands on behalf of hostrun clients.
//
// Each connection carries exactly one command. The daemon maps the client's
// container cwd onto the host workspace, starts the command with the host's
// environment, streams its stdio over the connection and finishes with the
// exit code. If the client disconnects, the command's process group is killed.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// waitDelay bounds how long the daemon keeps forwarding output after the
// command exits, so a background grandchild holding stdout open cannot keep
// the client waiting forever.
const waitDelay = 2 * time.Second

// defaultRequestTimeout bounds how long a new connection may take to send
// its request, so idle connections cannot pile up and exhaust descriptors.
const defaultRequestTimeout = 10 * time.Second

// Server accepts client connections and runs their commands.
type Server struct {
	mapper         *workspace.Mapper
	log            *slog.Logger
	requestTimeout time.Duration
	onArm          func(protocol.Arm) (restart bool) // nil: arming changes nothing
	policy         Policy

	mu        sync.Mutex
	stopServe context.CancelFunc // stops the running Serve; nil when not serving
}

// Policy decides whether a command may run; rules.Policy implements it. A
// non-nil error denies the command, and its message is shown to the client,
// so it must not reveal host paths.
type Policy interface {
	Check(argv []string) error
}

// Option customizes a Server.
type Option func(*Server)

// WithRequestTimeout sets how long a connection may take to send its
// request before it is rejected (default 10 s).
func WithRequestTimeout(d time.Duration) Option {
	return func(s *Server) { s.requestTimeout = d }
}

// WithArmHandler sets what the Server does when `hostrunner up` arms it on
// a devcontainer start (e.g. rearm the container watcher). If f returns
// true, the Server answers that it is restarting and then stops Serve, so
// `up` can start a daemon with fresh rules. f is called from connection
// goroutines, so it must be safe for concurrent use.
func WithArmHandler(f func(protocol.Arm) (restart bool)) Option {
	return func(s *Server) { s.onArm = f }
}

// New returns a Server that runs the commands policy allows, confined to
// mapper's workspace. The policy is required so that no Server can run
// commands unchecked by accident.
func New(mapper *workspace.Mapper, policy Policy, log *slog.Logger, opts ...Option) *Server {
	if policy == nil {
		panic("daemon: nil Policy")
	}
	s := &Server{mapper: mapper, policy: policy, log: log, requestTimeout: defaultRequestTimeout}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Serve accepts connections on l until ctx is cancelled, an arm handler asks
// for a restart, or l fails; then it closes l, kills running commands and
// returns once every connection is finished. Temporary accept errors (e.g.
// out of file descriptors) are retried with backoff. It returns nil when
// stopped by cancellation or a restart, and the accept error otherwise.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.stopServe = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stopServe = nil
		s.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()

	var (
		wg      sync.WaitGroup
		backoff time.Duration
	)
	for {
		conn, err := l.Accept()
		if err != nil {
			if isTemporary(err) && ctx.Err() == nil {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				s.log.Warn("accept failed, retrying", "error", err, "backoff", backoff)
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
				}
			}
			stopped := ctx.Err() != nil // cancelled by the caller or a restart
			cancel()
			wg.Wait()
			if stopped {
				return nil
			}
			return err
		}
		backoff = 0
		wg.Go(func() { s.handle(ctx, conn) })
	}
}

// isTemporary reports accept errors caused by momentary resource shortage,
// after which accepting can succeed again.
func isTemporary(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// handle serves one connection: an arm request or one command. Cancelling
// ctx, or the client going away, kills the command without reporting a
// result.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	out := &frameWriter{conn: conn}
	// A bug triggered by one request (e.g. in the policy) must not take the
	// whole daemon down with every other container's session.
	defer func() {
		if r := recover(); r != nil {
			s.reject(out, protocol.ExitHostrunError, "internal error in the hostrunner daemon", fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(s.requestTimeout))
	f, err := readOpening(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	if f.Type == protocol.FrameArm {
		s.arm(out, f)
		return
	}
	req, err := decodeRequest(f)
	if err != nil {
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	s.run(ctx, cancel, conn, out, req)
}

// arm handles FrameArm from `hostrunner up`. When the handler asks for a
// restart, the answer is written before Serve is stopped, since stopping
// closes every connection.
func (s *Server) arm(out *frameWriter, f protocol.Frame) {
	var a protocol.Arm
	if err := protocol.DecodeJSON(f, &a); err != nil {
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	restart := s.onArm != nil && s.onArm(a)
	s.log.Info("armed by hostrunner up", "restart", restart)
	_ = out.writeJSON(protocol.FrameArmed, protocol.Armed{Version: protocol.Version, Restart: restart})
	if restart {
		s.mu.Lock()
		stop := s.stopServe
		s.stopServe = nil
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
	}
}

// run executes req on the connection if the policy allows it; cancel stops
// it early. The rules are checked first, so a denied command is reported as
// such whatever its working directory.
func (s *Server) run(ctx context.Context, cancel context.CancelFunc, conn net.Conn, out *frameWriter, req protocol.Request) {
	if err := s.policy.Check(req.Argv); err != nil {
		s.reject(out, protocol.ExitRejected, err.Error(), err)
		return
	}
	dir, hostPath, err := s.mapper.Open(req.Cwd)
	if err != nil {
		s.reject(out, protocol.ExitRejected, cwdMessage(req.Cwd, err), err)
		return
	}
	defer dir.Close()

	cmd := newCommand(ctx, req.Argv, dir, hostPath, out)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.reject(out, protocol.ExitHostrunError, "cannot start the command", err)
		return
	}
	if err := cmd.Start(); err != nil {
		code, message := startFailure(req.Argv[0], err)
		s.reject(out, code, message, err)
		return
	}

	queue := newStdinQueue(protocol.StdinWindow)
	var streams sync.WaitGroup
	streams.Go(func() {
		if err := readClient(conn, queue); err != nil {
			s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		}
		// The client is gone or broke the protocol: nobody is left to
		// receive the result, so stop the command.
		cancel()
	})
	streams.Go(func() { feedStdin(queue, stdin, out) })
	_ = out.writeJSON(protocol.FrameStdinCredit, protocol.Credit{Bytes: protocol.StdinWindow})

	waitErr := cmd.Wait()
	if ctx.Err() == nil {
		code := exitCode(cmd.ProcessState)
		s.log.Info("command finished", "argv", req.Argv, "dir", hostPath, "code", code, "wait_error", waitErr)
		_ = out.writeJSON(protocol.FrameExit, protocol.Exit{Code: code})
	} else {
		s.log.Info("command cancelled", "argv", req.Argv, "dir", hostPath)
	}
	conn.Close()
	streams.Wait()
}

// reject reports a command that could not run. The client gets message,
// which must not reveal host paths; the full err goes to the daemon log.
func (s *Server) reject(out *frameWriter, code int, message string, err error) {
	s.log.Warn("request rejected", "code", code, "error", err)
	_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: code, Message: message})
}

// cwdMessage describes a failed cwd mapping in container terms only.
func cwdMessage(cwd string, err error) string {
	if errors.Is(err, workspace.ErrOutsideWorkspace) {
		return fmt.Sprintf("working directory %s is outside the workspace", cwd)
	}
	return fmt.Sprintf("working directory %s is not available on the host", cwd)
}

// readOpening reads the opening frame of a connection (FrameRequest or
// FrameArm) and checks its protocol version before anything else, so a
// frame shaped by a newer protocol is reported as a version mismatch rather
// than a decoding error.
func readOpening(conn net.Conn) (protocol.Frame, error) {
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return f, fmt.Errorf("read request: %w", err)
	}
	if f.Type != protocol.FrameRequest && f.Type != protocol.FrameArm {
		return f, fmt.Errorf("expected a request frame, got type %d", f.Type)
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := protocol.DecodeJSON(f, &header); err != nil {
		return f, err
	}
	if header.Version != protocol.Version {
		return f, fmt.Errorf("protocol version mismatch: client %d, daemon %d", header.Version, protocol.Version)
	}
	return f, nil
}

// decodeRequest decodes and validates a FrameRequest.
func decodeRequest(f protocol.Frame) (protocol.Request, error) {
	var req protocol.Request
	if err := protocol.DecodeJSON(f, &req); err != nil {
		return req, err
	}
	if len(req.Argv) == 0 {
		return req, errors.New("empty command")
	}
	return req, nil
}

// newCommand prepares argv to run in the open directory dir, whose real
// path is hostPath. The child changes into the directory through
// /proc/self/fd (it inherits the descriptor until exec), so a symlink the
// container swaps into the path after the check cannot redirect it; PWD is
// set to hostPath because the /proc path is meaningless once the child runs.
// The environment is otherwise the daemon's (host) environment; nothing
// comes from the container. The command gets its own process group so
// cancellation also kills whatever it spawned (e.g. git's ssh).
func newCommand(ctx context.Context, argv []string, dir *os.File, hostPath string, out *frameWriter) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = fmt.Sprintf("/proc/self/fd/%d", dir.Fd())
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "PWD=")
	}), "PWD="+hostPath)
	cmd.Stdout = streamWriter{out: out, frame: protocol.FrameStdout}
	cmd.Stderr = streamWriter{out: out, frame: protocol.FrameStderr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	return cmd
}

// startFailure classifies why program could not start and describes it
// without host paths.
func startFailure(program string, err error) (int, string) {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return protocol.ExitNotFound, fmt.Sprintf("command %s not found on the host", program)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return protocol.ExitRejected, fmt.Sprintf("cannot run %s: %v", program, errno)
	}
	return protocol.ExitRejected, fmt.Sprintf("cannot run %s", program)
}

// exitCode converts a finished process state to a shell-style exit code:
// 128+N when killed by signal N, the exit status otherwise.
func exitCode(state *os.ProcessState) int {
	if state == nil {
		return protocol.ExitHostrunError
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

// readClient reads the client→daemon stream after the request and queues
// stdin for feedStdin. It never blocks on the command, so a disconnect is
// always noticed. It returns nil when the connection ends and an error when
// the client breaks the protocol. The queue is closed either way.
func readClient(conn net.Conn, queue *stdinQueue) error {
	defer queue.close()
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			return nil
		}
		switch f.Type {
		case protocol.FrameStdin:
			if err := queue.push(f.Payload); err != nil {
				return err
			}
		case protocol.FrameStdinClose:
			queue.close()
		default:
			return fmt.Errorf("unexpected frame type %d from the client", f.Type)
		}
	}
}

// feedStdin writes queued stdin to the command and grants the client credit
// for every byte taken off the queue. Bytes the command does not accept
// (it closed stdin or exited) are dropped but still credited, so the client
// is never left waiting. Closing the drained queue closes the command's stdin.
func feedStdin(queue *stdinQueue, stdin io.WriteCloser, out *frameWriter) {
	defer stdin.Close()
	for {
		data, ok := queue.pop()
		if !ok {
			return
		}
		_, _ = stdin.Write(data)
		queue.release(len(data))
		_ = out.writeJSON(protocol.FrameStdinCredit, protocol.Credit{Bytes: len(data)})
	}
}

// Errors a client causes by misusing stdin.
var (
	errStdinOverflow = errors.New("client sent more stdin than it was granted")
	errStdinClosed   = errors.New("client sent stdin after closing it")
)

// stdinQueue buffers stdin between readClient and feedStdin and mirrors the
// client's credit: outstanding counts bytes received but not yet credited
// back. A client that respects its credit never pushes outstanding past
// limit, so doing so is a protocol violation rather than a reason to block.
type stdinQueue struct {
	mu          sync.Mutex
	ready       sync.Cond
	buf         []byte
	outstanding int
	limit       int
	closed      bool
}

// newStdinQueue returns an empty queue that allows limit outstanding bytes.
func newStdinQueue(limit int) *stdinQueue {
	q := &stdinQueue{limit: limit}
	q.ready.L = &q.mu
	return q
}

// push appends p, failing if the queue is closed or p exceeds the credit.
func (q *stdinQueue) push(p []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.closed:
		return errStdinClosed
	case q.outstanding+len(p) > q.limit:
		return errStdinOverflow
	}
	q.outstanding += len(p)
	q.buf = append(q.buf, p...)
	q.ready.Signal()
	return nil
}

// pop blocks until data is queued or the queue is closed, and takes all
// queued data. It reports false once the queue is closed and drained.
func (q *stdinQueue) pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 && !q.closed {
		q.ready.Wait()
	}
	if len(q.buf) == 0 {
		return nil, false
	}
	data := q.buf
	q.buf = nil
	return data, true
}

// release returns n bytes of credit once feedStdin has handled them.
func (q *stdinQueue) release(n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.outstanding -= n
}

// close marks the end of stdin; queued data is still delivered by pop.
func (q *stdinQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.ready.Broadcast()
}

// frameWriter serializes frame writes from the stdout and stderr copiers and
// the final status frame onto one connection.
type frameWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

// write sends one frame.
func (w *frameWriter) write(f protocol.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return protocol.WriteFrame(w.conn, f)
}

// writeJSON sends v as a JSON frame of type t.
func (w *frameWriter) writeJSON(t protocol.FrameType, v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return protocol.WriteJSON(w.conn, t, v)
}

// streamWriter turns a command's output stream into frames of one type,
// splitting writes that exceed protocol.MaxPayload.
type streamWriter struct {
	out   *frameWriter
	frame protocol.FrameType
}

// Write sends p as one or more frames; it fails once the connection is gone.
func (w streamWriter) Write(p []byte) (int, error) {
	for sent := 0; sent < len(p); {
		n := min(len(p)-sent, protocol.MaxPayload)
		if err := w.out.write(protocol.Frame{Type: w.frame, Payload: p[sent : sent+n]}); err != nil {
			return sent, err
		}
		sent += n
	}
	return len(p), nil
}
