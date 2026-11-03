package client_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// Notices the tests send the session, and what it prints of each: the
// start of a repeat (run 1 of the session), its end, and a reread of the
// rules, which worked or failed.
var (
	repeatStarted = protocol.RunStarted{Run: 5, Argv: []string{"touch", "x"}, Cwd: containerRoot, Approval: protocol.ScopeStanding}
	repeatEnded   = protocol.RunEnded{Run: 5}
	rulesReread   = protocol.RulesReread{Digest: "new"}
	rereadFailed  = protocol.RulesReread{Error: "bad"}
)

const (
	repeatStartedText = "run 1 · touch x · by standing approval\n"
	repeatEndedText   = "── run 1 · touch x · exit 0 · 0.0s · 0 lines ──\n"
	rulesRereadText   = "the rules file changed; the daemon reread it\n"
	rereadFailedText  = "the rules file changed, but the daemon cannot reread it, so the old rules stay: bad\n"
)

// syncID is the request syncOnFake and ringOnFake send: one the tests
// send no other.
const syncID = 1000

// ringOnFake returns once the session serving the fake daemon on conn, a
// request shown, has handled every frame sent to it before: it sends
// another request, which the session queues behind the one shown, ringing
// the bell at once, and waits for the bell. The test must have read every
// bell rung before. The request stays queued.
func ringOnFake(t *testing.T, s *session, conn net.Conn) {
	t.Helper()
	sendFrame(t, conn, protocol.FrameApprovalRequest, protocol.ApprovalRequest{ID: syncID, Argv: []string{"true"}, Cwd: containerRoot, Reasons: []string{"no rule"}, TimeoutMs: 60000})
	s.waitFor(t, "\a")
}

// syncOnFake is ringOnFake with the request withdrawn right after: queued,
// it drops out silently, and the session has dropped it once it has
// handled a frame sent after it.
func syncOnFake(t *testing.T, s *session, conn net.Conn) {
	t.Helper()
	ringOnFake(t, s, conn)
	sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: syncID, Reason: protocol.WithdrawnByClient})
}

// sendNotices sends the session on conn the start and the end of a
// repeat: the notices most of these tests hold.
func sendNotices(t *testing.T, conn net.Conn) {
	t.Helper()
	sendFrame(t, conn, protocol.FrameRunStarted, repeatStarted)
	sendFrame(t, conn, protocol.FrameRunEnded, repeatEnded)
}

// assertNoNotices fails t if the session printed any of the notices the
// tests send.
func assertNoNotices(t *testing.T, s *session) {
	t.Helper()
	out := s.out.String()
	for _, notice := range []string{"run 1 ·", "── run 1 ", "the rules file changed;", "the rules file changed, but"} {
		if strings.Contains(out, notice) {
			t.Fatalf("the session printed %q while the request was shown:\n%s", notice, out)
		}
	}
}

// Notices that arrive while a request is shown, its answer on the way to
// the daemon included, are held: they are printed once the daemon has
// taken the answer, after its line, in the order they arrived, and before
// the next request is shown.
func TestNoticesWaitForTheAnswerToTheRequestShown(t *testing.T) {
	s, conn := startRunsFake(t)
	sendFrame(t, conn, protocol.FrameRunStarted, repeatStarted)
	sendFrame(t, conn, protocol.FrameRulesReread, rulesReread)
	sendFrame(t, conn, protocol.FrameApprovalRequest, protocol.ApprovalRequest{ID: 2, Argv: []string{"touch", "y"}, Cwd: containerRoot, Reasons: []string{"no rule"}, TimeoutMs: 60000})
	s.waitFor(t, "\a") // request 2's: the answer typed next answers the prompt shown then
	answerOnFake(t, s, conn, "o")
	sendFrame(t, conn, protocol.FrameRunEnded, repeatEnded)
	syncOnFake(t, s, conn)
	assertNoNotices(t, s)
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	s.waitFor(t, "approved once · run 2\n"+repeatStartedText+rulesRereadText+repeatEndedText+"\n── request 2 ──")
}

// Notices that arrive while a request's follow-up prompt is shown, here
// that of a refusal's note, are held until the request is withdrawn:
// they are printed after it is announced, in the order they arrived,
// before the idle prompt.
func TestNoticesWaitForTheRequestShownToBeWithdrawn(t *testing.T) {
	s, conn := startRunsFake(t)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	sendNotices(t, conn)
	sendFrame(t, conn, protocol.FrameRulesReread, rereadFailed)
	syncOnFake(t, s, conn)
	assertNoNotices(t, s)
	sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnExpired})
	s.waitFor(t, "request 1 expired\n"+repeatStartedText+repeatEndedText+rereadFailedText+idlePromptText)
}

