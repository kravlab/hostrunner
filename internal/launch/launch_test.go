package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/allow/allowtest"
	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/settings"
	"github.com/kravlab/hostrunner/internal/shared"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
)

// fakeDaemonEnv makes the test binary act as the daemon when Up spawns it.
// Values: "serve" listens and answers FrameArm on its approval socket;
// "older" does too, as a daemon older than DaemonInfo.Settings, which does
// not say its settings; "hang" never listens.
const fakeDaemonEnv = "HOSTRUNNER_LAUNCH_FAKE_DAEMON"

// runUpEnv makes the test binary run Up itself (as `hostrunner up` would),
// with the runtime dir and client given in the variable ("dir:client").
const runUpEnv = "HOSTRUNNER_LAUNCH_RUN_UP"

// addSharedEnv makes the test binary ask the shared process on the control
// socket the variable names to add a daemon, exiting 0 once it answered
// that it added it and 1 otherwise.
const addSharedEnv = "HOSTRUNNER_LAUNCH_ADD_SHARED"

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(addSharedEnv) != "":
		if err := shared.Add(context.Background(), os.Getenv(addSharedEnv), protocol.AddDaemon{}); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	case os.Getenv(runUpEnv) != "":
		dir, client, _ := strings.Cut(os.Getenv(runUpEnv), ":")
		os.Unsetenv(runUpEnv) // the daemon Up spawns must not run Up again
		os.Setenv(fakeDaemonEnv, "serve")
		cfg := Config{
			Dir: dir, Workspace: "/w", ContainerWorkspace: "/c",
			Daemon: os.Args[0], Client: client,
			Allows:   allow.Store{Dir: filepath.Join(dir, "allow")},
			Settings: settings.Settings{RulesFile: filepath.Join(dir, "missing-rules.yaml"), Reread: true, ReadyTimeout: 5 * time.Second, Process: settings.ProcessOwn},
		}
		// The rules file is missing, which has the digest "missing".
		if _, err := cfg.Allows.AllowAll(cfg.Workspace, []allow.Entry{{Place: cfg.place(), Target: allow.NewTarget(cfg.RulesFile, "missing", cfg.Settings.AllowedSettings())}}); err != nil {
			os.Exit(1)
		}
		if err := Up(context.Background(), cfg); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case os.Getenv(fakeDaemonEnv) != "":
		runFakeDaemon(os.Getenv(fakeDaemonEnv))
		return
	}
	code := m.Run()
	for _, dir := range []string{staticDir, hostrunnerDir} {
		if dir != "" {
			os.RemoveAll(dir)
		}
	}
	os.Exit(code)
}

// runFakeDaemon records its pid in fake.pids and, in "serve" and "older"
// mode, listens on --socket, where it refuses every arm as the real
// daemon does, and on its approval socket, where it answers hellos with
// what it serves and arms (see answerApprovalSocket). It insists on
// --watch, which `up` must always pass.
func runFakeDaemon(mode string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := fs.String("socket", "", "")
	workspace := fs.String("workspace", "", "")
	fs.String("container-workspace", "", "")
	var own settings.Settings
	config := fs.String("rules-file", "", "")
	watch := fs.Bool("watch", false, "")
	fs.DurationVar(&own.StartupTimeout, "startup-timeout", 0, "")
	fs.DurationVar(&own.Grace, "grace", 0, "")
	fs.DurationVar(&own.PollInterval, "poll-interval", 0, "")
	fs.DurationVar(&own.AskTimeout, "ask-timeout", 0, "")
	fs.DurationVar(&own.RequestTimeout, "request-timeout", 0, "")
	process := fs.String("process", "", "")
	fs.Bool("reread", false, "") // reread is !--no-reread: up passes one of the two
	noReread := fs.Bool("no-reread", false, "")
	fs.Bool("mount-check", false, "") // likewise, the mount check is !--no-mount-check
	noMountCheck := fs.Bool("no-mount-check", false, "")
	if len(os.Args) < 2 || os.Args[1] != "serve" || fs.Parse(os.Args[2:]) != nil || !*watch || *config == "" {
		os.Exit(2)
	}
	own.RulesFile, own.Reread, own.MountCheck, own.Process = *config, !*noReread, !*noMountCheck, settings.Process(*process)
	policy, err := rules.Load(*config)
	if err != nil {
		os.Exit(3)
	}
	dir := filepath.Dir(*socket)
	appendLine(filepath.Join(dir, "fake.pids"), strconv.Itoa(os.Getpid()))
	if mode == "hang" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	l, err := net.Listen("unix", *socket)
	if err != nil {
		os.Exit(1)
	}
	d := fakeDaemon{
		dir: dir, digest: policy.Digest(), noReread: *noReread,
		settings: own.ArmSettings(), // without a duration up did not pass, which is then not compared
		socket:   l,
	}
	d.info = protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: *workspace, RulesFile: *config, RulesDigest: d.digest, Stops: true, Rereads: !*noReread, NoReread: *noReread}
	if mode != "older" {
		d.info.Settings = own.AllowedSettings() // what the daemon says it serves: what the allow covers
	}
	d.answerApprovalSocket(ApprovalSocket(dir))
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(0)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameArm {
			_ = protocol.WriteJSON(c, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "an arm is taken on the approval socket only"})
		}
		c.Close()
	}
}

// fakeDaemon is what a fake daemon serves: its runtime directory, where
// it records what it is told, the digest of its rules, its --no-reread,
// its daemon settings, which an arm is compared with (its info says only
// those of them the allow covers, without process, as the real daemon's
// does), what it answers hellos with, and its socket, which it closes as it
// steps aside.
type fakeDaemon struct {
	dir      string
	digest   string
	noReread bool
	settings map[string]string
	info     protocol.DaemonInfo
	socket   net.Listener
}

// answerApprovalSocket listens on the approval socket socket, removing
// the one a fake daemon that stepped aside left behind, and answers every
// hello on it with d.info and every arm (see arm) until the process
// exits. It listens before it returns, so the daemon answers hellos and
// arms once it has returned.
func (d fakeDaemon) answerApprovalSocket(socket string) {
	os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(1)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				f, err := protocol.ReadFrame(c)
				switch {
				case err != nil:
				case f.Type == protocol.FrameHello:
					_ = protocol.WriteJSON(c, protocol.FrameDaemonInfo, d.info)
				case f.Type == protocol.FrameArm:
					d.arm(c, f, l)
				}
			}()
		}
	}()
}

// arm answers the arm f on c, logging it to fake.armed. Like the real
// daemon (armHandler in cmd/hostrunner, which a change of the comparison
// must keep it in step with) it compares the arm's rules digest with the
// rules it started with, the arm's --no-reread with its own, and each
// setting the arm carries with the one it parsed from its flags, and, on
// a mismatch, steps aside: it closes its sockets, approval first, and
// exits.
func (d fakeDaemon) arm(c net.Conn, f protocol.Frame, approval net.Listener) {
	var a protocol.Arm
	_ = protocol.DecodeJSON(f, &a)
	stepAside := a.ConfigDigest != d.digest || a.NoReread != d.noReread
	for name, v := range a.Settings {
		got, ok := d.settings[name]
		stepAside = stepAside || !ok || got != v
	}
	appendLine(filepath.Join(d.dir, "fake.armed"), map[bool]string{false: "armed", true: "step aside"}[stepAside])
	// What the arm reports not allowed, for the tests to read: the real
	// daemon warns hostrun of it (see pendingChanges).
	if a.NotAllowed != nil {
		if b, err := json.Marshal(a.NotAllowed); err == nil {
			appendLine(filepath.Join(d.dir, "fake.notallowed"), string(b))
		}
	}
	_ = protocol.WriteJSON(c, protocol.FrameArmed, protocol.Armed{Version: protocol.Version, Restart: stepAside})
	if stepAside {
		c.Close()
		approval.Close()
		d.socket.Close()
		os.Exit(0)
	}
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\n")
}

// lines returns the non-empty lines of a fake's record file.
func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// staticDir holds the static helper; TestMain removes it.
var staticDir string

// staticBinary builds a small CGO-free program once per test run.
var staticBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "hr-static")
	if err != nil {
		return "", err
	}
	staticDir = dir
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "static")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &exec.ExitError{Stderr: out}
	}
	return bin, nil
})

// hostrunnerDir holds the hostrunner binary; TestMain removes it.
var hostrunnerDir string

// hostrunnerBinary builds the hostrunner command once per test run, for
// the tests that start the real daemon.
var hostrunnerBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "hr-daemon")
	if err != nil {
		return "", err
	}
	hostrunnerDir = dir
	bin := filepath.Join(dir, "hostrunner")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/kravlab/hostrunner/cmd/hostrunner").CombinedOutput(); err != nil {
		return "", &exec.ExitError{Stderr: out}
	}
	return bin, nil
})

// mustHostrunner returns the hostrunner binary (see hostrunnerBinary).
func mustHostrunner(t *testing.T) string {
	t.Helper()
	bin, err := hostrunnerBinary()
	if err != nil {
		t.Fatalf("build hostrunner: %v", err)
	}
	return bin
}

func mustStatic(t *testing.T) string {
	t.Helper()
	bin, err := staticBinary()
	if err != nil {
		t.Fatalf("build static helper: %v", err)
	}
	return bin
}

// shortDir returns a temp directory with a short path, so socket paths stay
// under the sun_path limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fakeConfig returns a Config that spawns the fake daemon in mode, and
// kills every fake daemon started into its Dir when the test ends. Its
// rules file is missing and that, with its settings, is allowed.
func fakeConfig(t *testing.T, mode string) Config {
	t.Helper()
	base := shortDir(t)
	cfg := Config{
		Dir:                filepath.Join(base, "rt", "id"),
		Workspace:          "/home/u/app",
		ContainerWorkspace: "/workspaces/app",
		Daemon:             os.Args[0],
		Client:             mustStatic(t),
		Allows:             allow.Store{Dir: filepath.Join(base, "allow")},
		Settings:           settings.Settings{RulesFile: filepath.Join(base, "hostrun.yaml"), Reread: true, ReadyTimeout: 5 * time.Second, Process: settings.ProcessOwn},
	}
	allowCurrent(t, cfg)
	t.Setenv(fakeDaemonEnv, mode)
	t.Cleanup(func() {
		for _, pid := range lines(t, filepath.Join(cfg.Dir, "fake.pids")) {
			if n, err := strconv.Atoi(pid); err == nil {
				syscall.Kill(n, syscall.SIGKILL)
			}
		}
	})
	return cfg
}

// allowCurrent allows what a human running `hostrunner allow` would:
// the rules file of cfg as it is now, with the daemon settings of cfg,
// for its runtime directory.
func allowCurrent(t *testing.T, cfg Config) {
	t.Helper()
	policy, err := rules.Load(cfg.RulesFile)
	if err != nil {
		t.Fatal(err)
	}
	allowtest.Allow(t, cfg.Allows, cfg.place(), allow.NewTarget(cfg.RulesFile, policy.Digest(), cfg.Settings.AllowedSettings()))
}

// pendingOf returns the pending request of the runtime directory of cfg,
// as PendingAll returns it; ok is false when there is none.
func pendingOf(t *testing.T, cfg Config) (e allow.Entry, ok bool, err error) {
	t.Helper()
	requests, err := cfg.Allows.PendingAll(cfg.Workspace)
	if err != nil {
		return allow.Entry{}, false, err
	}
	for _, e := range requests {
		if same, err := allow.SameWorkspace(e.RuntimeDir, cfg.Dir); err != nil || same {
			return e, err == nil, err
		}
	}
	return allow.Entry{}, false, nil
}

// alivePid reports whether process pid exists.
func alivePid(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitGone fails unless process pid exits within 5 s.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alivePid(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sessionID reads the session id of pid from /proc.
func sessionID(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// Fields after the parenthesised command: state ppid pgrp session …
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func onlyPid(t *testing.T, dir string) int {
	t.Helper()
	pids := lines(t, filepath.Join(dir, "fake.pids"))
	if len(pids) != 1 {
		t.Fatalf("want exactly one daemon started, got pids %v", pids)
	}
	pid, _ := strconv.Atoi(pids[0])
	return pid
}

func TestUpStartsDaemonAndInstallsClient(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	// A pre-existing directory with loose permissions gets tightened.
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}

	pid := onlyPid(t, cfg.Dir)
	if !alivePid(pid) {
		t.Fatal("daemon is not running")
	}
	if sid := sessionID(t, pid); sid != pid {
		t.Fatalf("daemon session %d, want its own session %d", sid, pid)
	}
	info, err := os.Stat(cfg.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir: %v, mode %v; want 0700", err, info.Mode().Perm())
	}
	want, _ := os.ReadFile(cfg.Client)
	got, err := os.ReadFile(filepath.Join(cfg.Dir, ClientName))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("client not installed: %v", err)
	}
	if info, _ := os.Stat(filepath.Join(cfg.Dir, ClientName)); info.Mode().Perm() != 0o755 {
		t.Fatalf("client mode %v, want 0755", info.Mode().Perm())
	}
}

func TestUpArmsRunningDaemonInsteadOfStartingAnother(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	armedBefore := len(lines(t, filepath.Join(cfg.Dir, "fake.armed")))
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	onlyPid(t, cfg.Dir)
	if armed := len(lines(t, filepath.Join(cfg.Dir, "fake.armed"))); armed != armedBefore+1 {
		t.Fatalf("daemon armed %d times by the second Up, want 1", armed-armedBefore)
	}
}

func TestConcurrentUpsShareOneDaemon(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = Up(context.Background(), cfg) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Up %d: %v", i, err)
		}
	}
	onlyPid(t, cfg.Dir)
}

func TestUpReportsSocketThatDoesNotArm(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Daemon = "/nonexistent/hostrunner" // spawning would fail differently
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(cfg.Dir, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close() // an old or foreign listener that does not speak Arm
		}
	}()
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "arm") {
		t.Fatalf("got %v, want an arming error", err)
	}
}

// An approval socket that does not answer arms, as a foreign listener, is
// an arming error, and up starts nothing.
func TestUpReportsAnApprovalSocketThatDoesNotArm(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	listenClosing(t, ApprovalSocket(cfg.Dir))
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "the arm request") {
		t.Fatalf("got %v, want an arming error", err)
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 0 {
		t.Fatalf("a daemon was started: %v", pids)
	}
}

// A daemon older than arms on the approval socket answers the arm there
// with an error, as for any frame but a hello: up fails as for any arm
// that fails, naming the socket to free, and starts nothing.
func TestUpFailsForADaemonThatRefusesTheArmOnItsApprovalSocket(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	listenClosing(t, Socket(cfg.Dir))
	l, err := net.Listen("unix", ApprovalSocket(cfg.Dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if f, err := protocol.ReadFrame(c); err == nil {
				_ = protocol.WriteJSON(c, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "expected a hello on the approval socket, got frame type " + strconv.Itoa(int(f.Type))})
			}
			c.Close()
		}
	}()
	err = Up(context.Background(), cfg)
	want := "refused to arm: expected a hello on the approval socket, got frame type 9; stop the process serving " + Socket(cfg.Dir) + " and retry"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an error containing %q", err, want)
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 0 {
		t.Fatalf("a daemon was started: %v", pids)
	}
}

// listenClosing listens on socket until the test ends and closes every
// connection at once, as a listener that does not speak Arm.
func listenClosing(t *testing.T, socket string) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

