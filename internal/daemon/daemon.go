// Package daemon runs host commands on behalf of hostrun clients.
//
// Each connection carries exactly one command. The daemon maps the client's
// current directory onto the host workspace (the mirrored directory),
// starts the command there, or in the fixed directory its rule names, with
// the host's environment, streams its stdio over the connection and
// finishes with the exit code. If the client disconnects, the command's process group is killed.
//
// With an approval socket (WithApprovals), a command the rules refuse can
// be approved by a human: the client asks with FrameAsk, and the request
// waits for the answer of the approval session that serves the daemon (see
// approvals.go). The approval socket lies outside what the container
// mounts, and takes no frames from the container's socket, nor that socket
// any of its frames. A session that adds a rule to the rules file makes the
// daemon reread it (WithReload).
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
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
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
	onArm          func(protocol.Arm) (stepAside bool) // nil: arming changes nothing
	reload         func() (Policy, string, error)      // nil: the rules are never reread
	approvals      *approvals                          // nil: no approval socket, so nothing is approved

	mu        sync.Mutex
	stopServe context.CancelFunc // stops the running Serve; nil when not serving
	policy    Policy             // the rules served; see rules
	digest    string             // their digest; see RulesDigest
}

// Policy decides whether a command may run; rules.Policy implements it. A
// non-nil error denies the command, and its message is shown to the client,
// so it must not reveal host paths. For an allowed command it returns the
// allowing rule and the arguments the daemon still has to check as
// workspace files.
type Policy interface {
	Check(argv []string) (rules.Allowed, error)
}

// Option customizes a Server.
type Option func(*Server)

// WithRequestTimeout sets how long a connection may take to send its
// request before it is rejected (default 10 s).
func WithRequestTimeout(d time.Duration) Option {
	return func(s *Server) { s.requestTimeout = d }
}

// WithReload lets an approval session make the Server reread its rules
// file, once it has added a rule to it. digest identifies the rules the
// Server starts with (rules.Policy.Digest); reload returns the rules as
// `hostrunner up` would load them, and their digest, or why they are not
// valid, in which case the Server keeps the rules it has. Running
// commands are untouched; new requests are checked against the new rules.
func WithReload(digest string, reload func() (Policy, string, error)) Option {
	return func(s *Server) { s.digest, s.reload = digest, reload }
}

// RulesDigest returns the digest of the rules the Server serves, as
// WithReload gave it or a reread changed it; "" without WithReload.
func (s *Server) RulesDigest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digest
}

// WithArmHandler sets what the Server does when `hostrunner up` arms it on
// a devcontainer start (e.g. arm the container watcher). If f returns
// true, the Server steps aside: it answers so and then stops Serve, so
// `up` can start a new daemon. f is called from connection
// goroutines, so it must be safe for concurrent use.
func WithArmHandler(f func(protocol.Arm) (stepAside bool)) Option {
	return func(s *Server) { s.onArm = f }
}

