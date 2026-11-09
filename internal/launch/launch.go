// Package launch implements `hostrunner up`: the step devcontainer's
// initializeCommand runs on the host before every container start.
//
// It must be fast and idempotent: it prepares the per-container runtime
// directory, and either arms the daemon already serving it, on its
// approval socket, which only the host reaches, or installs the hostrun
// client there and starts a detached daemon. Arming matters because
// a rebuild removes the old container before building the new image: the
// daemon must wait for the new container rather than exit after its grace
// period. The runtime directory is what the container mounts (read-only) at
// /run/hostrunner. It starts or arms a daemon only for rules and daemon
// settings a human on the host has allowed: with rules or settings not
// allowed, it arms a running daemon that serves allowed ones, which keeps
// serving them, and otherwise fails.
package launch

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/settings"
	"github.com/kravlab/hostrunner/internal/transport"
)

// Names of the files in the runtime directory.
const (
	SocketName = "hostrunner.sock" // the daemon's socket
	ClientName = "hostrun"         // the client binary for the container
	LogName    = "daemon.log"      // the daemon's stdout and stderr
	lockName   = "up.lock"         // serializes `hostrunner up`, stop and restart
)

// ApprovalSocket is the approval socket of the daemon serving the runtime
// directory dir: beside dir, not in it, since the container mounts dir and
// must not reach the socket approval sessions answer on.
func ApprovalSocket(dir string) string { return filepath.Clean(dir) + protocol.ApprovalSuffix }

// Socket is the socket of the daemon serving the runtime directory dir,
// the one the container's hostrun connects to.
func Socket(dir string) string { return filepath.Join(dir, SocketName) }

// RuntimeDir is the runtime directory of the daemon whose approval socket
// is approvalSocket: the inverse of ApprovalSocket.
func RuntimeDir(approvalSocket string) string {
	return strings.TrimSuffix(approvalSocket, protocol.ApprovalSuffix)
}

// armTimeout bounds one arm request, including connecting.
const armTimeout = 5 * time.Second

// errNotListening means nothing accepts connections on the socket, so a
// daemon has to be started.
var errNotListening = errors.New("no daemon is listening")

// maxSocketPath is the usable length of sun_path on Linux (108 bytes
// including the terminating NUL).
const maxSocketPath = 107

// logTailSize bounds how much of the daemon log a startup error quotes.
const logTailSize = 2048

// Config describes one `hostrunner up` invocation.
type Config struct {
	Dir                string // runtime directory, e.g. $XDG_RUNTIME_DIR/hostrunner/<devcontainerId>
	Workspace          string // workspace folder on the host
	ContainerWorkspace string // the same folder inside the container
	Daemon             string // hostrunner executable to start as the daemon
	Client             string // hostrun binary to install into Dir
	// Allows is the allow store up checks: it starts or arms a daemon only
	// for the rules and daemon settings the store holds for Workspace. No
	// setting names it, so a workspace cannot point up at a store it
	// controls.
	Allows allow.Store
	// Stderr receives what up tells the human besides an error: that it
	// kept a running daemon serving allowed rules (see Up). nil prints
	// nothing.
	Stderr io.Writer
	// WorkspaceSettings is the workspace settings file up resolved
	// (settings.Flags.WorkspaceFile), given to the daemon, which protects
	// it in its mount check; empty gives none, and the daemon resolves it
	// itself.
	WorkspaceSettings string
	// Settings are the settings up, or restart, resolved. Each the daemon
	// takes is given to it as its flag and carried in every arm;
	// ReadyTimeout, up's own, is how long to wait for the daemon's socket
	// and is not passed on.
	settings.Settings
}