func TestUpDoesNotHoldCallerOutputOpen(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runUpEnv+"="+cfg.Dir+":"+cfg.Client)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// devcontainer waits for EOF on initializeCommand's output: it must come
	// as soon as `up` exits, although the daemon keeps running.
	eof := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stdout); eof <- err }()
	select {
	case <-eof:
	case <-time.After(10 * time.Second):
		t.Fatal("caller's stdout stayed open after up exited")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("up failed: %v", err)
	}
	if !alivePid(onlyPid(t, cfg.Dir)) {
		t.Fatal("daemon did not outlive up")
	}
}

func TestUpKillsDaemonThatNeverListens(t *testing.T) {
	cfg := fakeConfig(t, "hang")
	cfg.ReadyTimeout = 300 * time.Millisecond
	err := Up(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "did not listen") {
		t.Fatalf("got %v, want a readiness timeout", err)
	}
	waitGone(t, onlyPid(t, cfg.Dir))
}

func TestUpReportsDaemonThatExitsEarly(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"failure", "echo 'boom: bad flags' >&2\nexit 3"},
		{"success", "echo 'boom: nothing to do' >&2\nexit 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fakeConfig(t, "serve")
			script := filepath.Join(shortDir(t), "daemon")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg.Daemon = script
			err := Up(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "boom:") || strings.Contains(err.Error(), "<nil>") {
				t.Fatalf("got %v, want an error carrying the daemon log", err)
			}
		})
	}
}

func TestUpRefusesSymlinkedLog(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(shortDir(t), "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(cfg.Dir, LogName)); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), cfg); err == nil {
		t.Fatal("Up followed a symlinked daemon log")
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep\n" {
		t.Fatalf("symlink target was written: %q", data)
	}
}

func TestUpRejectsDynamicallyLinkedClient(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh in PATH")
	}
	cfg.Client, _ = filepath.EvalSymlinks(sh)
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=0") {
		t.Fatalf("got %v, want a static-build error", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, ClientName)); err == nil {
		t.Fatal("dynamic client was installed")
	}
}

func TestUpRejectsSocketPathOverLimit(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Dir = filepath.Join(shortDir(t), strings.Repeat("d", 120))
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("got %v, want a path-length error", err)
	}
}

func TestUpExplainsUnsetRuntimeDir(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	t.Setenv("XDG_RUNTIME_DIR", "")
	// ${localEnv:XDG_RUNTIME_DIR}/hostrunner/<id> becomes /hostrunner/<id>,
	// which root can create; no one can create a path under a regular file.
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Dir = filepath.Join(notDir, "hostrunner", "id")
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatalf("got %v, want a hint about XDG_RUNTIME_DIR", err)
	}
}

func TestCheckStatic(t *testing.T) {
	if err := checkStatic(mustStatic(t)); err != nil {
		t.Errorf("static binary rejected: %v", err)
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		if err := checkStatic(sh); err == nil {
			t.Errorf("dynamic %s accepted", sh)
		}
	}
	notELF := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkStatic(notELF); err == nil {
		t.Error("non-ELF file accepted")
	}
}

func TestUpRejectsInvalidRules(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), cfg.RulesFile) {
		t.Fatalf("got %v, want an error naming the rules file", err)
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 0 {
		t.Fatalf("a daemon was started despite invalid rules: %v", pids)
	}
}

func TestUpReplacesDaemonThatStepsAside(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: git status\n    args: any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allowCurrent(t, cfg)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)

	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: git status\n    args: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allowCurrent(t, cfg)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up after the rules changed: %v", err)
	}
	waitGone(t, first)
	pids := lines(t, filepath.Join(cfg.Dir, "fake.pids"))
	if len(pids) != 2 {
		t.Fatalf("want a second daemon, got pids %v", pids)
	}
	second, _ := strconv.Atoi(pids[1])
	if !alivePid(second) {
		t.Fatal("the new daemon is not running")
	}
}

// An up whose --no-reread differs from the running daemon's replaces it
// with one started with the up's setting, which the next such up arms.
func TestUpReplacesDaemonWhoseRereadingDiffers(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)
	cfg.Reread = false
	allowCurrent(t, cfg)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up with --no-reread: %v", err)
	}
	waitGone(t, first)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("another Up with --no-reread: %v", err)
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 2 {
		t.Fatalf("got pids %v, want the daemon replaced once", pids)
	}
	if got := lines(t, filepath.Join(cfg.Dir, "fake.armed")); strings.Join(got, " ") != "armed step aside armed armed" {
		t.Fatalf("arms %q, want armed, step aside, then the new daemon armed twice", got)
	}
}

// The daemon `up` starts gets every setting as an explicit flag, so its
// argv shows them and it resolves none again, and the workspace settings
// file up resolved, which the daemon protects; rereading and the mount
// check are explicit both ways, the process (own or shared) is given too
// so the daemon knows it, and ready_timeout, up's own, is no flag.
func TestServeArgsGiveEverySetting(t *testing.T) {
	base := Config{Workspace: "/home/u/app", ContainerWorkspace: "/workspaces/app", WorkspaceSettings: "/home/u/app/.devcontainer/hostrunner.yaml", Settings: settings.Settings{
		RulesFile: "/home/u/app/.devcontainer/hostrun.yaml", StartupTimeout: 30 * time.Minute, Grace: 15 * time.Second,
		PollInterval: 2 * time.Second, AskTimeout: 10 * time.Minute, RequestTimeout: 10 * time.Second, ReadyTimeout: time.Minute,
	}}
	start := []string{"serve", "--socket", "/run/h.sock", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app", "--workspace-settings", "/home/u/app/.devcontainer/hostrunner.yaml", "--rules-file", "/home/u/app/.devcontainer/hostrun.yaml", "--watch",
		"--startup-timeout", "30m0s", "--grace", "15s", "--poll-interval", "2s", "--ask-timeout", "10m0s", "--request-timeout", "10s"}
	for _, tt := range []struct {
		reread, mountCheck bool
		process            settings.Process
		flags              []string
	}{
		{reread: true, mountCheck: true, process: settings.ProcessOwn, flags: []string{"--process", "own", "--reread", "--mount-check"}},
		{reread: false, mountCheck: true, process: settings.ProcessOwn, flags: []string{"--process", "own", "--no-reread", "--mount-check"}},
		{reread: true, mountCheck: false, process: settings.ProcessOwn, flags: []string{"--process", "own", "--reread", "--no-mount-check"}},
		{reread: true, mountCheck: true, process: settings.ProcessShared, flags: []string{"--process", "shared", "--reread", "--mount-check"}},
	} {
		cfg := base
		cfg.Reread, cfg.MountCheck, cfg.Process = tt.reread, tt.mountCheck, tt.process
		want := append(slices.Clone(start), tt.flags...)
		if got := serveArgs(cfg, "/run/h.sock"); !slices.Equal(got, want) {
			t.Errorf("reread %v, mount check %v, process %s: got %q, want %q", tt.reread, tt.mountCheck, tt.process, got, want)
		}
	}
}

// Without a workspace settings file in the Config, as restart gives none,
// the daemon gets no --workspace-settings and resolves the file itself.
func TestServeArgsOmitAnUnnamedWorkspaceSettingsFile(t *testing.T) {
	cfg := Config{Workspace: "/home/u/app", ContainerWorkspace: "/workspaces/app", Settings: settings.Settings{RulesFile: "/r.yaml"}}
	if got := serveArgs(cfg, "/run/h.sock"); slices.Contains(got, "--workspace-settings") {
		t.Fatalf("got %q, want no --workspace-settings", got)
	}
}

func TestLockDirMakesTheNextWait(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan func(), 1)
	go func() {
		next, err := LockDir(dir)
		if err != nil {
			t.Error(err)
			close(locked)
			return
		}
		locked <- next
	}()
	select {
	case <-locked:
		t.Fatal("a second LockDir took the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case next, ok := <-locked:
		if ok {
			next()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second LockDir did not take the lock once it was released")
	}
}

func TestWaitStoppedReturnsOnceNothingAccepts(t *testing.T) {
	socket := filepath.Join(t.TempDir(), SocketName)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, func() { l.Close() })
	if err := WaitStopped(context.Background(), socket, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitStoppedGivesUpAfterTheTimeout(t *testing.T) {
	socket := filepath.Join(t.TempDir(), SocketName)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	err = WaitStopped(context.Background(), socket, 100*time.Millisecond)
	if want := "the daemon on " + socket + " did not stop within 100ms"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestTryLockDirDoesNotWait(t *testing.T) {
	dir := t.TempDir()
	unlock, busy, err := TryLockDir(dir)
	if err != nil || busy || unlock == nil {
		t.Fatalf("free lock: unlock %v, busy %v, err %v", unlock != nil, busy, err)
	}
	if again, busy, err := TryLockDir(dir); err != nil || !busy || again != nil {
		t.Fatalf("held lock: unlock %v, busy %v, err %v", again != nil, busy, err)
	}
	unlock()
	again, busy, err := TryLockDir(dir)
	if err != nil || busy {
		t.Fatalf("released lock: busy %v, err %v", busy, err)
	}
	again()
}

func TestPrepareRejectsInvalidRules(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(cfg); err == nil || !strings.Contains(err.Error(), cfg.RulesFile) {
		t.Fatalf("got %v, want an error naming the rules file", err)
	}
}

// UpLocked runs under the caller's lock: it does not wait for the lock
// itself.
func TestUpLockedStartsADaemonUnderTheCallersLock(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	p, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockDir(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := make(chan error, 1)
	go func() { done <- p.UpLocked(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UpLocked waited for the lock its caller holds")
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 1 {
		t.Fatalf("started %d daemons, want 1", len(pids))
	}
}

func TestSocketPaths(t *testing.T) {
	const dir = "/run/user/1000/hostrunner/abc"
	if got, want := Socket(dir+"/"), dir+"/hostrunner.sock"; got != want {
		t.Errorf("Socket = %q, want %q", got, want)
	}
	approval := ApprovalSocket(dir + "/")
	if want := dir + ".approval.sock"; approval != want {
		t.Errorf("ApprovalSocket = %q, want %q", approval, want)
	}
	if got := RuntimeDir(approval); got != dir {
		t.Errorf("RuntimeDir(%q) = %q, want %q", approval, got, dir)
	}
}

// An arm carries the daemon settings of up as Settings.ArmSettings names them,
// and still carries --no-reread for a daemon that predates the settings.
func TestArmCarriesTheDaemonSettings(t *testing.T) {
	for _, tt := range []struct {
		name         string
		s            settings.Settings
		want         map[string]string
		wantNoReread bool
	}{
		{
			name: "every setting",
			s: settings.Settings{
				RulesFile: "/w/rules.yaml", StartupTimeout: time.Hour, Grace: time.Minute, Reread: false,
				PollInterval: time.Second, AskTimeout: time.Minute, RequestTimeout: time.Second, ReadyTimeout: time.Minute,
				Process: settings.ProcessShared, MountCheck: true,
			},
			want: map[string]string{
				"rules_file": "/w/rules.yaml", "startup_timeout": "1h0m0s", "grace": "1m0s", "reread": "false",
				"poll_interval": "1s", "ask_timeout": "1m0s", "request_timeout": "1s", "process": "shared", "mount_check": "true",
			},
			wantNoReread: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "s")
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { l.Close() })
			got := make(chan protocol.Arm, 1)
			go func() {
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				var a protocol.Arm
				if f, err := protocol.ReadFrame(c); err == nil && protocol.DecodeJSON(f, &a) == nil {
					got <- a
				}
				_ = protocol.WriteJSON(c, protocol.FrameArmed, protocol.Armed{Version: protocol.Version})
			}()
			if _, err := arm(context.Background(), socket, upArm("digest", tt.s)); err != nil {
				t.Fatal(err)
			}
			a := <-got
			if !maps.Equal(a.Settings, tt.want) {
				t.Errorf("settings %v, want %v", a.Settings, tt.want)
			}
			if a.NoReread != tt.wantNoReread || a.ConfigDigest != "digest" {
				t.Errorf("no_reread %v and digest %q, want %v and digest", a.NoReread, a.ConfigDigest, tt.wantNoReread)
			}
		})
	}
}

// The daemon up starts, parsing the flags serveArgs gives it as serve
// does, has the daemon settings up resolved, so its first arm does not
// make it step aside.
func TestServeArgsResolveToUpsSettings(t *testing.T) {
	full := settings.Settings{
		RulesFile: "/home/u/app/.devcontainer/hostrun.yaml", StartupTimeout: 30 * time.Minute, Grace: 15 * time.Second,
		Reread: true, PollInterval: 2 * time.Second, AskTimeout: 10 * time.Minute, RequestTimeout: 10 * time.Second,
		ReadyTimeout: 10 * time.Second, Process: settings.ProcessOwn, MountCheck: true,
	}
	elsewhere := full
	elsewhere.RulesFile, elsewhere.Reread, elsewhere.Grace = "/home/u/rules/hostrun.yaml", false, 90*time.Second
	elsewhere.MountCheck, elsewhere.Process = false, settings.ProcessShared
	for _, tt := range []struct {
		name string
		s    settings.Settings
	}{{"the defaults", full}, {"another rules file", elsewhere}} {
		cfg := Config{Workspace: "/home/u/app", ContainerWorkspace: "/workspaces/app", Settings: tt.s}
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.String("socket", "", "")
		fs.String("workspace", "", "")
		fs.String("container-workspace", "", "")
		fs.Bool("watch", false, "")
		var sf settings.Flags
		sf.Register(fs)
		if err := fs.Parse(serveArgs(cfg, "/run/h.sock")[1:]); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		got, err := sf.ResolveAsGiven(func(string) string { return "" }, cfg.Workspace)
		if err != nil || !maps.Equal(got.ArmSettings(), tt.s.ArmSettings()) {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got.ArmSettings(), err, tt.s.ArmSettings())
		}
	}
}

// An up whose daemon settings differ from the running daemon's replaces
// it, and the new daemon follows the next up.
func TestUpReplacesDaemonWhoseSettingsDiffer(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Grace = 15 * time.Second
	allowCurrent(t, cfg)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)
	cfg.Grace = time.Minute
	allowCurrent(t, cfg)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up with another grace: %v", err)
	}
	waitGone(t, first)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("another Up with that grace: %v", err)
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 2 {
		t.Fatalf("got pids %v, want the daemon replaced once", pids)
	}
	if got := lines(t, filepath.Join(cfg.Dir, "fake.armed")); strings.Join(got, " ") != "armed step aside armed armed" {
		t.Fatalf("arms %q, want armed, step aside, then the new daemon armed twice", got)
	}
}

// gitStatusRules is a valid rules file, and gitStatusDigest the digest of
// exactly these bytes.
const (
	gitStatusRules  = "rules:\n  - command: git status\n    args: any\n"
	gitStatusDigest = "5dae3cd4d8455c5930e19b895e6c5d29acf4ac18b6c64010d05cd47dc2c97ac7"
)

