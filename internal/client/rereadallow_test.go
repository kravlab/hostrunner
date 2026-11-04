package client_test

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// allowSettings are the daemon settings the allow checks of these tests
// name.
var allowSettings = map[string]string{"reread": "true"}

// allowStore is an allow store, and the workspace whose allow the
// daemons of these tests check in it.
type allowStore struct {
	store     allow.Store
	workspace string
}

// newAllowStore returns an empty allow store in a temporary directory.
func newAllowStore(t *testing.T) allowStore {
	t.Helper()
	return allowStore{store: allow.Store{Dir: t.TempDir()}, workspace: t.TempDir()}
}

// check is the allow check a daemon of these tests runs (see
// daemon.WithAllowed): whether the store allows digest and settings for
// the workspace.
func (a allowStore) check(digest string, settings map[string]string) (bool, error) {
	return a.store.Allowed(a.workspace, digest, settings)
}

// allow records an allow of content for the workspace, as `hostrunner
// allow` does.
func (a allowStore) allow(t *testing.T, rulesFile, content string) {
	t.Helper()
	if _, err := a.store.Allow(a.workspace, rulesFile, rules.ContentDigest([]byte(content)), allowSettings); err != nil {
		t.Fatal(err)
	}
}

// startDaemon starts a daemon serving the rules file content, with
// allowSettings, that rereads the file by itself once it changes,
// provided a's store allows the new content.
func (a allowStore) startDaemon(t *testing.T, log *slog.Logger, content string) harness {
	t.Helper()
	return startRereadingDaemon(t, log, content, daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check))
}

// notAllowedLog is how the daemon logs a change of its rules file to
// content not allowed.
const notAllowedLog = "msg=\"rules changed but not allowed: the old rules stay until `hostrunner allow`\" file="

// notAllowedNotice is how the session shows a change of the rules file to
// content of the given digest not allowed.
func notAllowedNotice(digest string) string {
	return "\nthe rules file changed, but the daemon cannot reread it, so the old rules stay: its content (digest " +
		digest + ") is not allowed; run `hostrunner allow` on the host\n"
}

// An edit to content not allowed keeps the old rules, is logged once
// however often the file is touched without changing, and is shown in the
// session.
func TestRereadKeepsTheOldRulesOfAnEditNotAllowed(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	s := h.startSession(t)
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	waitForLog(t, &log, notAllowedLog+h.rulesFile+" digest="+digest)
	s.waitFor(t, notAllowedNotice(digest))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(h.rulesFile, later, later); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * rereadEvery)
	if n := strings.Count(log.String(), notAllowedLog); n != 1 {
		t.Fatalf("the change is logged %d times, want once:\n%s", n, log.String())
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the edit: got %+v, want it refused", got)
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(digest)}) {
		t.Fatalf("a command of the old rules: got %+v, want it to run, with the warning", got)
	}
}

// Once the edit is allowed, the daemon serves it without the file
// changing again, and standing and session approvals stay.
func TestRereadTakesAnEditOnceAllowed(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	s := h.startSession(t)
	h.grantStanding(t, s, false, "ls", "-d", ".")
	h.grantSession(t, s, false, "ls", "-a", ".")
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, notAllowedLog)
	a.allow(t, h.rulesFile, touchRules)
	h.waitForCode(t, 0, "touch", "marker")
	waitForLog(t, &log, `msg="rules reread" file=`+h.rulesFile+" digest="+rules.ContentDigest([]byte(touchRules)))
	s.waitFor(t, "\nthe rules file changed; the daemon reread it\n")
	for _, flag := range []string{"-d", "-a"} {
		if got := h.run(t, containerRoot, "", "ls", flag, "."); got.code != 0 {
			t.Fatalf("ls %s .: got %+v, want its approval to let it through", flag, got)
		}
	}
}

// The file going back to the rules served ends the change not allowed,
// which is logged but not shown in the session; the same edit made again
// is reported again.
func TestRereadEndsAnEditNotAllowedThatIsUndone(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	s := h.startSession(t)
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	s.waitFor(t, notAllowedNotice(digest))
	writeRules(t, h.rulesFile, echoRules)
	waitForLog(t, &log, `msg="rules change undone: the file holds the rules served" file=`+h.rulesFile)
	writeRules(t, h.rulesFile, touchRules)
	// What the session printed up to the edit's next notice holds no
	// notice of the undo.
	if got := s.waitFor(t, notAllowedNotice(digest)); strings.Count(got, "the rules file changed") != 1 {
		t.Fatalf("the session printed a notice of the undo: %q", got)
	}
	if n := strings.Count(log.String(), notAllowedLog); n != 2 {
		t.Fatalf("the edit is logged %d times, want twice:\n%s", n, log.String())
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the edit: got %+v, want it refused", got)
	}
}

// An allow that cannot be checked keeps the old rules and is logged once;
// the check is made again at every look, so the edit is served once it
// passes.
func TestRereadKeepsTheOldRulesWhenTheAllowCannotBeChecked(t *testing.T) {
	var (
		log    lockedBuffer
		broken atomic.Bool
	)
	broken.Store(true)
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules,
		daemon.WithAllowed(func(string, map[string]string) (bool, error) {
			if broken.Load() {
				return false, errors.New("the allow store is broken")
			}
			return true, nil
		}))
	writeRules(t, h.rulesFile, touchRules)
	const notChecked = `msg="rules change not checked: the old rules stay" file=`
	waitForLog(t, &log, notChecked+h.rulesFile+" digest="+rules.ContentDigest([]byte(touchRules))+` error="the allow store is broken"`)
	time.Sleep(20 * rereadEvery)
	if n := strings.Count(log.String(), notChecked); n != 1 {
		t.Fatalf("the failed check is logged %d times, want once:\n%s", n, log.String())
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the edit: got %+v, want it refused", got)
	}
	broken.Store(false)
	h.waitForCode(t, 0, "touch", "marker")
}