// Up makes sure an armed daemon serves cfg.Dir with the current rules. It
// validates cfg.RulesFile first (invalid rules are an error), then checks
// that the rules and the daemon settings are allowed (see checkAllowed).
// If they are, it arms a running daemon, and starts a new one if there is
// none or the running one loaded other rules (it steps aside when armed
// with a different digest). If they are not, it arms a running daemon that
// serves allowed rules and settings, which keeps serving them, and says so
// on cfg.Stderr, the arm reporting the change not allowed so that the daemon
// warns hostrun of it (see keepAllowed); without one it fails with a
// *NotAllowedError, starting nothing. It returns once the daemon has
// answered an arm request, or an error carrying the daemon's log output if
// it did not come up. Concurrent calls for the same directory are
// serialized, so they end up sharing one daemon.
func Up(ctx context.Context, cfg Config) error {
	p, err := Prepare(cfg)
	if err != nil {
		return err
	}
	allowErr := p.checkAllowed()
	notAllowed, ok := errors.AsType[*NotAllowedError](allowErr)
	if allowErr != nil && !ok {
		return allowErr
	}
	unlock, err := LockDir(cfg.Dir)
	if err != nil {
		return err
	}
	defer unlock()
	if notAllowed != nil {
		return p.keepAllowed(ctx, notAllowed)
	}
	return p.UpLocked(ctx)
}

// keepAllowed is Up, under the lock, for rules or settings that are not
// allowed, notAllowed saying so: when the daemon serving cfg.Dir serves
// allowed rules and settings for the workspace (see allowedDaemon), it
// arms that daemon so that it keeps serving them and reports the change
// not allowed, for the daemon to warn hostrun of (see armAllowed).
// Otherwise it returns notAllowed and starts nothing, a running daemon
// staying as it is. A store that cannot be read is an
// error of its own, as in checkAllowed.
func (p *Prepared) keepAllowed(ctx context.Context, notAllowed *NotAllowedError) error {
	info, ok, err := p.allowedDaemon(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return notAllowed
	}
	return p.armAllowed(ctx, info, notAllowed)
}

// allowedDaemon returns what the daemon on the approval socket of cfg.Dir
// says of itself, ok when it serves allowed rules and settings for the
// workspace of the Config: it answers, says its rules digest and its
// settings (a daemon older than DaemonInfo.Settings does not), serves
// that workspace (symbolic links followed, see allow.SameWorkspace), and
// the store allows what it serves for it. No daemon answering is not an
// error; a workspace that cannot be compared, or a store that cannot be
// read, is.
func (p *Prepared) allowedDaemon(ctx context.Context) (info protocol.DaemonInfo, ok bool, err error) {
	cfg := p.cfg
	info, err = daemonlist.Probe(ctx, ApprovalSocket(cfg.Dir))
	if err != nil || info.RulesDigest == "" || info.Settings == nil {
		return info, false, nil
	}
	same, err := allow.SameWorkspace(info.Workspace, cfg.Workspace)
	if err != nil {
		return info, false, fmt.Errorf("compare the workspace of the daemon on %s: %w", hostterm.Display(p.socket), err)
	}
	if !same {
		return info, false, nil
	}
	ok, err = cfg.allowed(allow.Scope{Digest: info.RulesDigest, Settings: info.Settings})
	return info, ok, err
}