// notAllowed fails the test unless err says that the rules file with
// digest, and the daemon settings, are not allowed for the workspace of
// cfg, and that a human allows them with `hostrunner allow`.
func notAllowed(t *testing.T, err error, cfg Config, digest string) {
	t.Helper()
	if err == nil {
		t.Fatal("Up succeeded, want an error that the rules and settings are not allowed")
	}
	for _, want := range []string{cfg.RulesFile, "(digest " + digest + ")", "not allowed for " + cfg.Workspace, "hostrunner allow " + cfg.Workspace} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// nothingStarted fails the test if a daemon was started or armed in dir.
func nothingStarted(t *testing.T, dir string) {
	t.Helper()
	if pids := lines(t, filepath.Join(dir, "fake.pids")); len(pids) != 0 {
		t.Errorf("daemons were started: %v", pids)
	}
	if armed := lines(t, filepath.Join(dir, "fake.armed")); len(armed) != 0 {
		t.Errorf("daemons were armed: %v", armed)
	}
}

func TestUpWithoutAnAllowFailsAndStartsNothing(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Allows = allow.Store{Dir: filepath.Join(t.TempDir(), "allow")}
	notAllowed(t, Up(context.Background(), cfg), cfg, "missing")
	nothingStarted(t, cfg.Dir)
}

func TestUpFailsWithANotAllowedError(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Up(context.Background(), cfg)
	var na *NotAllowedError
	if !errors.As(err, &na) {
		t.Fatalf("got %v, want a *NotAllowedError", err)
	}
	if na.Workspace != cfg.Workspace || na.RulesFile != cfg.RulesFile || na.Digest != gitStatusDigest {
		t.Fatalf("got %+v, want workspace %q, rules file %q and digest %q", na, cfg.Workspace, cfg.RulesFile, gitStatusDigest)
	}
}

// The error shows the rules file and the workspace escaped, so a control
// character in a path cannot reach the terminal raw.
func TestNotAllowedErrorEscapesThePaths(t *testing.T) {
	e := &NotAllowedError{Workspace: "/w\x1b[2J", RulesFile: "/w/r\x1b[2J.yaml", Digest: "abc123"}
	want := `the rules file "/w/r\x1b[2J.yaml" (digest abc123) and the daemon settings are not allowed for "/w\x1b[2J": ` +
		`a human on the host runs hostrunner allow "/w\x1b[2J" to allow them`
	if got := e.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestUpAfterAnAllowStartsTheDaemon(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Allows = allow.Store{Dir: filepath.Join(t.TempDir(), "allow")}
	notAllowed(t, Up(context.Background(), cfg), cfg, "missing")
	allowCurrent(t, cfg)
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up after the allow: %v", err)
	}
	if !alivePid(onlyPid(t, cfg.Dir)) {
		t.Fatal("daemon is not running")
	}
}

// An allow covers one content of the rules file with the daemon settings
// as they were: another content or another daemon setting needs a new
// one.
func TestUpAfterAChangeSinceTheAllowFails(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(t *testing.T, cfg *Config)
		digest string
	}{
		{"a daemon setting", func(t *testing.T, cfg *Config) { cfg.Grace = time.Minute }, "missing"},
		{"the rules file content", func(t *testing.T, cfg *Config) {
			if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
				t.Fatal(err)
			}
		}, gitStatusDigest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fakeConfig(t, "serve")
			tt.change(t, &cfg)
			notAllowed(t, Up(context.Background(), cfg), cfg, tt.digest)
			nothingStarted(t, cfg.Dir)
		})
	}
}

// ready_timeout is up's own setting, which no daemon carries.
func TestUpAfterAChangeOfTheReadyTimeoutNeedsNoNewAllow(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.ReadyTimeout = 7 * time.Second
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}
	onlyPid(t, cfg.Dir)
}

// process is a daemon setting outside the allow: the allow of an up in a
// process of its own covers an up in the shared process of its build, and
// the reverse, so a change of it needs no new allow. The check of the
// allow is what PrepareAllowed runs; starting the shared process needs the
// real daemon, which the tests of the shared process below start.
func TestUpAfterAChangeOfTheProcessNeedsNoNewAllow(t *testing.T) {
	for _, process := range []settings.Process{settings.ProcessShared, settings.ProcessOwn} {
		t.Run(string(process), func(t *testing.T) {
			cfg := fakeConfig(t, "serve") // allowed with process own
			cfg.Process = process
			if _, err := PrepareAllowed(cfg); err != nil {
				t.Fatalf("PrepareAllowed with process %s: %v", process, err)
			}
			if _, ok, err := pendingOf(t, cfg); err != nil || ok {
				t.Fatalf("Pending = %v, %v, want no request", ok, err)
			}
		})
	}
}

// An up whose rules are not allowed arms a running daemon that serves
// allowed rules and settings, which keeps serving them, so a container
// restart does not drop them; it says so on stderr and records its
// request.
func TestUpOfChangedRulesNotAllowedArmsTheDaemonServingAllowedRules(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)
	if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cfg.Stderr = &stderr
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up of rules not allowed: %v", err)
	}
	if pid := onlyPid(t, cfg.Dir); pid != first || !alivePid(first) {
		t.Fatalf("daemon pid %d, alive %v; want %d kept", pid, alivePid(first), first)
	}
	if got := lines(t, filepath.Join(cfg.Dir, "fake.armed")); strings.Join(got, " ") != "armed armed" {
		t.Fatalf("arms %q, want the running daemon armed again", got)
	}
	want := "hostrunner: the rules file " + cfg.RulesFile + " (digest " + gitStatusDigest + ") and the daemon settings are not allowed for " + cfg.Workspace +
		": a human on the host runs hostrunner allow " + cfg.Workspace + " to allow them; the daemon keeps serving the allowed rules (digest missing)\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	if _, ok, err := pendingOf(t, cfg); err != nil || !ok {
		t.Errorf("Pending = %v, %v, want the request recorded", ok, err)
	}
}

// A change of process made together with rules not allowed does not make
// the daemon serving the allowed rules step aside: the arm of the not
// allowed path carries the settings the daemon says it serves, which hold
// no process, so the daemon keeps serving them.
func TestUpOfChangedRulesNotAllowedAndProcessKeepsTheDaemonServingAllowedRules(t *testing.T) {
	cfg := fakeConfig(t, "serve") // allowed with process own
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)
	if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Process = settings.ProcessShared
	cfg.Stderr = io.Discard
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up of rules not allowed and another process: %v", err)
	}
	if pid := onlyPid(t, cfg.Dir); pid != first || !alivePid(first) {
		t.Fatalf("daemon pid %d, alive %v; want %d kept", pid, alivePid(first), first)
	}
	if got := lines(t, filepath.Join(cfg.Dir, "fake.armed")); strings.Join(got, " ") != "armed armed" {
		t.Errorf("arms %q, want the running daemon armed twice and not stepping aside", got)
	}
}

// The arm of a daemon serving allowed rules says what is not allowed, the
// digest and the daemon settings up found, so the daemon warns hostrun of
// the change. An up whose rules and settings are allowed says nothing.
func TestUpNotAllowedTellsTheDaemonWhatIsNotAllowed(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(t *testing.T, cfg *Config)
		digest string
	}{
		{"a daemon setting", func(t *testing.T, cfg *Config) { cfg.Grace = time.Minute }, "missing"},
		{"the rules file content", func(t *testing.T, cfg *Config) {
			if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
				t.Fatal(err)
			}
		}, gitStatusDigest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fakeConfig(t, "serve")
			if err := Up(context.Background(), cfg); err != nil {
				t.Fatalf("first Up: %v", err)
			}
			if got := notAllowedArms(t, cfg.Dir); len(got) != 0 {
				t.Fatalf("the allowed Up reported %v, want nothing", got)
			}
			tt.change(t, &cfg)
			if err := Up(context.Background(), cfg); err != nil {
				t.Fatalf("Up of a change not allowed: %v", err)
			}
			got := notAllowedArms(t, cfg.Dir)
			want := protocol.ArmNotAllowed{Digest: tt.digest, Settings: cfg.Settings.AllowedSettings()}
			if len(got) != 1 || got[0].Digest != want.Digest || !maps.Equal(got[0].Settings, want.Settings) {
				t.Fatalf("the arm reported %+v, want [%+v]", got, want)
			}
			lacksProcess(t, "the arm reported", got[0].Settings)
		})
	}
}

// lacksProcess fails the test when the settings s, which what says it
// holds (the arm, request or daemon settings the allow covers), hold the
// process, a setting the allow does not cover.
func lacksProcess(t *testing.T, what string, s map[string]string) {
	t.Helper()
	if _, ok := s["process"]; ok {
		t.Errorf("%s the settings %v, which hold the process the allow does not cover", what, s)
	}
}

// notAllowedArms returns what the arms the fake daemon in dir received
// reported not allowed.
func notAllowedArms(t *testing.T, dir string) []protocol.ArmNotAllowed {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "fake.notallowed"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []protocol.ArmNotAllowed
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var na protocol.ArmNotAllowed
		if err := json.Unmarshal([]byte(l), &na); err != nil {
			t.Fatalf("fake.notallowed line %q: %v", l, err)
		}
		out = append(out, na)
	}
	return out
}

// Not allowed is fail-closed: a running daemon whose own rules and
// settings are not allowed either, that serves another workspace or that
// does not say its settings is neither armed nor stopped by an up whose
// rules are not allowed.
func TestUpOfChangedRulesNotAllowedLeavesADaemonNotServingAllowedRules(t *testing.T) {
	for _, tt := range []struct {
		name, mode string
		spoil      func(t *testing.T, cfg *Config)
	}{
		{"its allow denied", "serve", func(t *testing.T, cfg *Config) {
			if _, err := cfg.Allows.Deny(cfg.Workspace); err != nil {
				t.Fatal(err)
			}
		}},
		{"other rules allowed", "serve", func(t *testing.T, cfg *Config) {
			allowtest.Allow(t, cfg.Allows, cfg.place(), allow.NewTarget(cfg.RulesFile, "other", cfg.Settings.AllowedSettings()))
		}},
		{"a daemon of another workspace", "serve", func(t *testing.T, cfg *Config) {
			// The other workspace allows what the daemon serves, for the
			// workspace it was started for.
			cfg.Workspace = "/home/u/other"
			allowtest.Allow(t, cfg.Allows, cfg.place(), allow.NewTarget(cfg.RulesFile, "missing", cfg.Settings.AllowedSettings()))
		}},
		{"an older daemon", "older", func(*testing.T, *Config) {}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fakeConfig(t, tt.mode)
			if err := Up(context.Background(), cfg); err != nil {
				t.Fatalf("first Up: %v", err)
			}
			first := onlyPid(t, cfg.Dir)
			if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
				t.Fatal(err)
			}
			tt.spoil(t, &cfg)
			var stderr strings.Builder
			cfg.Stderr = &stderr
			notAllowed(t, Up(context.Background(), cfg), cfg, gitStatusDigest)
			if !alivePid(first) {
				t.Fatal("the running daemon was stopped")
			}
			if pid := onlyPid(t, cfg.Dir); pid != first {
				t.Fatalf("daemon pid %d, want %d", pid, first)
			}
			if got := lines(t, filepath.Join(cfg.Dir, "fake.armed")); strings.Join(got, " ") != "armed" {
				t.Fatalf("arms %q, want only the first Up's", got)
			}
			if stderr.Len() != 0 {
				t.Errorf("printed %q, want nothing", stderr.String())
			}
		})
	}
}

// A refused up records what it asked for, for a human on the host to
// allow with `hostrunner allow`.
func TestUpRefusedRecordsItsRequest(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Grace = time.Minute
	if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	notAllowed(t, Up(context.Background(), cfg), cfg, gitStatusDigest)
	e, ok, err := pendingOf(t, cfg)
	if err != nil || !ok {
		t.Fatalf("Pending = %v, %v, want a request", ok, err)
	}
	if e.Workspace != cfg.Workspace || e.RuntimeDir != cfg.Dir || e.RulesFile != cfg.RulesFile || e.Digest != gitStatusDigest || !maps.Equal(e.Settings, cfg.Settings.AllowedSettings()) {
		t.Fatalf("request %+v, want workspace %q, runtime directory %q, rules file %q, digest %q and settings %v", e, cfg.Workspace, cfg.Dir, cfg.RulesFile, gitStatusDigest, cfg.Settings.AllowedSettings())
	}
	if e.Settings["grace"] != "1m0s" {
		t.Errorf("request grace %q, want 1m0s", e.Settings["grace"])
	}
}

// The request a refused up records is what the allow covers: the daemon
// settings without process, which a human allowing it does not decide.
func TestUpRefusedRecordsItsRequestWithoutTheProcess(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Process = settings.ProcessShared
	if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	notAllowed(t, Up(context.Background(), cfg), cfg, gitStatusDigest)
	e, ok, err := pendingOf(t, cfg)
	if err != nil || !ok {
		t.Fatalf("Pending = %v, %v, want a request", ok, err)
	}
	lacksProcess(t, "the request holds", e.Settings)
	if !maps.Equal(e.Settings, cfg.Settings.AllowedSettings()) {
		t.Errorf("request settings %v, want %v", e.Settings, cfg.Settings.AllowedSettings())
	}
}

