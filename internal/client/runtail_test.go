package client_test

import (
	"net"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// runOnFake approves request 1 of the fake daemon on conn once, confirms
// the answer, and runs touch marker as run 1, which prints output and
// ends after 1.5s with exit 0; it returns once the session printed the
// run's summary line and what follows it up to the idle prompt.
func runOnFake(t *testing.T, s *session, conn net.Conn, output ...protocol.RunOutput) string {
	t.Helper()
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 1, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1})
	for _, m := range output {
		m.Run = 1
		sendFrame(t, conn, protocol.FrameRunOutput, m)
	}
	sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: 1, DurationMs: 1500})
	s.waitFor(t, "── run 1 · touch marker · exit 0 · 1.5s · ")
	return s.waitFor(t, "\n> ")
}

// outChunk returns a chunk of a run's stdout.
func outChunk(data string) protocol.RunOutput { return protocol.RunOutput{Data: []byte(data)} }

// errChunk returns a chunk of a run's stderr.
func errChunk(data string) protocol.RunOutput {
	return protocol.RunOutput{Stderr: true, Data: []byte(data)}
}

// The last 3 lines of a run's output follow its summary line, stdout's
// marked │ and stderr's !, in the order they arrived, and a line says
// how many are above them and how to see the run's output.
func TestARunsLastLinesFollowItsSummary(t *testing.T) {
	s, conn := startRunsFake(t)
	got := runOnFake(t, s, conn, outChunk("one\ntwo\n"), errChunk("three\n"), outChunk("four\nfive"))
	if want := "5 lines ──\n! three\n│ four\n│ five\n(2 lines above · out 1)\n> "; got != want {
		t.Fatalf("the session printed %q, want %q", got, want)
	}
}

// tailing returns a session's config that shows n lines of a run's tail.
func tailing(n int) approval.Config { return approval.Config{TailLines: &n} }

// The number of tail lines the session is given sets how many of a run's
// last lines follow its summary, a line saying how many are above them
// when there are more; a session given none shows 3.
func TestTheTailLinesSetHowManyLinesFollowTheSummary(t *testing.T) {
	output := []protocol.RunOutput{outChunk("one\ntwo\n"), errChunk("three\n")}
	for _, tt := range []struct {
		name string
		cfg  approval.Config
		want string
	}{
		{"1", tailing(1), "3 lines ──\n! three\n(2 lines above · out 1)\n> "},
		{"3", tailing(3), "3 lines ──\n│ one\n│ two\n! three\n> "},
		{"5", tailing(5), "3 lines ──\n│ one\n│ two\n! three\n> "},
		{"unset", approval.Config{}, "3 lines ──\n│ one\n│ two\n! three\n> "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFakeWith(t, tt.cfg)
			if got := runOnFake(t, s, conn, output...); got != tt.want {
				t.Fatalf("the session printed %q, want %q", got, tt.want)
			}
		})
	}
}

// With no tail lines a run's summary is printed alone, with no line
// saying what is above, and out still prints the run's output.
func TestNoTailLinesPrintTheSummaryAlone(t *testing.T) {
	s, conn := startRunsFakeWith(t, tailing(0))
	if got, want := runOnFake(t, s, conn, outChunk("one\ntwo\n"), errChunk("three")), "3 lines ──\n> "; got != want {
		t.Fatalf("the session printed %q, want %q", got, want)
	}
	want := "── run 1 · touch marker · exit 0 · 1.5s · 3 lines ──\n│ one\n│ two\n! three\n> "
	if got := s.command(t, "out 1"); got != want {
		t.Fatalf("out 1: the session printed %q, want %q", got, want)
	}
}

