package client_test

import (
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// addRuleRules are the rules these tests add a rule to: none lets touch
// run.
const addRuleRules = "rules:\n  - command: echo\n    args: any\n"

// ruleKeptOldRulesText is what the session prints once the daemon kept its
// old rules after a rule was added.
const ruleKeptOldRulesText = "rule added, but the daemon kept its old rules: "

// notRecordedLog is how the daemon logs an allow of a rule added from the
// approval session that it could not record.
const notRecordedLog = "msg=\"allow not recorded\" file="

// startAddRuleDaemon starts a daemon serving addRuleRules, whose allow of
// the rules file is checked in a's store (with allowSettings) and, for a
// rule added from the approval session, recorded through record.
func startAddRuleDaemon(t *testing.T, a allowStore, log *slog.Logger, record func(digest string) error) harness {
	t.Helper()
	return startRulesDaemon(t, log, addRuleRules, false,
		daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check), daemon.WithAllowRecorder(record))
}

// addRuleKeptOld adds a rule as addRule does, waits for the session to say
// the daemon kept its old rules, and returns the digest of the rules file
// as the session wrote it.
func (s *session) addRuleKeptOld(t *testing.T, rulesFile string) (digest string) {
	t.Helper()
	s.addRule(t)
	got := s.waitFor(t, ruleKeptOldRulesText)
	if strings.Contains(got, ruleAddedText) {
		t.Fatalf("the session reports the daemon reread the rules: %q", got)
	}
	// The session has written the file before the daemon answered.
	return rules.ContentDigest([]byte(readFile(t, rulesFile)))
}

// A rule added to a rules file that is allowed is allowed too: the daemon
// rereads it, the request runs under it, a later run needs no approval,
// and the store allows the file as written.
func TestAddRuleToAnAllowedFileIsAllowed(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startAddRuleDaemon(t, a, slog.New(slog.NewTextHandler(&log, nil)), a.record)
	a.allow(t, h.rulesFile, addRuleRules)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.addRule(t)
	s.waitFor(t, ruleAddedText+" · request 1 runs under it\n")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the request to run", got)
	}
	digest := rules.ContentDigest([]byte(readFile(t, h.rulesFile)))
	if ok, err := a.store.Allowed(a.workspace, allow.Scope{Digest: digest, Settings: allowSettings}); err != nil || !ok {
		t.Fatalf("the store allows the file as written (digest %s): %v, %v; want it allowed", digest, ok, err)
	}
	waitForLog(t, &log, `msg="rules reread" file=`+h.rulesFile+" digest="+digest)
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got != (result{}) {
		t.Fatalf("a later run: got %+v, want it to run without asking", got)
	}
}

// A rule added to a rules file that is not allowed is not allowed either:
// no allow is recorded, the daemon keeps its old rules and treats the
// written file as a change not allowed, the request runs once, and a later
// run of the rule's command is refused.
func TestAddRuleToAFileNotAllowedRecordsNoAllow(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startAddRuleDaemon(t, a, slog.New(slog.NewTextHandler(&log, nil)), a.record)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	digest := s.addRuleKeptOld(t, h.rulesFile)
	s.waitFor(t, "(digest "+digest+") is not allowed; run `hostrunner allow` on the host · request 1 runs once\n")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the request to run once", got)
	}
	if ok, err := a.store.Allowed(a.workspace, allow.Scope{Digest: digest, Settings: allowSettings}); err != nil || ok {
		t.Fatalf("the store allows the file as written (digest %s): %v, %v; want it not allowed", digest, ok, err)
	}
	waitForLog(t, &log, notAllowedLog+h.rulesFile+" digest="+digest)
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a later run of the new rule's command: got %+v, want it refused", got)
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(digest)}) {
		t.Fatalf("a command of the old rules: got %+v, want it to run, with the warning of the change pending", got)
	}
}

// A rule added to a rules file that is allowed, whose allow the daemon
// cannot record, is not reread: the old rules stay, the session says so,
// the request runs once, the failure is logged as a warning, and the
// written file is a change not allowed that is pending, though the daemon
// does not reread by itself: it is logged and hostrun warns of it.
func TestAddRuleKeepsTheOldRulesWhenTheAllowFails(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startAddRuleDaemon(t, a, slog.New(slog.NewTextHandler(&log, nil)),
		func(string) error { return errors.New("the allow store is read-only") })
	a.allow(t, h.rulesFile, addRuleRules)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	digest := s.addRuleKeptOld(t, h.rulesFile)
	s.waitFor(t, "the allow store is read-only · request 1 runs once\n")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the request to run once", got)
	}
	waitForLog(t, &log, notRecordedLog+h.rulesFile+" digest="+digest+` error="the allow store is read-only"`)
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.Contains(line, notRecordedLog) && !strings.Contains(line, "level=WARN") {
			t.Fatalf("the failure is not logged as a warning: %s", line)
		}
	}
	if strings.Contains(log.String(), `msg="rules reread"`) {
		t.Fatalf("the daemon reread the rules:\n%s", log.String())
	}
	waitForLog(t, &log, notAllowedLog+h.rulesFile+" digest="+digest)
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a later run of the new rule's command: got %+v, want it refused", got)
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(digest)}) {
		t.Fatalf("a command of the old rules: got %+v, want it to run, with the warning of the change pending", got)
	}
}