// fakeContainerRuntime puts a docker and a podman in PATH, until the test
// ends, that answer every command with no output: the real daemon's watch,
// which needs one, finds no container and waits for it. Both names are
// faked because the watch runs every runtime it finds: a real one further
// in PATH would run too.
func fakeContainerRuntime(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	writeScripts(t, dir, "exit 0", "docker", "podman")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeScripts writes an executable shell script running body into dir
// under each of names.
func writeScripts(t *testing.T, dir, body string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// After fakeContainerRuntime, the daemon's watch finds only the fakes,
// never a real docker or podman later in PATH: a real podman run with the
// test's XDG_DATA_HOME leaves storage there that TempDir cleanup cannot
// remove.
func TestFakeContainerRuntimeHidesEveryRealRuntime(t *testing.T) {
	realDir := t.TempDir()
	writeScripts(t, realDir, "exit 1", "docker", "podman")
	t.Setenv("PATH", realDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fakeContainerRuntime(t)

	found := watch.Detect()
	var binaries []string
	for _, r := range found {
		cli, ok := r.(watch.CLI)
		if !ok {
			t.Fatalf("the watch finds %#v, want a CLI runtime", r)
		}
		binaries = append(binaries, cli.Binary)
	}
	if len(binaries) != 2 {
		t.Fatalf("the watch finds %q, want the fake docker and podman", binaries)
	}
	fakeDir := filepath.Dir(binaries[0])
	if want := []string{filepath.Join(fakeDir, "docker"), filepath.Join(fakeDir, "podman")}; fakeDir == realDir || !slices.Equal(binaries, want) {
		t.Errorf("the watch finds %q, want the fakes of one directory other than the real runtimes' %s", binaries, realDir)
	}
}

// The regression of issue #144 with the real daemon: two devcontainer
// configs of one workspace, each with its runtime directory, whose ups
// get different settings flags. Both ups are refused first, each
// recording its own request; once `hostrunner allow` allows both, each up
// starts its own daemon with its settings; and a new allow of one, for
// other settings, does not make the up of the other fail.
func TestUpOfTwoRuntimeDirsStartsTheRealDaemonOfEachOnceAllowed(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	fakeContainerRuntime(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // the store the daemons open too
	store, err := allow.Open(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	base := shortDir(t)
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	rulesFile := filepath.Join(ws, "hostrun.yaml")
	if err := os.WriteFile(rulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	config := func(name string, grace time.Duration) Config {
		return Config{
			Dir: filepath.Join(base, name), Workspace: ws, ContainerWorkspace: "/workspaces/app",
			Daemon: daemon, Client: mustStatic(t), Allows: store,
			Settings: settings.Settings{
				RulesFile: rulesFile, StartupTimeout: 10 * time.Minute, Grace: grace,
				PollInterval: time.Second, AskTimeout: 10 * time.Minute, RequestTimeout: 10 * time.Second,
				Reread: true, MountCheck: true, ReadyTimeout: 10 * time.Second, Process: settings.ProcessOwn,
			},
		}
	}
	a, b := config("a", 15*time.Second), config("b", time.Minute)
	for _, cfg := range []Config{a, b} {
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(stopCtx, daemon, "stop", "--dir", cfg.Dir, "--kill").CombinedOutput(); err != nil {
				t.Logf("stop %s: %v: %s", cfg.Dir, err, out)
			}
		})
	}
	// served returns the grace the daemon of cfg says it serves with.
	served := func(cfg Config) string {
		t.Helper()
		info, err := daemonlist.Probe(ctx, ApprovalSocket(cfg.Dir))
		if err != nil {
			t.Fatalf("probe the daemon of %s: %v", cfg.Dir, err)
		}
		return info.Settings["grace"]
	}
	// allowPending allows every pending request, as `hostrunner allow`.
	allowPending := func(want int) {
		t.Helper()
		requests, err := store.PendingAll(ws)
		if err != nil || len(requests) != want {
			t.Fatalf("PendingAll = %+v, %v; want %d requests", requests, err, want)
		}
		if _, err := store.AllowAll(ws, requests); err != nil {
			t.Fatal(err)
		}
	}

	notAllowed(t, Up(ctx, a), a, gitStatusDigest)
	notAllowed(t, Up(ctx, b), b, gitStatusDigest)
	allowPending(2)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s once allowed: %v", cfg.Dir, err)
		}
		if got, want := served(cfg), cfg.Grace.String(); got != want {
			t.Errorf("the daemon of %s serves grace %s, want %s", cfg.Dir, got, want)
		}
	}

	// Not allowed yet, the up of A keeps its daemon and asks for the allow.
	a.Grace = 2 * time.Minute
	if err := Up(ctx, a); err != nil {
		t.Fatalf("Up of A with new settings not allowed: %v", err)
	}
	if got := served(a); got != "15s" {
		t.Errorf("the daemon of A kept serves grace %s, want 15s", got)
	}
	allowPending(1)
	if err := Up(ctx, a); err != nil {
		t.Fatalf("Up of A with its new settings once allowed: %v", err)
	}
	if err := Up(ctx, b); err != nil {
		t.Fatalf("Up of B after the new allow of A: %v", err)
	}
	if got := served(a); got != "2m0s" {
		t.Errorf("the daemon of A serves grace %s, want 2m0s", got)
	}
	if got := served(b); got != "1m0s" {
		t.Errorf("the daemon of B serves grace %s, want 1m0s", got)
	}
}

// sharedPair returns the allowed Configs of two runtime directories, A and
// B, under one runtime root, each of its own workspace, whose ups add
// their daemons to the shared process of the real hostrunner daemon, with
// a fake container runtime (see sharedConfigs). Both rules allow printenv
// and pwd; those of B also allow true.
func sharedPair(t *testing.T, daemon string) (a, b Config) {
	t.Helper()
	cfgs := sharedConfigs(t, daemon, sharedRules, sharedRules+"  - command: true\n    args: none\n")
	return cfgs[0], cfgs[1]
}

// sharedRules are rules that allow printenv and pwd.
const sharedRules = "rules:\n  - command: printenv\n    args: any\n  - command: pwd\n    args: none\n"

// sharedConfigs returns the allowed Configs of one runtime directory per
// rules text in rulesTexts, named a, b, c and on, under one runtime root,
// each of its own workspace whose rules file holds that text, whose ups add
// their daemons to the shared process of the real hostrunner daemon, with
// a fake container runtime. When the test ends it stops every daemon and
// fails if a shared process in the root outlives them, killing it.
func sharedConfigs(t *testing.T, daemon string, rulesTexts ...string) []Config {
	t.Helper()
	fakeContainerRuntime(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // the store the daemons open too
	store, err := allow.Open(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	base := shortDir(t)
	config := func(name, rulesText string) Config {
		ws := filepath.Join(base, "ws-"+name)
		if err := os.Mkdir(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		rulesFile := filepath.Join(ws, "hostrun.yaml")
		if err := os.WriteFile(rulesFile, []byte(rulesText), 0o644); err != nil {
			t.Fatal(err)
		}
		return Config{
			Dir: filepath.Join(base, "rt", name), Workspace: ws, ContainerWorkspace: "/workspaces/app",
			Daemon: daemon, Client: mustStatic(t), Allows: store,
			Settings: settings.Settings{
				RulesFile: rulesFile, StartupTimeout: 10 * time.Minute, Grace: time.Minute,
				PollInterval: time.Second, AskTimeout: 10 * time.Minute, RequestTimeout: 10 * time.Second,
				Reread: true, ReadyTimeout: 10 * time.Second, Process: settings.ProcessShared,
			},
		}
	}
	cfgs := make([]Config, len(rulesTexts))
	for i, rulesText := range rulesTexts {
		cfgs[i] = config(string(rune('a'+i)), rulesText)
	}
	t.Cleanup(func() {
		pids := sharedPids(t, filepath.Join(base, "rt"))
		for _, cfg := range cfgs {
			stopDaemon(t, daemon, cfg)
		}
		for _, pid := range pids {
			deadline := time.Now().Add(5 * time.Second)
			for alivePid(pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if alivePid(pid) {
				syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("the shared process %d outlived its last daemon", pid)
			}
		}
	})
	for _, cfg := range cfgs {
		allowCurrent(t, cfg)
	}
	return cfgs
}

// stopDaemon stops the daemon of cfg as `hostrunner stop --dir <dir>
// --kill` does, with the hostrunner executable daemon; a stop that fails,
// for a daemon already gone, is only logged.
func stopDaemon(t *testing.T, daemon string, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, daemon, "stop", "--dir", cfg.Dir, "--kill").CombinedOutput(); err != nil {
		t.Logf("stop %s: %v: %s", cfg.Dir, err, out)
	}
}

// restartDaemon restarts the daemon of cfg as `hostrunner restart --dir
// <dir> --kill` does, with the hostrunner executable daemon, and returns
// what it printed. The new daemon is resolved from the command line and the
// environment, not from cfg: the flags and variables give it the settings
// of cfg (the rules file, the process, the mount check, the timeouts and
// rereading), so that the allow of cfg covers it, and an empty settings
// directory keeps the host's settings files out of it. restart installs the
// hostrun beside daemon, so the static helper is put there under that name.
// A restart that fails fails the test.
func restartDaemon(t *testing.T, daemon string, cfg Config) string {
	t.Helper()
	out, err := restartDaemonErr(t, daemon, cfg)
	if err != nil {
		t.Fatalf("restart %s: %v: %s", cfg.Dir, err, out)
	}
	return out
}

// restartDaemonErr is restartDaemon for a restart that may fail: it returns
// what the restart printed and its error.
func restartDaemonErr(t *testing.T, daemon string, cfg Config) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := restartCommand(ctx, t, daemon, cfg, "--kill").CombinedOutput()
	return string(out), err
}

// restartCommand returns the command `hostrunner restart --dir <dir>
// <answer>` of the hostrunner executable daemon for the daemon of cfg,
// where answer is --kill or --wait, with the flags and variables that give
// the new daemon the settings of cfg (see restartDaemon). It ends with ctx.
func restartCommand(ctx context.Context, t *testing.T, daemon string, cfg Config, answer string) *exec.Cmd {
	t.Helper()
	copyExecutable(t, mustStatic(t), filepath.Join(filepath.Dir(daemon), ClientName))
	s := cfg.Settings
	return hostCommand(ctx, t, daemon, timingsEnv(s), "restart", "--dir", cfg.Dir, answer,
		"--rules-file", s.RulesFile, "--process", string(s.Process), boolFlag("mount-check", s.MountCheck))
}

// upCommand returns the command `hostrunner up` of the hostrunner
// executable daemon for cfg, as a devcontainer's initializeCommand runs
// it, with the flags and variables that give the daemon the settings of
// cfg but the process, which no source sets, so that its default applies
// (an empty HOSTRUNNER_PROCESS is unset, whatever the test's environment
// holds). up installs the hostrun beside daemon, so the static helper is
// put there under that name. It ends with ctx.
func upCommand(ctx context.Context, t *testing.T, daemon string, cfg Config) *exec.Cmd {
	t.Helper()
	copyExecutable(t, mustStatic(t), filepath.Join(filepath.Dir(daemon), ClientName))
	s := cfg.Settings
	env := append(timingsEnv(s), "HOSTRUNNER_READY_TIMEOUT="+s.ReadyTimeout.String(), "HOSTRUNNER_PROCESS=")
	return hostCommand(ctx, t, daemon, env, "up", "--dir", cfg.Dir, "--workspace", cfg.Workspace,
		"--container-workspace", cfg.ContainerWorkspace, "--rules-file", s.RulesFile, boolFlag("mount-check", s.MountCheck))
}

// timingsEnv returns the variables that set the timings (startup timeout,
// grace, poll interval, ask timeout, request timeout) and the rereading of
// s for the daemon that a real hostrunner command starts.
func timingsEnv(s settings.Settings) []string {
	return []string{
		"HOSTRUNNER_STARTUP_TIMEOUT=" + s.StartupTimeout.String(),
		"HOSTRUNNER_GRACE=" + s.Grace.String(),
		"HOSTRUNNER_POLL_INTERVAL=" + s.PollInterval.String(),
		"HOSTRUNNER_ASK_TIMEOUT=" + s.AskTimeout.String(),
		"HOSTRUNNER_REQUEST_TIMEOUT=" + s.RequestTimeout.String(),
		"HOSTRUNNER_REREAD=" + strconv.FormatBool(s.Reread),
	}
}

// hostCommand returns the command `daemon <args>` of a real hostrunner
// executable as a human runs it on the host: it ends with ctx, and runs with
// the environment of the test plus env, and with an empty settings directory,
// so that the host's settings files stay out of it.
func hostCommand(ctx context.Context, t *testing.T, daemon string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, daemon, args...)
	cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir()), env...)
	return cmd
}

// sharedPids returns the pids of the running `hostrunner shared` processes
// whose control socket is in the runtime root root, as /proc shows them. A
// process whose parent is itself one of them is left out: a shared process
// forks children to run the container runtime, and until a child execs its
// /proc/<pid>/cmdline is still its parent's, so it would count as a second
// shared process.
func sharedPids(t *testing.T, root string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	parents := map[int]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) >= 4 && args[1] == "shared" && args[2] == "--socket" && filepath.Dir(args[3]) == root {
			parents[pid] = parentPid(pid)
		}
	}
	var pids []int
	for pid, ppid := range parents {
		if _, forked := parents[ppid]; !forked {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// parentPid returns the parent pid of pid as /proc/<pid>/stat shows it, or 0
// when the process is gone.
func parentPid(pid int) int {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// The command name, field 2, is in parentheses and may hold spaces: the
	// fields that follow the last ')' are the state, then the parent pid.
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// expectOneProcess fails the test unless the daemons of a and b are served
// by one process, and returns its pid.
func expectOneProcess(t *testing.T, a, b Config) int {
	t.Helper()
	pid := servingPid(t, a)
	if other := servingPid(t, b); other != pid {
		t.Fatalf("A is served by process %d, B by %d; want one process", pid, other)
	}
	return pid
}

// servingPid is the pid of the process listening on the socket of cfg.
func servingPid(t *testing.T, cfg Config) int {
	t.Helper()
	c, err := transport.Unix{Path: Socket(cfg.Dir)}.Dial(context.Background())
	if err != nil {
		t.Fatalf("dial the socket of %s: %v", cfg.Dir, err)
	}
	defer c.Close()
	_, pid, err := transport.PeerCred(c)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// runIn runs argv through the daemon of cfg from the container workspace,
// returning its stdout and exit code.
func runIn(t *testing.T, cfg Config, argv ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := client.Run(context.Background(), transport.Unix{Path: Socket(cfg.Dir)}, argv, cfg.ContainerWorkspace, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Logf("%v in %s: exit %d: %s", argv, cfg.Dir, code, stderr.String())
	}
	return stdout.String(), code
}

// With process: shared, the ups of two runtime directories, of two
// workspaces, start one host process that serves both daemons, each with
// its own rules, workspace and the environment of the up that added it.
func TestUpOfTwoRuntimeDirsWithProcessSharedServesBothInOneProcess(t *testing.T) {
	ctx := context.Background()
	a, b := sharedPair(t, mustHostrunner(t))
	t.Setenv("HR_UP_MARK", "a")
	if err := Up(ctx, a); err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	t.Setenv("HR_UP_MARK", "b")
	if err := Up(ctx, b); err != nil {
		t.Fatalf("Up of B: %v", err)
	}

	sharedPid := expectOneProcess(t, a, b)
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(sharedPid) + "/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Split(string(cmdline), "\x00"); len(args) < 2 || args[1] != "shared" {
		t.Fatalf("the process serving both runs %q, want hostrunner shared", args)
	}

	for _, c := range []struct {
		cfg  Config
		mark string
	}{{a, "a"}, {b, "b"}} {
		if got, code := runIn(t, c.cfg, "printenv", "HR_UP_MARK"); got != c.mark+"\n" || code != 0 {
			t.Errorf("printenv HR_UP_MARK in %s: got %q, exit %d; want %q, exit 0", c.cfg.Dir, got, code, c.mark+"\n")
		}
		if got, code := runIn(t, c.cfg, "pwd"); got != c.cfg.Workspace+"\n" || code != 0 {
			t.Errorf("pwd in %s: got %q, exit %d; want %q, exit 0", c.cfg.Dir, got, code, c.cfg.Workspace+"\n")
		}
	}
	if _, code := runIn(t, b, "true"); code != 0 {
		t.Errorf("true, allowed by the rules of B, exits %d there, want 0", code)
	}
	if _, code := runIn(t, a, "true"); code != protocol.ExitRejected {
		t.Errorf("true, not in the rules of A, exits %d there, want %d", code, protocol.ExitRejected)
	}
}

// With no process setting from any source, the real `hostrunner up` of two
// runtime directories adds both daemons to one shared process: shared is
// the default.
func TestUpWithNoProcessSettingAddsTwoDaemonsToOneSharedProcess(t *testing.T) {
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := upCommand(ctx, t, daemon, cfg).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("hostrunner up of %s: %v: %s", cfg.Dir, err, out)
		}
	}
	pid := expectOneProcess(t, a, b)
	if pids := sharedPids(t, filepath.Dir(a.Dir)); !slices.Equal(pids, []int{pid}) {
		t.Fatalf("the shared processes of the runtime root are %v; want %d alone, serving both", pids, pid)
	}
}

// Concurrent ups of one build, for runtime directories of one runtime
// root, all succeed and end up in one shared process holding every daemon.
func TestConcurrentUpsWithProcessSharedEndUpInOneSharedProcess(t *testing.T) {
	cfgs := sharedConfigs(t, mustHostrunner(t), slices.Repeat([]string{sharedRules}, 5)...)
	start := make(chan struct{})
	errs := make([]error, len(cfgs))
	var wg sync.WaitGroup
	for i, cfg := range cfgs {
		wg.Go(func() {
			<-start
			errs[i] = Up(context.Background(), cfg)
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Up of %s: %v", cfgs[i].Dir, err)
		}
	}
	pid := servingPid(t, cfgs[0])
	for _, cfg := range cfgs[1:] {
		if got := servingPid(t, cfg); got != pid {
			t.Errorf("%s is served by process %d, %s by %d; want one process", cfgs[0].Dir, pid, cfg.Dir, got)
		}
	}
	if kind := processKind(t, pid); kind != "shared" {
		t.Errorf("the process serving every daemon runs %s, want shared", kind)
	}
}

// fakeEndingProcess listens on the control socket of the shared process
// of cfg in its runtime root, as an ending shared process would, and hands
// each of the first n connections to answer, closing it after; then it
// closes the listener, which removes the socket. It returns a channel
// closed once it did, and closes the listener when the test ends at the
// latest.
func fakeEndingProcess(t *testing.T, cfg Config, n int, answer func(c net.Conn)) <-chan struct{} {
	t.Helper()
	control, _, err := shared.Files(filepath.Dir(cfg.Dir), cfg.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		defer l.Close()
		for range n {
			c, err := l.Accept()
			if err != nil {
				return
			}
			answer(c)
			c.Close()
		}
	}()
	return closed
}

// answerEnding reads the add request on c and answers that the process is
// ending.
func answerEnding(c net.Conn) {
	if _, err := protocol.ReadFrame(c); err == nil {
		_ = protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameEnding})
	}
}

