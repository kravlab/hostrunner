package client_test

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// command types line at the idle prompt and returns what the session
// printed for it, up to the idle prompt shown again.
func (s *session) command(t *testing.T, line string) string {
	t.Helper()
	s.answer(t, line)
	return s.waitFor(t, "\n"+idlePromptText)
}

// syncIdle returns once the session serving the fake daemon on conn, at
// the idle prompt, has handled every frame sent to it before: it sends a
// reread of the rules, a notice printed at once, and waits for it.
func syncIdle(t *testing.T, s *session, conn net.Conn) {
	t.Helper()
	sendFrame(t, conn, protocol.FrameRulesReread, rulesReread)
	s.waitFor(t, rulesRereadText+idlePromptText)
}

// numbered returns n lines of size bytes each, newline included, every
// one starting with its number, from first on, in three digits, then
// fill.
func numbered(first, n, size int, fill string) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("%03d", first+i) + strings.Repeat(fill, size-4) + "\n"
	}
	return lines
}

// shown returns lines as out shows them, of stdout.
func shown(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("│ " + l)
	}
	return b.String()
}

// chunks returns lines as chunks of stdout, one each.
func chunks(lines []string) []protocol.RunOutput {
	out := make([]protocol.RunOutput, len(lines))
	for i, l := range lines {
		out[i] = outChunk(l)
	}
	return out
}

// The idle prompt's command list names out and runs.
func TestTheIdlePromptNamesOutAndRuns(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	want := "waiting for requests · commands: standing, revoke <n>, out [n], runs, daemons, quit\n"
	if out := s.out.String(); !strings.Contains(out, want) {
		t.Fatalf("the session printed %q, want %q", out, want)
	}
}

// Before any run, out and runs say there is none; out with an argument
// that is not a number prints its usage, and out N for a run never given
// says there is no such run.
func TestOutAndRunsBeforeAnyRun(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	s.waitFor(t, idlePromptText)
	for _, tt := range []struct{ line, want string }{
		{"out", "no runs yet\n> "},
		{"runs", "no runs yet\n> "},
		{"out x", "usage: out [n], n from runs\n> "},
		{"out 1 2", "usage: out [n], n from runs\n> "},
		{"out -1", "usage: out [n], n from runs\n> "},
		{"out 5", "run 5: no such run\n> "},
		{"out 0", "run 0: no such run\n> "},
		{"out 99999999999999999999", "run 99999999999999999999: no such run\n> "},
	} {
		if got := s.command(t, tt.line); got != tt.want {
			t.Fatalf("%s: the session printed %q, want %q", tt.line, got, tt.want)
		}
	}
}

// out prints an approved run's output, as the container got it, below
// its summary line.
func TestOutPrintsAnApprovedRunsOutput(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	ch := h.ask(t, "sh", "-c", "echo one; echo two")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved", got)
	}
	summary := summaryPattern(1, "sh -c 'echo one; echo two'", "exit 0", "2 lines")
	s.waitForMatch(t, summary)
	s.waitFor(t, idlePromptText)
	for _, line := range []string{"out", "out 1"} {
		s.answer(t, line)
		if got := s.waitForMatch(t, summary+regexp.QuoteMeta("│ one\n│ two\n> ")); !regexp.MustCompile("^" + summary).MatchString(got) {
			t.Fatalf("%s: the session printed %q", line, got)
		}
	}
}

// out prints the last run's kept output, out N run N's, of both streams in
// the order their lines began, marked as the tail marks them.
func TestOutPrintsARunsKeptOutput(t *testing.T) {
	s, conn := startRunsFake(t)
	runOnFake(t, s, conn, outChunk("one\ntwo\n"), errChunk("thr"), outChunk("four\n"), errChunk("ee\n"), outChunk("five"))
	want := "── run 1 · touch marker · exit 0 · 1.5s · 5 lines ──\n│ one\n│ two\n! three\n│ four\n│ five\n> "
	for _, line := range []string{"out", "out 1", "out  1"} {
		if got := s.command(t, line); got != want {
			t.Fatalf("%s: the session printed %q, want %q", line, got, want)
		}
	}
}

