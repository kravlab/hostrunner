package client_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// startLinePattern matches a start line the session prints for a run,
// one that no answer line names (see TestARepeatIsShownWithItsApproval).
var startLinePattern = regexp.MustCompile(`(?m)^run \d+ · `)

// assertStartLines fails t unless the session printed n start lines. The
// caller has waited for a line the session prints after any start line it
// would print: a later run's start line or summary, or a later answer's
// line, which follows the run frames sent before it.
func assertStartLines(t *testing.T, s *session, n int) {
	t.Helper()
	if got := len(startLinePattern.FindAllString(s.out.String(), -1)); got != n {
		t.Fatalf("the session printed %d start lines, want %d:\n%s", got, n, s.out.String())
	}
}

// A repeat a standing or session approval lets through, with --ask or
// without, is an approved run: the session prints a start line naming the
// approval, the command shown as on the cmd line, then its summary. The
// fresh approval that granted it prints no start line: its answer line
// names its run.
func TestARepeatIsShownWithItsApproval(t *testing.T) {
	for _, tt := range []struct {
		name   string
		answer grantAnswer
		by     string
	}{
		{"standing", standingAnswer, "by standing approval"},
		{"session", sessionAnswer, "by session approval"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, echoAlone))
			s := h.startSession(t)
			// The command is shown escaped, as on the cmd line.
			argv := []string{"sh", "-c", "echo $((6*7))", "\x1b]0;owned\a"}
			const argvLine = `sh -c 'echo $((6*7))' $'\x1b]0;owned\x07'`
			// grantAs waits for the grant's summary, which follows its
			// start line, if any.
			h.grantAs(t, s, tt.answer, false, argv...)
			assertStartLines(t, s, 0)
			for i, run := range [][]string{argv, append([]string{"--ask"}, argv...)} {
				if got := h.run(t, containerRoot, "", run...); got != (result{stdout: "42\n"}) {
					t.Fatalf("%v: got %+v, want it to run without asking", run, got)
				}
				// The grant is run 1: the repeats are runs 2 and 3.
				n := i + 2
				s.waitFor(t, fmt.Sprintf("\nrun %d · %s · %s\n", n, argvLine, tt.by))
				s.waitForMatch(t, summaryPattern(n, argvLine, "exit 0", "1 line"))
			}
			assertStartLines(t, s, 2)
		})
	}
}

// A dry run of a repeat is no approved run: the session prints nothing
// of it, and numbers the next repeat as if it had not been. The next
// repeat's summary follows whatever the dry run would have printed.
func TestADryRunOfARepeatIsNotShown(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "marker")
	if got := h.run(t, containerRoot, "", "--dry-run", "touch", "marker"); got != (result{stderr: "hostrun: dry run: allowed by a standing approval\n"}) {
		t.Fatalf("dry run: got %+v", got)
	}
	h.run(t, containerRoot, "", "touch", "marker")
	s.waitFor(t, "\nrun 2 · touch marker · by standing approval\n")
	s.waitForMatch(t, summaryPattern(2, "touch marker", "exit 0", "0 lines"))
	assertStartLines(t, s, 1)
}

// A repeat a rule added since allows is let through by the rule, not by
// its approval: the session prints nothing of it, with --ask or without.
// A once approval asked after them shows that those runs took no run
// number: it is run 3, after the grant and the run under the new rule,
// and its summary follows whatever they would have printed.
func TestARepeatARuleNowAllowsIsNotShown(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoAlone, false)
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "marker")
	ch := h.ask(t, "touch", "-a", "marker")
	s.addRule(t)
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it run under the new rule", got)
	}
	s.waitFor(t, "approved under the new rule · run 2\n")
	s.waitForMatch(t, summaryPattern(2, "touch -a marker", "exit 0", "0 lines"))
	for _, argv := range [][]string{{"touch", "marker"}, {"--ask", "touch", "marker"}} {
		if got := h.run(t, containerRoot, "", argv...); got != (result{}) {
			t.Fatalf("%v: got %+v, want the new rule to allow it", argv, got)
		}
	}
	ch = h.ask(t, "touch", "other")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
	s.waitFor(t, "approved once · run 3\n")
	s.waitForMatch(t, summaryPattern(3, "touch other", "exit 0", "0 lines"))
	assertStartLines(t, s, 0)
}

// A command the rules allow whose argument fails its path check runs only
// as the approval held for it passes that failure as given: the approval
// let it through, so the repeat is shown as its approval's.
func TestARepeatWhoseApprovalPassesAPathFailureIsShown(t *testing.T) {
	h := startDaemon(t, policy(t, `
rules:
  - command: echo
    positional:
      path: open
`))
	s := h.startSession(t)
	// An absolute path fails the check wherever it leads.
	if got := h.grantStanding(t, s, true, "echo", "/srv/example"); got.stdout != "/srv/example\n" {
		t.Fatalf("grant: got %+v, want the path passed as given", got)
	}
	if got := h.run(t, containerRoot, "", "echo", "/srv/example"); got != (result{stdout: "/srv/example\n"}) {
		t.Fatalf("got %+v, want the approval to let it through", got)
	}
	s.waitFor(t, "\nrun 2 · echo /srv/example · by standing approval\n")
	s.waitForMatch(t, summaryPattern(2, "echo /srv/example", "exit 0", "1 line"))
}

// A repeat cancelled because its client went away says so in its summary,
// after its start line.
func TestARepeatCancelledForItsClientIsSummarised(t *testing.T) {
	h := startDaemon(t, policy(t, echoAlone))
	// The grant ends at once; the repeat, once quick is gone, runs on.
	quick := filepath.Join(h.hostRoot, "quick")
	if err := os.WriteFile(quick, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	argv := []string{"sh", "-c", "echo started; test -e quick || exec sleep 60"}
	const argvLine = "sh -c 'echo started; test -e quick || exec sleep 60'"
	s := h.startSession(t)
	h.grantStanding(t, s, false, argv...)
	if err := os.Remove(quick); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	line, ch := h.askStreaming(t, ctx, argv...)
	<-line
	cancel()
	if got := awaitResult(t, ch); got.code != exitInterrupted {
		t.Fatalf("got %+v, want it interrupted", got)
	}
	s.waitFor(t, "\nrun 2 · "+argvLine+" · by standing approval\n")
	s.waitForMatch(t, summaryPattern(2, argvLine, "cancelled: client gone", "1 line"))
}

// A repeat of an approval kind the session does not know is shown with
// the kind as the daemon names it. The fake's request is withdrawn first:
// a start line waits while a request is shown.
func TestARepeatOfAnUnknownApprovalKindIsShownByItsKind(t *testing.T) {
	s, conn := startRunsFake(t)
	sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnByClient})
	s.waitFor(t, "request 1 withdrawn\n> ")
	sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 9, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: "rulebook"})
	s.waitFor(t, "\nrun 1 · touch marker · by rulebook\n")
}