// An up whose add reaches a shared process that is ending, which refuses
// the add once or several times or closes the connection unanswered and
// then stops listening, starts a new shared process and adds the daemon
// there.
func TestUpWithProcessSharedThatMeetsAnEndingProcessAddsTheDaemonToANewOne(t *testing.T) {
	cases := []struct {
		name   string
		n      int              // the connections the ending process answers
		answer func(c net.Conn) // how it answers each
	}{
		{"refused once", 1, answerEnding},
		{"refused three times", 3, answerEnding},
		{"closed unanswered", 1, func(c net.Conn) { _, _ = protocol.ReadFrame(c) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := sharedPair(t, mustHostrunner(t))
			closed := fakeEndingProcess(t, a, tc.n, tc.answer)
			if err := Up(context.Background(), a); err != nil {
				t.Fatalf("Up of A: %v", err)
			}
			select {
			case <-closed:
			default:
				t.Fatal("Up succeeded before the ending process stopped listening")
			}
			pid := servingPid(t, a)
			if pid == os.Getpid() {
				t.Fatal("the daemon of A is served by the test process")
			}
			if kind := processKind(t, pid); kind != "shared" {
				t.Errorf("the daemon of A is served by a process that runs %s, want shared", kind)
			}
		})
	}
}

// socketFds returns the sockets the process pid holds open, as the targets
// of its descriptors in /proc show them ("socket:[<inode>]").
func socketFds(t *testing.T, pid int) map[string]bool {
	t.Helper()
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sockets := map[string]bool{}
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && strings.HasPrefix(target, "socket:") {
			sockets[target] = true
		}
	}
	return sockets
}

// An up racing the end of a real shared process, whose add the process
// refuses as ending, succeeds in a new shared process.
func TestUpWithProcessSharedRacingTheEndOfTheProcessAddsTheDaemonToANewOne(t *testing.T) {
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	a.ReadyTimeout = 15 * time.Second
	if err := Up(context.Background(), b); err != nil {
		t.Fatalf("Up of B: %v", err)
	}
	ending := servingPid(t, b)
	control, processLog, err := shared.Files(filepath.Dir(b.Dir), daemon)
	if err != nil {
		t.Fatal(err)
	}
	// The process stays reachable on held, a second link to its socket,
	// while the test listens on the control socket's name, so that it
	// holds the add of A there until the end of the process is decided.
	held := control + ".held"
	if err := os.Link(control, held); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(held) })
	if err := os.Remove(control); err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: control, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	// The process removes the socket at control as it ends; the test
	// never does, so that it removes no socket of a new process.
	l.SetUnlinkOnClose(false)
	upErr := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		upErr <- Up(context.Background(), a)
	}()
	var fromUp net.Conn
	defer func() {
		// Whatever failed, the up is let go and waited for before the
		// cleanup of sharedConfigs stops the daemons.
		l.Close()
		if fromUp != nil {
			fromUp.Close()
		}
		<-finished
	}()
	_ = l.SetDeadline(time.Now().Add(10 * time.Second))
	if fromUp, err = l.Accept(); err != nil {
		t.Fatalf("the add of A: %v", err)
	}
	_ = fromUp.SetDeadline(time.Now().Add(10 * time.Second))
	f, err := protocol.ReadFrame(fromUp)
	if err != nil {
		t.Fatalf("the add request of A: %v", err)
	}
	var request bytes.Buffer
	if err := protocol.WriteFrame(&request, f); err != nil {
		t.Fatal(err)
	}
	before := socketFds(t, ending)
	toProcess, err := net.Dial("unix", held)
	if err != nil {
		t.Fatal(err)
	}
	defer toProcess.Close()
	_ = toProcess.SetDeadline(time.Now().Add(10 * time.Second))
	// All of the request but its last byte: the process reads it until
	// its end is decided, then refuses it as ending.
	if _, err := toProcess.Write(request.Bytes()[:request.Len()-1]); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		after := socketFds(t, ending)
		if slices.ContainsFunc(slices.Collect(maps.Keys(after)), func(s string) bool { return !before[s] }) {
			break // the process accepted the connection
		}
		if time.Now().After(deadline) {
			t.Fatal("the shared process does not accept the add of A")
		}
	}
	stopDaemon(t, daemon, b)
	answer, err := protocol.ReadFrame(toProcess)
	if err != nil {
		t.Fatalf("the answer of the shared process to the add of A: %v, want FrameEnding", err)
	}
	if answer.Type != protocol.FrameEnding {
		t.Fatalf("the answer of the shared process to the add of A: frame type %d, want FrameEnding (%d)", answer.Type, protocol.FrameEnding)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Lstat(control); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the ending shared process does not remove its control socket")
		}
	}
	if err := protocol.WriteFrame(fromUp, answer); err != nil {
		t.Fatal(err)
	}
	fromUp.Close()
	if err := <-upErr; err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	pid := servingPid(t, a)
	if pid == ending {
		t.Fatal("the daemon of A is served by the ending shared process")
	}
	if kind := processKind(t, pid); kind != "shared" {
		t.Errorf("the daemon of A is served by a process that runs %s, want shared", kind)
	}
	waitGone(t, ending)
	if _, code := runIn(t, a, "pwd"); code != 0 {
		t.Errorf("pwd through the daemon of A: exit %d, want 0", code)
	}
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	if log := string(data); !hasLine(log, `msg="add refused"`, "the shared process is ending") {
		t.Errorf("the process log records no add refused as the process is ending:\n%s", log)
	}
}

// timingSlack is how much longer than its ready timeout an up of the
// shared path may take in the tests that bound it: room for a loaded
// machine, yet clearly less than the 5 s that one exchange on the control
// socket or one arm takes at most, which an up whose ready timeout did
// not bound it would wait for.
const timingSlack = 2500 * time.Millisecond

// An up whose add reaches a shared process that keeps answering it is
// ending past the up's ready timeout fails within about that timeout,
// saying the shared process is ending.
func TestUpWithProcessSharedFailsWhenTheProcessKeepsEndingPastTheReadyTimeout(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.ReadyTimeout = 500 * time.Millisecond
	fakeEndingProcess(t, a, 200, answerEnding) // 10 s of refusals at least
	start := time.Now()
	err := Up(context.Background(), a)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "the shared process is ending") {
		t.Fatalf("Up of A: %v, want an error saying the shared process is ending", err)
	}
	if limit := a.ReadyTimeout + timingSlack; elapsed > limit {
		t.Errorf("Up of A failed after %v, want within %v", elapsed, limit)
	}
}

// answerNever reads the add request on c and never answers it, as a stuck
// shared process would, until the connection is closed.
func answerNever(c net.Conn) {
	_, _ = protocol.ReadFrame(c)
	_, _ = io.Copy(io.Discard, c)
}

// An up whose add reaches a shared process that takes the request and
// never answers fails within about its ready timeout, well before the
// timeout of one exchange, saying the process did not answer.
func TestUpWithProcessSharedFailsWithinTheReadyTimeoutWhenTheProcessDoesNotAnswer(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.ReadyTimeout = 500 * time.Millisecond
	fakeEndingProcess(t, a, 1, answerNever)
	start := time.Now()
	err := Up(context.Background(), a)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "did not answer the add request") {
		t.Fatalf("Up of A: %v, want an error saying the shared process did not answer", err)
	}
	if limit := a.ReadyTimeout + timingSlack; elapsed > limit {
		t.Errorf("Up of A failed after %v, want within %v", elapsed, limit)
	}
}

// An up that finds the lock of the shared process held by another, which
// keeps it, fails within about its ready timeout, saying it waited for
// that lock.
func TestUpWithProcessSharedFailsWithinTheReadyTimeoutWhenTheLockOfTheProcessStaysHeld(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.ReadyTimeout = 500 * time.Millisecond
	control, _, err := shared.Files(filepath.Dir(a.Dir), a.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, _, err := lockFile(shared.LockFile(control), "shared process", syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- Up(context.Background(), a) }()
	limit := a.ReadyTimeout + timingSlack
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "waited for the lock of the shared process") {
			t.Fatalf("Up of A: %v, want an error saying it waited for the lock of the shared process", err)
		}
		if elapsed := time.Since(start); elapsed > limit {
			t.Errorf("Up of A failed after %v, want within %v", elapsed, limit)
		}
	case <-time.After(limit + 2*time.Second):
		t.Fatalf("Up of A still waits for the lock of the shared process after %v", time.Since(start))
	}
}

// An up whose add the shared process refuses fails with the process's
// reason and what the daemon wrote to its log meanwhile.
func TestUpWithProcessSharedReportsTheReasonOfARefusedAddAndTheTailOfTheDaemonLog(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	logPath := filepath.Join(a.Dir, LogName)
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendLine(logPath, "written before the up")
	fakeEndingProcess(t, a, 1, func(c net.Conn) {
		if _, err := protocol.ReadFrame(c); err != nil {
			return
		}
		appendLine(logPath, "written while the add was refused")
		_ = protocol.WriteJSON(c, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "the reason of the refusal"})
	})
	err := Up(context.Background(), a)
	if err == nil {
		t.Fatal("Up of A succeeded with its add refused")
	}
	for _, want := range []string{"refused to add the daemon: the reason of the refusal", "written while the add was refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Up of A: %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "written before the up") {
		t.Errorf("Up of A: %v, want only what the daemon log got since the up began", err)
	}
}

// An up whose add the shared process it started refuses, here as the
// PATH the add carries holds no container runtime, quotes no process log:
// the process started, so its log is not part of the failure.
func TestUpWithProcessSharedQuotesNoProcessLogForAnAddTheProcessItStartedRefuses(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	t.Setenv("PATH", t.TempDir())
	err := Up(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "refused to add the daemon: --watch needs docker or podman in PATH") {
		t.Fatalf("Up of A with no container runtime in PATH: %v, want the refusal of its add", err)
	}
	if strings.Contains(err.Error(), "shared process log") {
		t.Errorf("Up of A: %v, want no process log quoted for a refused add", err)
	}
}

// An up whose shared process exits before it listens, here as something
// that is not a socket sits at its control socket, fails saying so, with
// what the process wrote to its log.
func TestUpWithProcessSharedReportsTheLogOfASharedProcessThatFailsToStart(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	control, _, err := shared.Files(filepath.Dir(a.Dir), a.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Up(context.Background(), a)
	if err == nil {
		t.Fatal("Up of A succeeded with a regular file at the control socket")
	}
	for _, want := range []string{"shared process exited during startup", control + " exists and is not a socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Up of A: %v, want it to contain %q", err, want)
		}
	}
}

// An up that started the shared process fails at once when that process
// exits while up waits for the daemon's arm, here as the daemon fails
// right after its add (its runtime directory lies inside the workspace)
// and the process ends with it: the error says how the process exited and
// quotes both the daemon log and the process log.
func TestUpWithProcessSharedFailsAtOnceWhenTheProcessItStartedExitsBeforeTheArm(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.Dir = filepath.Join(a.Workspace, "rt", "c")
	a.ReadyTimeout = 15 * time.Second
	allowCurrent(t, a)
	start := time.Now()
	err := Up(context.Background(), a)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Up of A succeeded with a daemon that fails at once")
	}
	for _, want := range []string{
		"shared process exited (exit status 0)",
		"hostrunner: ",        // the error the daemon ended with, in its log
		`msg="daemon ended"`,  // the process log
		ApprovalSocket(a.Dir), // named by both
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Up of A: %v, want it to contain %q", err, want)
		}
	}
	if limit := 5 * time.Second; elapsed > limit {
		t.Errorf("Up of A failed after %v, want at once, within %v", elapsed, limit)
	}
}

// One ready timeout bounds the whole shared path of an up: an up whose
// add is refused as ending for most of it, then taken, waits for the
// daemon's arm only for what remains, an arm the daemon takes and never
// answers included, then gives up and stops the daemon.
func TestUpWithProcessSharedWaitsForTheArmOnlyForWhatRemainsOfTheReadyTimeout(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.ReadyTimeout = time.Second
	ending := 700 * time.Millisecond
	gaveUp := make(chan time.Time, 1) // when the stop of the daemon first said hello
	var once sync.Once
	start := time.Now()
	fakeEndingProcess(t, a, 1000, func(c net.Conn) {
		if time.Since(start) < ending {
			answerEnding(c)
			return
		}
		if _, err := protocol.ReadFrame(c); err != nil {
			return
		}
		// The daemon the add asked for: it takes arms and never answers
		// them, and closes the connection of a stop unanswered.
		approval, err := transport.Unix{Path: ApprovalSocket(a.Dir)}.Listen()
		if err != nil {
			t.Error(err)
			return
		}
		t.Cleanup(func() { approval.Close() })
		go func() {
			for {
				ac, err := approval.Accept()
				if err != nil {
					return
				}
				go func() {
					defer ac.Close()
					if f, err := protocol.ReadFrame(ac); err == nil && f.Type == protocol.FrameHello {
						once.Do(func() { gaveUp <- time.Now() })
						return
					}
					_, _ = io.Copy(io.Discard, ac)
				}()
			}
		}()
		_ = protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameDaemonAdded})
	})
	err := Up(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "daemon did not listen within 1s") {
		t.Fatalf("Up of A: %v, want an error saying the daemon did not listen within the ready timeout", err)
	}
	select {
	case at := <-gaveUp:
		if waited, limit := at.Sub(start), a.ReadyTimeout+timingSlack; waited > limit {
			t.Errorf("Up of A gave up on the daemon after %v, want within %v", waited, limit)
		}
	default:
		t.Error("Up of A did not try to stop the daemon it gave up on")
	}
}

