package stop_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// newDaemonEnv makes the test binary, started by a restart as
// `<it> serve …`, act as the new daemon: it records its arguments in
// new.args and its pid in new.pid beside its socket, and answers every
// arm request on the socket.
const newDaemonEnv = "HOSTRUNNER_STOP_TEST_NEW_DAEMON"

// runNewDaemon is the process a newDaemonEnv test binary runs.
func runNewDaemon() {
	socket := ""
	for i, a := range os.Args {
		if a == "--socket" && i+1 < len(os.Args) {
			socket = os.Args[i+1]
		}
	}
	dir := filepath.Dir(socket)
	_ = os.WriteFile(filepath.Join(dir, "new.args"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "new.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	l, err := transport.Unix{Path: socket}.Listen()
	if err != nil {
		os.Exit(3)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func(c net.Conn) {
			defer c.Close()
			if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameArm {
				_ = protocol.WriteJSON(c, protocol.FrameArmed, protocol.Armed{})
			}
		}(c)
	}
}

// staticClientDir holds the static client; TestMain removes it.
var staticClientDir string

// staticClient builds, once per test run, a CGO-free program for a
// restart to install as the hostrun client.
var staticClient = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "hr-stop-static")
	if err != nil {
		return "", err
	}
	staticClientDir = dir
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "hostrun")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &exec.ExitError{Stderr: out}
	}
	return bin, nil
})

// restartConfig is how a test restarts: the test binary as the new
// daemon, a static client, runtimes asked for an older daemon's
// container. The new daemon is killed when the test ends.
func restartConfig(t *testing.T, runtimes ...watch.Runtime) *stop.Restart {
	t.Helper()
	client, err := staticClient()
	if err != nil {
		t.Fatalf("build static client: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(newDaemonEnv, "1")
	return &stop.Restart{Daemon: exe, Client: client, ReadyTimeout: 5 * time.Second, Runtimes: runtimes}
}

// newDaemonArgs returns the arguments the new daemon of dir started
// with, or nil if none started; it is killed when the test ends.
func newDaemonArgs(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "new.args"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if p, err := os.ReadFile(filepath.Join(dir, "new.pid")); err == nil {
		if pid, err := strconv.Atoi(string(p)); err == nil {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		}
	}
	return strings.Split(string(b), "\n")
}

// currentDaemon is a daemon of this hostrunner serving a runtime
// directory in the test, which saw its armed container stopped as
// containerStopped says.
type currentDaemon struct {
	dir, workspace, rulesFile string
	served                    chan error // receives what Serve returned
}

// startCurrentDaemon starts a daemon of this hostrunner for a new runtime
// directory, with the rules in rulesText; it stops when the test ends.
func startCurrentDaemon(t *testing.T, rulesText string, containerStopped bool) *currentDaemon {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rulesFile := filepath.Join(ws, "hostrun.yaml")
	if err := os.WriteFile(rulesFile, []byte(rulesText), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := rules.Load(rulesFile)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := workspace.NewMapper(ws, "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	l, err := transport.Unix{Path: filepath.Join(dir, launch.SocketName)}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	al, err := transport.Unix{Path: launch.ApprovalSocket(dir)}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.New(mapper, policy, slog.New(slog.DiscardHandler),
		daemon.WithApprovals(al, rulesFile), daemon.WithContainerStopped(func() bool { return containerStopped }))
	ctx, cancel := context.WithCancel(context.Background())
	d := &currentDaemon{dir: dir, workspace: ws, rulesFile: rulesFile, served: make(chan error, 1)}
	go func() { d.served <- srv.Serve(ctx, l) }()
	t.Cleanup(cancel)
	return d
}

// waitStopped fails the test unless the daemon stops within 5 s.
func (d *currentDaemon) waitStopped(t *testing.T) {
	t.Helper()
	select {
	case err := <-d.served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}

// running fails the test unless the daemon still serves.
func (d *currentDaemon) stillServes(t *testing.T) {
	t.Helper()
	select {
	case err := <-d.served:
		t.Fatalf("the daemon stopped: %v", err)
	default:
	}
}

// fakeRuntime reports one container for every workspace, running as
// running says.
type fakeRuntime struct{ running bool }

func (r fakeRuntime) Containers(context.Context, string) ([]watch.Container, error) {
	return []watch.Container{{ID: "c", Running: r.running, StartedAt: time.Now()}}, nil
}

const validRules = "rules:\n  - command: echo\n    args: any\n"

func TestRestartReplacesTheDaemon(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false)
	out, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t)}, "")
	if err != nil {
		t.Fatalf("restart: %v; printed %q", err, out)
	}
	if out != "hostrunner: restarted "+d.dir+"\n" {
		t.Fatalf("printed %q", out)
	}
	d.waitStopped(t)
	args := newDaemonArgs(t, d.dir)
	want := []string{"serve", "--socket", filepath.Join(d.dir, launch.SocketName), "--workspace", d.workspace, "--container-workspace", "/workspaces/app", "--config", d.rulesFile, "--watch"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("the new daemon runs %q, want %q", args, want)
	}
	if _, err := os.Stat(filepath.Join(d.dir, launch.ClientName)); err != nil {
		t.Fatalf("the client was not installed: %v", err)
	}
}