// rules returns the rules the Server serves: policy, or what a reread
// replaced it with.
func (s *Server) rules() Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// rereadRules rereads the rules file (see WithReload) and serves the new
// rules from then on, provided the file has the digest want, the one the
// session left it with; it returns their digest, or why it keeps the old
// rules.
func (s *Server) rereadRules(want string) (string, error) {
	if s.reload == nil {
		return "", errors.New("this daemon does not reread its rules")
	}
	p, digest, err := s.reload()
	if err != nil {
		return "", err
	}
	if digest != want {
		return "", errors.New("the rules file changed meanwhile")
	}
	s.mu.Lock()
	s.policy, s.digest = p, digest
	s.mu.Unlock()
	return digest, nil
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

// Serve accepts connections on l until ctx is cancelled, an arm handler
// steps aside, or l fails; then it closes l, kills running commands and
// returns once every connection is finished. Temporary accept errors (e.g.
// out of file descriptors) are retried with backoff. It returns nil when
// stopped by cancellation or a step aside, and the accept error otherwise.
// With WithApprovals it accepts approval sessions on the approval socket
// meanwhile, and closes that socket when it stops too.
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
	// The approval socket closes first: `hostrunner up` starts the next
	// daemon once the socket is closed, and that daemon takes the approval
	// socket too.
	stop := context.AfterFunc(ctx, func() {
		if s.approvals != nil {
			s.approvals.listener.Close()
		}
		l.Close()
	})
	defer stop()

	var (
		wg      sync.WaitGroup
		backoff time.Duration
	)
	if s.approvals != nil {
		wg.Go(func() { s.serveApprovals(ctx, &wg) })
	}
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
			stopped := ctx.Err() != nil // cancelled by the caller or a step aside
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

// handle serves one connection: an arm request, a dry run or one command.
// Cancelling ctx, or the client going away, kills the command without
// reporting a result.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	out := &frameWriter{conn: conn}
	// logArgs mark in the log every refusal of this connection's request,
	// once its kind is known (a dry run).
	var logArgs []any
	// A bug triggered by one request (e.g. in the policy) must not take the
	// whole daemon down with every other container's session.
	defer func() {
		if r := recover(); r != nil {
			s.reject(out, protocol.ExitHostrunError, "internal error in the hostrunner daemon", fmt.Errorf("panic: %v\n%s", r, debug.Stack()), logArgs...)
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
	if f.Type == protocol.FrameAsk {
		// A request waiting for an answer is told that the daemon stops
		// before its connection closes: ask closes it itself.
		stop()
		s.ask(ctx, cancel, conn, out, f)
		return
	}
	if f.Type == protocol.FrameDryRun {
		logArgs = dryRunLogArgs
	}
	req, err := decodeRequest(f)
	if err != nil {
		s.reject(out, protocol.ExitHostrunError, err.Error(), err, logArgs...)
		return
	}
	if f.Type == protocol.FrameDryRun {
		s.dryRun(out, req)
		return
	}
	s.run(ctx, cancel, conn, out, req)
}

// dryRunLogArgs mark a dry run's lines in the daemon log.
var dryRunLogArgs = []any{"dry_run", true}

// dryRun answers whether req would start: it runs the same checks as run,
// then looks the program up as starting it would, and releases what the
// checks opened, never starting the command or reading stdin.
func (s *Server) dryRun(out *frameWriter, req protocol.Request) {
	p, r := s.prepare(req, true)
	if r == nil {
		r = lookupStart(p, req)
	}
	if r != nil {
		r = s.withAskHint(req, r)
		s.reject(out, r.code, r.message, r.err, dryRunLogArgs...)
		return
	}
	p.close()
	s.log.Info("dry run allowed", append(append(p.logArgs(req), "rule", p.rule), dryRunLogArgs...)...)
	_ = out.writeJSON(protocol.FrameAllowed, protocol.Allowed{Rule: p.rule, Dir: p.fixedDir, Secrets: p.secrets, Standing: p.standing})
}

// arm handles FrameArm from `hostrunner up`. When the handler steps aside,
// the answer is written before Serve is stopped, since stopping
// closes every connection.
func (s *Server) arm(out *frameWriter, f protocol.Frame) {
	var a protocol.Arm
	if err := protocol.DecodeJSON(f, &a); err != nil {
		s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		return
	}
	stepAside := s.onArm != nil && s.onArm(a)
	s.log.Info("armed by hostrunner up", "step_aside", stepAside)
	_ = out.writeJSON(protocol.FrameArmed, protocol.Armed{Version: protocol.Version, Restart: stepAside})
	if stepAside {
		s.mu.Lock()
		stop := s.stopServe
		s.stopServe = nil
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
	}
}

// prepared is a command that passed every check before its start: what
// starting it needs. Its owner calls close once it is done with it.
type prepared struct {
	rule     string     // the command of the rule that allowed it
	fixedDir string     // the rule's fixed directory, cleaned; "" if none
	dir      *os.File   // the working directory, opened
	hostPath string     // dir's real path on the host
	argv     []string   // argv to run: path: open arguments replaced, injection placed
	files    []*os.File // path: open, then injected files: the command's descriptors 3, 4, …
	env      []string   // injected NAME=value variables over the daemon's
	secrets  []string   // names of the secrets injected, for the log
	masked   []string   // their values, masked in the command's output
	// pathFailures are the arguments that failed their path checks and are
	// passed as given, as the passes allowed (see prepareAllowed).
	pathFailures []pathFailure
	// standing is set when a standing approval lets the command through.
	standing bool
}

// close releases the daemon's copies of the directory and of the files
// the command has not inherited yet.
func (p *prepared) close() {
	p.dir.Close()
	closeAll(p.files)
}

// logArgs describe the command p prepared for req in the daemon log: the
// argv the container sent, never the argv run, which holds the secrets'
// values, and the names of the secrets injected.
func (p *prepared) logArgs(req protocol.Request) []any {
	args := []any{"argv", req.Argv, "dir", p.hostPath}
	if len(p.secrets) > 0 {
		args = append(args, "secrets", p.secrets)
	}
	if p.standing {
		args = append(args, "approved", "standing")
	}
	return args
}

// refusal is why a command may not start: the exit code and message for
// the client, which must not reveal host paths, and the error, with host
// paths, for the daemon log.
type refusal struct {
	code    int
	message string
	err     error
}

// prepare runs the checks req has to pass before its command may start.
// The rules are checked first, so a denied command is reported as such
// whatever its directories; the path checks they ask for come after the
// mirrored directory is open, since paths are resolved from it. The
// mirrored directory has to be inside the workspace even for a rule with a
// fixed directory, which is opened last and replaces it as the working
// directory. The secrets are obtained once every check has passed, and the
// rule's injection is placed in the argv to run; for a dry run they are
// only checked (see checkSources), and the argv is the container's. On
// refusal nothing is left open.
//
// A standing approval for req lets it through what the rules refuse, as
// the rule that refused it would run it, and passes as given the path
// failures the human saw when granting it.
func (s *Server) prepare(req protocol.Request, dryRun bool) (*prepared, *refusal) {
	allowed, err := s.rules().Check(req.Argv)
	standing := s.approvals.standingFor(req)
	// byStanding: the rules refuse req, and its standing approval overrides
	// them.
	byStanding := false
	if err != nil {
		var d *rules.Denial
		if standing == nil || !errors.As(err, &d) || !d.Approvable() {
			return nil, &refusal{protocol.ExitRejected, err.Error(), err}
		}
		allowed, byStanding = d.Approved, true
	}
	var passes pathPasses
	if standing != nil {
		passes = standing.passes
	}
	p, r := s.prepareAllowed(req, allowed, dryRun, passes)
	if p != nil {
		p.standing = byStanding || len(p.pathFailures) > 0
	}
	return p, r
}

// prepareAllowed runs the checks after the rules for req, a command the
// rules allowed, or a human approved, as allowed describes: its working
// directory, path checks, fixed directory and secrets (see prepare). A
// path failure that passes.pass lets through does not refuse it: the
// argument is passed as given, and the failure is in pathFailures.
func (s *Server) prepareAllowed(req protocol.Request, allowed rules.Allowed, dryRun bool, passes pathPasses) (*prepared, *refusal) {
	var err error
	p := &prepared{rule: allowed.Rule, fixedDir: allowed.Dir, secrets: allowed.SecretNames()}
	if p.dir, p.hostPath, err = s.mapper.Open(req.Cwd); err != nil {
		return nil, &refusal{protocol.ExitRejected, cwdMessage(req.Cwd, err), err}
	}
	// From here on, a refusal releases what p holds so far.
	refuse := func(code int, message string, err error) (*prepared, *refusal) {
		p.close()
		return nil, &refusal{code, message, err}
	}
	var message string
	if p.argv, p.files, p.pathFailures, message, err = s.openPaths(req.Argv, allowed.Paths, p.hostPath, passes); err != nil {
		return refuse(protocol.ExitRejected, message, err)
	}
	if allowed.Dir != "" {
		// The mirrored directory was only needed to resolve the paths.
		p.dir.Close()
		if p.dir, p.hostPath, err = s.mapper.OpenFixed(allowed.Dir); err != nil {
			return refuse(protocol.ExitRejected, fixedMessage(allowed.Dir, err), err)
		}
	}
	if dryRun {
		// Nothing is obtained, run or created: the sources are only
		// checked, and the argv is not assembled, which needs the values.
		if r := checkSources(allowed.Secrets, p.dir); r != nil {
			return refuse(r.code, r.message, r.err)
		}
		return p, nil
	}
	values, r := obtainSecrets(allowed.Secrets, p.dir, p.hostPath)
	if r != nil {
		return refuse(r.code, r.message, r.err)
	}
	if p.files, err = injectFiles(allowed.Inject.Files, values, p.files); err != nil {
		return refuse(protocol.ExitHostrunError, "cannot create the files the rule hands the command", err)
	}
	if p.argv, err = allowed.Assemble(p.argv, values); err != nil {
		return refuse(protocol.ExitHostrunError, err.Error(), err)
	}
	p.env = allowed.Environment(values)
	p.masked = secretValues(allowed.Secrets, values)
	return p, nil
}

// run executes req on the connection if it passes prepare; cancel stops it
// early.
func (s *Server) run(ctx context.Context, cancel context.CancelFunc, conn net.Conn, out *frameWriter, req protocol.Request) {
	p, r := s.prepare(req, false)
	if r != nil {
		r = s.withAskHint(req, r)
		s.reject(out, r.code, r.message, r.err)
		return
	}
	s.start(ctx, cancel, conn, out, req, p, func() (protocol.Frame, error) { return protocol.ReadFrame(conn) })
}

// frameSource returns the client's next frame, or an error once the
// connection ends.
type frameSource func() (protocol.Frame, error)

// start runs p, the prepared command of req, on the connection: it streams
// stdio, reading the client's frames from next, and reports the exit code;
// cancel stops it early. logArgs (slog key-value pairs) go to its log
// lines.
func (s *Server) start(ctx context.Context, cancel context.CancelFunc, conn net.Conn, out *frameWriter, req protocol.Request, p *prepared, next frameSource, logArgs ...any) {
	// The command inherits the files; the daemon's copies go once it has
	// started (p.files is cleared then), or with the request on failure.
	defer p.close()

	cmd := newCommand(ctx, p.argv, p.dir, p.hostPath, p.env, out)
	stdout, stderr := newMaskWriter(cmd.Stdout, p.masked), newMaskWriter(cmd.Stderr, p.masked)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = p.files
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
	closeAll(p.files)
	p.files = nil

	queue := newStdinQueue(protocol.StdinWindow)
	var streams sync.WaitGroup
	streams.Go(func() {
		if err := readClient(next, queue); err != nil {
			s.reject(out, protocol.ExitHostrunError, err.Error(), err)
		}
		// The client is gone or broke the protocol: nobody is left to
		// receive the result, so stop the command.
		cancel()
	})
	streams.Go(func() { feedStdin(queue, stdin, out) })
	_ = out.writeJSON(protocol.FrameStdinCredit, protocol.Credit{Bytes: protocol.StdinWindow})

	waitErr := cmd.Wait()
	// The output is complete: what a mask held back can go now.
	_ = stdout.Flush()
	_ = stderr.Flush()
	if ctx.Err() == nil {
		code := exitCode(cmd.ProcessState)
		s.log.Info("command finished", append(append(p.logArgs(req), logArgs...), "code", code, "wait_error", waitErr)...)
		_ = out.writeJSON(protocol.FrameExit, protocol.Exit{Code: code})
	} else {
		s.log.Info("command cancelled", append(p.logArgs(req), logArgs...)...)
	}
	conn.Close()
	streams.Wait()
}

// reject reports a command that could not run. The client gets message,
// which must not reveal host paths; the full err, and logArgs (slog
// key-value pairs), go to the daemon log.
func (s *Server) reject(out *frameWriter, code int, message string, err error, logArgs ...any) {
	s.log.Warn("request rejected", append([]any{"code", code, "error", err}, logArgs...)...)
	_ = out.writeJSON(protocol.FrameError, protocol.Error{Code: code, Message: message})
}

// cwdMessage describes a failed cwd mapping in container terms only.
func cwdMessage(cwd string, err error) string {
	if errors.Is(err, workspace.ErrOutsideWorkspace) {
		return fmt.Sprintf("working directory %s is outside the workspace", cwd)
	}
	return fmt.Sprintf("working directory %s is not available on the host", cwd)
}

// fixedMessage describes why the fixed directory dir, the rules file's path
// cleaned, is refused. The container reads the rules file, so naming dir
// reveals nothing; where it leads on the host is left to the daemon log.
func fixedMessage(dir string, err error) string {
	var reason string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		reason = "does not exist"
	case errors.Is(err, syscall.ENOTDIR):
		reason = "is not a directory"
	case errors.Is(err, workspace.ErrInsideWorkspace):
		reason = "is inside the workspace"
	default:
		reason = "cannot be opened on the host"
	}
	return fmt.Sprintf("fixed directory %s %s", dir, reason)
}

// openPaths performs the path checks the policy asked for on argv, whose
// working directory is the host directory dirPath. It returns the argv to
// run and the files to hand to the command as its descriptors 3, 4, …; the
// caller closes the files, which it gets on failure too. A failed check
// returns a message for the client, in container terms only, and an error
// with host paths for the daemon log. A failure that passes.pass lets
// through refuses nothing: the argument is passed as given, and its
// failure returned.
//
// A PathModeCheck argument is opened to check it and closed again; the
// program gets it as given. A PathModeOpen argument stays open and is
// replaced with the /proc/self/fd path of the descriptor the program
// inherits, keeping the part of its token before the path (--file=, -f).
func (s *Server) openPaths(argv []string, paths []rules.PathArg, dirPath string, passes pathPasses) ([]string, []*os.File, []pathFailure, string, error) {
	if len(paths) == 0 {
		return argv, nil, nil, "", nil
	}
	runArgv := slices.Clone(argv)
	var files []*os.File
	var failed []pathFailure
	for _, p := range paths {
		name := argv[p.Index][len(p.Prefix):]
		f, err := s.mapper.OpenFile(dirPath, name)
		if err != nil && passes.pass(p.Index, pathMessage(name, err)) {
			failed = append(failed, pathFailure{p.Index, pathMessage(name, err)})
			continue
		}
		if err != nil {
			return nil, files, nil, pathMessage(name, err), err
		}
		if p.Mode != rules.PathModeOpen {
			f.Close()
			continue
		}
		// ExtraFiles[i] becomes descriptor 3+i in the command.
		runArgv[p.Index] = fmt.Sprintf("%s/proc/self/fd/%d", p.Prefix, 3+len(files))
		files = append(files, f)
	}
	return runArgv, files, failed, "", nil
}

// pathFailure is an argument that failed its path check: its argv index,
// and why, for the client.
type pathFailure struct {
	index   int
	message string
}

// pathPasses says which failed path checks are passed as given rather than
// refusing the command. The zero value passes none.
type pathPasses struct {
	// all passes every failure: they are collected to ask a human about.
	all bool
	// seen passes, by argv index, the failure a human saw and approved,
	// for that reason only: the same argument failing otherwise (a file
	// swapped for a symlink since) is refused.
	seen map[int]string
}

// pass reports whether the failure of the argument at argv index i, for
// message, is passed as given.
func (pp pathPasses) pass(i int, message string) bool {
	return pp.all || pp.seen[i] != "" && pp.seen[i] == message
}

// seenPaths passes the failures a human saw and approved.
func seenPaths(failures []pathFailure) pathPasses {
	seen := make(map[int]string, len(failures))
	for _, f := range failures {
		seen[f.index] = f.message
	}
	return pathPasses{seen: seen}
}

// closeAll closes the daemon's copies of the files openPaths opened. Close
// errors are ignored: the files were opened read-only, so nothing is lost.
func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// pathMessage describes why the argument name is not a workspace file, in
// container terms only.
func pathMessage(name string, err error) string {
	var reason string
	switch {
	case errors.Is(err, workspace.ErrEmptyPath):
		reason = "it is empty"
	case errors.Is(err, workspace.ErrAbsolutePath):
		reason = "it is an absolute path"
	case errors.Is(err, fs.ErrNotExist):
		reason = "it does not exist"
	case errors.Is(err, workspace.ErrNotRegularFile):
		reason = "it is not a regular file"
	case errors.Is(err, workspace.ErrOutsideWorkspace):
		reason = "it leads outside the workspace"
	default:
		reason = "it cannot be opened on the host"
	}
	return fmt.Sprintf("argument %q is not a workspace file: %s", name, reason)
}

// readOpening reads the opening frame of a connection to the container's
// socket (FrameRequest, FrameDryRun, FrameAsk or FrameArm) and checks its
// protocol version before anything else, so a frame shaped by a newer
// protocol is reported as a version mismatch rather than a decoding error.
// Every other frame, the approval socket's included, is refused.
func readOpening(conn net.Conn) (protocol.Frame, error) {
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return f, fmt.Errorf("read request: %w", err)
	}
	switch f.Type {
	case protocol.FrameRequest, protocol.FrameDryRun, protocol.FrameAsk, protocol.FrameArm:
	default:
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

// decodeRequest decodes and validates a FrameRequest or FrameDryRun.
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

// fdPath names the open directory dir in the daemon's /proc, so a
// command started in it, or a path resolved from it, gets that directory
// even if the container swaps a symlink into its path later. A child keeps
// the path valid only until exec, as it inherits the descriptor until then.
func fdPath(dir *os.File) string {
	return fmt.Sprintf("/proc/self/fd/%d", dir.Fd())
}

// newCommand prepares argv to run in the open directory dir, whose real
// path is hostPath. The child changes into the directory through
// /proc/self/fd (it inherits the descriptor until exec), so a symlink the
// container swaps into the path after the check cannot redirect it; PWD is
// set to hostPath because the /proc path is meaningless once the child runs.
// The environment is otherwise the daemon's (host) environment with env,
// the rule's injected variables, over it; nothing comes from the
// container. The command gets its own process group so
// cancellation also kills whatever it spawned (e.g. git's ssh).
func newCommand(ctx context.Context, argv []string, dir *os.File, hostPath string, env []string, out *frameWriter) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = fdPath(dir)
	cmd.Env = commandEnv(hostPath, env...)
	cmd.Stdout = streamWriter{out: out, frame: protocol.FrameStdout}
	cmd.Stderr = streamWriter{out: out, frame: protocol.FrameStderr}
	ownGroup(cmd)
	return cmd
}

// ownGroup starts cmd in a process group of its own, so that cancelling it
// kills whatever it spawned too (e.g. git's ssh), and waits at most
// waitDelay for its output after it exits.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
}