// processKind returns the hostrunner command the process pid runs, the
// first argument after its executable in /proc: serve for a daemon in a
// process of its own, shared for the shared process.
func processKind(t *testing.T, pid int) string {
	t.Helper()
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(cmdline), "\x00")
	if len(args) < 2 {
		t.Fatalf("the command line of process %d is %q, want a hostrunner command", pid, args)
	}
	return args[1]
}

// A change of process alone, with no new allow, is not refused, and the
// up moves the daemon: the one in the old kind of process, which steps
// aside on the arm and logs the setting it changed, gives way to a new
// one in the other kind, a process of its own or the shared process.
func TestUpAfterAChangeOfTheProcessMovesTheDaemonIntoTheOtherKindOfProcess(t *testing.T) {
	for _, c := range []struct {
		name             string
		from, to         settings.Process
		fromKind, toKind string
	}{
		{"own to shared", settings.ProcessOwn, settings.ProcessShared, "serve", "shared"},
		{"shared to own", settings.ProcessShared, settings.ProcessOwn, "shared", "serve"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			daemon := mustHostrunner(t)
			cfg, _ := sharedPair(t, daemon) // allowed with process shared
			cfg.Process = c.from
			if err := Up(ctx, cfg); err != nil {
				t.Fatalf("Up with process %s: %v", c.from, err)
			}
			oldPid := servingPid(t, cfg)
			if got := processKind(t, oldPid); got != c.fromKind {
				t.Fatalf("process %d serving with process %s runs hostrunner %s, want %s", oldPid, c.from, got, c.fromKind)
			}

			cfg.Process = c.to // only the process changes; nothing is allowed anew
			if err := Up(ctx, cfg); err != nil {
				t.Fatalf("Up with process %s after an allow with process %s: %v", c.to, c.from, err)
			}
			if e, ok, err := pendingOf(t, cfg); err != nil || ok {
				t.Fatalf("pending request = %+v, %v, %v; want none", e, ok, err)
			}
			waitGone(t, oldPid)
			newPid := servingPid(t, cfg)
			if newPid == oldPid {
				t.Fatalf("process %d still serves after the change of process to %s", oldPid, c.to)
			}
			if got := processKind(t, newPid); got != c.toKind {
				t.Errorf("process %d serving with process %s runs hostrunner %s, want %s", newPid, c.to, got, c.toKind)
			}
			wantShared := []int{newPid}
			if c.to == settings.ProcessOwn {
				wantShared = nil
			}
			if got := sharedPids(t, filepath.Dir(cfg.Dir)); !slices.Equal(got, wantShared) {
				t.Errorf("shared processes %v after the change to %s, want %v", got, c.to, wantShared)
			}

			info, err := daemonlist.Probe(ctx, ApprovalSocket(cfg.Dir))
			if err != nil {
				t.Fatalf("probe the new daemon: %v", err)
			}
			lacksProcess(t, "the new daemon says", info.Settings)
			if got, code := runIn(t, cfg, "pwd"); got != cfg.Workspace+"\n" || code != 0 {
				t.Errorf("pwd through the new daemon: got %q, exit %d; want %q, exit 0", got, code, cfg.Workspace+"\n")
			}

			data, err := os.ReadFile(filepath.Join(cfg.Dir, LogName))
			if err != nil {
				t.Fatal(err)
			}
			if want := "process=\"" + string(c.from) + " → " + string(c.to) + "\""; !hasLine(string(data), "stepping aside: the settings changed", want) {
				t.Errorf("the log has no step aside for %s:\n%s", want, data)
			}
		})
	}
}

// hasLine reports whether a line of log contains every one of parts.
func hasLine(log string, parts ...string) bool {
	for line := range strings.Lines(log) {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return true
		}
	}
	return false
}

// With process: shared, each daemon logs to the daemon.log of its own
// runtime directory, and the process logs to its own log in the runtime
// root its start, each daemon added and ended, and its exit.
func TestUpWithProcessSharedKeepsTheLogOfEachDaemonAndOfTheProcessApart(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	pids := sharedPids(t, filepath.Dir(a.Dir))
	if len(pids) != 1 {
		t.Fatalf("shared processes %v, want one", pids)
	}
	for _, cfg := range []Config{a, b} {
		stopDaemon(t, daemon, cfg)
	}
	waitGone(t, pids[0])

	for _, c := range []struct{ own, other Config }{{a, b}, {b, a}} {
		data, err := os.ReadFile(filepath.Join(c.own.Dir, LogName))
		if err != nil {
			t.Fatal(err)
		}
		log := string(data)
		for _, want := range []string{"socket=" + Socket(c.own.Dir), "workspace=" + c.own.Workspace} {
			if !strings.Contains(log, want) {
				t.Errorf("the log of %s lacks %q:\n%s", c.own.Dir, want, log)
			}
		}
		for _, other := range []string{c.other.Dir, c.other.Workspace} {
			if strings.Contains(log, other) {
				t.Errorf("the log of %s mentions %s of the other daemon:\n%s", c.own.Dir, other, log)
			}
		}
	}

	_, processLog, err := shared.Files(filepath.Dir(a.Dir), daemon)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range [][]string{
		{`msg="shared process started"`},
		{`msg="daemon added"`, "socket=" + Socket(a.Dir) + " "},
		{`msg="daemon added"`, "socket=" + Socket(b.Dir) + " "},
		{`msg="daemon ended"`, "socket=" + Socket(a.Dir) + " "},
		{`msg="daemon ended"`, "socket=" + Socket(b.Dir) + " "},
		{`msg="shared process exiting"`},
	} {
		if !hasLine(log, want...) {
			t.Errorf("the process log has no line with %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "msg=listening") {
		t.Errorf("the process log holds the events of a daemon:\n%s", log)
	}
}

// copyExecutable copies the executable src to dst, mode 0755, and returns
// dst.
func copyExecutable(t *testing.T, src, dst string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

// anotherBuild returns the path of a copy of the hostrunner executable src,
// in a directory of its own, whose content differs from src's by a few
// bytes at its end, so that it is another build of hostrunner, and that
// runs as src does.
func anotherBuild(t *testing.T, src string) string {
	t.Helper()
	build := copyExecutable(t, src, filepath.Join(t.TempDir(), "hostrunner"))
	f, err := os.OpenFile(build, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("another build"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, build, "version").CombinedOutput(); err != nil {
		t.Fatalf("the changed copy does not run: %v: %s", err, out)
	}
	return build
}

// The control socket of the shared process takes no add from another
// user, even one the file system lets connect: the process answers it
// with a refusal that is not ErrEnding, so that the other user's up does
// not start a process of its own for it, and logs the refusal, but not as
// a refused add.
func TestUpWithProcessSharedRefusesAnAddFromAnotherUser(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to run a process as another user")
	}
	const nobody = 65534
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, _ := sharedPair(t, daemon)
	if err := Up(ctx, a); err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	root := filepath.Dir(a.Dir)
	control, processLog, err := shared.Files(root, daemon)
	if err != nil {
		t.Fatal(err)
	}
	// Let the other user reach the control socket and run the test binary.
	base := filepath.Dir(root)
	for _, path := range []string{base, root} {
		if err := os.Chmod(path, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(control, 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, copyExecutable(t, os.Args[0], filepath.Join(base, "add")))
	cmd.Env = append(os.Environ(), addSharedEnv+"="+control)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody}}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the add of uid %d was accepted: %s", nobody, out)
	}
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("run the add as uid %d: %v", nobody, err)
	}
	if got := string(out); !strings.Contains(got, "refused to add the daemon: ") || strings.Contains(got, shared.ErrEnding.Error()) {
		t.Errorf("the add of uid %d: %s, want a refusal that is not ErrEnding", nobody, got)
	}
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	pid := "pid=" + strconv.Itoa(cmd.Process.Pid) + " "
	if log := string(data); !hasLine(log, `msg="control socket connection refused"`, "uid="+strconv.Itoa(nobody), pid) || hasLine(log, `msg="add refused"`, pid) {
		t.Errorf("the process log does not show the connection of uid %d refused (add: %s):\n%s", nobody, out, log)
	} else if !hasLine(log, `msg="control socket connection refused"`, `reason="the peer's uid is not this user's"`) || strings.Contains(log, "error=<nil>") {
		t.Errorf("the process log does not give the reason, or has a nil error (add: %s):\n%s", out, log)
	}
}

// The check of up that the control socket of a new shared process listens
// connects and closes without a request: the process takes it for a probe,
// not a refused add, and logs no refusal.
func TestUpWithProcessSharedLogsNoRefusedAddForItsOwnProbe(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, _ := sharedPair(t, daemon)
	if err := Up(ctx, a); err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	root := filepath.Dir(a.Dir)
	pids := sharedPids(t, root)
	if len(pids) != 1 {
		t.Fatalf("shared processes %v, want one", pids)
	}
	stopDaemon(t, daemon, a)
	waitGone(t, pids[0])
	_, processLog, err := shared.Files(root, daemon)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	if log := string(data); hasLine(log, `msg="add refused"`) {
		t.Errorf("the process log has a refused add for the probe of up:\n%s", log)
	}
}

// The daemons of a shared process are listed under the runtime root, as
// approve, stop and restart list them, on their approval sockets; its
// control socket, in the same root, is not taken for a daemon's.
func TestUpWithProcessSharedListsTheDaemonsAndNotTheControlSocket(t *testing.T) {
	ctx := context.Background()
	a, b := sharedPair(t, mustHostrunner(t))
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	var got []string
	for _, d := range daemonlist.List(ctx, filepath.Dir(a.Dir)) {
		got = append(got, d.Socket)
	}
	if want := []string{ApprovalSocket(a.Dir), ApprovalSocket(b.Dir)}; !slices.Equal(got, want) {
		t.Errorf("listed %v, want %v", got, want)
	}
}

// lockedBuffer is a buffer that a command's output is copied into while a
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hostSession is a subcommand of the hostrunner executable run as a human
// runs it on the host, without a terminal: the test types its lines and
// reads what it prints.
type hostSession struct {
	out  *lockedBuffer
	in   io.WriteCloser
	done chan struct{} // closed once the command ended and err is set
	err  error         // what Wait returned, read only after done is closed
	seen int           // how much of out the test has read
}

// startHostSession runs `daemon <args>` as hostCommand builds it, so that
// the host's settings files stay out of it. It ends when the test does.
func startHostSession(t *testing.T, daemon string, args ...string) *hostSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	return runHostSession(t, hostCommand(ctx, t, daemon, nil, args...), cancel)
}

// runHostSession starts cmd, which ends when its context is canceled by
// cancel, as a host session that ends when the test does.
func runHostSession(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc) *hostSession {
	t.Helper()
	cmd.WaitDelay = 5 * time.Second
	s := &hostSession{out: &lockedBuffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = s.out, s.out
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	s.in = in
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		s.err = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		cancel()
		in.Close()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("the host session did not end")
		}
	})
	return s
}

// waitFor waits until the session prints text after what the test has
// read, and returns what it printed up to the end of text.
func (s *hostSession) waitFor(t *testing.T, text string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := s.out.String()
		if i := strings.Index(out[s.seen:], text); i >= 0 {
			got := out[s.seen : s.seen+i+len(text)]
			s.seen += i + len(text)
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session did not print %q; it printed %q", text, out[s.seen:])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// answer types line and Enter.
func (s *hostSession) answer(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// wait waits for the command to end and returns its error, the same one
// however often it is called.
func (s *hostSession) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-s.done:
		return s.err
	case <-time.After(10 * time.Second):
		t.Fatalf("the host session did not end; it printed %q", s.out.String())
		return nil
	}
}

// listLinePrefix matches what a line of a daemon list starts with, its
// number and its mark (see hostterm.List.Print); listFieldSep matches the
// three or more spaces that part its fields.
var (
	listLinePrefix = regexp.MustCompile(`^\s*\d+ [* ] `)
	listFieldSep   = regexp.MustCompile(`\s{3,}`)
)

// listsAs reports whether the daemon list in out has a line for the daemon
// of the host workspace workspace, as a whole field and not as the start of
// a longer one, that shows it in a process of the kind process. A line is
// `<n> <mark> <workspace>   <version>   <process>   <note>`, whatever the
// note: the one of a daemon of another version is not that of the others.
func listsAs(out, workspace, process string) bool {
	for _, line := range strings.Split(out, "\n") {
		prefix := listLinePrefix.FindString(line)
		if prefix == "" {
			continue
		}
		fields := listFieldSep.Split(strings.TrimRight(line[len(prefix):], " "), -1)
		if len(fields) >= 3 && fields[0] == workspace && fields[2] == process {
			return true
		}
	}
	return false
}

// `hostrunner stop --choose` lists the daemons of the runtime root with the
// kind of process each runs in: shared for the two daemons of one shared
// process, own for a third in a process of its own; quitting the list
// stops none of them.
func TestStopChooseListsTheProcessKindOfEachDaemon(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	cfgs := sharedConfigs(t, daemon, sharedRules, sharedRules, sharedRules)
	a, b, c := cfgs[0], cfgs[1], cfgs[2]
	c.Settings.Process = settings.ProcessOwn
	for _, cfg := range []Config{a, b, c} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}

	s := startHostSession(t, daemon, "stop", "--choose", "--runtime-root", filepath.Dir(a.Dir))
	list := s.waitFor(t, "choose [")
	s.answer(t, "q")
	if err := s.wait(t); err == nil {
		t.Errorf("stop --choose quit without a choice succeeded, want it to report that nothing was stopped")
	}

	for _, w := range []struct {
		cfg     Config
		process string
	}{{a, "shared"}, {b, "shared"}, {c, "own"}} {
		if !listsAs(list, w.cfg.Workspace, w.process) {
			t.Errorf("the list has no line of %s showing process %s: %q", w.cfg.Workspace, w.process, list)
		}
	}
	for _, cfg := range []Config{a, b, c} {
		if got, code := runIn(t, cfg, "pwd"); got != cfg.Workspace+"\n" || code != 0 {
			t.Errorf("pwd in %s after quitting the list: got %q, exit %d; want %q, exit 0", cfg.Dir, got, code, cfg.Workspace+"\n")
		}
	}
}