// armAllowed arms the daemon of p, on its approval socket, with the
// digest and the daemon settings it said it serves, info, which
// allowedDaemon found allowed, so it follows the arm and keeps serving
// them. The arm also reports the change not allowed, the digest of p and
// the daemon settings of the Config, exactly what checkAllowed checked
// and recorded, so the daemon warns hostrun of it while it stays not
// allowed. It then tells cfg.Stderr what is not allowed, notAllowed, and
// that the daemon keeps serving the allowed rules, and returns nil. A
// daemon that stepped aside, its rules reread since it answered, is
// waited for until it has stopped, and notAllowed returned; an arm that
// fails, or a daemon that does not stop after stepping aside, is joined
// to notAllowed.
func (p *Prepared) armAllowed(ctx context.Context, info protocol.DaemonInfo, notAllowed *NotAllowedError) error {
	cfg := p.cfg
	stepAside, err := arm(ctx, ApprovalSocket(cfg.Dir), protocol.Arm{
		ConfigDigest: info.RulesDigest, NoReread: info.NoReread, Settings: info.Settings,
		NotAllowed: &protocol.ArmNotAllowed{Digest: p.digest, Settings: cfg.Settings.Daemon()},
	})
	if err != nil {
		return errors.Join(notAllowed, err)
	}
	if stepAside {
		if err := WaitStopped(ctx, p.socket, cfg.ReadyTimeout); err != nil {
			return errors.Join(notAllowed, fmt.Errorf("%w after stepping aside", err))
		}
		return notAllowed
	}
	if cfg.Stderr != nil {
		fmt.Fprintf(cfg.Stderr, "hostrunner: %v; the daemon keeps serving the allowed rules (digest %s)\n", notAllowed, hostterm.Display(info.RulesDigest))
	}
	return nil
}

// Prepared is an `up` whose runtime directory is ready and whose rules
// are valid, to run with UpLocked. Whether the rules and settings are
// allowed is not part of being prepared: PrepareAllowed checks it too.
type Prepared struct {
	cfg            Config
	socket, digest string
}

// Prepare readies cfg.Dir and validates cfg.RulesFile for an `up`; invalid
// rules are an error.
func Prepare(cfg Config) (*Prepared, error) {
	socket := Socket(cfg.Dir)
	if len(socket) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is too long (%d bytes, max %d)", hostterm.Display(socket), len(socket), maxSocketPath)
	}
	if err := prepareDir(cfg.Dir); err != nil {
		return nil, err
	}
	// Invalid rules must fail `devcontainer up` visibly, even when a daemon
	// with older rules is still running.
	policy, err := rules.Load(cfg.RulesFile)
	if err != nil {
		return nil, fmt.Errorf("rules file: %w", err)
	}
	return &Prepared{cfg: cfg, socket: socket, digest: policy.Digest()}, nil
}

// PrepareAllowed is Prepare, then the check that the rules and the daemon
// settings are allowed (see checkAllowed): the entry point of `up
// --restart` and `hostrunner restart`, before they take the lock or touch
// a running daemon, which content not allowed leaves as it is. Up checks
// the allow itself, since content not allowed can still keep a running
// daemon (see keepAllowed).
func PrepareAllowed(cfg Config) (*Prepared, error) {
	p, err := Prepare(cfg)
	if err != nil {
		return nil, err
	}
	if err := p.checkAllowed(); err != nil {
		return nil, err
	}
	return p, nil
}

// NotAllowedError says that the content of the rules file, with the
// daemon settings, is not allowed for the workspace: no human on the host
// ran `hostrunner allow` for exactly this since it last changed. The
// refused up recorded its request, which `hostrunner allow` allows.
type NotAllowedError struct {
	Workspace string // the workspace folder on the host
	RulesFile string // the rules file as up resolved it
	Digest    string // the digest of the rules file's content, "missing" for a missing file
}

// Error names the rules file and its digest, so the human sees what is
// refused, and the command that allows it. The paths are shown through
// hostterm.Display, so a control character in one cannot reach the
// terminal raw.
func (e *NotAllowedError) Error() string {
	ws := hostterm.Display(e.Workspace)
	return fmt.Sprintf("the rules file %s (digest %s) and the daemon settings are not allowed for %s: "+
		"a human on the host runs hostrunner allow %s to allow them",
		hostterm.Display(e.RulesFile), e.Digest, ws, ws)
}

