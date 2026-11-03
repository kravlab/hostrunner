package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// echoAlone allows echo and nothing else: every other command is asked
// about.
const echoAlone = "rules:\n  - command: echo\n    args: any\n"

// durationPattern matches a duration as a run's summary gives it.
const durationPattern = `(\d+\.\ds|\d+m\d\ds|\d+h\d\dm)`

// A command a human approved is an approved run: the answer line names
// its run, and once it ends the session prints one summary line with the
// command, its exit code, how long it ran and how many lines it printed.
// The container gets the command's output and exit code as without a
// session, and the daemon log gains none of the output.
func TestAnApprovedRunIsSummarised(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoAlone))
	s := h.startSession(t)
	// The output is not in the argv, which the log holds.
	ch := h.ask(t, "sh", "-c", "echo out$((6*7)); echo out$((6*7)); echo err$((6*7)) >&2; exit 3")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	got := awaitResult(t, ch)
	want := result{code: 3, stdout: "out42\nout42\n", stderr: "hostrun: waiting for approval on the host: no rule allows \"sh -c\"\nhostrun: approved on the host: this request only\nerr42\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	s.waitFor(t, "approved once · run 1\n")
	s.waitForMatch(t, regexp.QuoteMeta(`── run 1 · sh -c 'echo out$((6*7)); echo out$((6*7)); echo err$((6*7)) >&2; exit 3' · exit 3 · `)+durationPattern+` · 3 lines ──\n`)
	waitForLog(t, &log, `msg="command finished"`)
	if strings.Contains(log.String(), "out42") || strings.Contains(log.String(), "err42") {
		t.Fatalf("the daemon log holds the command's output:\n%s", log.String())
	}
}

// summaryPattern matches the summary line of run n of argvLine, as the
// session shows the command, ending as outcome says, with lines lines.
func summaryPattern(n int, argvLine, outcome, lines string) string {
	return regexp.QuoteMeta(fmt.Sprintf("── run %d · %s · %s · ", n, argvLine, outcome)) + durationPattern + regexp.QuoteMeta(" · "+lines+" ──\n")
}

// Runs are numbered 1, 2, 3… apart from requests: a refused request gets
// no run.
func TestRunNumbersAreApartFromRequestNumbers(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "refused")
	s.waitFor(t, "── request 1 ──")
	s.waitFor(t, answerPromptText)
	s.refuse(t)
	awaitResult(t, ch)
	for i, name := range []string{"first", "second"} {
		ch := h.ask(t, "touch", name)
		s.waitFor(t, fmt.Sprintf("── request %d ──", i+2))
		s.waitFor(t, answerPromptText)
		s.answer(t, "o")
		if got := awaitResult(t, ch); got.code != 0 {
			t.Fatalf("got %+v, want it approved", got)
		}
		s.waitFor(t, fmt.Sprintf("approved once · run %d\n", i+1))
		s.waitForMatch(t, summaryPattern(i+1, "touch "+name, "exit 0", "0 lines"))
	}
}

// startRunsFake starts a fake daemon that reports approved runs and a
// session serving it, which shows request 1; it returns the session and
// the fake's connection.
func startRunsFake(t *testing.T) (*session, net.Conn) {
	t.Helper()
	socket, served := fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Runs: true})
	s := startSessionOn(t, socket)
	conn := <-served
	s.waitFor(t, answerPromptText)
	return s, conn
}

// answerOnFake types line as the answer to the prompt the session
// serving the fake daemon on conn shows, and returns once the fake has
// read the answer it sends: line "o" approves request 1 once.
func answerOnFake(t *testing.T, s *session, conn net.Conn, line string) {
	t.Helper()
	s.answer(t, line)
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameAnswer {
		t.Fatalf("frame %v, err %v; want the answer", f.Type, err)
	}
}

// sendFrame writes v as a frame of type typ to the session on conn.
func sendFrame(t *testing.T, conn net.Conn, typ protocol.FrameType, v any) {
	t.Helper()
	if err := protocol.WriteJSON(conn, typ, v); err != nil {
		t.Fatal(err)
	}
}

