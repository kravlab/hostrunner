package client_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/workspace"
)

const containerRoot = "/workspaces/app"

// harness is a daemon serving a temporary host workspace over a temporary
// socket, torn down when the test ends.
type harness struct {
	transport transport.Unix
	hostRoot  string
	stop      func() // shuts the daemon down; safe to call more than once
}

// allowAll is a policy that allows every command, for tests about
// everything but the rules.
type allowAll struct{}

func (allowAll) Check([]string) ([]rules.PathArg, error) { return nil, nil }

// startDaemon starts a daemon with the given policy, or one allowing every
// command.
func startDaemon(t *testing.T, policies ...daemon.Policy) harness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hostRoot := filepath.Join(base, "app")
	if err := os.MkdirAll(filepath.Join(hostRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mapper, err := workspace.NewMapper(hostRoot, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	tr := transport.Unix{Path: filepath.Join(base, "h.sock")}
	l, err := tr.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var p daemon.Policy = allowAll{}
	if len(policies) > 0 {
		p = policies[0]
	}
	srv := daemon.New(mapper, p, slog.New(slog.DiscardHandler))
	go func() { done <- srv.Serve(ctx, l) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	t.Cleanup(stop)
	return harness{transport: tr, hostRoot: hostRoot, stop: stop}
}

// result is what the user of hostrun observes.
type result struct {
	code           int
	stdout, stderr string
}

func (h harness) run(t *testing.T, cwd, stdin string, argv ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := client.Run(context.Background(), h.transport, argv, cwd, strings.NewReader(stdin), &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestRunStreamsOutputAndPropagatesExitCode(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot, "", "sh", "-c", "echo out; echo err >&2; exit 3")
	want := result{code: 3, stdout: "out\n", stderr: "err\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRunForwardsStdin(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot, "hello\nworld\n", "cat")
	if want := (result{stdout: "hello\nworld\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRunStreamsOutputLargerThanOneFrame(t *testing.T) {
	h := startDaemon(t)
	const size = 3 * protocol.MaxPayload
	got := h.run(t, containerRoot, "", "head", "-c", strconv.Itoa(size), "/dev/zero")
	if got.code != 0 || len(got.stdout) != size || got.stderr != "" {
		t.Fatalf("got code %d, %d stdout bytes, stderr %q; want 0, %d, empty", got.code, len(got.stdout), got.stderr, size)
	}
}

func TestRunMirrorsContainerCwdOnHost(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot+"/sub", "", "pwd")
	if want := (result{stdout: filepath.Join(h.hostRoot, "sub") + "\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRunRejectsCwdOutsideWorkspace(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, "/etc", "", "pwd")
	if got.code != protocol.ExitRejected || got.stdout != "" || !strings.HasPrefix(got.stderr, "hostrun: ") {
		t.Fatalf("got %+v, want code %d, no stdout, hostrun error", got, protocol.ExitRejected)
	}
}

func TestRunReportsUnknownCommand(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot, "", "hostrunner-no-such-command")
	if got.code != protocol.ExitNotFound || !strings.HasPrefix(got.stderr, "hostrun: ") {
		t.Fatalf("got %+v, want code %d with a hostrun error", got, protocol.ExitNotFound)
	}
}

func TestRunReportsTerminationBySignal(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot, "", "sh", "-c", "kill -TERM $$")
	if want := 128 + int(syscall.SIGTERM); got.code != want {
		t.Fatalf("got code %d, want %d", got.code, want)
	}
}

func TestRunUsesHostEnvironment(t *testing.T) {
	t.Setenv("HOSTRUNNER_TEST_VAR", "from-host")
	h := startDaemon(t)
	got := h.run(t, containerRoot, "", "sh", "-c", "printf %s \"$HOSTRUNNER_TEST_VAR\"")
	if want := (result{stdout: "from-host"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRunRejectsEmptyCommand(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot, "")
	if got.code != protocol.ExitHostrunError || !strings.Contains(got.stderr, "usage") {
		t.Fatalf("got %+v, want code %d with usage", got, protocol.ExitHostrunError)
	}
}

func TestRunFailsWhenDaemonIsUnreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	tr := transport.Unix{Path: filepath.Join(t.TempDir(), "missing.sock")}
	code := client.Run(context.Background(), tr, []string{"true"}, containerRoot, strings.NewReader(""), &stdout, &stderr)
	if code != protocol.ExitHostrunError || !strings.HasPrefix(stderr.String(), "hostrun: ") {
		t.Fatalf("got code %d, stderr %q; want %d with a hostrun error", code, stderr.String(), protocol.ExitHostrunError)
	}
}

// rawRequest sends req on a fresh connection and returns the first
// status frame (Exit or Error) the daemon answers with.
func (h harness) rawRequest(t *testing.T, req protocol.Request) protocol.Frame {
	t.Helper()
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == protocol.FrameExit || f.Type == protocol.FrameError {
			return f
		}
	}
}

func TestDaemonRejectsProtocolViolations(t *testing.T) {
	h := startDaemon(t)
	cases := map[string]protocol.Request{
		"version mismatch": {Version: protocol.Version + 1, Argv: []string{"true"}, Cwd: containerRoot},
		"empty argv":       {Version: protocol.Version, Cwd: containerRoot},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			f := h.rawRequest(t, req)
			var e protocol.Error
			if f.Type != protocol.FrameError {
				t.Fatalf("got frame type %d, want FrameError", f.Type)
			}
			if err := protocol.DecodeJSON(f, &e); err != nil {
				t.Fatal(err)
			}
			if e.Code != protocol.ExitHostrunError {
				t.Fatalf("got code %d, want %d", e.Code, protocol.ExitHostrunError)
			}
		})
	}
}

// readStdoutLine reads frames until the first stdout line and returns it.
func readStdoutLine(t *testing.T, conn net.Conn) string {
	t.Helper()
	var out []byte
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if f.Type != protocol.FrameStdout {
			continue
		}
		out = append(out, f.Payload...)
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			return string(out[:i])
		}
	}
}

func TestDaemonKillsCommandAndItsChildrenWhenClientDisconnects(t *testing.T) {
	h := startDaemon(t)
	// The shell stays the direct child; the sleep is a grandchild in its
	// process group, as ssh is under git.
	conn := h.startRaw(t, "sh", "-c", "sleep 60 & echo $!; wait")
	pid, err := strconv.Atoi(readStdoutLine(t, conn))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitForExit(t, pid)
}

// endlessReader is a stdin that never ends, like `yes | hostrun ...`.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// firstLineWriter delivers the first line written to it on a channel.
type firstLineWriter struct {
	line chan string // buffered, receives exactly one line

	mu   sync.Mutex
	buf  []byte
	sent bool
}

func (w *firstLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if i := bytes.IndexByte(w.buf, '\n'); i >= 0 && !w.sent {
		w.line <- string(w.buf[:i])
		w.sent = true
	}
	return len(p), nil
}

// waitForExit fails the test unless process pid is gone within 5 s.
func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestInterruptKillsCommandThatIgnoresFloodedStdin(t *testing.T) {
	h := startDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	stdout := &firstLineWriter{line: make(chan string, 1)}
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- client.Run(ctx, h.transport, []string{"sh", "-c", "echo $$; exec sleep 60"}, containerRoot, endlessReader{}, stdout, io.Discard)
	}()
	pid, err := strconv.Atoi(<-stdout.line)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let stdin back up behind the sleeping command
	cancel()

	if code := <-codeCh; code != exitInterrupted {
		t.Errorf("got code %d, want %d", code, exitInterrupted)
	}
	waitForExit(t, pid)
}

// exitInterrupted is the conventional exit code after Ctrl+C (128 + SIGINT).
const exitInterrupted = 130

func TestRunReportsInterruption(t *testing.T) {
	h := startDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	stdout := &firstLineWriter{line: make(chan string, 1)}
	var stderr bytes.Buffer
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- client.Run(ctx, h.transport, []string{"sh", "-c", "echo started; exec sleep 60"}, containerRoot, strings.NewReader(""), stdout, &stderr)
	}()
	<-stdout.line
	cancel()
	if code := <-codeCh; code != exitInterrupted || stderr.String() != "hostrun: interrupted\n" {
		t.Fatalf("got code %d, stderr %q; want %d, %q", code, stderr.String(), exitInterrupted, "hostrun: interrupted\n")
	}
}

