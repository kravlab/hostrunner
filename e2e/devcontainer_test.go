//go:build e2e

// Package e2e checks the devcontainer integration end to end: it brings up
// examples/devcontainer through the devcontainer CLI with freshly built
// binaries, once per available container runtime, and walks the container
// through its lifecycle (re-up, rebuild with a slow image build, stop,
// restart).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// cliTimeout bounds one devcontainer CLI call, including image builds.
	cliTimeout = 5 * time.Minute
	// stopTimeout covers the daemon's 2 s poll plus its 15 s grace period.
	stopTimeout = 60 * time.Second
	// attachTimeout bounds waitAttached, which runs once `up` has returned
	// and hostrun works: a few 2 s polls, plus what is left of the 15 s
	// grace after the rearm if the container came up sooner than that.
	attachTimeout = 30 * time.Second
	// slowBuild makes an image build outlast the daemon's 15 s grace period,
	// as real rebuilds often do.
	slowBuild = "FROM docker.io/library/alpine:3\nRUN sleep 20\n"
)

// env is the environment the devcontainer CLI (and so initializeCommand)
// runs with: the fresh binaries first in PATH. XDG_RUNTIME_DIR is left as
// is: rootless podman keeps its own container state there, so overriding it
// would leave containers that later podman calls cannot stop.
type env struct {
	bin     string
	runtime string // docker or podman binary
	ws      string // host workspace
}