func TestRestartWithInvalidRulesLeavesTheDaemonRunning(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false)
	if err := os.WriteFile(d.rulesFile, []byte("rules:\n  - command: git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t)}, "")
	if err == nil || !strings.Contains(err.Error(), "rules file") || !strings.Contains(err.Error(), "keeps running") {
		t.Fatalf("got %v, want the rules file refused", err)
	}
	d.stillServes(t)
	if args := newDaemonArgs(t, d.dir); args != nil {
		t.Fatalf("a new daemon started: %q", args)
	}
}

func TestRestartOfADaemonWhoseContainerStoppedOnlyStopsIt(t *testing.T) {
	d := startCurrentDaemon(t, validRules, true)
	out, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t)}, "")
	if err != nil {
		t.Fatalf("restart: %v; printed %q", err, out)
	}
	if out != "hostrunner: stopped "+d.dir+"; its container is not running\n" {
		t.Fatalf("printed %q", out)
	}
	d.waitStopped(t)
	if args := newDaemonArgs(t, d.dir); args != nil {
		t.Fatalf("a new daemon started: %q", args)
	}
}

func TestRestartOfAnOlderDaemonAsksTheContainerRuntime(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(strconv.FormatBool(running), func(t *testing.T) {
			d := startOlderDaemon(t, false, "")
			out, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t, fakeRuntime{running}), Commands: stop.Commands{Answer: stop.Kill}}, "")
			if err != nil {
				t.Fatalf("restart: %v; printed %q", err, out)
			}
			d.waitTerminated(t)
			args := newDaemonArgs(t, d.dir)
			if running != (args != nil) {
				t.Fatalf("container running %v, new daemon %q; printed %q", running, args, out)
			}
		})
	}
}

func TestRestartOfAnOlderDaemonNeedsAContainerRuntime(t *testing.T) {
	d := startOlderDaemon(t, false, "")
	_, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t), Commands: stop.Commands{Answer: stop.Kill}}, "")
	if err == nil || !strings.Contains(err.Error(), "docker or podman") {
		t.Fatalf("got %v, want a container runtime asked for", err)
	}
	if !d.running() {
		t.Fatal("the older daemon was stopped")
	}
}

func TestRestartOfADaemonFromBeforeApprovalsFails(t *testing.T) {
	d := startOlderDaemon(t, true, "")
	_, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t, fakeRuntime{true}), Commands: stop.Commands{Answer: stop.Kill}}, "")
	if err == nil || !strings.Contains(err.Error(), "cannot be restarted") {
		t.Fatalf("got %v, want the restart refused", err)
	}
	if !d.running() {
		t.Fatal("the daemon was stopped")
	}
}
