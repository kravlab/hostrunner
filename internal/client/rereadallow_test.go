package client_test

import (
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
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

// allowStore is an allow store, and the workspace and the runtime
// directory whose allow the daemons of these tests check in it. rulesFile
// is a rules file of the workspace a test may name in an allow.
type allowStore struct {
	store      allow.Store
	workspace  string
	runtimeDir string
	rulesFile  string
}

// newAllowStore returns an empty allow store in a temporary directory.
func newAllowStore(t *testing.T) allowStore {
	t.Helper()
	workspace := t.TempDir()
	return allowStore{store: allow.Store{Dir: t.TempDir()}, workspace: workspace, runtimeDir: t.TempDir(), rulesFile: filepath.Join(workspace, "hostrun.yaml")}
}

// place is where the daemons of these tests keep their allow in the
// store: the workspace and the runtime directory.
func (a allowStore) place() allow.Place {
	return allow.Place{Workspace: a.workspace, RuntimeDir: a.runtimeDir}
}

// check is the allow check a daemon of these tests runs (see
// daemon.WithAllowed): whether the store allows the Scope for the
// workspace and the runtime directory.
func (a allowStore) check(sc allow.Scope) (bool, error) {
	return a.store.Allowed(a.place(), sc)
}

// allow records an allow of content for the workspace and the runtime
// directory, as `hostrunner allow` does.
func (a allowStore) allow(t *testing.T, rulesFile, content string) {
	t.Helper()
	if _, err := a.store.Allow(a.place(), allow.Target{RulesFile: rulesFile, Scope: allow.Scope{Digest: rules.ContentDigest([]byte(content)), Settings: allowSettings}}); err != nil {
		t.Fatal(err)
	}
}

// record is the allow a daemon of these tests records (see
// daemon.WithAllowRecorder) for a rule added from the approval session:
// the allows of the workspace that covered the content written over moved
// to the content of the given digest, as `hostrunner serve` records them.
func (a allowStore) record(writtenOver allow.Scope, digest string) error {
	_, err := a.store.AllowAddedRule(a.place(), writtenOver, digest)
	return err
}

// startDaemon starts a daemon serving the rules file content, with
// allowSettings, that rereads the file by itself once it changes,
// provided a's store allows the new content, and records in the store the
// allow of a rule added from the approval session.
func (a allowStore) startDaemon(t *testing.T, log *slog.Logger, content string) harness {
	t.Helper()
	return startRereadingDaemon(t, log, content, daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check), daemon.WithAllowRecorder(a.record))
}

// notAllowedLog is how the daemon logs a change of its rules file to
// content not allowed.
const notAllowedLog = "msg=\"rules changed but not allowed: the old rules stay until `hostrunner allow`\" file="

// notAllowedNotice is how the session shows a change of the rules file to
// content of the given digest not allowed: in the words of hostrun's
// warning, on a line of its own.
func notAllowedNotice(digest string) string {
	return "\n" + strings.TrimPrefix(notAllowedWarningLine(digest), "hostrun: warning: ")
}

// notCheckedNotice is how the session shows a change of the rules file to
// content of the given digest whose allow cannot be checked.
func notCheckedNotice(digest string) string {
	return "\n" + strings.TrimPrefix(notCheckedWarningLine(digest), "hostrun: warning: ")
}

// readsNotAllowedOnly is how the raw sessions of these tests serve: they read
// the notice of a change not allowed, and no other notice.
var readsNotAllowedOnly = protocol.Serve{RulesNotAllowed: true}

// readNotAllowed reads the next frame the daemon sends conn, a serving
// session's that reads only the notice of a change not allowed, and checks
// it is that notice.
func readNotAllowed(t *testing.T, conn net.Conn) protocol.RulesNotAllowed {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var n protocol.RulesNotAllowed
	if err == nil && f.Type == protocol.FrameRulesNotAllowed {
		err = protocol.DecodeJSON(f, &n)
	}
	if err != nil || f.Type != protocol.FrameRulesNotAllowed {
		t.Fatalf("frame %v %q, err %v; want the notice of a change not allowed", f.Type, f.Payload, err)
	}
	return n
}

