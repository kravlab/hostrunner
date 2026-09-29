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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// cliTimeout bounds one devcontainer CLI call, including image builds.
	cliTimeout = 5 * time.Minute
	// stopTimeout covers the daemon's 2 s poll plus its 15 s grace period.
	stopTimeout = 60 * time.Second
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

// newWorkspace copies the example devcontainer into a fresh workspace whose
// path contains a space, switching it from a prebuilt image to a slow build.
func newWorkspace(t *testing.T) string {
	t.Helper()
	ws := filepath.Join(shortTemp(t, "hrws"), "my app")
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile("../examples/devcontainer/.devcontainer/devcontainer.json")
	if err != nil {
		t.Fatal(err)
	}
	const image = `"image": "docker.io/library/alpine:3",`
	if !bytes.Contains(cfg, []byte(image)) {
		t.Fatalf("example devcontainer.json no longer contains %s", image)
	}
	cfg = bytes.Replace(cfg, []byte(image), []byte(`"build": { "dockerfile": "Dockerfile" },`), 1)
	for name, data := range map[string][]byte{"devcontainer.json": cfg, "Dockerfile": []byte(slowBuild)} {
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

// daemonStarts counts how many daemons have started in the runtime dir.
func daemonStarts(t *testing.T, runtimeDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runtimeDir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "msg=listening")
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
			testLifecycle(t, env{bin: bin, runtime: rtPath, ws: newWorkspace(t)})
		})
	}
}

func testLifecycle(t *testing.T, e env) {
	container := e.up(t)
	runtimeDir := e.runtimeDirOf(t, container)
	socket := filepath.Join(runtimeDir, "hostrunner.sock")
	// Cleanups run last-registered first: remove the container, then the
	// runtime dir the (then exiting) daemon used.
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Cleanup(func() { _, _ = run(t, os.Environ(), e.runtime, "rm", "-f", container) })

	t.Run("hostrun runs in the host workspace", func(t *testing.T) {
		e.expectHostrunWorks(t)
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

	t.Run("daemon survives a rebuild slower than its grace period", func(t *testing.T) {
		container = e.up(t, "--remove-existing-container", "--build-no-cache")
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != 1 {
			t.Fatalf("%d daemons started, want 1", n)
		}
	})

	t.Run("daemon exits after the container stops", func(t *testing.T) {
		if _, err := run(t, os.Environ(), e.runtime, "stop", container); err != nil {
			t.Fatalf("stop: %v", err)
		}
		deadline := time.Now().Add(stopTimeout)
		for {
			if _, err := os.Stat(socket); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("daemon still serving %s %v after the container stopped", socket, stopTimeout)
			}
			time.Sleep(500 * time.Millisecond)
		}
	})

	t.Run("restart brings a new daemon", func(t *testing.T) {
		container = e.up(t)
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != 2 {
			t.Fatalf("%d daemons started in total, want 2", n)
		}
	})
}