// out shows each line whole, however long, sanitised as the tail is.
func TestOutShowsLinesWholeAndSanitised(t *testing.T) {
	s, conn := startRunsFake(t)
	long := strings.Repeat("é", 300)
	runOnFake(t, s, conn, outChunk(long+"\x1b[31mred\x1b[0m\a\n"), errChunk("old\rnew\r\n"))
	want := "── run 1 · touch marker · exit 0 · 1.5s · 2 lines ──\n│ " + long + `red\a` + "\n! new\n> "
	if got := s.command(t, "out 1"); got != want {
		t.Fatalf("the session printed %q, want %q", got, want)
	}
}

// out and runs show a run still going as running, with the output that
// has arrived so far; once it ends, its summary.
func TestOutAndRunsShowARunStillGoing(t *testing.T) {
	s, conn := startRunsFake(t)
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	s.waitFor(t, "approved once · run 1\n")
	sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 1, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1})
	sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 1, Data: []byte("a\nb")})
	syncIdle(t, s, conn)
	running := `^── run 1 · touch marker · running · ` + durationPattern + ` · 2 lines ──\n`
	for _, tt := range []struct{ line, after string }{
		{"runs", "> "},
		{"out", "│ a\n│ b\n> "},
		{"out 1", "│ a\n│ b\n> "},
	} {
		if got := s.command(t, tt.line); !regexp.MustCompile(running + regexp.QuoteMeta(tt.after) + "$").MatchString(got) {
			t.Fatalf("%s: the session printed %q, want run 1 running", tt.line, got)
		}
	}
	sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: 1, DurationMs: 1500})
	s.waitFor(t, "│ b\n"+idlePromptText)
	if got, want := s.command(t, "runs"), "── run 1 · touch marker · exit 0 · 1.5s · 2 lines ──\n> "; got != want {
		t.Fatalf("the session printed %q, want %q", got, want)
	}
}

// out says how much of a run's output it dropped beyond the 64 KiB it
// keeps, a line the cut falls in counted whole, of either stream, and
// shows the whole lines kept.
func TestOutSaysHowMuchOutputWasDropped(t *testing.T) {
	lines := numbered(1, 70, 1000, "x")
	stdout := numbered(1, 64, 1024, "o")
	for _, tt := range []struct {
		name   string
		output []protocol.RunOutput
		want   string
	}{
		{"cut in a line", chunks(lines), "70 lines ──\n(5000 bytes dropped: out keeps a run's last 64 KiB)\n" + shown(lines[5:])},
		{"cut at a line's end", append([]protocol.RunOutput{outChunk("\n")}, chunks(stdout)...), "65 lines ──\n(1 byte dropped: out keeps a run's last 64 KiB)\n" + shown(stdout)},
		{"streams cut apart", append(append([]protocol.RunOutput{errChunk(strings.Repeat("e", 1000))}, chunks(stdout)...), errChunk("tail\n")), "65 lines ──\n(2029 bytes dropped: out keeps a run's last 64 KiB)\n" + shown(stdout[1:])},
		{"one line over 64 KiB", []protocol.RunOutput{outChunk(strings.Repeat("y", 70000) + "\n"), outChunk("last\n")}, "2 lines ──\n(70001 bytes dropped: out keeps a run's last 64 KiB)\n│ last\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFake(t)
			runOnFake(t, s, conn, tt.output...)
			if got, want := s.command(t, "out 1"), "── run 1 · touch marker · exit 0 · 1.5s · "+tt.want+"> "; got != want {
				t.Fatalf("the session printed %q, want %q", got, want)
			}
		})
	}
}

// The session keeps the last 20 runs: runs lists them, oldest first, and
// out N for an older one says its output was dropped.
func TestOutAndRunsKeepTheLast20Runs(t *testing.T) {
	s, conn := startRunsFake(t)
	runOnFake(t, s, conn, outChunk("r1\n"))
	summaryOf := func(n int) string {
		return fmt.Sprintf("── run %d · echo %d · exit 0 · 0.1s · 1 line ──\n", n, n)
	}
	for n := 2; n <= 22; n++ {
		run := uint64(100 + n)
		sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: run, Argv: []string{"echo", fmt.Sprint(n)}, Cwd: containerRoot, Approval: protocol.ScopeStanding})
		sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: run, Data: []byte(fmt.Sprintf("r%d\n", n))})
		sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: run, DurationMs: 100})
	}
	s.waitFor(t, summaryOf(22)+"│ r22\n"+idlePromptText)
	var list strings.Builder
	for n := 3; n <= 22; n++ {
		list.WriteString(summaryOf(n))
	}
	for _, tt := range []struct{ line, want string }{
		{"runs", list.String() + "> "},
		{"out 1", "run 1: output dropped\n> "},
		{"out 2", "run 2: output dropped\n> "},
		{"out 3", summaryOf(3) + "│ r3\n> "},
		{"out", summaryOf(22) + "│ r22\n> "},
		{"out 23", "run 23: no such run\n> "},
	} {
		if got := s.command(t, tt.line); got != tt.want {
			t.Fatalf("%s: the session printed %q, want %q", tt.line, got, tt.want)
		}
	}
}