// noticesUntil reads the notices of a change not allowed the daemon sends
// conn (see readNotAllowed) up to the one of the given digest, and returns
// them, that one included.
func noticesUntil(t *testing.T, conn net.Conn, digest string) []protocol.RulesNotAllowed {
	t.Helper()
	var got []protocol.RulesNotAllowed
	for {
		n := readNotAllowed(t, conn)
		got = append(got, n)
		if n.Digest == digest {
			return got
		}
	}
}

// noticesBeforeStandingList asks the daemon for the standing list on conn, a
// serving session's that reads only the notice of a change not allowed, and
// returns the notices it sent before the list.
func noticesBeforeStandingList(t *testing.T, conn net.Conn) []protocol.RulesNotAllowed {
	t.Helper()
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStanding}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []protocol.RulesNotAllowed
	for {
		f, err := protocol.ReadFrame(conn)
		if err == nil && f.Type == protocol.FrameStandingList {
			return got
		}
		var n protocol.RulesNotAllowed
		if err == nil && f.Type == protocol.FrameRulesNotAllowed {
			err = protocol.DecodeJSON(f, &n)
		}
		if err != nil || f.Type != protocol.FrameRulesNotAllowed {
			t.Fatalf("frame %v %q, err %v; want notices of a change not allowed, then the standing list", f.Type, f.Payload, err)
		}
		got = append(got, n)
	}
}

// checkNotices checks that the notices of a change not allowed a session
// got are want, in that order.
func checkNotices(t *testing.T, got []protocol.RulesNotAllowed, want ...protocol.RulesNotAllowed) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("the session got the notices %+v, want %+v", got, want)
	}
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
		daemon.WithAllowed(func(allow.Scope) (bool, error) {
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
// rule the session wrote over the rules served, which are allowed, is not
// taken for undone when the daemon then finds the file holding the rules
// served.
func TestRereadOfThePendingEditIsNotUndone(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	a.allow(t, a.rulesFile, echoRules)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	conn := h.serveRaw(t)
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, notAllowedLog)
	digest := rules.ContentDigest([]byte(touchRules))
	if err := protocol.WriteJSON(conn, protocol.FrameReread, protocol.Reread{Rule: "touch", Digest: digest, WrittenOver: rules.ContentDigest([]byte(echoRules))}); err != nil {
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
		daemon.WithAllowed(func(allow.Scope) (bool, error) {
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
	s.waitFor(t, notCheckedNotice(digest))
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

// A session that reads the notices of a reread but not that of a change
// not allowed, as one older than it, takes an unknown frame for an error,
// so it is sent none.
func TestRulesNotAllowedIsNotSentToAnOlderSession(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	conn := helloThenServe(t, h, func() {}, protocol.Serve{Notices: true, Runs: true, MountCheck: true})
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, notAllowedLog+h.rulesFile+" digest="+rules.ContentDigest([]byte(touchRules)))
	assertNoFrameBeforeStandingList(t, conn)
}

// A session that starts serving after the daemon refused an edit, with no
// session serving, is told of the refusal pending as it starts, once: the
// daemon told only the session serving as it refused the edit, and that
// edit is not refused again.
func TestRereadTellsASessionStartingAfterAnEditNotAllowed(t *testing.T) {
	for _, tt := range []struct {
		name    string
		allowed func(allow.Scope) (bool, error)
		logged  string
		notice  func(digest string) string
	}{
		{"not allowed", func(allow.Scope) (bool, error) { return false, nil }, notAllowedLog, notAllowedNotice},
		{"not checked", func(allow.Scope) (bool, error) { return false, errors.New("the allow store is broken") },
			`msg="rules change not checked: the old rules stay" file=`, notCheckedNotice},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var log lockedBuffer
			h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules, daemon.WithAllowed(tt.allowed))
			writeRules(t, h.rulesFile, touchRules)
			digest := rules.ContentDigest([]byte(touchRules))
			waitForLog(t, &log, tt.logged+h.rulesFile+" digest="+digest)
			s := h.startSession(t)
			s.waitFor(t, tt.notice(digest))
			// The notice is not repeated: a later edit's is the next one.
			time.Sleep(20 * rereadEvery)
			writeRules(t, h.rulesFile, handDate)
			got := s.waitFor(t, tt.notice(rules.ContentDigest([]byte(handDate))))
			if strings.Contains(got, digest) {
				t.Fatalf("the session is told of the first edit again: %q", got)
			}
		})
	}
}

// A session that does not read the notice of a change not allowed, as one
// older than it, is not sent the refusal pending as it starts serving
// either.
func TestRulesNotAllowedPendingIsNotSentToAnOlderSession(t *testing.T) {
	h, _, _, _ := pendingEdit(t)
	conn := helloThenServe(t, h, func() {}, protocol.Serve{Notices: true, Runs: true, MountCheck: true})
	assertNoFrameBeforeStandingList(t, conn)
}

// rereadNoticed sends rr on conn, a serving session's that reads only the
// notice of a change not allowed, as sendReread does, and returns the
// notices the daemon sent before its answer, and the answer.
func rereadNoticed(t *testing.T, conn net.Conn, rr protocol.Reread) ([]protocol.RulesNotAllowed, protocol.RereadResult) {
	t.Helper()
	if err := protocol.WriteJSON(conn, protocol.FrameReread, rr); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var notices []protocol.RulesNotAllowed
	for {
		f, err := protocol.ReadFrame(conn)
		var res protocol.RereadResult
		if err == nil && f.Type == protocol.FrameRereadResult && protocol.DecodeJSON(f, &res) == nil {
			return notices, res
		}
		var n protocol.RulesNotAllowed
		if err == nil && f.Type == protocol.FrameRulesNotAllowed {
			err = protocol.DecodeJSON(f, &n)
		}
		if err != nil || f.Type != protocol.FrameRulesNotAllowed {
			t.Fatalf("reread: frame %v %q, err %v; want notices of a change not allowed, then the result", f.Type, f.Payload, err)
		}
		notices = append(notices, n)
	}
}

// startQuietAllowDaemon starts a daemon serving echoRules with
// allowSettings, whose allow of the rules file is checked in a's store, and
// which rereads by itself, but does not look at the file before the test is
// over: only a session's reread refuses an edit.
func startQuietAllowDaemon(t *testing.T, a allowStore) harness {
	t.Helper()
	return startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules,
		daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check), daemon.WithRereadEvery(time.Hour))
}