// startRaw opens a connection and sends a request for argv, returning the
// connection for the test to drive by hand.
func (h harness) startRaw(t *testing.T, argv ...string) net.Conn {
	t.Helper()
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	req := protocol.Request{Version: protocol.Version, Argv: argv, Cwd: containerRoot}
	if err := protocol.WriteJSON(conn, protocol.FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	return conn
}

// expectHostrunError reads frames until a status frame and asserts it is an
// Error with ExitHostrunError.
func expectHostrunError(t *testing.T, conn net.Conn) {
	t.Helper()
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("connection ended without an error frame: %v", err)
		}
		switch f.Type {
		case protocol.FrameExit:
			t.Fatalf("got FrameExit, want FrameError")
		case protocol.FrameError:
			var e protocol.Error
			if err := protocol.DecodeJSON(f, &e); err != nil {
				t.Fatal(err)
			}
			if e.Code != protocol.ExitHostrunError {
				t.Fatalf("got code %d, want %d", e.Code, protocol.ExitHostrunError)
			}
			return
		}
	}
}

func TestDaemonReportsUnexpectedFrameFromClient(t *testing.T) {
	h := startDaemon(t)
	conn := h.startRaw(t, "sleep", "60")
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdout, Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	expectHostrunError(t, conn)
}

func TestDaemonReportsStdinBeyondGrantedCredit(t *testing.T) {
	h := startDaemon(t)
	conn := h.startRaw(t, "sleep", "60")
	// The daemon grants StdinWindow up front plus whatever the command's stdin
	// pipe absorbs (64 KiB by default), so four windows exceed any credit.
	// Writing stops once the daemon hangs up on the violation.
	chunk := make([]byte, 32*1024)
	for sent := 0; sent < 4*protocol.StdinWindow; sent += len(chunk) {
		if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdin, Payload: chunk}); err != nil {
			break
		}
	}
	expectHostrunError(t, conn)
}

