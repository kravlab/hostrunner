//go:build e2e

// Package e2e checks the devcontainer integration end to end: it brings up
// examples/devcontainer through the devcontainer CLI with freshly built
// binaries, once per available container runtime, and walks the container
// through its lifecycle (re-up, rebuild with a slow image build, stop,
// restart). It also brings up a variant of the example without the read-only
// .devcontainer mount and checks that the daemon refuses every command, and
// that it runs them once the host turns the mount check off.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
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
	// slowBuild makes an image build outlast the daemon's 15 s grace period,
	// as real rebuilds often do.
	slowBuild = "FROM docker.io/library/alpine:3\nRUN sleep 20\n"
)

// env is the environment the devcontainer CLI (and so initializeCommand)
// runs with: the runtime's wrapper and the fresh binaries first in PATH,
// and XDG_DATA_HOME in a
// directory of the test's, so the allows the suite records stay out of the
// host's own allow store. XDG_RUNTIME_DIR is left as
// is: rootless podman keeps its own container state there, so overriding it
// would leave containers that later podman calls cannot stop. The container
// runtime must not see the test's XDG_DATA_HOME either: rootless podman
// keeps its whole storage under it, so its containers would be created in a
// storage of the test's, out of reach of the calls the test makes outside
// the devcontainer CLI. The runtime is therefore run through a wrapper (see
// runtimeWrapper) that restores the host's XDG_DATA_HOME, and every runtime
// call, the CLI's and hostrunner's included, uses the host's storage.
type env struct {
	bin     string
	runtime string   // wrapper of the docker or podman binary, see runtimeWrapper
	ws      string   // host workspace
	data    string   // XDG_DATA_HOME, which holds the allow store
	extra   []string // more variables, NAME=value, for every command the test runs on the host
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

// runtimeWrapper writes an executable POSIX sh script, named name as the
// runtime is (so hostrunner finds it in PATH under the runtime's name; the
// devcontainer CLI tells docker from podman by the output of `-v`, which
// the script passes through), alone in its directory, that runs the runtime binary at real with the host's own
// XDG_DATA_HOME, as it is now (unset when it is unset), and passes the
// arguments, the standard streams and the exit code through. It returns the
// script's path. Call it before the test changes any variable for the
// commands it runs: real is an absolute path, and the quoting keeps paths
// with spaces or quotes intact.
func runtimeWrapper(t *testing.T, name, real string) string {
	t.Helper()
	setup := "unset XDG_DATA_HOME"
	if data, ok := os.LookupEnv("XDG_DATA_HOME"); ok {
		setup = "XDG_DATA_HOME=" + shellQuote(data) + "\nexport XDG_DATA_HOME"
	}
	script := "#!/bin/sh\n" + setup + "\nexec " + shellQuote(real) + " \"$@\"\n"
	path := filepath.Join(shortTemp(t, "hrrt"), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// shellQuote quotes s as one word for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// readOnlyMount is the example's mount that keeps .devcontainer read-only
// in the container, with the comma that separates it from the mount before
// it.
var readOnlyMount = regexp.MustCompile(`,\s*//[^\n]*\n\s*"source=\$\{localWorkspaceFolder\}/\.devcontainer,[^"]*"`)

// newWorkspace copies the example devcontainer into a fresh git workspace
// whose path contains a space, adding testRules to the example rules. With
// writable false it switches the example from a prebuilt image to a slow
// build; with writable true it drops the read-only .devcontainer mount, so
// the container can write the daemon's configuration.
func newWorkspace(t *testing.T, writable bool) string {
	t.Helper()
	return newWorkspaceWith(t, writable, nil)
}

// newWorkspaceWith is newWorkspace whose writable variant, with edit not
// nil, has its devcontainer.json edited by edit, given the workspace,
// instead of the read-only .devcontainer mount dropped.
func newWorkspaceWith(t *testing.T, writable bool, edit func(ws string, cfg []byte) []byte) string {
	t.Helper()
	ws := filepath.Join(shortTemp(t, "hrws"), "my app")
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", ws).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	rules, err := os.ReadFile("../examples/devcontainer/.devcontainer/hostrunner-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rules = append(bytes.TrimRight(rules, "\n"), testRules...)
	cfg, err := os.ReadFile("../examples/devcontainer/.devcontainer/devcontainer.json")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"hostrunner-rules.yaml": rules}
	if writable {
		if !readOnlyMount.Match(cfg) {
			t.Fatal("example devcontainer.json no longer contains the read-only .devcontainer mount")
		}
		if edit != nil {
			cfg = edit(ws, cfg)
		} else if cfg = readOnlyMount.ReplaceAll(cfg, nil); bytes.Contains(cfg, []byte("/.devcontainer,")) {
			t.Fatal("the variant still mounts .devcontainer")
		}
	} else {
		const image = `"image": "docker.io/library/alpine:3",`
		if !bytes.Contains(cfg, []byte(image)) {
			t.Fatalf("example devcontainer.json no longer contains %s", image)
		}
		cfg = bytes.Replace(cfg, []byte(image), []byte(`"build": { "dockerfile": "Dockerfile" },`), 1)
		files["Dockerfile"] = []byte(slowBuild)
	}
	files["devcontainer.json"] = cfg
	for name, data := range files {
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

// Unwrap gives errors.As the command's *exec.ExitError, for its exit code.
func (c *cmdError) Unwrap() error { return c.err }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// vars is the environment of the commands the test runs on the host: the
// test's own, with the runtime's wrapper and then the fresh binaries first
// in PATH, XDG_DATA_HOME set to the test's directory, and extra. It is for
// hostrunner and the devcontainer CLI only: the container runtime restores
// the host's XDG_DATA_HOME itself (see runtimeWrapper), so the commands the
// test runs on the runtime take os.Environ(). The wrapper comes first in
// PATH because hostrunner finds the runtime there (watch.Detect): its own
// runtime calls must use the host's storage too, where the CLI created the
// container.
func (e env) vars() []string {
	sep := string(os.PathListSeparator)
	path := filepath.Dir(e.runtime) + sep + e.bin + sep + os.Getenv("PATH")
	out := append(os.Environ(), "PATH="+path, "XDG_DATA_HOME="+e.data)
	return append(out, e.extra...)
}

// allowPoll and allowWait bound the retry of `hostrunner allow` while a
// daemon that rereads by itself (every 2 s) has not yet recorded its
// request.
const (
	allowPoll = 500 * time.Millisecond
	allowWait = 10 * time.Second
)

// allow runs `hostrunner allow` for the workspace, as the human on the
// host does after a refused up or a hand edit of the rules: it allows the
// pending request of the workspace. A daemon that rereads by itself records
// its request only at its next look (every 2 s), so allow is retried every
// allowPoll for up to allowWait while it fails.
func (e env) allow(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(allowWait)
	for {
		_, err := run(t, e.vars(), filepath.Join(e.bin, "hostrunner"), "allow", e.ws)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hostrunner allow: %v", err)
		}
		time.Sleep(allowPoll)
	}
}

// expectUpRefused runs `devcontainer up` while the rules and settings are not
// allowed, as a human's first start does: the `hostrunner up` of
// initializeCommand refuses and records its request, so `devcontainer up`
// fails and starts no container; the test fails unless it does, and unless
// its output names `hostrunner allow`.
func (e env) expectUpRefused(t *testing.T) {
	t.Helper()
	args := []string{"up", "--workspace-folder", e.ws, "--docker-path", e.runtime}
	out, err := run(t, e.vars(), "devcontainer", args...)
	if err == nil {
		t.Fatalf("devcontainer up succeeded with the rules not allowed: %q", out)
	}
	if text := out + err.Error(); !strings.Contains(text, "hostrunner allow") {
		t.Fatalf("devcontainer up refused without naming `hostrunner allow`: %q", text)
	}
}

// allowAfterRefusedUp brings the workspace to its first allow, as a human
// does before the first start: a refused `devcontainer up` records the
// request (expectUpRefused), and `hostrunner allow` allows it.
func (e env) allowAfterRefusedUp(t *testing.T) {
	t.Helper()
	e.expectUpRefused(t)
	e.allow(t)
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

// pendingPoll and pendingWait bound the retry of exec while the daemon has
// not yet checked the container's mounts.
const (
	pendingPoll = 500 * time.Millisecond
	pendingWait = 30 * time.Second
	// pendingMessage is the daemon's refusal (exit 125) until it has
	// attached to the armed container, which it does on its next poll after
	// the container starts, or, for a running container `up` reused, on its
	// first poll a grace (15 s) after the arm: within pendingWait.
	pendingMessage = "the mount check has not run yet"
)

// exec runs a command in the container through the devcontainer CLI. It
// retries, for up to pendingWait, while the daemon refuses with the
// pending mount check message (and only that one): the container may have
// started before the daemon attached to it.
func (e env) exec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"exec", "--workspace-folder", e.ws, "--docker-path", e.runtime}, args...)
	deadline := time.Now().Add(pendingWait)
	for {
		out, err := run(t, e.vars(), "devcontainer", full...)
		if err == nil || !strings.Contains(err.Error(), pendingMessage) || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(pendingPoll)
	}
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

// eachRuntime runs test once per available container runtime, in a
// workspace that newWs makes.
func eachRuntime(t *testing.T, newWs func(*testing.T) string, test func(*testing.T, env)) {
	t.Helper()
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
			if rtPath, err = filepath.Abs(rtPath); err != nil {
				t.Fatal(err)
			}
			// devcontainer CLI 0.89 waits for the container's start event from
			// `podman events`, which sometimes never arrives (also without
			// hostrunner, ~1 run in 4), hanging `devcontainer up`. Opt in.
			if runtime == "podman" && os.Getenv("HOSTRUNNER_E2E_PODMAN") != "1" {
				t.Skip("podman e2e is opt-in (HOSTRUNNER_E2E_PODMAN=1): devcontainer up sometimes hangs on podman events")
			}
			test(t, env{bin: bin, runtime: runtimeWrapper(t, runtime, rtPath), ws: newWs(t), data: shortTemp(t, "hrdata")})
		})
	}
}

