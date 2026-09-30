// Package launch implements `hostrunner up`: the step devcontainer's
// initializeCommand runs on the host before every container start.
//
// It must be fast and idempotent: it prepares the per-container runtime
// directory, and either arms the daemon already serving it or installs the
// hostrun client there and starts a detached daemon. Arming matters because
// a rebuild removes the old container before building the new image: the
// daemon must wait for the new container rather than exit after its grace
// period. The runtime directory is what the container mounts (read-only) at
// /run/hostrunner.
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

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
)

// Names of the files in the runtime directory.
const (
	SocketName = "hostrunner.sock" // the daemon's socket
	ClientName = "hostrun"         // the client binary for the container
	LogName    = "daemon.log"      // the daemon's stdout and stderr
	lockName   = "up.lock"         // serializes concurrent `hostrunner up`s
)

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
	Dir                string        // runtime directory, e.g. $XDG_RUNTIME_DIR/hostrunner/<devcontainerId>
	Workspace          string        // workspace folder on the host
	ContainerWorkspace string        // the same folder inside the container
	Rules              string        // the rules file (.devcontainer/hostrun.yaml)
	Daemon             string        // hostrunner executable to start as the daemon
	Client             string        // hostrun binary to install into Dir
	ReadyTimeout       time.Duration // how long to wait for the daemon's socket
}

// Up makes sure an armed daemon serves cfg.Dir with the current rules. It
// validates cfg.Rules first (invalid rules are an error), arms a running
// daemon, and starts a new one if there is none or the running one loaded
// other rules (it steps aside when armed with a different digest). It
// returns once the daemon has answered an arm request, or an error carrying
// the daemon's log output if it did not come up. Concurrent calls for the
// same directory are serialized, so they end up sharing one daemon.
func Up(ctx context.Context, cfg Config) error {
	socket := filepath.Join(cfg.Dir, SocketName)
	if len(socket) > maxSocketPath {
		return fmt.Errorf("socket path %s is too long (%d bytes, max %d)", socket, len(socket), maxSocketPath)
	}
	if err := prepareDir(cfg.Dir); err != nil {
		return err
	}
	// Invalid rules must fail `devcontainer up` visibly, even when a daemon
	// with older rules is still running.
	policy, err := rules.Load(cfg.Rules)
	if err != nil {
		return fmt.Errorf("rules file: %w", err)
	}
	digest := policy.Digest()
	unlock, err := lockDir(cfg.Dir)
	if err != nil {
		return err
	}
	defer unlock()

	restart, err := arm(ctx, socket, digest)
	switch {
	case err == nil && !restart:
		return nil
	case err == nil: // the daemon runs stale rules and is stepping aside
		if err := waitStopped(ctx, socket, cfg.ReadyTimeout); err != nil {
			return err
		}
	case !errors.Is(err, errNotListening):
		return fmt.Errorf("%w; stop the process serving %s and retry", err, socket)
	}
	if err := installClient(cfg.Client, cfg.Dir); err != nil {
		return err
	}
	return startDaemon(ctx, cfg, socket, digest)
}

// waitStopped waits until nothing accepts connections on socket.
func waitStopped(ctx context.Context, socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := transport.Unix{Path: socket}.Dial(ctx)
		if err != nil {
			return nil
		}
		c.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon on %s did not stop within %v to apply changed rules", socket, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// prepareDir creates dir owner-only; the mode is enforced even when the
// directory already exists, since it guards the socket and the client.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			return fmt.Errorf("create runtime directory %s: %w (XDG_RUNTIME_DIR is not set, so ${localEnv:XDG_RUNTIME_DIR} in devcontainer.json expands to nothing)", dir, err)
		}
		return fmt.Errorf("create runtime directory: %w", err)
	}
	return os.Chmod(dir, 0o700)
}

// lockDir takes an exclusive lock on dir for the rest of `up`; the returned
// function releases it. The lock file is close-on-exec, so the daemon does
// not inherit (and hold) it.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock runtime directory: %w", err)
	}
	return func() { f.Close() }, nil
}

// arm asks the daemon on socket to wait for its devcontainer again, telling
// it the digest of the current rules. restart reports that the daemon runs
// other rules and is shutting down. err wraps errNotListening when nothing
// accepts connections, and is another error when something listens but does
// not arm.
func arm(ctx context.Context, socket, digest string) (restart bool, err error) {
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
	if err := protocol.WriteJSON(c, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: digest}); err != nil {
		return false, fmt.Errorf("daemon on %s did not accept the arm request: %w", socket, err)
	}
	f, err := protocol.ReadFrame(c)
	if err != nil {
		return false, fmt.Errorf("daemon on %s did not answer the arm request: %w", socket, err)
	}
	switch f.Type {
	case protocol.FrameArmed:
		var a protocol.Armed
		if err := protocol.DecodeJSON(f, &a); err != nil {
			return false, fmt.Errorf("daemon on %s: %w", socket, err)
		}
		return a.Restart, nil
	case protocol.FrameError:
		var e protocol.Error
		_ = protocol.DecodeJSON(f, &e)
		return false, fmt.Errorf("daemon on %s refused to arm: %s", socket, e.Message)
	default:
		return false, fmt.Errorf("daemon on %s answered the arm request with frame type %d", socket, f.Type)
	}
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
		return fmt.Errorf("client %s is not an ELF executable: %w", path, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("client %s is dynamically linked; rebuild it with CGO_ENABLED=0 (make install)", path)
		}
	}
	return nil
}

// startDaemon spawns the daemon in its own session with output going to the
// log, so it outlives `hostrunner up` and never holds the caller's stdio
// (which would make devcontainer wait for it), then waits until it arms.
func startDaemon(ctx context.Context, cfg Config, socket, digest string) error {
	logPath := filepath.Join(cfg.Dir, LogName)
	logFile, err := openLog(logPath)
	if err != nil {
		return err
	}
	logStart, _ := logFile.Seek(0, io.SeekEnd)

	cmd := exec.Command(cfg.Daemon, "serve",
		"--socket", socket,
		"--workspace", cfg.Workspace,
		"--container-workspace", cfg.ContainerWorkspace,
		"--config", cfg.Rules,
		"--watch")
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
			restart, err := arm(ctx, socket, digest)
			if err == nil && !restart {
				return nil
			}
			if err == nil { // the rules changed again while it started
				err = errors.New("the new daemon loaded other rules than up validated; retry")
			}
			if !errors.Is(err, errNotListening) {
				_ = cmd.Process.Kill()
				return fmt.Errorf("%w: %s", err, logTail(logPath, logStart))
			}
		}
	}
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
		return nil, fmt.Errorf("daemon log %s is not a regular file", path)
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