// Notices are held at every prompt a request asks: the guarded "yes",
// adding a rule, and rereading a rules file changed by hand alike.
func TestNoticesWaitThroughEveryPromptOfTheRequestShown(t *testing.T) {
	for _, tt := range []struct {
		name   string
		digest string // the digest the fake says its rules have
		reach  func(t *testing.T, s *session, conn net.Conn) uint64
	}{
		{"guarded yes", "", func(t *testing.T, s *session, conn net.Conn) uint64 {
			sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnByClient})
			s.waitFor(t, "request 1 withdrawn\n"+idlePromptText)
			sendFrame(t, conn, protocol.FrameApprovalRequest, protocol.ApprovalRequest{ID: 2, Argv: []string{"touch", "y"}, Cwd: containerRoot, Reasons: []string{"no rule"}, Guards: []string{"deny list"}, TimeoutMs: 60000})
			s.waitFor(t, guardedPromptText)
			s.answer(t, "o")
			s.waitFor(t, `Type "yes" to run it once: `)
			return 2
		}},
		{"adding a rule", "", func(t *testing.T, s *session, conn net.Conn) uint64 {
			s.answer(t, "a")
			s.waitFor(t, addPromptText)
			return 1
		}},
		{"rereading a file changed by hand", "stale", func(t *testing.T, s *session, conn net.Conn) uint64 {
			s.answer(t, "a")
			s.waitFor(t, rereadPromptText)
			return 1
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rulesFile := filepath.Join(t.TempDir(), "hostrun.yaml")
			if err := os.WriteFile(rulesFile, []byte(echoAlone), 0o644); err != nil {
				t.Fatal(err)
			}
			socket, served := fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Runs: true, RulesFile: rulesFile, RulesDigest: tt.digest})
			s := startSessionOn(t, socket)
			conn := <-served
			s.waitFor(t, answerPromptText)
			id := tt.reach(t, s, conn)
			sendNotices(t, conn)
			syncOnFake(t, s, conn)
			assertNoNotices(t, s)
			sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: id, Reason: protocol.WithdrawnByClient})
			s.waitFor(t, fmt.Sprintf("request %d withdrawn\n", id)+repeatStartedText+repeatEndedText+idlePromptText)
		})
	}
}

// Notices that arrive while an answer is on its way to the daemon, which
// its request's withdrawal crosses, are held until the daemon refuses the
// answer: they follow the request's end.
func TestNoticesWaitForAnAnswerAWithdrawalCrosses(t *testing.T) {
	s, conn := startRunsFake(t)
	answerOnFake(t, s, conn, "o")
	sendNotices(t, conn)
	sendFrame(t, conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnExpired})
	syncOnFake(t, s, conn)
	assertNoNotices(t, s)
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1, Error: "request 1 is not waiting any more"})
	s.waitFor(t, "request 1 expired\n"+repeatStartedText+repeatEndedText+idlePromptText)
}

// Notices held while a request is shown are not lost when the session
// stops serving the daemon meanwhile: they are printed after it says
// why, after a blank line, before what it says of waiting for the next
// daemon. Ringing the bell to wait for the notices, the test leaves a
// second request waiting.
func TestNoticesHeldArePrintedWhenTheSessionStopsServing(t *testing.T) {
	for _, tt := range []struct {
		name          string
		stop          func(t *testing.T, s *session, conn net.Conn)
		before, after string // what the session says before the notices, and after them
	}{
		{"leaving", func(t *testing.T, s *session, conn net.Conn) { s.cancel() }, "\nrefused 2 waiting requests\n", ""},
		{"taken over", func(t *testing.T, s *session, conn net.Conn) {
			sendFrame(t, conn, protocol.FrameTakenOver, nil)
		}, "\ntaken over by another session\n", ""},
		{"the daemon gone", func(t *testing.T, s *session, conn net.Conn) { conn.Close() }, "", "\nthe daemon ended: waiting for the next one\n"},
		{"a write failing", func(t *testing.T, s *session, conn net.Conn) {
			// The daemon reads no more: a write to it fails.
			if err := conn.(*net.UnixConn).CloseRead(); err != nil {
				t.Fatal(err)
			}
			s.answer(t, "o")
			s.waitFor(t, "\nthe connection to the daemon failed: ")
		}, "\nrefused 2 waiting requests\n", "waiting for the next one\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn := startRunsFake(t)
			sendNotices(t, conn)
			ringOnFake(t, s, conn)
			assertNoNotices(t, s)
			tt.stop(t, s, conn)
			s.waitFor(t, tt.before+"\n"+repeatStartedText+repeatEndedText+tt.after)
		})
	}
}

// idlePromptText is the idle prompt.
const idlePromptText = "> "