// checkAllowed returns a *NotAllowedError unless the allow store of the
// Config holds exactly these rules and daemon settings for its workspace.
// Before it refuses, it records them in the store as the workspace's
// request (see allow.Store.Request), so the human who runs `hostrunner
// allow` allows what this up asked for. Up and PrepareAllowed call it,
// before the lock is taken or a running daemon touched; UpLocked does not
// check.
// A store that cannot be read, or where the request cannot be
// recorded, is an error of its own, not a refusal, and is wrapped.
func (p *Prepared) checkAllowed() error {
	cfg := p.cfg
	target := allow.NewTarget(cfg.RulesFile, p.digest, cfg.Settings.Daemon())
	ok, err := cfg.allowed(target.Scope)
	if err != nil {
		return err
	}
	if !ok {
		if err := cfg.Allows.Request(cfg.Workspace, target); err != nil {
			return fmt.Errorf("record the request of %s: %w", hostterm.Display(cfg.Workspace), err)
		}
		return &NotAllowedError{Workspace: cfg.Workspace, RulesFile: cfg.RulesFile, Digest: p.digest}
	}
	return nil
}

// allowed reports whether the allow store of cfg holds the Scope sc, the
// rules digest and the daemon settings, for its workspace (see
// allow.Store.Allowed). A store that cannot be read is an error naming
// the workspace, wrapping the store's.
func (cfg Config) allowed(sc allow.Scope) (bool, error) {
	ok, err := cfg.Allows.Allowed(cfg.Workspace, sc)
	if err != nil {
		return false, fmt.Errorf("check the allow of %s: %w", hostterm.Display(cfg.Workspace), err)
	}
	return ok, nil
}

// UpLocked is Up for a caller that holds the runtime directory's lock
// (see LockDir) and for allowed rules and settings: restart, which
// stopped the daemon under it. It does not check the allow: Up, `up
// --restart` and `hostrunner restart` checked it before (see
// PrepareAllowed). It arms the daemon on its approval socket; a daemon
// that does not take the arm there (one older than arms on the approval
// socket, which refuses it, or one with no approval socket while its
// socket still listens) fails the up as any arm that fails, naming the
// socket to free.
func (p *Prepared) UpLocked(ctx context.Context) error {
	cfg, socket := p.cfg, p.socket
	a := upArm(p.digest, cfg.Settings)
	stepAside, err := arm(ctx, ApprovalSocket(cfg.Dir), a)
	if errors.Is(err, errNotListening) && listening(ctx, socket) {
		// Not errNotListening: something serves the socket, and a new
		// daemon could not take it.
		err = fmt.Errorf("daemon on %s does not take arms on its approval socket: %v", hostterm.Display(socket), err)
	}
	switch {
	case err == nil && !stepAside:
		return nil
	case err == nil: // the daemon is stopping for a new one
		if err := WaitStopped(ctx, socket, cfg.ReadyTimeout); err != nil {
			return fmt.Errorf("%w after stepping aside", err)
		}
	case !errors.Is(err, errNotListening):
		return fmt.Errorf("%w; stop the process serving %s and retry", err, hostterm.Display(socket))
	}
	if err := installClient(cfg.Client, cfg.Dir); err != nil {
		return err
	}
	return startDaemon(ctx, cfg, socket, a)
}

// listening reports whether something accepts connections on socket.
func listening(ctx context.Context, socket string) bool {
	ctx, cancel := context.WithTimeout(ctx, armTimeout)
	defer cancel()
	c, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// WaitStopped waits until nothing accepts connections on socket, for
// timeout at most.
func WaitStopped(ctx context.Context, socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := transport.Unix{Path: socket}.Dial(ctx)
		if err != nil {
			return nil
		}
		c.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon on %s did not stop within %v", hostterm.Display(socket), timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// prepareDir creates dir owner-only; the mode is enforced even when the
// directory already exists, since it guards the socket and the client.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			return fmt.Errorf("create runtime directory %s: %w (XDG_RUNTIME_DIR is not set, so ${localEnv:XDG_RUNTIME_DIR} in devcontainer.json expands to nothing)", hostterm.Display(dir), err)
		}
		return fmt.Errorf("create runtime directory: %w", err)
	}
	return os.Chmod(dir, 0o700)
}

