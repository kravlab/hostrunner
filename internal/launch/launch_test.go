package launch

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
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

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
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
			Rules:  filepath.Join(dir, "missing-rules.yaml"),
			Daemon: os.Args[0], Client: client, ReadyTimeout: 5 * time.Second,
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

// runFakeDaemon records its pid in fake.pids and, in "serve" mode, listens
// on --socket and answers every FrameArm, logging it to fake.armed. Like the
// real daemon it compares the arm's rules digest with the rules it started
// with and, on a mismatch, steps aside and exits. It insists on
// --watch, which `up` must always pass.
func runFakeDaemon(mode string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := fs.String("socket", "", "")
	fs.String("workspace", "", "")
	fs.String("container-workspace", "", "")
	config := fs.String("config", "", "")
	watch := fs.Bool("watch", false, "")
	if len(os.Args) < 2 || os.Args[1] != "serve" || fs.Parse(os.Args[2:]) != nil || !*watch || *config == "" {
		os.Exit(2)
	}
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
			stepAside := a.ConfigDigest != policy.Digest()
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
		Rules:              filepath.Join(base, "hostrun.yaml"),
		Daemon:             os.Args[0],
		Client:             mustStatic(t),
		ReadyTimeout:       5 * time.Second,
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
	cfg.Dir = "/hostrunner/id" // what ${localEnv:XDG_RUNTIME_DIR}/hostrunner/<id> becomes
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
	if err := os.WriteFile(cfg.Rules, []byte("rules:\n  - command: git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), cfg.Rules) {
		t.Fatalf("got %v, want an error naming the rules file", err)
	}
	if pids := lines(t, filepath.Join(cfg.Dir, "fake.pids")); len(pids) != 0 {
		t.Fatalf("a daemon was started despite invalid rules: %v", pids)
	}
}

func TestUpReplacesDaemonThatStepsAside(t *testing.T) {
	cfg := fakeConfig(t, "serve")
	if err := os.WriteFile(cfg.Rules, []byte("rules:\n  - command: git status\n    args: any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := onlyPid(t, cfg.Dir)

	if err := os.WriteFile(cfg.Rules, []byte("rules:\n  - command: git status\n    args: none\n"), 0o644); err != nil {
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