// Adding a rule after a hand edit not allowed adds none: the daemon does
// not reread the edit, the session says why and asks for the answer
// again, and the old rules stay.
func TestAddRuleRefusesAHandEditNotAllowed(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	// Rereading by itself, but not before the test is over.
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), loadedTouch, false,
		daemon.WithRereadEvery(time.Hour), daemon.WithAllowed(a.check))
	writeRules(t, h.rulesFile, handDate)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-m", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	digest := rules.ContentDigest([]byte(handDate))
	s.waitFor(t, "cannot add a rule: the rules file changed since the daemon loaded it, and the daemon cannot reread it: its content (digest "+
		digest+") is not allowed; run `hostrunner allow` on the host\n")
	waitForLog(t, &log, notAllowedLog+h.rulesFile+" digest="+digest)
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	if got := h.run(t, containerRoot, "", "date"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the hand edit: got %+v, want it refused", got)
	}
}

// An edit not allowed is reported once across the daemon's looks and the
// session's adding of a rule: `a` after the daemon reported it, however
// often, logs nothing more.
func TestAddRuleDoesNotReportAnEditNotAllowedAgain(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), loadedTouch, false,
		daemon.WithRereadEvery(rereadEvery), daemon.WithAllowed(a.check))
	writeRules(t, h.rulesFile, handDate)
	waitForLog(t, &log, notAllowedLog)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-m", "marker")
	for range 3 {
		s.waitFor(t, answerPromptText)
		s.answer(t, "a")
		s.waitFor(t, "cannot add a rule: ")
	}
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	if n := strings.Count(log.String(), notAllowedLog); n != 1 {
		t.Fatalf("the edit is logged %d times, want once:\n%s", n, log.String())
	}
}

// A pending edit not allowed that a session's reread serves, as one of a
// rule the session wrote, is not taken for undone when the daemon then
// finds the file holding the rules served.
func TestRereadOfThePendingEditIsNotUndone(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	conn := h.serveRaw(t)
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, notAllowedLog)
	digest := rules.ContentDigest([]byte(touchRules))
	if err := protocol.WriteJSON(conn, protocol.FrameReread, protocol.Reread{Rule: "touch", Digest: digest}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var res protocol.RereadResult
	if err != nil || f.Type != protocol.FrameRereadResult || protocol.DecodeJSON(f, &res) != nil || res.Digest != digest {
		t.Fatalf("reread: frame %v, err %v, result %+v; want digest %s", f.Type, err, res, digest)
	}
	time.Sleep(20 * rereadEvery)
	if strings.Contains(log.String(), "rules change undone") {
		t.Fatalf("the session's reread is logged as undone:\n%s", log.String())
	}
}

// The same content refused for another reason, its allow not allowed and
// then not checked or the other way round, is reported again, in the log
// and in the session.
func TestRereadReportsAnotherReasonAgain(t *testing.T) {
	var (
		log    lockedBuffer
		broken atomic.Bool
	)
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules,
		daemon.WithAllowed(func(string, map[string]string) (bool, error) {
			if broken.Load() {
				return false, errors.New("the allow store is broken")
			}
			return false, nil
		}))
	s := h.startSession(t)
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	s.waitFor(t, notAllowedNotice(digest))
	broken.Store(true)
	s.waitFor(t, "so the old rules stay: the allow of its content (digest "+digest+") cannot be checked: the allow store is broken\n")
	broken.Store(false)
	s.waitFor(t, notAllowedNotice(digest))
	const notChecked = `msg="rules change not checked: the old rules stay"`
	if n, m := strings.Count(log.String(), notAllowedLog), strings.Count(log.String(), notChecked); n != 2 || m != 1 {
		t.Fatalf("logged %d times not allowed and %d times not checked, want 2 and 1:\n%s", n, m, log.String())
	}
}

// An edit not allowed records an allow request once for its content,
// however often the daemon looks and the session adds a rule; a request
// that fails is logged and keeps the old rules served.
func TestRereadRecordsAnAllowRequestOnce(t *testing.T) {
	var (
		log      lockedBuffer
		requests atomic.Int32
	)
	a := newAllowStore(t)
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules,
		daemon.WithAllowed(a.check),
		daemon.WithAllowRequest(func(string) error {
			requests.Add(1)
			return errors.New("the allow store is read-only")
		}))
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, `msg="allow request not recorded" file=`+h.rulesFile+" digest="+rules.ContentDigest([]byte(touchRules))+` error="the allow store is read-only"`)
	time.Sleep(20 * rereadEvery)
	if n := requests.Load(); n != 1 {
		t.Fatalf("the request was recorded %d times, want once", n)
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(rules.ContentDigest([]byte(touchRules)))}) {
		t.Fatalf("a command of the old rules: got %+v, want it to run, with the warning", got)
	}
}