// LockDir takes an exclusive lock on the runtime directory dir, which
// `up`, stop and restart each hold while they start or stop its daemon, so
// they run one after the other; the returned function releases it. The
// lock file is close-on-exec, so the daemon does not inherit (and hold)
// it.
func LockDir(dir string) (func(), error) {
	unlock, _, err := lockDir(dir, syscall.LOCK_EX)
	return unlock, err
}

// TryLockDir is LockDir without waiting: busy reports that another holds
// the lock, unlock nil then.
func TryLockDir(dir string) (unlock func(), busy bool, err error) {
	return lockDir(dir, syscall.LOCK_EX|syscall.LOCK_NB)
}

// lockDir takes the lock of LockDir with the flock operation how; busy
// reports that a non-blocking how found it held.
func lockDir(dir string, how int) (unlock func(), busy bool, err error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("lock runtime directory: %w", err)
	}
	return func() { f.Close() }, false, nil
}

// arm asks the daemon on socket, its approval socket, to wait for its
// devcontainer again,
// sending it a, with the protocol version set: the digest of the rules,
// the daemon settings by key name (as settings.Settings.Daemon returns
// them; a setting left out of them is not compared) and, for a daemon
// that predates the settings, whether it is not to reread the rules by
// itself. stepAside reports that the daemon will not follow the arm (it
// runs other rules or other settings, or its watch has ended) and is
// stopping. err wraps errNotListening when nothing accepts connections,
// and is another error when something listens but does not arm.
func arm(ctx context.Context, socket string, a protocol.Arm) (stepAside bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, armTimeout)
	defer cancel()
	c, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errNotListening, err)
	}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	a.Version = protocol.Version
	name := hostterm.Display(socket)
	if err := protocol.WriteJSON(c, protocol.FrameArm, a); err != nil {
		return false, fmt.Errorf("daemon on %s did not accept the arm request: %w", name, err)
	}
	f, err := protocol.ReadFrame(c)
	if err != nil {
		return false, fmt.Errorf("daemon on %s did not answer the arm request: %w", name, err)
	}
	switch f.Type {
	case protocol.FrameArmed:
		var a protocol.Armed
		if err := protocol.DecodeJSON(f, &a); err != nil {
			return false, fmt.Errorf("daemon on %s: %w", name, err)
		}
		return a.Restart, nil
	case protocol.FrameError:
		var e protocol.Error
		_ = protocol.DecodeJSON(f, &e)
		return false, fmt.Errorf("daemon on %s refused to arm: %s", name, e.Message)
	default:
		return false, fmt.Errorf("daemon on %s answered the arm request with frame type %d", name, f.Type)
	}
}

// upArm is the arm of an up for rules of digest and the settings s: their
// daemon settings (see settings.Settings.Daemon) and, for a daemon that
// predates the settings, whether it is not to reread the rules by itself.
func upArm(digest string, s settings.Settings) protocol.Arm {
	return protocol.Arm{ConfigDigest: digest, NoReread: !s.Reread, Settings: s.Daemon()}
}

// installClient atomically replaces dir/hostrun with a copy of src, so a
// container that is running the old client is not disturbed.
func installClient(src, dir string) error {
	if err := checkStatic(src); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open client: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, "."+ClientName+"-*")
	if err != nil {
		return fmt.Errorf("install client: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return fmt.Errorf("install client: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("install client: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("install client: %w", err)
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, ClientName))
}

// checkStatic fails unless path is a statically linked ELF executable. The
// client runs inside arbitrary images (musl, distroless), so it must not
// depend on the host's dynamic loader or libc.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("client %s is not an ELF executable: %w", hostterm.Display(path), err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("client %s is dynamically linked; rebuild it with CGO_ENABLED=0 (mise run install)", hostterm.Display(path))
		}
	}
	return nil
}