// A fresh approval's answer line names its run, and its summary and tail
// follow, whether the daemon confirms the answer before the run's frames or
// after them: they come from two of its goroutines.
func TestARunIsNumberedWhicheverFrameComesFirst(t *testing.T) {
	for _, answeredFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("answered first %v", answeredFirst), func(t *testing.T) {
			s, conn := startRunsFake(t)
			answerOnFake(t, s, conn, "o")
			answered := func() { sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1}) }
			if answeredFirst {
				answered()
			}
			sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 7, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1})
			sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 7, Data: []byte("a\nb")})
			sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 7, Stderr: true, Data: []byte("c\n")})
			sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: 7, DurationMs: 1500})
			if !answeredFirst {
				answered()
			}
			s.waitFor(t, "approved once · run 1\n")
			s.waitFor(t, "── run 1 · touch marker · exit 0 · 1.5s · 3 lines ──\n│ a\n│ b\n! c\n")
		})
	}
}

// The summary says how the run ended: its exit code, or why it never
// started or was cancelled, how long it took, its lines, and how much of
// its output the session did not get.
func TestARunsSummaryShowsHowItEnded(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		ended  protocol.RunEnded
		want   string
	}{
		{"exited", "", protocol.RunEnded{Code: 2, DurationMs: 12345}, "exit 2 · 12.3s · 0 lines"},
		{"not started", "", protocol.RunEnded{Code: 127, Error: "command ./run.sh not found on the host"}, "not started: command ./run.sh not found on the host · 0.0s · 0 lines"},
		{"client gone", "x\n", protocol.RunEnded{Cancelled: protocol.CancelledClientGone, DurationMs: 187_000}, "cancelled: client gone · 3m07s · 1 line"},
		{"daemon stopping", "", protocol.RunEnded{Cancelled: protocol.CancelledStopping, DurationMs: 3_725_000}, "cancelled: daemon stopping · 1h02m · 0 lines"},
		{"output dropped", "x\ny\n", protocol.RunEnded{Code: 1, DurationMs: 200, Dropped: 4096}, "exit 1 · 0.2s · 2 lines · 4096 bytes not received"},
		{"one byte dropped", "", protocol.RunEnded{DurationMs: 59_990, Dropped: 1}, "exit 0 · 59.9s · 0 lines · 1 byte not received"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFake(t)
			answerOnFake(t, s, conn, "o")
			sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
			sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 1, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1})
			if tt.output != "" {
				sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 1, Data: []byte(tt.output)})
			}
			tt.ended.Run = 1
			sendFrame(t, conn, protocol.FrameRunEnded, tt.ended)
			s.waitFor(t, "\n── run 1 · touch marker · "+tt.want+" ──\n")
		})
	}
}

// A session serving a daemon that does not report approved runs, one
// older than them, keeps serving it, and says it gets no run output.
func TestASessionSaysADaemonWithoutRunsSendsNoRunOutput(t *testing.T) {
	socket, served := fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: "/home/u/app", ContainerWorkspace: containerRoot})
	s := startSessionOn(t, socket)
	conn := <-served
	s.waitFor(t, answerPromptText)
	if out := s.out.String(); !strings.Contains(out, "\nno run output: this daemon does not report approved runs\nwaiting for requests") {
		t.Fatalf("the session printed %q, lacking that it gets no run output", out)
	}
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	if out := s.waitFor(t, "approved once\n"); strings.Contains(out, "run") {
		t.Fatalf("the session printed %q, naming a run", out)
	}
}

// A daemon that reports approved runs says so; the session says nothing
// of run output.
func TestASessionOfADaemonWithRunsSaysNothingOfThem(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	if out := s.out.String(); strings.Contains(out, "no run output") {
		t.Fatalf("the session printed %q", out)
	}
}

// A session that does not ask for approved runs, as one older than them,
// gets no run frames: the next frame after an answer's answers what it
// asks next, and none follows.
func TestRunFramesAreNotSentToAnOlderSession(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	conn := h.serveRaw(t)
	ch := h.ask(t, "sh", "-c", "echo hi")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var req protocol.ApprovalRequest
	if err != nil || f.Type != protocol.FrameApprovalRequest || protocol.DecodeJSON(f, &req) != nil {
		t.Fatalf("frame %v, err %v; want the request", f.Type, err)
	}
	sendFrame(t, conn, protocol.FrameAnswer, protocol.Answer{ID: req.ID, Scope: protocol.ScopeOnce})
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameAnswered {
		t.Fatalf("frame %v, err %v; want the answer confirmed", f.Type, err)
	}
	if got := awaitResult(t, ch); got.code != 0 || got.stdout != "hi\n" {
		t.Fatalf("got %+v, want the approved command's output", got)
	}
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStanding}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameStandingList {
		t.Fatalf("frame %v, err %v; want the standing list", f.Type, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if f, err := protocol.ReadFrame(conn); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("frame %v, err %v; want none", f.Type, err)
	}
}

