package stop_test

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	"github.com/kravlab/hostrunner/internal/settings"
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

// resolving returns how a test's restart resolves the new daemon's
// settings: from restart's flags args and the variables vars alone, as
// the terminal running restart would give them.
func resolving(t *testing.T, vars map[string]string, args ...string) func(workspace string) (settings.Resolved, error) {
	t.Helper()
	var f settings.Flags
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	f.RegisterRestart(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return func(workspace string) (settings.Resolved, error) {
		return f.Resolve(func(name string) string { return vars[name] }, workspace)
	}
}

// besideRules are the variables of a restart whose new daemon takes the
// rules file hostrun.yaml in its workspace, as a current daemon of the
// tests does.
var besideRules = map[string]string{"HOSTRUNNER_RULES_FILE": "hostrun.yaml"}

// restartConfig is how a test restarts: the test binary as the new
// daemon, a static client, the settings resolved from besideRules,
// runtimes asked for an older daemon's container. The new daemon is
// killed when the test ends.
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
	return &stop.Restart{Daemon: exe, Client: client, Settings: resolving(t, besideRules), Runtimes: runtimes}
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
// directory, with the rules in rulesText and the Server options opts
// besides its approval socket; it stops when the test ends.
func startCurrentDaemon(t *testing.T, rulesText string, containerStopped bool, opts ...daemon.Option) *currentDaemon {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return startCurrentDaemonAt(t, dir, ws, rulesText, containerStopped, opts...)
}

// startCurrentDaemonAt is startCurrentDaemon for the existing runtime
// directory dir and the host workspace ws, which another daemon may
// serve too.
func startCurrentDaemonAt(t *testing.T, dir, ws, rulesText string, containerStopped bool, opts ...daemon.Option) *currentDaemon {
	t.Helper()
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
	srv := daemon.New(mapper, policy, slog.New(slog.DiscardHandler), append([]daemon.Option{
		daemon.WithApprovals(al, rulesFile), daemon.WithContainerStopped(func() bool { return containerStopped }),
	}, opts...)...)
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

// rereading makes a current daemon one that rereads its rules file by
// itself, as `serve` without --no-reread does, though not before the
// test is over.
var rereading = []daemon.Option{
	daemon.WithReread("", func() (daemon.Policy, string, error) { return nil, "", errors.New("not reread in this test") }),
	daemon.WithRereadEvery(time.Hour),
}

func TestRestartReplacesTheDaemon(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false, rereading...)
	out, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t)}, "")
	if err != nil {
		t.Fatalf("restart: %v; printed %q", err, out)
	}
	if out != "hostrunner: restarted "+d.dir+"\n" {
		t.Fatalf("printed %q", out)
	}
	d.waitStopped(t)
	args := newDaemonArgs(t, d.dir)
	want := []string{"serve", "--socket", filepath.Join(d.dir, launch.SocketName), "--workspace", d.workspace, "--container-workspace", "/workspaces/app", "--rules-file", d.rulesFile, "--watch",
		"--startup-timeout", "30m0s", "--grace", "15s", "--poll-interval", "2s", "--ask-timeout", "10m0s", "--request-timeout", "10s", "--reread"}
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