func TestRunErrorsDoNotRevealHostPaths(t *testing.T) {
	h := startDaemon(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(h.hostRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{containerRoot + "/missing", containerRoot + "/escape"} {
		got := h.run(t, cwd, "", "pwd")
		if got.code != protocol.ExitRejected {
			t.Errorf("cwd %s: got code %d, want %d", cwd, got.code, protocol.ExitRejected)
		}
		if strings.Contains(got.stderr, h.hostRoot) || strings.Contains(got.stderr, outside) {
			t.Errorf("cwd %s: error reveals a host path: %q", cwd, got.stderr)
		}
		if !strings.Contains(got.stderr, cwd) {
			t.Errorf("cwd %s: error does not name the container path: %q", cwd, got.stderr)
		}
	}
}

func TestDaemonChecksVersionBeforeRequestShape(t *testing.T) {
	h := startDaemon(t)
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	future := []byte(`{"version": 99, "argv": {"program": "git"}}`)
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameRequest, Payload: future}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil || f.Type != protocol.FrameError {
		t.Fatalf("got frame %v, err %v; want FrameError", f.Type, err)
	}
	var e protocol.Error
	if err := protocol.DecodeJSON(f, &e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Message, "version") {
		t.Fatalf("message %q does not mention the version mismatch", e.Message)
	}
}

func TestRunFinishesWhenBackgroundChildHoldsOutputOpen(t *testing.T) {
	h := startDaemon(t)
	start := time.Now()
	got := h.run(t, containerRoot, "", "sh", "-c", "sleep 30 & echo done")
	if got.code != 0 || got.stdout != "done\n" {
		t.Fatalf("got %+v, want code 0 and stdout %q", got, "done\n")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hostrun waited %v for a background child", elapsed)
	}
}

func TestRunReportsDaemonGoingAwayMidCommand(t *testing.T) {
	h := startDaemon(t)
	stdout := &firstLineWriter{line: make(chan string, 1)}
	var stderr bytes.Buffer
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- client.Run(context.Background(), h.transport, []string{"sh", "-c", "echo started; exec sleep 60"}, containerRoot, strings.NewReader(""), stdout, &stderr)
	}()
	<-stdout.line
	h.stop()
	if code := <-codeCh; code != protocol.ExitHostrunError || !strings.HasPrefix(stderr.String(), "hostrun: ") {
		t.Fatalf("got code %d, stderr %q; want %d with a hostrun error", code, stderr.String(), protocol.ExitHostrunError)
	}
}