// Commands the rules allow, with --ask or without, and dry runs are not
// approved runs: the session shows none of them, and numbers the next
// approved run 1.
func TestOnlyApprovedRunsAreShown(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	for _, argv := range [][]string{{"echo", "plain"}, {"--ask", "echo", "asked"}, {"--dry-run", "echo", "probed"}, {"--dry-run", "touch", "probed"}} {
		h.run(t, containerRoot, "", argv...)
	}
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
	s.waitFor(t, "approved once · run 1\n")
	s.waitForMatch(t, summaryPattern(1, "touch marker", "exit 0", "0 lines"))
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(s.out.String(), "── run "); n != 1 {
		t.Fatalf("the session printed %d summaries, want 1:\n%s", n, s.out.String())
	}
}

// The run of an "add a rule" answer is an approved run; later runs the new
// rule allows are not.
func TestAnAddedRulesRunIsShown(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoAlone, false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.addRule(t)
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it run under the new rule", got)
	}
	s.waitFor(t, "approved under the new rule · run 1\n")
	s.waitForMatch(t, summaryPattern(1, "touch marker", "exit 0", "0 lines"))
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != 0 {
		t.Fatalf("got %+v, want the new rule to allow it", got)
	}
	ch = h.ask(t, "touch", "other")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
	s.waitFor(t, "approved once · run 2\n")
}

// A run that never starts after its approval ends with a failure to
// start: a start that fails (its program gone meanwhile), or a check that
// refuses it when it is checked again (its directory gone meanwhile).
func TestARunThatNeverStartsIsSummarised(t *testing.T) {
	for _, tt := range []struct {
		name, cwd, argvLine string
		argv                []string
		remove              string // made before asking, removed before answering
		want                result
		outcome             string
	}{
		{
			name: "program gone", cwd: containerRoot, argv: []string{"./run.sh"}, argvLine: "./run.sh", remove: "run.sh",
			want:    result{code: protocol.ExitNotFound, stderr: "hostrun: waiting for approval on the host: no rule allows \"./run.sh\"\nhostrun: approved on the host: this request only\nhostrun: command ./run.sh not found on the host\n"},
			outcome: "not started: command ./run.sh not found on the host",
		},
		{
			name: "directory gone", cwd: containerRoot + "/gone", argv: []string{"touch", "marker"}, argvLine: "touch marker", remove: "gone",
			want:    result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: approved on the host: this request only\nhostrun: working directory " + containerRoot + "/gone is not available on the host\n"},
			outcome: "not started: working directory " + containerRoot + "/gone is not available on the host",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, echoAlone))
			path := filepath.Join(h.hostRoot, tt.remove)
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.remove == "run.sh" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			s := h.startSession(t)
			ch := make(chan result, 1)
			go func() { ch <- h.run(t, tt.cwd, "", append([]string{"--ask"}, tt.argv...)...) }()
			s.waitFor(t, answerPromptText)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			s.answer(t, "o")
			if got := awaitResult(t, ch); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			s.waitFor(t, "approved once · run 1\n")
			s.waitForMatch(t, summaryPattern(1, tt.argvLine, tt.outcome, "0 lines"))
		})
	}
}

// askStreaming runs hostrun --ask argv on h until ctx ends, and returns
// the first line it prints on stdout and its result: its exit code and
// stderr.
func (h harness) askStreaming(t *testing.T, ctx context.Context, argv ...string) (<-chan string, <-chan result) {
	t.Helper()
	stdout := &firstLineWriter{line: make(chan string, 1)}
	ch := make(chan result, 1)
	go func() {
		var stderr bytes.Buffer
		code := client.Run(ctx, h.transport, append([]string{"--ask"}, argv...), containerRoot, strings.NewReader(""), stdout, &stderr)
		ch <- result{code: code, stderr: stderr.String()}
	}()
	return stdout.line, ch
}

