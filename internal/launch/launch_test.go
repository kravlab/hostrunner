package launch

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"maps"
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

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/settings"
)

// fakeDaemonEnv makes the test binary act as the daemon when Up spawns it.
// Values: "serve" listens and answers FrameArm; "hang" never listens.
const fakeDaemonEnv = "HOSTRUNNER_LAUNCH_FAKE_DAEMON"

// runUpEnv makes the test binary run Up itself (as `hostrunner up` would),
// with the runtime dir and client given in the variable ("dir:client").
const runUpEnv = "HOSTRUNNER_LAUNCH_RUN_UP"

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(runUpEnv) != "":
		dir, client, _ := strings.Cut(os.Getenv(runUpEnv), ":")
		os.Unsetenv(runUpEnv) // the daemon Up spawns must not run Up again
		os.Setenv(fakeDaemonEnv, "serve")
		err := Up(context.Background(), Config{
			Dir: dir, Workspace: "/w", ContainerWorkspace: "/c",
			Daemon: os.Args[0], Client: client,
			Settings: settings.Settings{RulesFile: filepath.Join(dir, "missing-rules.yaml"), Reread: true, ReadyTimeout: 5 * time.Second},
		})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case os.Getenv(fakeDaemonEnv) != "":
		runFakeDaemon(os.Getenv(fakeDaemonEnv))
		return
	}
	code := m.Run()
	if staticDir != "" {
		os.RemoveAll(staticDir)
	}
	os.Exit(code)
}

// runFakeDaemon records its pid in fake.pids and, in "serve" mode,
// listens on --socket and answers every FrameArm, logging it to
// fake.armed. Like the real daemon (armHandler in cmd/hostrunner, which a
// change of the comparison must keep it in step with) it compares the
// arm's rules digest with the rules it started with, the arm's
// --no-reread with its own, and each setting the arm carries with the one
// it parsed from its flags, and, on a mismatch, steps aside and exits. It
// insists on --watch, which `up` must always pass.
func runFakeDaemon(mode string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := fs.String("socket", "", "")
	fs.String("workspace", "", "")
	fs.String("container-workspace", "", "")
	var own settings.Settings
	config := fs.String("rules-file", "", "")
	watch := fs.Bool("watch", false, "")
	fs.DurationVar(&own.StartupTimeout, "startup-timeout", 0, "")
	fs.DurationVar(&own.Grace, "grace", 0, "")
	fs.DurationVar(&own.PollInterval, "poll-interval", 0, "")
	fs.DurationVar(&own.AskTimeout, "ask-timeout", 0, "")
	fs.DurationVar(&own.RequestTimeout, "request-timeout", 0, "")
	fs.Bool("reread", false, "") // reread is !--no-reread: up passes one of the two
	noReread := fs.Bool("no-reread", false, "")
	if len(os.Args) < 2 || os.Args[1] != "serve" || fs.Parse(os.Args[2:]) != nil || !*watch || *config == "" {
		os.Exit(2)
	}
	own.RulesFile, own.Reread = *config, !*noReread
	ownSettings := own.Daemon() // without a duration up did not pass, which is then not compared
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
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(0)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameArm {
			var a protocol.Arm
			_ = protocol.DecodeJSON(f, &a)
			stepAside := a.ConfigDigest != policy.Digest() || a.NoReread != *noReread
			for name, v := range a.Settings {
				got, ok := ownSettings[name]
				stepAside = stepAside || !ok || got != v
			}
			appendLine(filepath.Join(dir, "fake.armed"), map[bool]string{false: "armed", true: "step aside"}[stepAside])
			_ = protocol.WriteJSON(c, protocol.FrameArmed, protocol.Armed{Version: protocol.Version, Restart: stepAside})
			if stepAside {
				c.Close()
				l.Close()
				os.Exit(0)
			}
		}
		c.Close()
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
// kills every fake daemon started into its Dir when the test ends.
func fakeConfig(t *testing.T, mode string) Config {
	t.Helper()
	base := shortDir(t)
	cfg := Config{
		Dir:                filepath.Join(base, "rt", "id"),
		Workspace:          "/home/u/app",
		ContainerWorkspace: "/workspaces/app",
		Daemon:             os.Args[0],
		Client:             mustStatic(t),
		Settings:           settings.Settings{RulesFile: filepath.Join(base, "hostrun.yaml"), Reread: true, ReadyTimeout: 5 * time.Second},
	}
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
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)

	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: git status\n    args: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
// argv shows them and it resolves none again; rereading is explicit both
// ways, and ready_timeout, up's own, is no flag.
func TestServeArgsGiveEverySetting(t *testing.T) {
	base := Config{Workspace: "/home/u/app", ContainerWorkspace: "/workspaces/app", Settings: settings.Settings{
		RulesFile: "/home/u/app/.devcontainer/hostrun.yaml", StartupTimeout: 30 * time.Minute, Grace: 15 * time.Second,
		PollInterval: 2 * time.Second, AskTimeout: 10 * time.Minute, RequestTimeout: 10 * time.Second, ReadyTimeout: time.Minute,
	}}
	start := []string{"serve", "--socket", "/run/h.sock", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app", "--rules-file", "/home/u/app/.devcontainer/hostrun.yaml", "--watch",
		"--startup-timeout", "30m0s", "--grace", "15s", "--poll-interval", "2s", "--ask-timeout", "10m0s", "--request-timeout", "10s"}
	for _, tt := range []struct {
		reread bool
		flag   string
	}{
		{reread: true, flag: "--reread"},
		{reread: false, flag: "--no-reread"},
	} {
		cfg := base
		cfg.Reread = tt.reread
		want := append(slices.Clone(start), tt.flag)
		if got := serveArgs(cfg, "/run/h.sock"); !slices.Equal(got, want) {
			t.Errorf("reread %v: got %q, want %q", tt.reread, got, want)
		}
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

// An arm carries the daemon settings of up as Settings.Daemon names them,
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
			},
			want: map[string]string{
				"rules_file": "/w/rules.yaml", "startup_timeout": "1h0m0s", "grace": "1m0s", "reread": "false",
				"poll_interval": "1s", "ask_timeout": "1m0s", "request_timeout": "1s",
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
			if _, err := arm(context.Background(), socket, "digest", tt.s); err != nil {
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
		ReadyTimeout: 10 * time.Second,
	}
	elsewhere := full
	elsewhere.RulesFile, elsewhere.Reread, elsewhere.Grace = "/home/u/rules/hostrun.yaml", false, 90*time.Second
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
		if err != nil || !maps.Equal(got.Daemon(), tt.s.Daemon()) {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got.Daemon(), err, tt.s.Daemon())
		}
	}
}

// An up whose daemon settings differ from the running daemon's replaces
// it, and the new daemon follows the next up.
func TestUpReplacesDaemonWhoseSettingsDiffer(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	cfg.Grace = 15 * time.Second
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)
	cfg.Grace = time.Minute
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