// A run whose end never came, the session having stopped serving its
// daemon, is listed as such, with the output it got.
func TestRunsSaysARunsEndWasNotReceived(t *testing.T) {
	socket, served := fakeDaemonsWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Runs: true})
	s := startSessionOn(t, socket)
	conn := <-served
	s.waitFor(t, answerPromptText)
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 1, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1})
	sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 1, Data: []byte("a\n")})
	conn.Close()
	s.waitFor(t, "the daemon ended: waiting for the next one\n")
	var next net.Conn
	select {
	case next = <-served:
	case <-time.After(listWait):
		t.Fatal("the session did not serve the next daemon")
	}
	s.waitFor(t, answerPromptText)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	answerOnFake(t, s, next, "")
	sendFrame(t, next, protocol.FrameAnswered, protocol.Answered{ID: 1})
	s.waitFor(t, "refused\n"+idlePromptText)
	ended := `^── run 1 · touch marker · end not received · ` + durationPattern + ` · 1 line ──\n`
	for _, tt := range []struct{ line, after string }{
		{"runs", "> "},
		{"out 1", "│ a\n> "},
	} {
		if got := s.command(t, tt.line); !regexp.MustCompile(ended + regexp.QuoteMeta(tt.after) + "$").MatchString(got) {
			t.Fatalf("%s: the session printed %q, want run 1's end not received", tt.line, got)
		}
	}
}

// out shows the output a run got before it was numbered, its answer not
// yet confirmed.
func TestOutKeepsOutputThatCameBeforeTheRunWasNumbered(t *testing.T) {
	s, conn := startRunsFake(t)
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 7, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1})
	sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 7, Data: []byte("early\n")})
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 7, Stderr: true, Data: []byte("late\n")})
	sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: 7, DurationMs: 1500})
	s.waitFor(t, "approved once · run 1\n")
	s.waitFor(t, "! late\n"+idlePromptText)
	if got, want := s.command(t, "out 1"), "── run 1 · touch marker · exit 0 · 1.5s · 2 lines ──\n│ early\n! late\n> "; got != want {
		t.Fatalf("the session printed %q, want %q", got, want)
	}
}

// A number given to an answer whose run never started has no run: out
// says so, until the number is in the range of runs no longer kept, when
// it says that output was dropped.
func TestOutOfANumberWhoseRunNeverStarted(t *testing.T) {
	s, conn := startRunsFake(t)
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	s.waitFor(t, "approved once · run 1\n"+idlePromptText)
	repeat := func(n int) {
		run := uint64(100 + n)
		sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: run, Argv: []string{"echo", fmt.Sprint(n)}, Cwd: containerRoot, Approval: protocol.ScopeStanding})
		sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: run})
		s.waitFor(t, fmt.Sprintf("── run %d · echo %d · exit 0 · 0.0s · 0 lines ──\n%s", n, n, idlePromptText))
	}
	for n := 2; n <= 21; n++ {
		repeat(n)
	}
	if got, want := s.command(t, "out 1"), "run 1: no such run\n> "; got != want {
		t.Fatalf("with 20 runs kept, the session printed %q, want %q", got, want)
	}
	repeat(22)
	for _, tt := range []struct{ line, want string }{
		{"out 1", "run 1: output dropped\n> "},
		{"out 2", "run 2: output dropped\n> "},
		{"out 3", "── run 3 · echo 3 · exit 0 · 0.0s · 0 lines ──\n> "},
	} {
		if got := s.command(t, tt.line); got != tt.want {
			t.Fatalf("%s: the session printed %q, want %q", tt.line, got, tt.want)
		}
	}
}