// A line that arrives in chunks, the other stream's lines in between, is
// shown whole, where its first chunk arrived; no line follows the tail
// when it holds all the run's lines, and a run that printed nothing has
// its summary alone.
func TestARunsLinesInChunksAreShownWhole(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output []protocol.RunOutput
		want   string
	}{
		{"in chunks", []protocol.RunOutput{outChunk("par"), errChunk("err\n"), outChunk("t"), outChunk("ial\nend\n")}, "3 lines ──\n│ partial\n! err\n│ end\n> "},
		{"empty lines", []protocol.RunOutput{outChunk("\n\n")}, "2 lines ──\n│ \n│ \n> "},
		{"nothing", nil, "0 lines ──\n> "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFake(t)
			if got := runOnFake(t, s, conn, tt.output...); got != tt.want {
				t.Fatalf("the session printed %q, want %q", got, tt.want)
			}
		})
	}
}

// A tail line longer than 200 characters is cut after them, the bytes
// left out counted.
func TestARunsLongLinesAreCut(t *testing.T) {
	s, conn := startRunsFake(t)
	long := strings.Repeat("é", 199) + "xyz"
	got := runOnFake(t, s, conn, outChunk(long+"\n"), errChunk(long))
	cut := strings.Repeat("é", 199) + "x…(+2 bytes)"
	if want := "2 lines ──\n│ " + cut + "\n! " + cut + "\n> "; got != want {
		t.Fatalf("the session printed %q, want %q", got, want)
	}
}

// A tail shows output that cannot steer the terminal: colour sequences
// removed, a line rewritten with \r shown as last written, a line ending
// in \r\n shown whole, every other control character escaped on its own,
// and tabs kept.
func TestARunsTailIsSanitised(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output []protocol.RunOutput
		want   string
	}{
		{"colour", []protocol.RunOutput{outChunk("\x1b[1;31mred\x1b[0m and \x1b[38:5:196mmore\x1b[m\n")}, "red and more"},
		{"kilobytes of colour", []protocol.RunOutput{outChunk(strings.Repeat("\x1b[0m", 1200) + "text\n")}, "text"},
		{"rewritten", []protocol.RunOutput{outChunk("10%\r50%\r100%\n")}, "100%"},
		{"rewritten across chunks", []protocol.RunOutput{outChunk("old\r"), outChunk("new\n")}, "new"},
		{"crlf", []protocol.RunOutput{outChunk("whole\r\n")}, "whole"},
		{"crlf across chunks", []protocol.RunOutput{outChunk("whole\r"), outChunk("\n")}, "whole"},
		{"control", []protocol.RunOutput{outChunk("a\x00b\x1b[2Kc\x7fd\x1b]0;title\x07\n")}, `a\x00b\x1b[2Kc\x7fd\x1b]0;title\a`},
		{"not printable", []protocol.RunOutput{outChunk("zero\u200bwidth \xff\n")}, `zero\u200bwidth \xff`},
		{"tab", []protocol.RunOutput{outChunk("a\tb\n")}, "a\tb"},
		{"quotes and backslashes", []protocol.RunOutput{outChunk(`"a\b"` + "\n")}, `"a\b"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFake(t)
			if got, want := runOnFake(t, s, conn, tt.output...), "1 line ──\n│ "+tt.want+"\n> "; got != want {
				t.Fatalf("the session printed %q, want %q", got, want)
			}
		})
	}
}

// A tail shows a rule's secret values masked, as the container gets them,
// and the container gets the output as without a session.
func TestARunsTailShowsSecretsMasked(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, `
rules:
  - command: printf
    secrets:
      token: {env: TOKEN}
    inject:
      trailing_args: ["${token}"]
    flags:
      allow: []
    positional:
      allow: ["ok"]
`))
	s := h.startSession(t)
	ch := h.ask(t, "printf", `first\n%s\n`)
	s.approveYes(t)
	if got := awaitResult(t, ch); got.code != 0 || got.stdout != "first\n***\n" {
		t.Fatalf("got %+v, want the injected secret printed masked", got)
	}
	got := s.waitForMatch(t, summaryPattern(1, `printf 'first\n%s\n'`, "exit 0", "2 lines")+`.*\n.*\n`)
	if !strings.HasSuffix(got, " lines ──\n│ first\n│ ***\n") || strings.Contains(s.out.String(), testToken) {
		t.Fatalf("the session printed %q, want the secret masked in the tail", got)
	}
}