// An approved run cancelled because its client went away says so in its
// summary; the client exits as when interrupted.
func TestARunCancelledForItsClientIsSummarised(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	line, ch := h.askStreaming(t, ctx, "sh", "-c", "echo started; exec sleep 60")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	<-line
	cancel()
	if got := awaitResult(t, ch); got.code != exitInterrupted {
		t.Fatalf("got %+v, want it interrupted", got)
	}
	s.waitFor(t, "approved once · run 1\n")
	s.waitForMatch(t, summaryPattern(1, "sh -c 'echo started; exec sleep 60'", "cancelled: client gone", "1 line"))
}

// An approved run cancelled because the daemon stops says so in its
// summary, before the session says the daemon ended.
func TestARunCancelledForTheDaemonStoppingIsSummarised(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	line, ch := h.askStreaming(t, context.Background(), "sh", "-c", "echo started; exec sleep 60")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	<-line
	h.stop()
	if got := awaitResult(t, ch); got.code != protocol.ExitHostrunError {
		t.Fatalf("got %+v, want the daemon gone", got)
	}
	s.waitFor(t, "approved once · run 1\n")
	s.waitForMatch(t, summaryPattern(1, "sh -c 'echo started; exec sleep 60'", "cancelled: daemon stopping", "1 line"))
	s.waitFor(t, "the daemon ended: waiting for the next one")
}

// pausedOutput is a session's output that the test can stop, as a human
// stopping their terminal (Ctrl+S) does: a write waits while it is
// paused.
type pausedOutput struct {
	*lockedBuffer
	mu      sync.Mutex
	resumed chan struct{} // closed to resume; nil while not paused
	blocked chan struct{} // closed once a write waits for resumed
}

// Write writes p once the output is not paused.
func (o *pausedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	resumed, blocked := o.resumed, o.blocked
	if resumed != nil {
		select {
		case <-blocked:
		default:
			close(blocked)
		}
	}
	o.mu.Unlock()
	if resumed != nil {
		<-resumed
	}
	return o.lockedBuffer.Write(p)
}

// pause stops the output and returns a channel closed once the session
// waits to print, and a function that resumes it.
func (o *pausedOutput) pause() (blocked <-chan struct{}, resume func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resumed, o.blocked = make(chan struct{}), make(chan struct{})
	resumed := o.resumed
	return o.blocked, func() {
		o.mu.Lock()
		o.resumed = nil
		o.mu.Unlock()
		close(resumed)
	}
}

// A session that falls behind, its terminal stopped while it prints a
// request, loses the output of an approved run, and its summary counts
// what it lost; it is not dropped, though it takes no frame for longer
// than a session is given for other frames, and the run's container gets
// all of its output at full speed.
func TestASessionFallingBehindLosesOutputNotItsConnection(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	out := &pausedOutput{lockedBuffer: &lockedBuffer{}}
	s := launchSessionTo(t, approval.Config{Socket: h.approvalSocket}, out.lockedBuffer, out)
	s.waitFor(t, "waiting for requests")
	const size = 4 << 20
	script := fmt.Sprintf("while [ ! -e go ]; do sleep 0.01; done; head -c %d /dev/zero", size)
	flood := h.ask(t, "sh", "-c", script)
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	s.waitFor(t, "approved once · run 1\n")
	s.waitFor(t, "> ")
	blocked, resume := out.pause()
	other := h.ask(t, "touch", "marker") // shown as the output is stopped
	<-blocked
	if err := os.WriteFile(filepath.Join(h.hostRoot, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := awaitResult(t, flood); got.code != 0 || len(got.stdout) != size {
		t.Fatalf("got code %d, %d bytes of stdout, stderr %q; want all %d", got.code, len(got.stdout), got.stderr, size)
	}
	time.Sleep(2500 * time.Millisecond) // longer than a session is given to take another frame
	resume()
	// The summary waits while the other request is shown: it follows the
	// answer's line.
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	s.waitFor(t, "approved once · run 2\n")
	s.waitForMatch(t, regexp.QuoteMeta("── run 1 · sh -c '"+script+"' · exit 0 · ")+durationPattern+` · \d+ lines? · \d+ bytes not received ──\n`)
	if got := awaitResult(t, other); got.code != 0 {
		t.Fatalf("got %+v, want the session to serve the daemon still", got)
	}
}
