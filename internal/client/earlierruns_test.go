package client_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// goingScript prints a line, then waits until the file go lies in its
// directory, then prints another and exits 0: a run a test keeps going
// while a session starts serving its daemon. goingLine is its command as
// the session shows it.
const (
	goingScript = "echo before; until [ -e go ]; do sleep 0.01; done; echo after"
	goingLine   = "sh -c '" + goingScript + "'"
)

// letGo lets the runs of goingScript in h's workspace finish, and those
// started later run through.
func (h harness) letGo(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.hostRoot, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// holdGoing makes the runs of goingScript in h's workspace started from
// now on wait again.
func (h harness) holdGoing(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(h.hostRoot, "go")); err != nil {
		t.Fatal(err)
	}
}

// earlierReport matches the report of run n of goingScript, picked up
// after it printed its first line: exit 0, the one line it printed since,
// marked as missing its earlier output, then that line as its tail.
func earlierReport(n int) string {
	return summaryPattern(n, goingLine, "exit 0", "1 line · earlier output not received") + regexp.QuoteMeta("│ after\n")
}

// waitForOutput waits until out n, typed at the idle prompt, shows text
// among the output the session keeps of run n: the session got it.
func (s *session) waitForOutput(t *testing.T, n int, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.command(t, fmt.Sprintf("out %d", n)), text) {
		if time.Now().After(deadline) {
			t.Fatalf("out %d never showed %q", n, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startGoing runs goingScript with --ask on h in the background, and
// returns the first line it prints and its result, to come.
func (h harness) startGoing(t *testing.T) (<-chan string, <-chan result) {
	t.Helper()
	return h.askStreaming(t, context.Background(), "sh", "-c", goingScript)
}

// waitStarted waits for line, the first line of a run of goingScript.
func waitStarted(t *testing.T, line <-chan string) {
	t.Helper()
	select {
	case got := <-line:
		if got != "before" {
			t.Fatalf("the run printed %q first, want before", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not print its first line")
	}
}

// awaitGoing waits for the run of goingScript started with startGoing,
// let go, to exit 0.
func awaitGoing(t *testing.T, ch <-chan result) {
	t.Helper()
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want exit 0", got)
	}
}

// answerWith returns what answers the shown request with key.
func answerWith(key string) func(*testing.T, *session) {
	return func(t *testing.T, s *session) {
		t.Helper()
		s.waitFor(t, answerPromptText)
		s.answer(t, key)
	}
}

// A session that takes a daemon over picks up the approved run going,
// whichever answer approved it: it prints the run's start line, numbered
// on its own counter, naming that answer and marked as started before
// it, before the requests passed on to it; the run's end follows, its
// summary saying the output before the take over was not received, its
// tail the output since. The session taken over ends as it always has.
func TestATakeOverPicksUpTheApprovedRunGoing(t *testing.T) {
	for _, tt := range []struct {
		name    string
		answer  func(*testing.T, *session)
		outcome string
		want    string
	}{
		{"once", answerWith("o"), "approved once", "approved once"},
		{"session", answerWith("s"), "approved for this session (until it ends)", "approved for a session"},
		{"standing", answerWith("d"), "approved as standing (until the daemon stops)", "approved as standing"},
		{"add a rule", func(t *testing.T, s *session) { s.addRule(t) }, "approved under the new rule", "approved under a new rule"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoAlone, false)
			first := h.startSession(t)
			line, going := h.startGoing(t)
			tt.answer(t, first)
			first.waitFor(t, tt.outcome+" · run 1\n"+idlePromptText)
			waitStarted(t, line)
			first.waitForOutput(t, 1, "│ before\n")
			waiting := h.ask(t, "touch", "marker")
			first.waitFor(t, answerPromptText)

			second := launchSession(t, approval.Config{Socket: h.approvalSocket})
			second.waitFor(t, takeOverPromptText)
			second.answer(t, "y")
			first.waitFor(t, "taken over by another session\n")
			waitEnded(t, first)
			if first.err != nil {
				t.Fatalf("the session taken over ended with %v, want none", first.err)
			}
			second.waitFor(t, "\nrun 1 · "+goingLine+" · "+tt.want+" · started before this session\n")
			second.waitFor(t, "── request 2 ──")
			second.waitFor(t, answerPromptText)
			h.letGo(t)
			awaitGoing(t, going)
			// The summary waits while request 2 is shown.
			second.answer(t, "o")
			second.waitFor(t, "approved once · run 2\n")
			second.waitForMatch(t, earlierReport(1))
			if got := awaitResult(t, waiting); got.code != 0 {
				t.Fatalf("got %+v, want it approved by the new session", got)
			}
		})
	}
}

// A session that starts serving a daemon nobody serves, or serves it
// again, picks up the approved runs going alike, whichever approval let
// them through: a repeat that started while nobody served the daemon, and
// a run the session approved itself before it left the daemon for the
// list, which it numbers anew, its first number's end not received. A
// repeat of a session approval goes on after a take over ends the
// approval, and is picked up too.
func TestServingADaemonPicksUpTheApprovedRunsGoing(t *testing.T) {
	t.Run("a repeat nobody served", func(t *testing.T) {
		h := startDaemon(t, policy(t, echoAlone))
		first := h.startSession(t)
		h.letGo(t)
		h.grantStanding(t, first, false, "sh", "-c", goingScript)
		h.holdGoing(t)
		first.answer(t, "quit")
		waitEnded(t, first)
		line, going := h.startGoing(t)
		waitStarted(t, line)

		s := h.startSession(t)
		s.waitFor(t, "\nrun 1 · "+goingLine+" · by standing approval · started before this session\n")
		h.letGo(t)
		awaitGoing(t, going)
		s.waitForMatch(t, earlierReport(1))
	})

	t.Run("a run of its own after daemons", func(t *testing.T) {
		base, root := newBase(t)
		h := startHarnessAt(t, slog.New(slog.DiscardHandler), base, func(string) (daemon.Policy, []daemon.Option) {
			return policy(t, echoAlone), nil
		})
		s := startSessionWith(t, approval.Config{Root: root, Cwd: h.hostRoot})
		s.waitForListed(t)
		s.answer(t, "")
		s.waitFor(t, "serving "+h.hostRoot)
		line, going := h.startGoing(t)
		s.waitFor(t, answerPromptText)
		s.answer(t, "o")
		s.waitFor(t, "approved once · run 1\n"+idlePromptText)
		waitStarted(t, line)
		s.waitForOutput(t, 1, "│ before\n")
		s.answer(t, "daemons")
		s.waitForListed(t)
		s.answer(t, "")
		s.waitFor(t, "serving "+h.hostRoot)
		s.waitFor(t, "\nrun 2 · "+goingLine+" · approved once · started before this session\n")
		h.letGo(t)
		awaitGoing(t, going)
		s.waitForMatch(t, earlierReport(2)+regexp.QuoteMeta(idlePromptText))
		want := regexp.QuoteMeta("── run 1 · "+goingLine+" · end not received · ") + durationPattern + regexp.QuoteMeta(" · 1 line ──\n") +
			regexp.QuoteMeta("── run 2 · "+goingLine+" · exit 0 · ") + durationPattern + regexp.QuoteMeta(" · 1 line · earlier output not received ──\n> ")
		if got := s.command(t, "runs"); !regexp.MustCompile("^" + want + "$").MatchString(got) {
			t.Fatalf("runs printed %q, want run 1's end not received and run 2 ended", got)
		}
	})

	t.Run("a session approval's repeat after a take over", func(t *testing.T) {
		h := startDaemon(t, policy(t, echoAlone))
		first := h.startSession(t)
		h.letGo(t)
		h.grantAs(t, first, sessionAnswer, false, "sh", "-c", goingScript)
		h.holdGoing(t)
		line, going := h.startGoing(t)
		waitStarted(t, line)
		first.waitFor(t, "\nrun 2 · "+goingLine+" · by session approval\n")

		second := launchSession(t, approval.Config{Socket: h.approvalSocket})
		second.waitFor(t, takeOverPromptText)
		second.answer(t, "y")
		first.waitFor(t, "taken over by another session\n")
		second.waitFor(t, "\nrun 1 · "+goingLine+" · by session approval · started before this session\n")
		h.letGo(t)
		awaitGoing(t, going)
		second.waitForMatch(t, earlierReport(1))
	})
}

// A run the rules allow is no approved run: a session taking the daemon
// over while one goes picks up nothing, and numbers the next approved run
// 1.
func TestATakeOverDoesNotPickUpARunTheRulesAllow(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSession(t)
	line, going := h.startGoing(t)
	waitStarted(t, line)

	second := launchSession(t, approval.Config{Socket: h.approvalSocket})
	second.waitFor(t, takeOverPromptText)
	second.answer(t, "y")
	second.waitFor(t, "waiting for requests")
	ch := h.ask(t, "touch", "marker")
	second.waitFor(t, answerPromptText)
	second.answer(t, "o")
	awaitResult(t, ch)
	second.waitFor(t, "approved once · run 1\n")
	h.letGo(t)
	awaitGoing(t, going)
	second.waitForMatch(t, summaryPattern(1, "touch marker", "exit 0", "0 lines"))
	assertStartLines(t, second, 0)
}

// The start line of a run that started before the session served its
// daemon names the approval that let it through: a fresh approval's
// scope, or, for a repeat, the approval held, or a kind the session does
// not know as the daemon names it. The fake's request is withdrawn first:
// a start line waits while a request is shown.
func TestARunStartedBeforeTheSessionNamesItsApproval(t *testing.T) {
	for _, tt := range []struct {
		name     string
		approval protocol.Scope
		request  uint64
		want     string
	}{
		{"once", protocol.ScopeOnce, 3, "approved once"},
		{"session", protocol.ScopeSession, 3, "approved for a session"},
		{"standing", protocol.ScopeStanding, 3, "approved as standing"},
		{"add a rule", protocol.ScopeRule, 3, "approved under a new rule"},
		{"standing repeat", protocol.ScopeStanding, 0, "by standing approval"},
		{"session repeat", protocol.ScopeSession, 0, "by session approval"},
		{"unknown kind", "rulebook", 3, "by rulebook"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFake(t)
			sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnByClient})
			s.waitFor(t, "request 1 withdrawn\n> ")
			sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 9, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: tt.approval, Request: tt.request, Earlier: true})
			s.waitFor(t, "\nrun 1 · touch marker · "+tt.want+" · started before this session\n")
		})
	}
}