// A daemon that saw its container stop is only stopped, so neither its
// settings nor its rules file, which no new daemon would use, keep a
// restart from stopping it.
func TestRestartOfADaemonWhoseContainerStoppedChecksNothingItWouldStart(t *testing.T) {
	for _, tt := range []struct {
		name  string
		spoil func(t *testing.T, d *currentDaemon, r *stop.Restart)
	}{
		{"invalid settings", func(t *testing.T, _ *currentDaemon, r *stop.Restart) {
			host := filepath.Join(t.TempDir(), "settings.yaml")
			if err := os.WriteFile(host, []byte("grace: soon\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r.Settings = resolving(t, map[string]string{"HOSTRUNNER_SETTINGS": host})
		}},
		{"invalid rules", func(t *testing.T, d *currentDaemon, _ *stop.Restart) {
			if err := os.WriteFile(d.rulesFile, []byte("rules:\n  - command: git\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := startCurrentDaemon(t, validRules, true)
			r := restartConfig(t)
			tt.spoil(t, d, r)
			out, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, "")
			if err != nil || out != "hostrunner: stopped "+d.dir+"; its container is not running\n" {
				t.Fatalf("restart: %v; printed %q", err, out)
			}
			d.waitStopped(t)
		})
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

// A restart says which of what the old daemon said of itself, its rules
// file and whether it rereads, the new daemon's settings change.
func TestRestartSaysWhatTheSettingsChange(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false, rereading...)
	other := filepath.Join(d.workspace, "other.yaml")
	if err := os.WriteFile(other, []byte(validRules), 0o600); err != nil {
		t.Fatal(err)
	}
	r := restartConfig(t)
	r.Settings = resolving(t, map[string]string{"HOSTRUNNER_RULES_FILE": "other.yaml", "HOSTRUNNER_REREAD": "false"})
	out, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, "")
	if want := "hostrunner: restarted " + d.dir + "; rules_file " + d.rulesFile + " → " + other + ", reread true → false\n"; err != nil || out != want {
		t.Fatalf("restart: %v; printed %q, want %q", err, out, want)
	}
	d.waitStopped(t)
}

// The new daemon rereads its rules file by itself as its settings say,
// whether the old one did or not.
func TestRestartTakesRereadingFromTheSettings(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts []daemon.Option
		vars map[string]string
		want string
	}{
		{"by default", nil, besideRules, "--reread"},
		{"with reread false", rereading, map[string]string{"HOSTRUNNER_RULES_FILE": "hostrun.yaml", "HOSTRUNNER_REREAD": "false"}, "--no-reread"},
	} {
		d := startCurrentDaemon(t, validRules, false, tt.opts...)
		r := restartConfig(t)
		r.Settings = resolving(t, tt.vars)
		if out, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, ""); err != nil {
			t.Fatalf("%s: restart: %v; printed %q", tt.name, err, out)
		}
		d.waitStopped(t)
		if args := newDaemonArgs(t, d.dir); !slices.Contains(args, tt.want) {
			t.Errorf("%s: the new daemon runs %q, want %s", tt.name, args, tt.want)
		}
	}
}

// A restart resolves the new daemon's settings from the settings files
// as they are when it runs, for the old daemon's workspace: the common
// part of the host settings file, the workspace's section and the
// workspace settings file all apply.
func TestRestartPicksUpTheSettingsFilesAsTheyAreNow(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false, rereading...)
	host := filepath.Join(t.TempDir(), "settings.yaml")
	content := "grace: 1m\nworkspaces:\n  " + d.workspace + ":\n    poll_interval: 3s\n"
	if err := os.WriteFile(host, []byte(content), 0o600); err != nil { // edited after the daemon started
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.workspace, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.workspace, ".devcontainer", "hostrunner.yaml"), []byte("ask_timeout: 4m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := restartConfig(t)
	r.Settings = resolving(t, map[string]string{"HOSTRUNNER_RULES_FILE": "hostrun.yaml", "HOSTRUNNER_SETTINGS": host})
	if out, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, ""); err != nil {
		t.Fatalf("restart: %v; printed %q", err, out)
	}
	d.waitStopped(t)
	args := strings.Join(newDaemonArgs(t, d.dir), " ")
	for _, want := range []string{"--grace 1m0s", "--poll-interval 3s", "--ask-timeout 4m0s"} {
		if !strings.Contains(args, want) {
			t.Errorf("the new daemon runs %q, want %s", args, want)
		}
	}
}

// Invalid settings leave the old daemon running, the error naming their
// source.
func TestRestartWithInvalidSettingsLeavesTheDaemonRunning(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false)
	host := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(host, []byte("grace: soon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := restartConfig(t)
	r.Settings = resolving(t, map[string]string{"HOSTRUNNER_SETTINGS": host})
	_, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, "")
	want := "settings file " + host + `: grace: invalid duration "soon"; the daemon keeps running`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an error containing %q", err, want)
	}
	d.stillServes(t)
	if args := newDaemonArgs(t, d.dir); args != nil {
		t.Fatalf("a new daemon started: %q", args)
	}
}