// A hand edit not allowed that a session's reread refuses, before the
// daemon looked at the file, is noticed to that session, once however
// often it rereads, and to a session that starts serving later.
func TestASessionsRereadOfAnEditNotAllowedIsNoticed(t *testing.T) {
	a := newAllowStore(t)
	h := startQuietAllowDaemon(t, a)
	conn := helloThenServe(t, h, func() {}, readsNotAllowedOnly)
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	want := protocol.RulesNotAllowed{Digest: digest}
	for _, wantNotices := range [][]protocol.RulesNotAllowed{{want}, nil} {
		notices, res := rereadNoticed(t, conn, protocol.Reread{Digest: digest})
		if res.Error == "" {
			t.Fatalf("reread: got %+v, want the edit refused", res)
		}
		checkNotices(t, notices, wantNotices...)
	}
	later := helloThenServe(t, h, func() {}, protocol.Serve{TakeOver: true, RulesNotAllowed: true})
	checkNotices(t, noticesBeforeStandingList(t, later), want)
}

// A session that starts serving after an edit refused as not allowed was
// allowed, before the daemon served it, is not told of it: the allow is
// checked again as the session starts.
func TestAnEditAllowedSinceIsNotNoticedToASessionStartingLater(t *testing.T) {
	a := newAllowStore(t)
	h := startQuietAllowDaemon(t, a)
	conn := h.serveRaw(t)
	writeRules(t, h.rulesFile, touchRules)
	if res := sendReread(t, conn, protocol.Reread{Digest: rules.ContentDigest([]byte(touchRules))}); res.Error == "" {
		t.Fatalf("reread: got %+v, want the edit refused", res)
	}
	a.allow(t, h.rulesFile, touchRules)
	later := helloThenServe(t, h, func() {}, protocol.Serve{TakeOver: true, RulesNotAllowed: true})
	checkNotices(t, noticesBeforeStandingList(t, later))
}