func TestDevcontainerIntegration(t *testing.T) {
	eachRuntime(t, func(t *testing.T) string { return newWorkspace(t, false) }, testLifecycle)
}

// TestDevcontainerWritableMount brings up the example without its read-only
// .devcontainer mount: the daemon must refuse every command, naming the
// container path only.
func TestDevcontainerWritableMount(t *testing.T) {
	eachRuntime(t, func(t *testing.T) string { return newWorkspace(t, true) }, testWritableMount)
}

// TestDevcontainerWritableVolumeMount brings up the example with its
// workspace mounted read-only and its .devcontainer mounted, in place of
// the read-only bind mount, from a writable local volume that binds the
// workspace's .devcontainer directory (type=none, o=bind), which the test
// creates and removes: only the volume lets the container write the
// daemon's configuration, so the daemon must tell it from the volume's
// options and refuse every command as for a writable bind mount.
func TestDevcontainerWritableVolumeMount(t *testing.T) {
	eachRuntime(t, func(t *testing.T) string { return newWorkspaceWith(t, true, volumeConfig(t)) }, func(t *testing.T, e env) {
		vol := volumeName(e.ws)
		// In the storage devcontainer runs the runtime with, the host's:
		// rootless podman keeps its volumes under XDG_DATA_HOME, and would
		// make a volume of its own of that name for the container otherwise.
		if _, err := run(t, os.Environ(), e.runtime, "volume", "create", "--driver", "local",
			"--opt", "type=none", "--opt", "o=bind", "--opt", "device="+filepath.Join(e.ws, ".devcontainer"), vol); err != nil {
			t.Fatalf("volume create: %v", err)
		}
		// Registered before testWritableMount's clean-up, so it runs after
		// the containers using the volume are gone.
		t.Cleanup(func() {
			if _, err := run(t, os.Environ(), e.runtime, "volume", "rm", "-f", vol); err != nil {
				t.Errorf("volume rm: %v", err)
			}
		})
		testWritableMount(t, e)
	})
}