// `hostrunner approve` serves the daemons of a shared process one by one,
// as those of their own: the list shows both as shared, the human picks A
// by its number, and the session shows and answers the requests of A only;
// a request of B, which no session serves, is refused at once.
func TestApproveServesOneDaemonOfASharedProcessAtATime(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}

	s := startHostSession(t, daemon, "approve", "--runtime-root", filepath.Dir(a.Dir))
	list := s.waitFor(t, "choose [")
	for _, cfg := range []Config{a, b} {
		if !listsAs(list, cfg.Workspace, "shared") {
			t.Errorf("the list has no line of %s showing process shared: %q", cfg.Workspace, list)
		}
	}
	number := regexp.MustCompile(`(?m)^\s+(\d+)\s.*` + regexp.QuoteMeta(a.Workspace)).FindStringSubmatch(list)
	if number == nil {
		t.Fatalf("the list has no numbered line of %s: %q", a.Workspace, list)
	}
	s.answer(t, number[1])
	s.waitFor(t, "serving "+a.Workspace)
	s.waitFor(t, "waiting for requests")

	// A request of A is shown, and runs once approved.
	type result struct {
		stdout, stderr string
		code           int
	}
	ask := func(cfg Config, word string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			code := client.Run(ctx, transport.Unix{Path: Socket(cfg.Dir)}, []string{"--ask", "echo", word}, cfg.ContainerWorkspace, strings.NewReader(""), &stdout, &stderr)
			ch <- result{stdout.String(), stderr.String(), code}
		}()
		return ch
	}
	fromA := ask(a, "from-a")
	if block := s.waitFor(t, "[o]nce"); !strings.Contains(block, "cmd   echo from-a") {
		t.Errorf("the request shown %q lacks the command of A", block)
	}
	s.answer(t, "o")
	select {
	case got := <-fromA:
		if got.code != 0 || got.stdout != "from-a\n" {
			t.Errorf("the approved echo in A: exit %d, stdout %q, stderr %q; want exit 0, stdout %q", got.code, got.stdout, got.stderr, "from-a\n")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the echo in A did not finish once approved")
	}

	// B is not served by the session: its request is refused, not shown.
	select {
	case got := <-ask(b, "from-b"):
		if want := "no approval session serves this devcontainer"; got.code != protocol.ExitRejected || !strings.Contains(got.stderr, want) {
			t.Errorf("the echo in B: exit %d, stderr %q; want exit %d and %q", got.code, got.stderr, protocol.ExitRejected, want)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the echo in B did not finish")
	}
	if out := s.out.String(); strings.Contains(out, "from-b") {
		t.Errorf("the session of A shows the request of B: %q", out)
	}

	s.answer(t, "quit")
	if err := s.wait(t); err != nil {
		t.Errorf("approve after quit: %v: %s", err, s.out.String())
	}
}

// Each build of hostrunner has its shared process: the ups of two runtime
// directories whose daemon executables differ in content add their
// daemons to two processes, each on its control socket in the root.
func TestUpWithProcessSharedOfAnotherExecutableStartsAnotherProcess(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	b.Daemon = anotherBuild(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	if pa, pb := servingPid(t, a), servingPid(t, b); pa == pb {
		t.Errorf("A and B are both served by process %d, want one process each", pa)
	}
	controls, err := filepath.Glob(filepath.Join(filepath.Dir(a.Dir), "shared-*.sock"))
	if err != nil || len(controls) != 2 {
		t.Errorf("control sockets %v, %v; want two", controls, err)
	}
}

// The shared process outlives a daemon that ends while another runs, and
// exits once its last daemon ended, removing its control socket.
func TestUpWithProcessSharedExitsAfterTheLastDaemonEnds(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	pid := servingPid(t, a)
	control, _, err := shared.Files(filepath.Dir(a.Dir), daemon)
	if err != nil {
		t.Fatal(err)
	}

	stopDaemon(t, daemon, a)
	if err := WaitStopped(ctx, Socket(a.Dir), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !alivePid(pid) {
		t.Fatalf("the shared process %d exited with the daemon of B still running", pid)
	}
	if got, code := runIn(t, b, "pwd"); got != b.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s after A stopped: got %q, exit %d; want %q, exit 0", b.Dir, got, code, b.Workspace+"\n")
	}

	stopDaemon(t, daemon, b)
	waitGone(t, pid)
	if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket %s after the process exited: %v, want it removed", control, err)
	}
}

// A restart of one daemon of a shared process replaces that daemon alone:
// the process, of the same build, serves the new daemon and keeps serving
// the other.
func TestUpWithProcessSharedRestartsOneDaemonAndLeavesTheOtherAndTheProcess(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	pid := servingPid(t, a)
	if got := servingPid(t, b); got != pid {
		t.Fatalf("A is served by process %d, B by %d; want one process", pid, got)
	}

	out := restartDaemon(t, daemon, a)

	if want := "hostrunner: restarted " + a.Dir; !strings.Contains(out, want) {
		t.Errorf("restart of A printed %q, want it to say %q", out, want)
	}
	if strings.Contains(out, b.Dir) {
		t.Errorf("restart of A printed %q, want no mention of B (%s)", out, b.Dir)
	}
	if !alivePid(pid) {
		t.Fatalf("the shared process %d exited with the restart of A", pid)
	}
	for _, cfg := range []Config{a, b} {
		if got := servingPid(t, cfg); got != pid {
			t.Errorf("%s is served by process %d after the restart, want the shared process %d", cfg.Dir, got, pid)
		}
		if got, code := runIn(t, cfg, "pwd"); got != cfg.Workspace+"\n" || code != 0 {
			t.Errorf("pwd in %s after the restart of A: got %q, exit %d; want %q, exit 0", cfg.Dir, got, code, cfg.Workspace+"\n")
		}
	}
}

// waitForCommand waits until a process runs the command argv, as /proc
// shows it, and fails the test when none does within 10 seconds.
func waitForCommand(t *testing.T, argv ...string) {
	t.Helper()
	want := strings.Join(argv, "\x00") + "\x00"
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && string(cmdline) == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no process runs %q", argv)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A restart of one daemon of a shared process with --wait drains it: the
// command that runs in that daemon finishes normally before it is replaced,
// the other daemon answers meanwhile and afterwards, and the new daemon is
// served by the same process.
func TestUpWithProcessSharedRestartWithWaitLetsTheCommandOfTheDaemonFinish(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	cfgs := sharedConfigs(t, daemon, sharedRules+"  - command: sleep\n    args: any\n", sharedRules)
	a, b := cfgs[0], cfgs[1]
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	pid := servingPid(t, a)
	if got := servingPid(t, b); got != pid {
		t.Fatalf("A is served by process %d, B by %d; want one process", pid, got)
	}

	// The restart starts once the command runs, so that it finds it running.
	// The duration is no other process's.
	const duration = "1.7"
	type result struct {
		stderr string
		code   int
	}
	finished := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		var stderr bytes.Buffer
		code := client.Run(ctx, transport.Unix{Path: Socket(a.Dir)}, []string{"sleep", duration}, a.ContainerWorkspace, strings.NewReader(""), io.Discard, &stderr)
		finished <- result{stderr.String(), code}
	}()
	waitForCommand(t, "sleep", duration)
	restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	s := runHostSession(t, restartCommand(restartCtx, t, daemon, a, "--wait"), cancel)
	s.waitFor(t, a.Dir+" runs:")

	select {
	case got := <-finished:
		t.Fatalf("the command in A ended (exit %d, stderr %q) as the restart began, want it to run on until it finishes", got.code, got.stderr)
	default:
	}
	if got, code := runIn(t, b, "pwd"); got != b.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s during the restart of A: got %q, exit %d; want %q, exit 0", b.Dir, got, code, b.Workspace+"\n")
	}
	if err := s.wait(t); err != nil {
		t.Fatalf("restart of A: %v: %s", err, s.out.String())
	}

	select {
	case got := <-finished:
		if got.code != 0 {
			t.Errorf("the command in A ended with exit %d (stderr %q), want it to finish normally", got.code, got.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command in A did not end")
	}
	if want := "hostrunner: restarted " + a.Dir; !strings.Contains(s.out.String(), want) {
		t.Errorf("restart of A printed %q, want it to say %q", s.out.String(), want)
	}
	for _, cfg := range []Config{a, b} {
		if got := servingPid(t, cfg); got != pid {
			t.Errorf("%s is served by process %d after the restart, want the shared process %d", cfg.Dir, got, pid)
		}
		if got, code := runIn(t, cfg, "pwd"); got != cfg.Workspace+"\n" || code != 0 {
			t.Errorf("pwd in %s after the restart of A: got %q, exit %d; want %q, exit 0", cfg.Dir, got, code, cfg.Workspace+"\n")
		}
	}
}

// `stop --all` ends every daemon of a shared process, and the process exits
// with the last of them, removing its control socket.
func TestUpWithProcessSharedEndsWithStopAll(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	pid := servingPid(t, a)
	control, _, err := shared.Files(filepath.Dir(a.Dir), daemon)
	if err != nil {
		t.Fatal(err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := hostCommand(stopCtx, t, daemon, nil, "stop", "--all", "--kill", "--runtime-root", filepath.Dir(a.Dir)).CombinedOutput()
	if err != nil {
		t.Fatalf("stop --all: %v: %s", err, out)
	}

	for _, cfg := range []Config{a, b} {
		if want := "hostrunner: stopped " + cfg.Dir; !strings.Contains(string(out), want) {
			t.Errorf("stop --all printed %q, want it to say %q", out, want)
		}
		for _, socket := range []string{Socket(cfg.Dir), ApprovalSocket(cfg.Dir)} {
			if err := WaitStopped(ctx, socket, 5*time.Second); err != nil {
				t.Errorf("%s after stop --all: %v", socket, err)
			}
		}
	}
	waitGone(t, pid)
	if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket %s after the process exited: %v, want it removed", control, err)
	}
}

// After an update replaced the build and removed the directory of the old
// one, the restart of a daemon with the new build puts it into the shared
// process of the new build, and the process of the old build keeps serving
// its other daemon until that one ends, then exits.
func TestUpWithProcessSharedRestartWithANewBuildLeavesTheOldProcessItsOtherDaemon(t *testing.T) {
	ctx := context.Background()
	oldDir := t.TempDir()
	oldBuild := copyExecutable(t, mustHostrunner(t), filepath.Join(oldDir, "hostrunner"))
	a, b := sharedPair(t, oldBuild)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	oldPid := servingPid(t, a)
	if got := servingPid(t, b); got != oldPid {
		t.Fatalf("A is served by process %d, B by %d; want one process", oldPid, got)
	}

	newBuild := anotherBuild(t, mustHostrunner(t))
	// The cleanup of sharedPair stops the daemons with the old build, which is
	// removed below; this one, which runs before it, stops them with the new.
	t.Cleanup(func() {
		for _, cfg := range []Config{a, b} {
			stopDaemon(t, newBuild, cfg)
		}
	})
	if err := os.RemoveAll(oldDir); err != nil {
		t.Fatal(err)
	}

	out := restartDaemon(t, newBuild, a)

	if want := "hostrunner: restarted " + a.Dir; !strings.Contains(out, want) {
		t.Errorf("restart of A printed %q, want it to say %q", out, want)
	}
	newPid := servingPid(t, a)
	if newPid == oldPid {
		t.Errorf("A is served by process %d after the restart, want the process of the new build", newPid)
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(newPid) + "/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Split(string(cmdline), "\x00"); len(args) < 2 || args[1] != "shared" {
		t.Errorf("the process serving A runs %q, want hostrunner shared", args)
	}
	if got, code := runIn(t, a, "pwd"); got != a.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s after the restart: got %q, exit %d; want %q, exit 0", a.Dir, got, code, a.Workspace+"\n")
	}
	if !alivePid(oldPid) {
		t.Fatalf("the shared process %d of the old build exited with the restart of A", oldPid)
	}
	if got := servingPid(t, b); got != oldPid {
		t.Errorf("B is served by process %d after the restart of A, want the old process %d", got, oldPid)
	}
	if got, code := runIn(t, b, "pwd"); got != b.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s after the restart of A: got %q, exit %d; want %q, exit 0", b.Dir, got, code, b.Workspace+"\n")
	}
	control, _, err := shared.Files(filepath.Dir(a.Dir), newBuild)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(control); err != nil {
		t.Errorf("the control socket %s of the new build: %v, want it present", control, err)
	}

	stopDaemon(t, newBuild, b)
	waitGone(t, oldPid)
}

// No stop or restart signals a shared process: when the approval socket of
// one of its daemons is gone, so that the stop takes the daemon for one from
// before approvals and reaches the process on its socket, it refuses by
// name, and the process keeps serving its daemons.
func TestStopAndRestartOfADaemonOfASharedProcessWithoutItsApprovalSocketSignalNothing(t *testing.T) {
	ctx := context.Background()
	daemon := mustHostrunner(t)
	a, b := sharedPair(t, daemon)
	for _, cfg := range []Config{a, b} {
		if err := Up(ctx, cfg); err != nil {
			t.Fatalf("Up of %s: %v", cfg.Dir, err)
		}
	}
	pid := servingPid(t, a)
	if got := servingPid(t, b); got != pid {
		t.Fatalf("A is served by process %d, B by %d; want one process", pid, got)
	}
	// B is stopped by the cleanup of sharedConfigs, which cannot reach A any
	// more: SIGTERM to the shared process ends A's daemon with the rest.
	t.Cleanup(func() {
		if alivePid(pid) {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	})
	if err := os.Remove(ApprovalSocket(a.Dir)); err != nil {
		t.Fatal(err)
	}
	// stillServing checks that the process lives and both daemons answer.
	stillServing := func(after string) {
		t.Helper()
		if !alivePid(pid) {
			t.Fatalf("the shared process %d was signalled by %s", pid, after)
		}
		for _, cfg := range []Config{a, b} {
			if got, code := runIn(t, cfg, "pwd"); got != cfg.Workspace+"\n" || code != 0 {
				t.Errorf("pwd in %s after %s: got %q, exit %d; want %q, exit 0", cfg.Dir, after, got, code, cfg.Workspace+"\n")
			}
		}
	}

	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(stopCtx, daemon, "stop", "--dir", a.Dir, "--kill").CombinedOutput()
	if err == nil {
		t.Errorf("stop of A succeeded: %s", out)
	}
	if want := a.Dir + ": process " + strconv.Itoa(pid) + " on its socket is a shared process, which is never signalled: no signal sent"; !strings.Contains(string(out), want) {
		t.Errorf("stop of A printed %q, want it to say %q", out, want)
	}
	stillServing("stop")

	restartOut, err := restartDaemonErr(t, daemon, a)
	if err == nil {
		t.Errorf("restart of A succeeded: %s", restartOut)
	}
	if want := "a daemon from before approvals cannot be restarted"; !strings.Contains(restartOut, want) {
		t.Errorf("restart of A printed %q, want it to say %q", restartOut, want)
	}
	stillServing("restart")
}

// SIGTERM or SIGINT to the shared process ends every daemon it runs, then
// the process itself.
func TestUpWithProcessSharedEndsItsDaemonsOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx := context.Background()
			a, b := sharedPair(t, mustHostrunner(t))
			for _, cfg := range []Config{a, b} {
				if err := Up(ctx, cfg); err != nil {
					t.Fatalf("Up of %s: %v", cfg.Dir, err)
				}
			}
			pid := servingPid(t, a)
			if err := syscall.Kill(pid, sig); err != nil {
				t.Fatal(err)
			}
			for _, cfg := range []Config{a, b} {
				if err := WaitStopped(ctx, Socket(cfg.Dir), 5*time.Second); err != nil {
					t.Error(err)
				}
			}
			waitGone(t, pid)
		})
	}
}