// An older daemon, which does not say whether it rereads, is restarted
// as one that does when the settings say nothing, the default.
func TestRestartOfAnOlderDaemonStartsOneThatRereads(t *testing.T) {
	d := startOlderDaemon(t, false, "")
	out, err := runStop(t, stop.Config{Dir: d.dir, Restart: restartConfig(t, fakeRuntime{true}), Commands: stop.Commands{Answer: stop.Kill}}, "")
	if err != nil {
		t.Fatalf("restart: %v; printed %q", err, out)
	}
	d.waitTerminated(t)
	args := newDaemonArgs(t, d.dir)
	if args == nil || slices.Contains(args, "--no-reread") {
		t.Fatalf("the new daemon runs %q, want it without --no-reread", args)
	}
	if strings.Contains(out, "reread") {
		t.Fatalf("printed %q, a change of rereading the older daemon did not say", out)
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

// devcontainerWith puts hostrunner-rules.yaml and, with older, hostrun.yaml
// into .devcontainer of the daemon's workspace and returns its path.
func (d *currentDaemon) devcontainerWith(t *testing.T, older bool) string {
	t.Helper()
	dir := filepath.Join(d.workspace, ".devcontainer")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{"hostrunner-rules.yaml"}
	if older {
		names = append(names, "hostrun.yaml")
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(validRules), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A restart whose new daemon takes the default rules file warns, once, when
// the older name lies beside it, and goes on; it is silent without it.
func TestRestartWarnsOfAnIgnoredRulesFile(t *testing.T) {
	for _, older := range []bool{true, false} {
		d := startCurrentDaemon(t, validRules, false, rereading...)
		dir := d.devcontainerWith(t, older)
		r := restartConfig(t)
		r.Settings = resolving(t, nil)
		out, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, "")
		if err != nil {
			t.Fatalf("restart: %v; printed %q", err, out)
		}
		warning := "hostrunner: warning: " + filepath.Join(dir, "hostrun.yaml") + " is ignored: " + filepath.Join(dir, "hostrunner-rules.yaml") + " is used; remove it\n"
		want := 0
		if older {
			want = 1
		}
		if got := strings.Count(out, warning); got != want || strings.Count(out, "warning") != want {
			t.Errorf("older file %t: printed %q, want the warning %d times", older, out, want)
		}
		if !strings.Contains(out, "hostrunner: restarted "+d.dir) {
			t.Errorf("older file %t: the restart did not go on: %q", older, out)
		}
		d.waitStopped(t)
	}
}

// A restart --all warns once per ignored file within one run, also when
// several daemons serve the workspace that holds it.
func TestRestartAllWarnsOnceForDaemonsOfOneWorkspace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var daemons []*currentDaemon
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		daemons = append(daemons, startCurrentDaemonAt(t, dir, ws, validRules, false, rereading...))
	}
	devcontainer := daemons[0].devcontainerWith(t, true)
	r := restartConfig(t)
	r.Settings = resolving(t, nil)
	out, err := runStop(t, stop.Config{Root: root, All: true, Restart: r}, "")
	if err != nil {
		t.Fatalf("restart --all: %v; printed %q", err, out)
	}
	warning := "hostrunner: warning: " + filepath.Join(devcontainer, "hostrun.yaml") + " is ignored: " + filepath.Join(devcontainer, "hostrunner-rules.yaml") + " is used; remove it\n"
	if got := strings.Count(out, warning); got != 1 || strings.Count(out, "warning") != 1 {
		t.Errorf("printed %q, want the warning once", out)
	}
	for _, d := range daemons {
		if !strings.Contains(out, "hostrunner: restarted "+d.dir) {
			t.Errorf("the restart of %s did not go on: %q", d.dir, out)
		}
		d.waitStopped(t)
	}
}

// An explicit rules_file is used as given: no warning, whatever lies
// beside the default.
func TestRestartWithAGivenRulesFileDoesNotWarn(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false, rereading...)
	dir := d.devcontainerWith(t, true)
	r := restartConfig(t)
	r.Settings = resolving(t, map[string]string{"HOSTRUNNER_RULES_FILE": filepath.Join(dir, "hostrunner-rules.yaml")})
	out, err := runStop(t, stop.Config{Dir: d.dir, Restart: r}, "")
	if err != nil || strings.Contains(out, "warning") {
		t.Fatalf("restart: %v; printed %q", err, out)
	}
	d.waitStopped(t)
}
