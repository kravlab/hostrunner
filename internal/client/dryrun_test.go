package client_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// assertNotRun fails the test if the command that creates marker in the
// host workspace ran.
func assertNotRun(t *testing.T, h harness, marker string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(h.hostRoot, marker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the command ran: %s exists (stat error %v)", marker, err)
	}
}

func TestDryRunReportsTheAllowingRuleWithoutRunning(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	got := h.run(t, containerRoot, "", "--dry-run", "sh", "-c", "touch marker")
	if want := (result{stderr: "hostrun: dry run: allowed by rule \"sh\"\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertNotRun(t, h, "marker")
}

// A dry run is refused with the code and message a run of the same command
// gets.
func TestDryRunIsRefusedAsTheRunWouldBe(t *testing.T) {
	const config = `
rules:
  - command: sh
    flags:
      allow: [-c]
  - command: cat
    positional:
      path: open
`
	h := startDaemon(t, policy(t, config))
	for _, tc := range []struct {
		name string
		cwd  string
		argv []string
	}{
		{"denied by the rules", containerRoot, []string{"sh", "-x", "-c", "touch marker"}},
		{"no rule", containerRoot, []string{"rm", "marker"}},
		{"cwd outside the workspace", "/etc", []string{"sh", "-c", "touch marker"}},
		{"cwd missing", containerRoot + "/missing", []string{"sh", "-c", "touch marker"}},
		{"not a workspace file", containerRoot, []string{"cat", "missing.txt"}},
		{"absolute path", containerRoot, []string{"cat", "/etc/hostname"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			real := h.run(t, tc.cwd, "", tc.argv...)
			if real.code != protocol.ExitRejected {
				t.Fatalf("the run was not refused: %+v", real)
			}
			got := h.run(t, tc.cwd, "", append([]string{"--dry-run"}, tc.argv...)...)
			if got != real {
				t.Fatalf("dry run %+v, want the run's %+v", got, real)
			}
			if strings.Contains(got.stderr, h.hostRoot) {
				t.Fatalf("message reveals a host path: %q", got.stderr)
			}
		})
	}
	assertNotRun(t, h, "marker")
}

// A path: open argument is checked like in a run, and the file is not
// handed to anything: the script it names does not run.
func TestDryRunChecksWorkspaceFiles(t *testing.T) {
	h := startDaemon(t, policy(t, "rules:\n  - command: sh\n    positional:\n      path: open\n"))
	if err := os.WriteFile(filepath.Join(h.hostRoot, "script.sh"), []byte("touch marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := h.run(t, containerRoot, "", "--dry-run", "sh", "script.sh")
	if want := (result{stderr: "hostrun: dry run: allowed by rule \"sh\"\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertNotRun(t, h, "marker")
	// The same command runs the script, so the marker proves something.
	if got := h.run(t, containerRoot, "", "sh", "script.sh"); got.code != 0 {
		t.Fatalf("the run failed: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatalf("the run did not create the marker: %v", err)
	}
}

func TestDryRunRequiresACommand(t *testing.T) {
	h := startDaemon(t)
	got := h.run(t, containerRoot, "", "--dry-run")
	want := result{code: protocol.ExitHostrunError, stderr: "hostrun: usage: hostrun [--dry-run] <command> [args...]\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Only hostrun's first argument can make a dry run; anywhere else
// --dry-run belongs to the command.
func TestDryRunFlagAfterTheProgramReachesTheCommand(t *testing.T) {
	h := startDaemon(t)
	if got := h.run(t, containerRoot, "", "echo", "--dry-run"); got != (result{stdout: "--dry-run\n"}) {
		t.Fatalf("got %+v, want echo to print --dry-run", got)
	}
}

// readSpy is stdin that records whether it was read.
type readSpy struct{ read atomic.Bool }

func (r *readSpy) Read([]byte) (int, error) {
	r.read.Store(true)
	return 0, io.EOF
}

func TestDryRunDoesNotReadStdin(t *testing.T) {
	h := startDaemon(t)
	var stdout, stderr bytes.Buffer
	stdin := &readSpy{}
	code := client.Run(context.Background(), h.transport, []string{"--dry-run", "cat"}, containerRoot, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code %d, stderr %q; want 0", code, stderr.String())
	}
	if stdin.read.Load() {
		t.Fatal("a dry run read stdin")
	}
}

// lockedBuffer is a log destination the daemon's goroutines can write to
// while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The daemon log records what the container probed, allowed or not.
func TestDryRunIsLogged(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	if got := h.run(t, containerRoot, "", "--dry-run", "echo", "hi"); got.code != 0 {
		t.Fatalf("allowed dry run: %+v", got)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "rm", "x"); got.code != protocol.ExitRejected {
		t.Fatalf("denied dry run: %+v", got)
	}
	lines := strings.Split(log.String(), "\n")
	for _, want := range []struct{ msg, attr string }{
		{`msg="dry run allowed"`, `rule=echo`},
		{`msg="request rejected"`, `code=126`},
	} {
		if !slices.ContainsFunc(lines, func(l string) bool {
			return strings.Contains(l, want.msg) && strings.Contains(l, want.attr) && strings.Contains(l, "dry_run=true")
		}) {
			t.Errorf("no log line with %s, %s and dry_run=true in:\n%s", want.msg, want.attr, log.String())
		}
	}
}

// A daemon from before dry runs knows no frame type after FrameArmed. It
// rejects a dry run the way it rejects any unknown frame, and hostrun
// reports that instead of the command having run.
func TestDryRunAgainstADaemonWithoutDryRunsFails(t *testing.T) {
	tr := transport.Unix{Path: filepath.Join(t.TempDir(), "old.sock")}
	l, err := tr.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var header [1]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		if header[0] > byte(protocol.FrameArmed) {
			_ = protocol.WriteJSON(conn, protocol.FrameError, protocol.Error{
				Code:    protocol.ExitHostrunError,
				Message: "read request: protocol: unknown frame type: " + strconv.Itoa(int(header[0])),
			})
		}
	}()
	var stdout, stderr bytes.Buffer
	code := client.Run(context.Background(), tr, []string{"--dry-run", "echo", "hi"}, containerRoot, strings.NewReader(""), &stdout, &stderr)
	if code != protocol.ExitHostrunError || !strings.HasPrefix(stderr.String(), "hostrun: read request: protocol: unknown frame type") {
		t.Fatalf("code %d, stderr %q; want %d and the old daemon's refusal", code, stderr.String(), protocol.ExitHostrunError)
	}
}

// A dry run looks the program up as starting it would, so a program the
// host lacks or cannot execute is refused with the run's code and message.
func TestDryRunChecksTheProgram(t *testing.T) {
	h := startDaemon(t)
	if err := os.WriteFile(filepath.Join(h.hostRoot, "script"), []byte("#!/bin/sh\ntouch marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		argv []string
		code int
	}{
		{"not on the host", []string{"hostrunner-no-such-command"}, protocol.ExitNotFound},
		{"missing absolute path", []string{"/no/such/program"}, protocol.ExitNotFound},
		{"not executable", []string{"./script"}, protocol.ExitRejected},
		{"a directory", []string{"./sub"}, protocol.ExitRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			real := h.run(t, containerRoot, "", tc.argv...)
			if real.code != tc.code {
				t.Fatalf("the run got %+v, want code %d", real, tc.code)
			}
			got := h.run(t, containerRoot, "", append([]string{"--dry-run"}, tc.argv...)...)
			if got != real {
				t.Fatalf("dry run %+v, want the run's %+v", got, real)
			}
			if strings.Contains(got.stderr, h.hostRoot) {
				t.Fatalf("message reveals a host path: %q", got.stderr)
			}
		})
	}
	assertNotRun(t, h, "marker")
}

// A dry run refused before its checks, for a malformed request, is logged
// as a dry run too.
func TestMalformedDryRunIsLoggedAsADryRun(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)))
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := protocol.Request{Version: protocol.Version, Cwd: containerRoot}
	if err := protocol.WriteJSON(conn, protocol.FrameDryRun, req); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameError {
		t.Fatalf("got frame %v, err %v; want FrameError", f.Type, err)
	}
	if !strings.Contains(log.String(), "dry_run=true") {
		t.Fatalf("the refusal is not logged as a dry run:\n%s", log.String())
	}
}