// startSharedProcess starts `hostrunner shared` of the executable daemon
// in the runtime root root, as up starts it, with the flags flags after
// --socket, and waits until its control socket accepts connections. It
// returns the control socket, the process log and a channel closed once
// the process exited; the process is killed when the test ends.
func startSharedProcess(t *testing.T, daemon, root string, flags ...string) (control, processLog string, exited <-chan struct{}) {
	t.Helper()
	control, processLog, err := shared.Files(root, daemon)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := OpenLog(processLog)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemon, append([]string{"shared", "--socket", control}, flags...)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	err = cmd.Start()
	logFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for !listening(context.Background(), control) {
		if time.Now().After(deadline) {
			t.Fatalf("the shared process did not listen on %s", control)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return control, processLog, done
}

// A shared process whose add is refused while it runs no daemon, as for
// the first add after it started, ends: nothing would end it otherwise.
func TestSharedProcessEndsWhenAnAddIsRefusedAndNoDaemonRuns(t *testing.T) {
	root := shortDir(t)
	control, _, exited := startSharedProcess(t, mustHostrunner(t), root)
	if err := shared.Add(context.Background(), control, protocol.AddDaemon{Args: []string{"--socket", filepath.Join(root, "a", "hostrunner.sock")}}); err == nil {
		t.Fatal("the add of a daemon without its flags was accepted")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared process runs on with no daemon after the refused add")
	}
}

// A shared process that gets no daemon within its --ready-timeout ends
// on its own, removing its control socket.
func TestSharedProcessEndsWhenNoDaemonIsAddedWithinItsReadyTimeout(t *testing.T) {
	start := time.Now()
	control, _, exited := startSharedProcess(t, mustHostrunner(t), shortDir(t), "--ready-timeout", "300ms")
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared process runs on with no daemon added past its ready timeout")
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("the shared process exited %v after its start, before its ready timeout of 300ms", elapsed)
	}
	if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket %s after the process exited: %v, want it removed", control, err)
	}
}

// A shared process answers an add that comes after its end is decided,
// on a connection it accepted before, with FrameEnding, which Add takes
// for ErrEnding, so that up starts a new process: whether the process
// cuts the connection, waiting for its request, at its end, or reads the
// request and refuses it, the answer is FrameEnding. The connection is
// opened well within the ready timeout, so that the process accepts it
// before its end.
func TestSharedProcessAnswersAnAddAfterItsEndIsDecidedWithFrameEnding(t *testing.T) {
	control, processLog, exited := startSharedProcess(t, mustHostrunner(t), shortDir(t), "--ready-timeout", "1s")
	c, err := net.Dial("unix", control)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Lstat(control); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket is not removed past the ready timeout")
		}
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// The write fails when the process already answered and closed the
	// connection; the answer is read all the same.
	_ = protocol.WriteJSON(c, protocol.FrameAddDaemon, protocol.AddDaemon{Version: protocol.SharedVersion})
	f, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatalf("the answer to the add after the end was decided: %v, want FrameEnding", err)
	}
	if f.Type != protocol.FrameEnding {
		t.Errorf("the answer to the add after the end was decided: frame type %d, want FrameEnding (%d)", f.Type, protocol.FrameEnding)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared process runs on past its end")
	}
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	if log := string(data); !hasLine(log, `msg="no daemon added"`) && !hasLine(log, `msg="add refused"`) {
		t.Errorf("the process log records neither its end nor the refused add:\n%s", log)
	}
}

// sharedReadyTimeout returns the --ready-timeout the shared process pid
// runs with, as its command line in /proc shows it; ok is false when it
// was started without one.
func sharedReadyTimeout(t *testing.T, pid int) (timeout time.Duration, ok bool) {
	t.Helper()
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	i := slices.Index(args, "--ready-timeout")
	if i < 0 || i+1 == len(args) {
		return 0, false
	}
	timeout, err = time.ParseDuration(args[i+1])
	if err != nil {
		t.Fatalf("the --ready-timeout of the shared process %d: %v", pid, err)
	}
	return timeout, true
}

// An up that starts the shared process gives it, for its first add, what
// remains of the up's own ready timeout, so that a process no daemon is
// added to ends by the time the up gives up: here the up waits for the
// lock of the process for part of its ready timeout before it starts it.
func TestUpWithProcessSharedGivesTheProcessItStartsWhatRemainsOfItsReadyTimeout(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.ReadyTimeout = 3 * time.Second
	const held = time.Second // how long the lock of the process stays held
	control, _, err := shared.Files(filepath.Dir(a.Dir), a.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, _, err := lockFile(shared.LockFile(control), "shared process", syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(held)
		unlock()
	}()
	defer func() { <-released }()
	if err := Up(context.Background(), a); err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	pid := servingPid(t, a)
	got, ok := sharedReadyTimeout(t, pid)
	if !ok {
		t.Fatalf("the shared process %d runs without --ready-timeout, want what remains of the ready timeout of the up", pid)
	}
	// The up took the lock held after it began, so less than its ready
	// timeout less half of that remained when it started the process.
	if limit := a.ReadyTimeout - held/2; got <= 0 || got > limit {
		t.Errorf("the shared process %d runs with --ready-timeout %v, want more than 0 and at most %v", pid, got, limit)
	}
}

// A shared process whose first daemon is added within its ready timeout
// runs on past that timeout: it ends with its last daemon only.
func TestUpWithProcessSharedKeepsTheProcessPastItsReadyTimeoutOnceADaemonIsAdded(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	a.ReadyTimeout = 2 * time.Second
	if err := Up(context.Background(), a); err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	pid := servingPid(t, a)
	time.Sleep(a.ReadyTimeout + time.Second)
	if !alivePid(pid) {
		t.Fatalf("the shared process %d exited past its ready timeout with the daemon of A running", pid)
	}
	if got, code := runIn(t, a, "pwd"); got != a.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s past the ready timeout: got %q, exit %d; want %q, exit 0", a.Dir, got, code, a.Workspace+"\n")
	}
}

// A refused add of a shared process is in the process log by the socket
// of the daemon it asked for.
func TestSharedProcessLogsTheSocketOfARefusedAdd(t *testing.T) {
	root := shortDir(t)
	control, processLog, exited := startSharedProcess(t, mustHostrunner(t), root)
	socket := filepath.Join(root, "a", "hostrunner.sock")
	if err := shared.Add(context.Background(), control, protocol.AddDaemon{Args: []string{"--socket", socket}}); err == nil {
		t.Fatal("the add of a daemon without its flags was accepted")
	}
	<-exited
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	if log := string(data); !hasLine(log, `msg="add refused"`, "socket="+socket+" ") {
		t.Errorf("the process log has no refused add naming %s:\n%s", socket, log)
	}
}

// An up whose wait for the daemon it added to the shared process fails,
// here as its context is cancelled once the daemon was added, stops that
// daemon, as an up of a daemon of its own process kills it; the other
// daemons of the process keep running.
func TestUpWithProcessSharedStopsTheDaemonItAddedWhenItsWaitFails(t *testing.T) {
	ctx := context.Background()
	a, b := sharedPair(t, mustHostrunner(t))
	if err := Up(ctx, b); err != nil {
		t.Fatalf("Up of B: %v", err)
	}
	_, processLog, err := shared.Files(filepath.Dir(a.Dir), a.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	upCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// The process logs the daemon added as the daemon starts to run,
		// about when it answers the add, and the wait for the arm tries
		// first 50 ms after it began: the cancel ends that wait, before
		// the daemon is armed.
		for upCtx.Err() == nil {
			if data, _ := os.ReadFile(processLog); hasLine(string(data), `msg="daemon added"`, "socket="+Socket(a.Dir)+" ") {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := Up(upCtx, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("Up of A cancelled once its daemon was added: %v, want %v", err, context.Canceled)
	}
	if err := WaitStopped(ctx, Socket(a.Dir), 5*time.Second); err != nil {
		t.Error(err)
	}
	if got, code := runIn(t, b, "pwd"); got != b.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s: got %q, exit %d; want %q, exit 0", b.Dir, got, code, b.Workspace+"\n")
	}
}

// An up whose wait for the arm of the daemon it added to the shared
// process fails as its ready timeout passes stops that daemon once the
// daemon answers on its approval socket; the other daemons of the process
// keep running. The rules file of A is a FIFO the test feeds, so that the
// daemon of A, which reads it first as it starts, listens only once the
// test lets it, and until then the approval socket of A is the test's: it
// takes the arms of the up and never answers them, and the hello of the
// stop of the up tells the test that the up gave up.
func TestUpWithProcessSharedStopsTheDaemonItAddedWhenItsReadyTimeoutPasses(t *testing.T) {
	ctx := context.Background()
	a, b := sharedPair(t, mustHostrunner(t))
	if err := Up(ctx, b); err != nil {
		t.Fatalf("Up of B: %v", err)
	}
	a.ReadyTimeout = 2 * time.Second
	_, processLog, err := shared.Files(filepath.Dir(a.Dir), a.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	// The FIFO keeps a second name, feed, so that a regular file with the
	// same rules can take the name of the rules file once the daemon read
	// the FIFO, for its rereads.
	rules, err := os.ReadFile(a.RulesFile)
	if err != nil {
		t.Fatal(err)
	}
	feed := a.RulesFile + ".fifo"
	if err := os.Remove(a.RulesFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(a.RulesFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a.RulesFile, feed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A reader or a writer still waiting on the FIFO gets the other
		// end, closed at once, so that it does not wait for ever.
		for _, flag := range []int{os.O_WRONLY, os.O_RDONLY} {
			if f, err := os.OpenFile(feed, flag|syscall.O_NONBLOCK, 0); err == nil {
				f.Close()
			}
		}
	})
	// feedRules waits for the next reader of the FIFO and writes it the
	// rules.
	feedRules := func() error {
		f, err := os.OpenFile(feed, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.Write(rules)
		return errors.Join(err, f.Close())
	}
	fedUp := make(chan error, 1)
	go func() { fedUp <- feedRules() }() // for the rules the up reads first

	gaveUp := make(chan struct{}) // closed once the approval socket of the test is gone
	go func() {
		// The process logs the daemon added as the daemon starts to run.
		added := func() bool {
			data, _ := os.ReadFile(processLog)
			return hasLine(string(data), `msg="daemon added"`, "socket="+Socket(a.Dir)+" ")
		}
		for deadline := time.Now().Add(30 * time.Second); !added(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("the daemon of A is not added")
				return
			}
		}
		approval, err := transport.Unix{Path: ApprovalSocket(a.Dir)}.Listen()
		if err != nil {
			t.Error(err)
			return
		}
		t.Cleanup(func() { approval.Close() })
		var once sync.Once
		for {
			c, err := approval.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameHello {
					// Closed before the daemon is fed, which removes the
					// socket, so that the daemon can listen on it.
					once.Do(func() {
						approval.Close()
						close(gaveUp)
					})
					return
				}
				_, _ = io.Copy(io.Discard, c) // an arm, never answered
			}()
		}
	}()
	fedDaemon := make(chan error, 1)
	go func() {
		<-gaveUp
		err := feedRules()
		if err == nil {
			err = os.WriteFile(feed+".rules", rules, 0o644)
		}
		if err == nil {
			err = os.Rename(feed+".rules", a.RulesFile)
		}
		fedDaemon <- err
	}()

	err = Up(ctx, a)
	if err == nil || !strings.Contains(err.Error(), "daemon did not listen within 2s") {
		t.Fatalf("Up of A: %v, want an error saying the daemon did not listen within the ready timeout", err)
	}
	if err := <-fedUp; err != nil {
		t.Fatalf("feed the rules to the up: %v", err)
	}
	select {
	case <-gaveUp:
	default:
		t.Fatal("Up of A did not try to stop the daemon it gave up on")
	}
	if err := <-fedDaemon; err != nil {
		t.Fatalf("feed the rules to the daemon: %v", err)
	}
	if err := WaitStopped(ctx, Socket(a.Dir), 5*time.Second); err != nil {
		t.Error(err)
	}
	if data, err := os.ReadFile(filepath.Join(a.Dir, LogName)); err != nil || !hasLine(string(data), `msg="stopping on request`) {
		t.Errorf("the daemon of A was not stopped on request by the up (%v):\n%s", err, data)
	}
	if got, code := runIn(t, b, "pwd"); got != b.Workspace+"\n" || code != 0 {
		t.Errorf("pwd in %s: got %q, exit %d; want %q, exit 0", b.Dir, got, code, b.Workspace+"\n")
	}
}

// addFailingDaemon starts the shared process of a with the up of a, then
// asks it to add the daemon of another runtime directory of the workspace
// of a, whose serve fails at once: the directory lies inside the
// workspace, so its approval socket would too. It returns the Config of
// that daemon.
func addFailingDaemon(t *testing.T, a Config) Config {
	t.Helper()
	ctx := context.Background()
	if err := Up(ctx, a); err != nil {
		t.Fatalf("Up of A: %v", err)
	}
	c := a
	c.Dir = filepath.Join(a.Workspace, "rt", "c")
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	control, _, err := shared.Files(filepath.Dir(a.Dir), a.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Add(ctx, control, protocol.AddDaemon{Args: serveArgs(c, Socket(c.Dir))[1:], Env: os.Environ()}); err != nil {
		t.Fatalf("add the daemon of %s: %v", c.Dir, err)
	}
	return c
}

// The error a daemon of the shared process ends with is in its own log,
// as `hostrunner: <error>`, as the error of a daemon of its own process.
func TestSharedProcessWritesTheErrorADaemonEndsWithToItsLog(t *testing.T) {
	a, _ := sharedPair(t, mustHostrunner(t))
	c := addFailingDaemon(t, a)
	logPath := filepath.Join(c.Dir, LogName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(logPath)
		if err == nil && hasLine(string(data), "hostrunner: ", ApprovalSocket(c.Dir)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log of %s has no line with its error: %v:\n%s", c.Dir, err, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A refused up records its request under its own runtime directory: the
// refusal of one runtime directory does not replace the request of another.
func TestUpRefusedRecordsTheRequestOfItsOwnRuntimeDir(t *testing.T) {
	ctx := context.Background()
	a := fakeConfig(t, "serve")
	a.Allows = allow.Store{Dir: filepath.Join(t.TempDir(), "allow")}
	b := a
	b.Dir = filepath.Join(filepath.Dir(a.Dir), "other")
	b.Grace = time.Minute
	notAllowed(t, Up(ctx, a), a, "missing")
	notAllowed(t, Up(ctx, b), b, "missing")
	for _, cfg := range []Config{a, b} {
		e, ok, err := pendingOf(t, cfg)
		if err != nil || !ok || !maps.Equal(e.Settings, cfg.Settings.AllowedSettings()) {
			t.Errorf("Pending(%s) = %+v, %v, %v; want its request", cfg.Dir, e, ok, err)
		}
	}
	nothingStarted(t, a.Dir)
	nothingStarted(t, b.Dir)
}

func TestUpAllowedRecordsNoRequest(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if _, ok, err := pendingOf(t, cfg); err != nil || ok {
		t.Fatalf("Pending = %v, %v, want no request", ok, err)
	}
}

// A store where the request cannot be written fails up with that error,
// not with the refusal, which would name an allow that cannot answer.
func TestUpRefusedFailsWhenTheRequestCannotBeRecorded(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.WriteFile(cfg.RulesFile, []byte(gitStatusRules), 0o644); err != nil {
		t.Fatal(err)
	}
	notAllowed(t, Up(context.Background(), cfg), cfg, gitStatusDigest)
	requests, err := filepath.Glob(filepath.Join(cfg.Allows.Dir, "*", "*.request.json"))
	if err != nil || len(requests) != 1 {
		t.Fatalf("request files %v, %v, want one", requests, err)
	}
	// A directory that is not empty cannot be replaced by the new request.
	if err := os.Remove(requests[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(requests[0], "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = Up(context.Background(), cfg)
	if na, ok := errors.AsType[*NotAllowedError](err); ok || err == nil {
		t.Fatalf("got %v (%v), want the error of recording the request", err, na)
	}
	if want := "record the request of " + cfg.Workspace; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q lacks %q", err, want)
	}
	nothingStarted(t, cfg.Dir)
}