// shortTemp makes a temp dir with a short path.
func shortTemp(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func buildBinaries(t *testing.T) string {
	t.Helper()
	bin := shortTemp(t, "hrbin")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin+"/", "./cmd/...")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// testRules extend the example rules with the commands the tests use to
// look at the host side.
const testRules = `
  - command: pwd
    args: none
  - command: sh
    flags:
      allow: [-c]
`

// newWorkspace copies the example devcontainer into a fresh git workspace
// whose path contains a space, switching it from a prebuilt image to a slow
// build and adding testRules to the example rules.
func newWorkspace(t *testing.T) string {
	t.Helper()
	ws := filepath.Join(shortTemp(t, "hrws"), "my app")
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", ws).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	rules, err := os.ReadFile("../examples/devcontainer/.devcontainer/hostrun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rules = append(bytes.TrimRight(rules, "\n"), testRules...)
	cfg, err := os.ReadFile("../examples/devcontainer/.devcontainer/devcontainer.json")
	if err != nil {
		t.Fatal(err)
	}
	const image = `"image": "docker.io/library/alpine:3",`
	if !bytes.Contains(cfg, []byte(image)) {
		t.Fatalf("example devcontainer.json no longer contains %s", image)
	}
	cfg = bytes.Replace(cfg, []byte(image), []byte(`"build": { "dockerfile": "Dockerfile" },`), 1)
	for name, data := range map[string][]byte{"devcontainer.json": cfg, "Dockerfile": []byte(slowBuild), "hostrun.yaml": rules} {
		if err := os.WriteFile(filepath.Join(ws, ".devcontainer", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// run executes a command with a timeout and returns its stdout; stderr is
// included in the error.
func run(t *testing.T, environ []string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = environ
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &cmdError{err: err, stderr: stderr.String()}
	}
	return stdout.String(), nil
}

type cmdError struct {
	err    error
	stderr string
}

func (c *cmdError) Error() string { return c.err.Error() + "\n" + lastLines(c.stderr, 20) }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (e env) vars() []string {
	return append(os.Environ(), "PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// up runs `devcontainer up` with extra flags and returns the container ID.
func (e env) up(t *testing.T, flags ...string) string {
	t.Helper()
	args := append([]string{"up", "--workspace-folder", e.ws, "--docker-path", e.runtime}, flags...)
	out, err := run(t, e.vars(), "devcontainer", args...)
	if err != nil {
		t.Fatalf("devcontainer up %v: %v", flags, err)
	}
	var result struct {
		Outcome     string `json:"outcome"`
		ContainerID string `json:"containerId"`
	}
	if err := json.Unmarshal([]byte(lastLines(out, 1)), &result); err != nil || result.Outcome != "success" {
		t.Fatalf("devcontainer up result %q: %v", out, err)
	}
	return result.ContainerID
}

// exec runs a command in the container through the devcontainer CLI.
func (e env) exec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return run(t, e.vars(), "devcontainer", append([]string{"exec", "--workspace-folder", e.ws, "--docker-path", e.runtime}, args...)...)
}

// expectHostrunWorks checks that hostrun reaches the daemon and runs in the
// host workspace.
func (e env) expectHostrunWorks(t *testing.T) {
	t.Helper()
	got, err := e.exec(t, "hostrun", "pwd")
	if err != nil {
		t.Fatalf("hostrun pwd: %v", err)
	}
	if strings.TrimSpace(got) != e.ws {
		t.Fatalf("hostrun pwd = %q, want %q", strings.TrimSpace(got), e.ws)
	}
}

// runtimeDirOf returns the host directory mounted at /run/hostrunner in
// the container: the daemon's runtime directory for this devcontainer.
func (e env) runtimeDirOf(t *testing.T, container string) string {
	t.Helper()
	out, err := run(t, os.Environ(), e.runtime, "inspect", "-f",
		`{{range .Mounts}}{{if eq .Destination "/run/hostrunner"}}{{.Source}}{{end}}{{end}}`, container)
	dir := strings.TrimSpace(out)
	if err != nil || dir == "" {
		t.Fatalf("find the runtime dir of %s: %v %s", container, err, out)
	}
	return dir
}

// daemonLog returns the runtime dir's daemon log, which every daemon
// started there appends to.
func daemonLog(t *testing.T, runtimeDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runtimeDir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// daemonStarts counts how many daemons have started in the runtime dir.
func daemonStarts(t *testing.T, runtimeDir string) int {
	t.Helper()
	return strings.Count(daemonLog(t, runtimeDir), "msg=listening")
}

// attachedSinceRearm reports whether a daemon log shows the watcher
// attached after its last rearm, i.e. after the latest `up`
// (internal/watch logs both events).
func attachedSinceRearm(log string) bool {
	return strings.LastIndex(log, `msg="devcontainer is running"`) >
		strings.LastIndex(log, `msg="rearmed: waiting for the devcontainer"`)
}

// waitAttached waits until the watcher has attached after the latest
// `up`'s rearm; only an attached watcher notices the container stop.
func waitAttached(t *testing.T, runtimeDir string) {
	t.Helper()
	eventually(t, attachTimeout,
		func() bool { return attachedSinceRearm(daemonLog(t, runtimeDir)) },
		func() string {
			return fmt.Sprintf("daemon did not attach to the container within %v; log:\n%s", attachTimeout, daemonLog(t, runtimeDir))
		})
}

// eventually polls cond every 500 ms until it holds, and fails the test
// with describe's message once timeout has passed.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(describe())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestAttachedSinceRearm checks the log reading waitAttached relies on; it
// needs no containers.
func TestAttachedSinceRearm(t *testing.T) {
	const (
		attach = `level=INFO msg="devcontainer is running" workspace=/w` + "\n"
		rearm  = `level=INFO msg="rearmed: waiting for the devcontainer" workspace=/w` + "\n"
	)
	tests := []struct {
		name string
		log  string
		want bool
	}{
		{"empty log", "", false},
		{"attached, never rearmed", attach, true},
		{"rearmed, never attached", rearm, false},
		{"rearmed, not attached yet", attach + rearm, false},
		{"attached after the last rearm", attach + rearm + attach, true},
		{"rearmed again after attaching", rearm + attach + rearm, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attachedSinceRearm(tt.log); got != tt.want {
				t.Fatalf("got %v, want %v for log:\n%s", got, tt.want, tt.log)
			}
		})
	}
}

func TestDevcontainerIntegration(t *testing.T) {
	if _, err := exec.LookPath("devcontainer"); err != nil {
		t.Skip("devcontainer CLI not installed")
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		t.Skip("XDG_RUNTIME_DIR is not set")
	}
	bin := buildBinaries(t)
	for _, runtime := range []string{"docker", "podman"} {
		t.Run(runtime, func(t *testing.T) {
			rtPath, err := exec.LookPath(runtime)
			if err != nil {
				t.Skipf("%s not installed", runtime)
			}
			// devcontainer CLI 0.89 waits for the container's start event from
			// `podman events`, which sometimes never arrives (also without
			// hostrunner, ~1 run in 4), hanging `devcontainer up`. Opt in.
			if runtime == "podman" && os.Getenv("HOSTRUNNER_E2E_PODMAN") != "1" {
				t.Skip("podman e2e is opt-in (HOSTRUNNER_E2E_PODMAN=1): devcontainer up sometimes hangs on podman events")
			}
			testLifecycle(t, env{bin: bin, runtime: rtPath, ws: newWorkspace(t)})
		})
	}
}

// cleanUp removes everything a run left behind for the workspace, whether
// or not `devcontainer up` succeeded: its containers (found by label), its
// daemons (found by their --workspace argument; one armed by the last up
// would otherwise wait up to 30 min for a container that never returns),
// and its runtime directories (found by their daemon.log).
func (e env) cleanUp(t *testing.T) {
	out, _ := run(t, os.Environ(), e.runtime, "ps", "-aq", "--filter", "label=devcontainer.local_folder="+e.ws)
	for _, id := range strings.Fields(out) {
		_, _ = run(t, os.Environ(), e.runtime, "rm", "-f", id)
	}
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, path := range cmdlines {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		args := strings.Split(string(data), "\x00")
		if len(args) > 1 && filepath.Base(args[0]) == "hostrunner" && args[1] == "serve" && slices.Contains(args, e.ws) {
			if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
	}
	logs, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "hostrunner", "*", "daemon.log"))
	for _, log := range logs {
		if data, err := os.ReadFile(log); err == nil && bytes.Contains(data, []byte(`workspace="`+e.ws+`"`)) {
			os.RemoveAll(filepath.Dir(log))
		}
	}
}

func testLifecycle(t *testing.T, e env) {
	t.Cleanup(func() { e.cleanUp(t) })
	container := e.up(t)
	runtimeDir := e.runtimeDirOf(t, container)
	socket := filepath.Join(runtimeDir, "hostrunner.sock")

	t.Run("hostrun runs in the host workspace", func(t *testing.T) {
		e.expectHostrunWorks(t)
	})

	t.Run("example rules allow git in the host workspace", func(t *testing.T) {
		got, err := e.exec(t, "hostrun", "git", "rev-parse", "--show-toplevel")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(got) != e.ws {
			t.Fatalf("git rev-parse --show-toplevel = %q, want %q", strings.TrimSpace(got), e.ws)
		}
	})

	t.Run("example rules deny a force push", func(t *testing.T) {
		_, err := e.exec(t, "hostrun", "git", "push", "--force")
		if err == nil || !strings.Contains(err.Error(), `denied by rule "git push": flag --force is not allowed`) {
			t.Fatalf("got %v, want the rule denial", err)
		}
		if _, err := e.exec(t, "hostrun", "rm", "-rf", "."); err == nil || !strings.Contains(err.Error(), `no rule allows "rm -rf"`) {
			t.Fatalf("got %v, want an unlisted command denied", err)
		}
	})

	t.Run("hostrun writes on the host", func(t *testing.T) {
		if _, err := e.exec(t, "hostrun", "sh", "-c", "echo from-host > created.txt"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(e.ws, "created.txt"))
		if err != nil || string(data) != "from-host\n" {
			t.Fatalf("host file: %q, %v", data, err)
		}
	})

	t.Run("container cannot write .devcontainer or the runtime dir", func(t *testing.T) {
		for _, path := range []string{".devcontainer/tampered", "/run/hostrunner/tampered"} {
			if _, err := e.exec(t, "sh", "-c", "touch "+path); err == nil {
				t.Errorf("writing %s succeeded", path)
			}
		}
		for _, path := range []string{filepath.Join(e.ws, ".devcontainer", "tampered"), filepath.Join(runtimeDir, "tampered")} {
			if _, err := os.Stat(path); err == nil {
				t.Errorf("%s appeared on the host", path)
			}
		}
	})

	t.Run("second up reuses the daemon", func(t *testing.T) {
		e.up(t)
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != 1 {
			t.Fatalf("%d daemons started, want 1", n)
		}
	})

	t.Run("changed rules apply on the next up", func(t *testing.T) {
		if _, err := e.exec(t, "hostrun", "echo", "hi"); err == nil {
			t.Fatal("echo allowed before the rule was added")
		}
		rulesFile := filepath.Join(e.ws, ".devcontainer", "hostrun.yaml")
		data, err := os.ReadFile(rulesFile)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, "\n  - command: echo\n    args: any\n"...)
		if err := os.WriteFile(rulesFile, data, 0o644); err != nil {
			t.Fatal(err)
		}
		before := daemonStarts(t, runtimeDir)
		e.up(t)
		if got, err := e.exec(t, "hostrun", "echo", "hi"); err != nil || strings.TrimSpace(got) != "hi" {
			t.Fatalf("echo after the rule was added: %q, %v", got, err)
		}
		if n := daemonStarts(t, runtimeDir); n != before+1 {
			t.Fatalf("%d daemon starts, want %d (restarted for the new rules)", n, before+1)
		}
	})

	t.Run("daemon survives a rebuild slower than its grace period", func(t *testing.T) {
		before := daemonStarts(t, runtimeDir)
		container = e.up(t, "--remove-existing-container", "--build-no-cache")
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != before {
			t.Fatalf("%d daemon starts, want %d (none during the rebuild)", n, before)
		}
		// The watcher polls every 2 s and the next subtest stops this
		// container at once: a container stopped before any poll saw it is
		// never attached to, so the daemon would not notice the stop.
		waitAttached(t, runtimeDir)
	})

	t.Run("daemon exits after the container stops", func(t *testing.T) {
		if _, err := run(t, os.Environ(), e.runtime, "stop", container); err != nil {
			t.Fatalf("stop: %v", err)
		}
		eventually(t, stopTimeout,
			func() bool { _, err := os.Stat(socket); return os.IsNotExist(err) },
			func() string {
				return fmt.Sprintf("daemon still serving %s %v after the container stopped", socket, stopTimeout)
			})
	})

	t.Run("restart brings a new daemon", func(t *testing.T) {
		before := daemonStarts(t, runtimeDir)
		container = e.up(t)
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != before+1 {
			t.Fatalf("%d daemon starts, want %d", n, before+1)
		}
	})
}