func TestRunRejectsNonExecutableFile(t *testing.T) {
	h := startDaemon(t)
	if err := os.WriteFile(filepath.Join(h.hostRoot, "script"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := h.run(t, containerRoot, "", "./script")
	if got.code != protocol.ExitRejected || got.stdout != "" || !strings.HasPrefix(got.stderr, "hostrun: ") {
		t.Fatalf("got %+v, want code %d with a hostrun error", got, protocol.ExitRejected)
	}
}

// policy parses a rules config for a test.
func policy(t *testing.T, config string) daemon.Policy {
	t.Helper()
	p, err := rules.Parse([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const echoOnly = `
rules:
  - command: echo
    args: any
  - command: sh
    flags:
      allow: [-c]
`

func TestRunAllowsCommandPermittedByRules(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestRunRejectsCommandDeniedByRules(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	for argv, want := range map[string]string{
		"rm -rf /tmp/x": "hostrun: no rule allows \"rm -rf\"\n",
		"sh -x script":  "hostrun: denied by rule \"sh\": flag -x is not allowed\n",
	} {
		got := h.run(t, containerRoot, "", strings.Fields(argv)...)
		if got != (result{code: protocol.ExitRejected, stderr: want}) {
			t.Errorf("%s: got %+v, want code %d and %q", argv, got, protocol.ExitRejected, want)
		}
	}
}

func TestRunChecksRulesBeforeTheWorkingDirectory(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	got := h.run(t, "/etc", "", "rm", "x")
	if got.code != protocol.ExitRejected || !strings.Contains(got.stderr, "no rule allows") {
		t.Fatalf("got %+v, want the rule denial", got)
	}
}

func TestRunDeniesEverythingWithoutConfig(t *testing.T) {
	p, err := rules.Load(filepath.Join(t.TempDir(), "hostrun.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h := startDaemon(t, p)
	got := h.run(t, containerRoot, "", "echo", "hi")
	if got.code != protocol.ExitRejected || !strings.Contains(got.stderr, "no rules file") {
		t.Fatalf("got %+v, want the missing-config denial", got)
	}
}

func TestRunSetsPhysicalAndLogicalWorkingDirectory(t *testing.T) {
	t.Setenv("PWD", "/daemon/started/elsewhere") // must not leak into the command
	h := startDaemon(t)
	want := filepath.Join(h.hostRoot, "sub")
	if got := h.run(t, containerRoot+"/sub", "", "pwd", "-P"); got != (result{stdout: want + "\n"}) {
		t.Fatalf("physical: got %+v, want %s", got, want)
	}
	// printenv reads the environment as given; a shell would repair a bad
	// PWD by itself and hide the difference.
	if got := h.run(t, containerRoot+"/sub", "", "printenv", "PWD"); got != (result{stdout: want + "\n"}) {
		t.Fatalf("logical: got %+v, want %s", got, want)
	}
}