// volumeConfig returns the edit of TestDevcontainerWritableVolumeMount's
// devcontainer.json: the read-only .devcontainer mount replaced by one of
// the workspace's volume (see volumeName), and the workspace mounted
// read-only where devcontainer mounts it by default,
// /workspaces/<its directory name>.
func volumeConfig(t *testing.T) func(ws string, cfg []byte) []byte {
	return func(ws string, cfg []byte) []byte {
		mount := "source=" + volumeName(ws) + ",target=${containerWorkspaceFolder}/.devcontainer,type=volume"
		cfg = readOnlyMount.ReplaceAllLiteral(cfg, []byte(",\n    "+strconv.Quote(mount)))
		const mounts = `"mounts": [`
		if !bytes.Contains(cfg, []byte(mounts)) {
			t.Fatalf("example devcontainer.json no longer contains %s", mounts)
		}
		workspace := `"workspaceMount": "source=${localWorkspaceFolder},target=/workspaces/${localWorkspaceFolderBasename},type=bind,readonly",` + "\n" +
			`  "workspaceFolder": "/workspaces/${localWorkspaceFolderBasename}",` + "\n  "
		return bytes.Replace(cfg, []byte(mounts), []byte(workspace+mounts), 1)
	}
}

// volumeName is the name of the volume TestDevcontainerWritableVolumeMount
// creates for the workspace ws: unique as ws's temp directory is.
func volumeName(ws string) string {
	return "hrvol-" + filepath.Base(filepath.Dir(ws))
}