// A run that started before the session served its daemon is listed and
// summarised as missing its earlier output, before the bytes the daemon
// dropped since, and listed as going since it started, by the time the
// daemon says it had gone; its start line, a notice, waits while a
// request is shown.
func TestARunStartedBeforeTheSessionSaysItsEarlierOutputWasNotReceived(t *testing.T) {
	s, conn := startRunsFake(t)
	sendFrame(t, conn, protocol.FrameRunStarted, protocol.RunStarted{Run: 4, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Approval: protocol.ScopeOnce, Request: 1, Earlier: true, ElapsedMs: 90_000})
	sendFrame(t, conn, protocol.FrameRunOutput, protocol.RunOutput{Run: 4, Data: []byte("a\n")})
	sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnByClient})
	s.waitFor(t, "request 1 withdrawn\nrun 1 · touch marker · approved once · started before this session\n> ")
	syncIdle(t, s, conn)
	// 1m30s and the moments since the session was told of the run.
	running := `^── run 1 · touch marker · running · 1m3\ds` + regexp.QuoteMeta(" · 1 line · earlier output not received ──\n> ") + "$"
	if got := s.command(t, "runs"); !regexp.MustCompile(running).MatchString(got) {
		t.Fatalf("runs printed %q, want run 1 running for 1m30s without its earlier output", got)
	}
	sendFrame(t, conn, protocol.FrameRunEnded, protocol.RunEnded{Run: 4, DurationMs: 1500, Dropped: 4096})
	s.waitFor(t, "\n── run 1 · touch marker · exit 0 · 1.5s · 1 line · earlier output not received · 4096 bytes not received ──\n│ a\n> ")
}