// commandEnv is the environment a command, or a secret's command source,
// runs with in the directory whose real path is hostPath: the daemon's,
// with PWD set to hostPath, then the NAME=value entries of extra (a rule's
// injected variables) over it.
func commandEnv(hostPath string, extra ...string) []string {
	set := append([]string{"PWD=" + hostPath}, extra...)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return slices.ContainsFunc(set, func(e string) bool {
			name, _, _ := strings.Cut(e, "=")
			return strings.HasPrefix(kv, name+"=")
		})
	})
	return append(env, set...)
}

// xOK is access(2)'s X_OK, which package syscall does not export.
const xOK = 0x1

// lookupStart looks the program of p, prepared for req, up as starting it
// would (see lookupProgram). On failure it releases p and returns the
// refusal starting it would get.
func lookupStart(p *prepared, req protocol.Request) *refusal {
	err := lookupProgram(p.argv[0], p.dir)
	if err == nil {
		return nil
	}
	p.close()
	code, message := startFailure(req.Argv[0], err)
	return &refusal{code, message, err}
}

// lookupProgram returns the error starting program in the working
// directory dir would fail with, as far as it can be told without
// starting it, so that startFailure classifies both alike. A name is
// looked up in the daemon's PATH, as newCommand's exec.Cmd does; a path
// is resolved from dir, as execve does after the child changed into it,
// and must be an executable file: execve refuses a directory with EACCES.
func lookupProgram(program string, dir *os.File) error {
	if !strings.Contains(program, "/") {
		_, err := exec.LookPath(program)
		return err
	}
	path := program
	if !filepath.IsAbs(path) {
		path = filepath.Join(fdPath(dir), path)
	}
	if err := syscall.Access(path, xOK); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err != nil {
		return err
	} else if fi.IsDir() {
		return syscall.EACCES
	}
	return nil
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
func readClient(next frameSource, queue *stdinQueue) error {
	defer queue.close()
	for {
		f, err := next()
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
// the final status frame onto one connection. With a timeout, a write the
// peer does not take within it fails instead of blocking the writer.
type frameWriter struct {
	mu      sync.Mutex
	conn    net.Conn
	timeout time.Duration // 0: writes may block as long as the peer lets them
}

// write sends one frame.
func (w *frameWriter) write(f protocol.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.setDeadline()
	return protocol.WriteFrame(w.conn, f)
}

// writeJSON sends v as a JSON frame of type t.
func (w *frameWriter) writeJSON(t protocol.FrameType, v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.setDeadline()
	return protocol.WriteJSON(w.conn, t, v)
}

// batch calls f holding w: the frames f sends, a JSON frame of type t
// with v or, for a nil v, one without payload, go out with no other frame
// between them, and frames others write wait until f returns.
func (w *frameWriter) batch(f func(send func(t protocol.FrameType, v any) error) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return f(func(t protocol.FrameType, v any) error {
		w.setDeadline()
		if v == nil {
			return protocol.WriteFrame(w.conn, protocol.Frame{Type: t})
		}
		return protocol.WriteJSON(w.conn, t, v)
	})
}

// setDeadline bounds the next write by w's timeout, if it has one.
func (w *frameWriter) setDeadline() {
	if w.timeout > 0 {
		_ = w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
	}
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