// TestDevcontainerWritableMountWithTheCheckOff brings up the same variant
// with the mount check turned off on the host (HOSTRUNNER_MOUNT_CHECK=false
// for the `hostrunner up` the devcontainer CLI runs, which records the
// setting in its request, and `hostrunner allow` allows that request): the
// daemon runs the commands its rules allow.
func TestDevcontainerWritableMountWithTheCheckOff(t *testing.T) {
	eachRuntime(t, func(t *testing.T) string { return newWorkspace(t, true) }, func(t *testing.T, e env) {
		e.extra = []string{"HOSTRUNNER_MOUNT_CHECK=false"}
		testWritableMountWithTheCheckOff(t, e)
	})
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
	e.allowAfterRefusedUp(t)
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

	t.Run("the approval socket is out of the container's reach", func(t *testing.T) {
		approvalSocket := runtimeDir + ".approval.sock"
		if info, err := os.Stat(approvalSocket); err != nil || info.Mode().Type() != os.ModeSocket {
			t.Fatalf("approval socket %s on the host: %v", approvalSocket, err)
		}
		// The container mounts the runtime directory alone, at
		// /run/hostrunner, and the workspace; no approval socket is in either
		// or beside the runtime directory.
		got, err := e.exec(t, "sh", "-c", `find /run "$PWD" -name '*.approval.sock'`)
		if err != nil {
			t.Fatalf("searching the container: %v", err)
		}
		if strings.TrimSpace(got) != "" {
			t.Fatalf("the container sees an approval socket:\n%s", got)
		}
		start := time.Now()
		_, err = e.exec(t, "hostrun", "--ask", "rm", "-rf", ".")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 || !strings.Contains(err.Error(), "not approved: no approval session serves this devcontainer") {
			t.Fatalf("got %v, want --ask refused with 126 without a session", err)
		}
		if waited := time.Since(start); waited > cliTimeout/10 {
			t.Fatalf("--ask without a session took %v, want it refused at once", waited)
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

	t.Run("a hand-edited rules file is reread without a restart", func(t *testing.T) {
		if _, err := e.exec(t, "hostrun", "echo", "hi"); err == nil {
			t.Fatal("echo allowed before the rule was added")
		}
		rulesFile := filepath.Join(e.ws, ".devcontainer", "hostrunner-rules.yaml")
		data, err := os.ReadFile(rulesFile)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, "\n  - command: echo\n    args: any\n"...)
		before := daemonStarts(t, runtimeDir)
		if err := os.WriteFile(rulesFile, data, 0o644); err != nil {
			t.Fatal(err)
		}
		e.allow(t)
		// The daemon looks at the file every 2 s.
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := e.exec(t, "hostrun", "echo", "hi")
			if err == nil && strings.TrimSpace(got) == "hi" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("echo after the rule was added: %q, %v", got, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		e.up(t)
		if n := daemonStarts(t, runtimeDir); n != before {
			t.Fatalf("%d daemon starts, want %d (the rules reread, the daemon kept)", n, before)
		}
	})

	t.Run("restart picks up a hand-edited rules file", func(t *testing.T) {
		if _, err := e.exec(t, "hostrun", "printf", "hi"); err == nil {
			t.Fatal("printf allowed before the rule was added")
		}
		rulesFile := filepath.Join(e.ws, ".devcontainer", "hostrunner-rules.yaml")
		data, err := os.ReadFile(rulesFile)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, "\n  - command: printf\n    args: any\n"...)
		if err := os.WriteFile(rulesFile, data, 0o644); err != nil {
			t.Fatal(err)
		}
		e.allow(t)
		before := daemonStarts(t, runtimeDir)
		// By its path: exec resolves a bare name with the test's own PATH,
		// not e.vars()'s, and would find an installed hostrunner.
		if _, err := run(t, e.vars(), filepath.Join(e.bin, "hostrunner"), "restart", "--dir", runtimeDir); err != nil {
			t.Fatalf("hostrunner restart: %v", err)
		}
		if got, err := e.exec(t, "hostrun", "printf", "hi"); err != nil || got != "hi" {
			t.Fatalf("printf after the restart: %q, %v", got, err)
		}
		if n := daemonStarts(t, runtimeDir); n != before+1 {
			t.Fatalf("%d daemon starts, want %d (restarted)", n, before+1)
		}
	})

	// The settings files are read as they are at the restart: a workspace
	// settings file written now names another rules file, which the new
	// daemon serves, and the next up arms it as it is.
	t.Run("restart picks up an edited workspace settings file", func(t *testing.T) {
		if _, err := e.exec(t, "hostrun", "uname"); err == nil {
			t.Fatal("uname allowed before the settings named rules allowing it")
		}
		dotDir := filepath.Join(e.ws, ".devcontainer")
		data, err := os.ReadFile(filepath.Join(dotDir, "hostrunner-rules.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, "\n  - command: uname\n    args: none\n"...)
		for name, content := range map[string][]byte{"other.yaml": data, "hostrunner.yaml": []byte("rules_file: .devcontainer/other.yaml\n")} {
			if err := os.WriteFile(filepath.Join(dotDir, name), content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// The daemon does not watch this file: a restart of settings not
		// allowed fails before the old daemon stops, and records the request.
		if _, err := run(t, e.vars(), filepath.Join(e.bin, "hostrunner"), "restart", "--dir", runtimeDir); err == nil {
			t.Fatal("hostrunner restart succeeded with the settings not allowed")
		}
		e.allow(t)
		before := daemonStarts(t, runtimeDir)
		if _, err := run(t, e.vars(), filepath.Join(e.bin, "hostrunner"), "restart", "--dir", runtimeDir); err != nil {
			t.Fatalf("hostrunner restart: %v", err)
		}
		if got, err := e.exec(t, "hostrun", "uname"); err != nil || strings.TrimSpace(got) == "" {
			t.Fatalf("uname after the restart: %q, %v", got, err)
		}
		if n := daemonStarts(t, runtimeDir); n != before+1 {
			t.Fatalf("%d daemon starts, want %d (restarted)", n, before+1)
		}
	})

	t.Run("daemon survives a rebuild slower than its grace period", func(t *testing.T) {
		before := daemonStarts(t, runtimeDir)
		container = e.up(t, "--remove-existing-container", "--build-no-cache")
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != before {
			t.Fatalf("%d daemon starts, want %d (none during the rebuild)", n, before)
		}
	})

	// Stopped at once, before the daemon may have polled: it still sees the
	// container the rebuild started (#12).
	t.Run("daemon exits after the rebuilt container stops at once", func(t *testing.T) {
		e.stopAndExpectDaemonExit(t, container, socket)
	})

	t.Run("restart brings a new daemon", func(t *testing.T) {
		before := daemonStarts(t, runtimeDir)
		container = e.up(t)
		e.expectHostrunWorks(t)
		if n := daemonStarts(t, runtimeDir); n != before+1 {
			t.Fatalf("%d daemon starts, want %d", n, before+1)
		}
	})

	// `up` started the stopped container again under the same ID.
	t.Run("daemon exits after the restarted container stops at once", func(t *testing.T) {
		e.stopAndExpectDaemonExit(t, container, socket)
	})
}

func testWritableMount(t *testing.T, e env) {
	t.Cleanup(func() { e.cleanUp(t) })
	e.allowAfterRefusedUp(t)
	e.up(t)

	// devcontainer's default workspace mount puts the workspace at
	// /workspaces/<its directory name>.
	want := "the container can write " + path.Join("/workspaces", filepath.Base(e.ws), ".devcontainer") + ","
	// --ask is refused by the same mount gate, before the daemon looks for an
	// approval session.
	for _, args := range [][]string{{"pwd"}, {"--dry-run", "pwd"}, {"--ask", "pwd"}} {
		_, err := e.exec(t, append([]string{"hostrun"}, args...)...)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 {
			t.Fatalf("hostrun %v = %v, want exit 126", args, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("hostrun %v = %v, want a refusal containing %q", args, err, want)
		}
		// The refusal names container paths only: nothing of the host's
		// workspace path, which holds the test's temp directory.
		if strings.Contains(err.Error(), filepath.Dir(e.ws)) {
			t.Fatalf("hostrun %v = %v, want no host path", args, err)
		}
		if off := "mount_check: false in the host settings file, HOSTRUNNER_MOUNT_CHECK=false or --no-mount-check"; !strings.Contains(err.Error(), off) {
			t.Fatalf("hostrun %v = %v, want a refusal saying how to turn the check off", args, err)
		}
	}
}

func testWritableMountWithTheCheckOff(t *testing.T, e env) {
	t.Cleanup(func() { e.cleanUp(t) })
	e.allowAfterRefusedUp(t)
	e.up(t)
	e.expectHostrunWorks(t)
}

// stopAndExpectDaemonExit stops the container and waits for the daemon
// serving socket to exit.
func (e env) stopAndExpectDaemonExit(t *testing.T, container, socket string) {
	t.Helper()
	if _, err := run(t, os.Environ(), e.runtime, "stop", container); err != nil {
		t.Fatalf("stop: %v", err)
	}
	eventually(t, stopTimeout,
		func() bool { _, err := os.Stat(socket); return os.IsNotExist(err) },
		func() string {
			return fmt.Sprintf("daemon still serving %s %v after the container stopped", socket, stopTimeout)
		})
}
