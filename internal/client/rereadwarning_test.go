package client_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// notAllowedWarningLine is what hostrun prints on stderr while a change of
// the rules file to content of the given digest is pending as not allowed.
func notAllowedWarningLine(digest string) string {
	return "hostrun: warning: the rules file changed (digest " + digest +
		"), but the change is not allowed, so the daemon keeps the old rules; a human on the host runs hostrunner allow to allow it\n"
}

// notCheckedWarningLine is what hostrun prints on stderr while a change of
// the rules file to content of the given digest is pending as not checked.
func notCheckedWarningLine(digest string) string {
	return "hostrun: warning: the rules file changed (digest " + digest +
		"), but its allow cannot be checked, so the daemon keeps the old rules; a human on the host looks at the daemon log\n"
}

// pendingEdit starts a daemon serving echoRules whose allow store allows
// nothing, edits the rules file to touchRules and waits until the daemon
// found the edit not allowed. It returns the daemon, its log and the
// digest of the edit.
func pendingEdit(t *testing.T) (h harness, a allowStore, log *lockedBuffer, digest string) {
	t.Helper()
	log = &lockedBuffer{}
	a = newAllowStore(t)
	h = a.startDaemon(t, slog.New(slog.NewTextHandler(log, nil)), echoRules)
	writeRules(t, h.rulesFile, touchRules)
	digest = rules.ContentDigest([]byte(touchRules))
	waitForLog(t, log, notAllowedLog+h.rulesFile+" digest="+digest)
	return h, a, log, digest
}

// While an edit not allowed is pending, a command the rules served allow
// runs as without it, and hostrun warns once on stderr.
func TestRunWarnsOfAnEditNotAllowedAndChangesNothingElse(t *testing.T) {
	h, _, _, digest := pendingEdit(t)
	got := h.run(t, containerRoot, "", "sh", "-c", "echo out; exit 3")
	want := result{code: 3, stdout: "out\n", stderr: notAllowedWarningLine(digest)}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if n := strings.Count(got.stderr, "hostrun: warning:"); n != 1 {
		t.Fatalf("the warning is printed %d times, want once: %q", n, got.stderr)
	}
}

// A dry run warns before it says the command is allowed.
func TestDryRunWarnsOfAnEditNotAllowed(t *testing.T) {
	h, _, _, digest := pendingEdit(t)
	got := h.run(t, containerRoot, "", "--dry-run", "echo", "old")
	want := result{stderr: notAllowedWarningLine(digest) + "hostrun: dry run: allowed by rule \"echo\"\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A refused command warns before the refusal and exits with its code.
func TestRefusalWarnsOfAnEditNotAllowedBeforeTheMessage(t *testing.T) {
	h, _, _, digest := pendingEdit(t)
	plain := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&lockedBuffer{}, nil)), echoRules)
	base := plain.run(t, containerRoot, "", "touch", "marker")
	if base.code != protocol.ExitRejected || base.stderr == "" {
		t.Fatalf("a refusal without a pending edit: got %+v, want a refusal", base)
	}
	got := h.run(t, containerRoot, "", "touch", "marker")
	want := result{code: base.code, stdout: base.stdout, stderr: notAllowedWarningLine(digest) + base.stderr}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A command asked for approval warns before it says it waits.
func TestAskWarnsOfAnEditNotAllowedBeforeTheWaitingLine(t *testing.T) {
	h, _, _, digest := pendingEdit(t)
	h.startSession(t)
	got := awaitResult(t, h.askFor(t, "300ms", "touch", "marker"))
	want := result{code: protocol.ExitRejected, stderr: notAllowedWarningLine(digest) + waitingNoRule + "hostrun: not approved: nobody answered on the host\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Once the edit is allowed and the daemon serves it, hostrun warns of
// nothing.
func TestNoWarningOnceTheEditIsAllowed(t *testing.T) {
	h, a, log, _ := pendingEdit(t)
	a.allow(t, h.rulesFile, touchRules)
	h.waitForCode(t, 0, "touch", "marker")
	waitForLog(t, log, `msg="rules reread" file=`+h.rulesFile)
	if got := h.run(t, containerRoot, "", "echo", "new"); got != (result{stdout: "new\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "echo", "new"); got != (result{stderr: "hostrun: dry run: allowed by rule \"echo\"\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
}

// Once the file goes back to the rules served, hostrun warns of nothing.
func TestNoWarningOnceTheEditIsUndone(t *testing.T) {
	h, _, log, _ := pendingEdit(t)
	writeRules(t, h.rulesFile, echoRules)
	waitForLog(t, log, `msg="rules change undone: the file holds the rules served" file=`+h.rulesFile)
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected || strings.Contains(got.stderr, "warning") {
		t.Fatalf("got %+v, want a refusal without a warning", got)
	}
}

// The warning of an edit pending as not allowed ends as soon as the edit
// is allowed, without waiting for the daemon's next look at the file: the
// daemon here looks only every hour, the refusal comes from a session's
// reread of the hand edit.
func TestNoWarningRightAfterTheEditIsAllowed(t *testing.T) {
	a := newAllowStore(t)
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules,
		daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check), daemon.WithRereadEvery(time.Hour))
	conn := h.serveRaw(t)
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	if err := protocol.WriteJSON(conn, protocol.FrameReread, protocol.Reread{Digest: digest}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var res protocol.RereadResult
	if err != nil || f.Type != protocol.FrameRereadResult || protocol.DecodeJSON(f, &res) != nil || res.Error == "" {
		t.Fatalf("reread: frame %v, err %v, result %+v; want the edit refused", f.Type, err, res)
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(digest)}) {
		t.Fatalf("before the allow: got %+v, want the warning", got)
	}
	a.allow(t, h.rulesFile, touchRules)
	noWarning(t, h)
}

// An allow that cannot be checked is warned of in its own words.
func TestRunWarnsWhenTheAllowOfAnEditCannotBeChecked(t *testing.T) {
	var log lockedBuffer
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules,
		daemon.WithAllowed(func(string, map[string]string) (bool, error) { return false, errors.New("the allow store is broken") }))
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	waitForLog(t, &log, `msg="rules change not checked: the old rules stay" file=`+h.rulesFile+" digest="+digest)
	got := h.run(t, containerRoot, "", "echo", "old")
	if want := (result{stdout: "old\n", stderr: notCheckedWarningLine(digest)}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Without a change of the rules file pending, hostrun warns of nothing,
// whatever the allow check.
func TestNoWarningWithoutAPendingEdit(t *testing.T) {
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&lockedBuffer{}, nil)), echoRules)
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "echo", "old"); got != (result{stderr: "hostrun: dry run: allowed by rule \"echo\"\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected || strings.Contains(got.stderr, "warning") {
		t.Fatalf("got %+v, want a refusal without a warning", got)
	}
}