// startDaemon spawns the daemon in its own session with output going to the
// log, so it outlives `hostrunner up` and never holds the caller's stdio
// (which would make devcontainer wait for it), then waits until it
// follows the arm a, sent to its approval socket.
func startDaemon(ctx context.Context, cfg Config, socket string, a protocol.Arm) error {
	logPath := filepath.Join(cfg.Dir, LogName)
	logFile, err := openLog(logPath)
	if err != nil {
		return err
	}
	logStart, _ := logFile.Seek(0, io.SeekEnd)

	cmd := exec.Command(cfg.Daemon, serveArgs(cfg, socket)...)
	cmd.Dir = cfg.Dir // do not keep the workspace busy
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	logFile.Close()
	if err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(cfg.ReadyTimeout)
	for {
		select {
		case <-exited:
			return fmt.Errorf("daemon exited during startup (%s): %s", cmd.ProcessState, logTail(logPath, logStart))
		case <-deadline:
			_ = cmd.Process.Kill()
			return fmt.Errorf("daemon did not listen within %v: %s", cfg.ReadyTimeout, logTail(logPath, logStart))
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return ctx.Err()
		case <-ticker.C:
			stepAside, err := arm(ctx, ApprovalSocket(cfg.Dir), a)
			if err == nil && !stepAside {
				return nil
			}
			if err == nil { // e.g. the rules changed again while it started
				err = errors.New("the new daemon stepped aside; retry")
			}
			if !errors.Is(err, errNotListening) {
				_ = cmd.Process.Kill()
				return fmt.Errorf("%w: %s", err, logTail(logPath, logStart))
			}
		}
	}
}

// serveArgs is the command line of the daemon cfg describes, listening
// on socket. Every setting serve takes is an explicit flag, so the
// daemon's argv shows what it runs with and it resolves none again;
// rereading and the mount check are given both ways (--reread or
// --no-reread, --mount-check or --no-mount-check). The workspace settings
// file, when cfg names one, is given too, so the daemon protects the file
// up read.
func serveArgs(cfg Config, socket string) []string {
	args := []string{"serve",
		"--socket", socket,
		"--workspace", cfg.Workspace,
		"--container-workspace", cfg.ContainerWorkspace}
	if cfg.WorkspaceSettings != "" {
		args = append(args, "--workspace-settings", cfg.WorkspaceSettings)
	}
	return append(args,
		"--rules-file", cfg.RulesFile,
		"--watch",
		"--startup-timeout", cfg.StartupTimeout.String(),
		"--grace", cfg.Grace.String(),
		"--poll-interval", cfg.PollInterval.String(),
		"--ask-timeout", cfg.AskTimeout.String(),
		"--request-timeout", cfg.RequestTimeout.String(),
		boolFlag("reread", cfg.Reread),
		boolFlag("mount-check", cfg.MountCheck))
}

// boolFlag is the flag of serve that gives the bool setting whose flag is
// --name as on: --name, or --no-name when off.
func boolFlag(name string, on bool) string {
	if on {
		return "--" + name
	}
	return "--no-" + name
}

// openLog opens the daemon log for appending. It refuses anything but a
// regular file: the log lives in a directory mounted into the container,
// and a planted symlink or FIFO must not redirect or block the daemon.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("daemon log %s is not a regular file", hostterm.Display(path))
	}
	return f, nil
}

// logTail returns what the daemon wrote to the log since offset start,
// trimmed to the last logTailSize bytes.
func logTail(path string, start int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "(no daemon log)"
	}
	defer f.Close()
	if end, err := f.Seek(0, io.SeekEnd); err == nil && end-start > logTailSize {
		start = end - logTailSize
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "(unreadable daemon log)"
	}
	data, err := io.ReadAll(f)
	if err != nil && !errors.Is(err, io.EOF) {
		return "(unreadable daemon log)"
	}
	return strings.TrimSpace(string(data))
}