// sendReread sends rr on conn, the serving approval session's, as a session
// that wrote a rule into the rules file does, and returns the daemon's
// answer.
func sendReread(t *testing.T, conn net.Conn, rr protocol.Reread) protocol.RereadResult {
	t.Helper()
	if err := protocol.WriteJSON(conn, protocol.FrameReread, rr); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var res protocol.RereadResult
	if err != nil || f.Type != protocol.FrameRereadResult || protocol.DecodeJSON(f, &res) != nil {
		t.Fatalf("reread: frame %v, err %v, result %+v", f.Type, err, res)
	}
	return res
}

// A rule added to a rules file that is allowed records no allow of content
// the daemon did not load as the session said: the file holds another
// content than the one reported, so the daemon keeps its old rules, the
// result says so, and the store allows neither the content reported nor
// the one in the file.
func TestAddRuleRecordsNoAllowWhenTheFileHoldsOtherContent(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startAddRuleDaemon(t, a, slog.New(slog.NewTextHandler(&log, nil)), a.record)
	a.allow(t, h.rulesFile, addRuleRules)
	conn := h.serveRaw(t)
	const other = addRuleRules + "  - command: ls\n    args: any\n"
	writeRules(t, h.rulesFile, other)
	reported := rules.ContentDigest([]byte(touchRules))
	res := sendReread(t, conn, protocol.Reread{Rule: "touch", Digest: reported, WrittenOver: rules.ContentDigest([]byte(addRuleRules))})
	if res.Error == "" || res.Digest != "" {
		t.Fatalf("got %+v, want an error: the daemon keeps its old rules", res)
	}
	for _, digest := range []string{reported, rules.ContentDigest([]byte(other))} {
		if ok, err := a.store.Allowed(a.workspace, allow.Scope{Digest: digest, Settings: allowSettings}); err != nil || ok {
			t.Fatalf("the store allows content of digest %s: %v, %v; want it not allowed", digest, ok, err)
		}
	}
	if got := h.run(t, containerRoot, "", "ls"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the file's content: got %+v, want it refused", got)
	}
}

// A rule added to a rules file that is allowed records no allow of content
// that does not parse as rules: the daemon keeps its old rules, the result
// says so, and the store does not allow the content.
func TestAddRuleRecordsNoAllowWhenTheFileDoesNotParse(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startAddRuleDaemon(t, a, slog.New(slog.NewTextHandler(&log, nil)), a.record)
	a.allow(t, h.rulesFile, addRuleRules)
	conn := h.serveRaw(t)
	const broken = "rules: [\n"
	writeRules(t, h.rulesFile, broken)
	digest := rules.ContentDigest([]byte(broken))
	res := sendReread(t, conn, protocol.Reread{Rule: "touch", Digest: digest, WrittenOver: rules.ContentDigest([]byte(addRuleRules))})
	if res.Error == "" || res.Digest != "" {
		t.Fatalf("got %+v, want an error: the daemon keeps its old rules", res)
	}
	if ok, err := a.store.Allowed(a.workspace, allow.Scope{Digest: digest, Settings: allowSettings}); err != nil || ok {
		t.Fatalf("the store allows content of digest %s: %v, %v; want it not allowed", digest, ok, err)
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n"}) {
		t.Fatalf("a command of the old rules: got %+v, want it to run", got)
	}
}

// A daemon that checks the allow of its rules file but records none (no
// WithAllowRecorder) treats a rule added from the approval session as a hand edit,
// even when the file written over is allowed: no allow is recorded, the
// written file is a change not allowed, the request runs once, and a later
// run of the rule's command is refused.
func TestAddRuleWithoutAnAllowRecordIsAHandEdit(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), addRuleRules, false,
		daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check))
	a.allow(t, h.rulesFile, addRuleRules)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	digest := s.addRuleKeptOld(t, h.rulesFile)
	s.waitFor(t, "(digest "+digest+") is not allowed; run `hostrunner allow` on the host · request 1 runs once\n")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the request to run once", got)
	}
	if ok, err := a.store.Allowed(a.workspace, allow.Scope{Digest: digest, Settings: allowSettings}); err != nil || ok {
		t.Fatalf("the store allows the file as written (digest %s): %v, %v; want it not allowed", digest, ok, err)
	}
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a later run of the new rule's command: got %+v, want it refused", got)
	}
}
