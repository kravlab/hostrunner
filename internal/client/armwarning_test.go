package client_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// armedSettings are the daemon settings of the changes these tests arm,
// other than allowSettings, the ones their daemons run with.
var armedSettings = map[string]string{"reread": "false"}

// changeNotAllowedText is the warning of a change of the rules or the
// daemon settings to those of the given digest that `hostrunner up` found
// not allowed.
func changeNotAllowedText(digest string) string {
	return "the rules or the daemon settings changed (digest " + digest +
		"), but the change is not allowed, so the daemon keeps the old ones; a human on the host runs hostrunner allow to allow it"
}

// armNotAllowed arms the daemon of h with a change of the given digest
// and settings not allowed.
func (h harness) armNotAllowed(t *testing.T, digest string, settings map[string]string) {
	t.Helper()
	h.arm(t, protocol.Arm{NotAllowed: &protocol.ArmNotAllowed{Digest: digest, Settings: settings}})
}

// changeNotCheckedText is the warning of a change of the rules or the
// daemon settings to those of the given digest whose allow cannot be
// checked.
func changeNotCheckedText(digest string) string {
	return "the rules or the daemon settings changed (digest " + digest +
		"), but its allow cannot be checked, so the daemon keeps the old ones; a human on the host looks at the daemon log"
}

// startArmedDaemon starts a daemon serving echoRules with allowSettings,
// whose allow store allows nothing, and arms it with a change of other
// settings not allowed. It returns the daemon, its store and the digest
// of the change.
func startArmedDaemon(t *testing.T) (h harness, a allowStore, digest string) {
	t.Helper()
	a = newAllowStore(t)
	h = a.startDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	digest = rules.ContentDigest([]byte(echoRules))
	h.armNotAllowed(t, digest, armedSettings)
	return h, a, digest
}

// noWarning checks that a run and a dry run of an allowed command on h
// warn of nothing.
func noWarning(t *testing.T, h harness) {
	t.Helper()
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "echo", "old"); got != (result{stderr: "hostrun: dry run: allowed by rule \"echo\"\n"}) {
		t.Fatalf("got %+v, want no warning", got)
	}
}

// An arm with a change of the daemon settings not allowed makes hostrun
// warn once of it, a dry run before it says the command is allowed; the
// command runs as without it.
func TestArmedChangeNotAllowedIsWarnedOf(t *testing.T) {
	h, _, digest := startArmedDaemon(t)
	got := h.run(t, containerRoot, "", "sh", "-c", "echo out; exit 3")
	want := result{code: 3, stdout: "out\n", stderr: "hostrun: warning: " + changeNotAllowedText(digest) + "\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if n := strings.Count(got.stderr, "hostrun: warning:"); n != 1 {
		t.Fatalf("the warning is printed %d times, want once: %q", n, got.stderr)
	}
	got = h.run(t, containerRoot, "", "--dry-run", "echo", "old")
	want = result{stderr: "hostrun: warning: " + changeNotAllowedText(digest) + "\nhostrun: dry run: allowed by rule \"echo\"\n"}
	if got != want {
		t.Fatalf("dry run: got %+v, want %+v", got, want)
	}
}

// Once the store allows the change armed, hostrun warns of nothing.
func TestNoWarningOnceTheArmedChangeIsAllowed(t *testing.T) {
	h, a, digest := startArmedDaemon(t)
	if _, err := a.store.Allow(a.workspace, allow.Target{RulesFile: h.rulesFile, Scope: allow.Scope{Digest: digest, Settings: armedSettings}}); err != nil {
		t.Fatal(err)
	}
	noWarning(t, h)
}

// A later arm without a change not allowed ends the warning.
func TestNoWarningAfterAnArmWithoutAChangeNotAllowed(t *testing.T) {
	h, _, _ := startArmedDaemon(t)
	h.arm(t, protocol.Arm{ConfigDigest: rules.ContentDigest([]byte(echoRules))})
	noWarning(t, h)
}

// A daemon that does not reread its rules file warns of an edit armed as
// not allowed in the words of the rules file, until the file holds the
// rules served again.
func TestArmedEditNotAllowedIsWarnedOfWithoutReread(t *testing.T) {
	a := newAllowStore(t)
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoRules, false,
		daemon.WithSettings(allowSettings), daemon.WithAllowed(a.check))
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	h.armNotAllowed(t, digest, allowSettings)
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(digest)}) {
		t.Fatalf("got %+v, want the warning of the rules file", got)
	}
	writeRules(t, h.rulesFile, echoRules)
	noWarning(t, h)
}

// An edit the daemon refused and an arm with the same edit not allowed
// are warned of once.
func TestArmedEditTheDaemonRefusedIsWarnedOfOnce(t *testing.T) {
	h, _, _, digest := pendingEdit(t)
	h.armNotAllowed(t, digest, allowSettings)
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notAllowedWarningLine(digest)}) {
		t.Fatalf("got %+v, want one warning of the rules file", got)
	}
}

// An edit the daemon refused and an arm with another change not allowed
// are warned of on one line, joined by "; ".
func TestArmedChangeAndARefusedEditAreWarnedOfTogether(t *testing.T) {
	h, _, _, edit := pendingEdit(t)
	armed := rules.ContentDigest([]byte(echoRules))
	h.armNotAllowed(t, armed, armedSettings)
	want := strings.TrimSuffix(notAllowedWarningLine(edit), "\n") + "; " + changeNotAllowedText(armed) + "\n"
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: want}) {
		t.Fatalf("got %+v, want %q", got, want)
	}
}

// A change armed as not allowed whose allow the daemon cannot check is
// warned of in the words of a check that failed, not of one that found it
// not allowed: for an edit of the rules file as for other settings.
func TestArmedChangeWhoseAllowCannotBeCheckedIsWarnedOfInItsOwnWords(t *testing.T) {
	broken := daemon.WithAllowed(func(allow.Scope) (bool, error) {
		return false, errors.New("the allow store is broken")
	})
	t.Run("rules file", func(t *testing.T) {
		h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoRules, false,
			daemon.WithSettings(allowSettings), broken)
		writeRules(t, h.rulesFile, touchRules)
		digest := rules.ContentDigest([]byte(touchRules))
		h.armNotAllowed(t, digest, allowSettings)
		if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: notCheckedWarningLine(digest)}) {
			t.Fatalf("got %+v, want the warning of a change not checked", got)
		}
	})
	t.Run("settings", func(t *testing.T) {
		h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoRules, false,
			daemon.WithSettings(allowSettings), broken)
		digest := rules.ContentDigest([]byte(echoRules))
		h.armNotAllowed(t, digest, armedSettings)
		if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n", stderr: "hostrun: warning: " + changeNotCheckedText(digest) + "\n"}) {
			t.Fatalf("got %+v, want the warning of a change not checked", got)
		}
	})
}

// armOnContainerSocket sends a on the container's socket of h, as a
// process in the container could, and checks that the daemon refuses it
// as a protocol error.
func (h harness) armOnContainerSocket(t *testing.T, a protocol.Arm) {
	t.Helper()
	conn, err := h.transport.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	a.Version = protocol.Version
	if err := protocol.WriteJSON(conn, protocol.FrameArm, a); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(conn)
	var e protocol.Error
	if err != nil || f.Type != protocol.FrameError || protocol.DecodeJSON(f, &e) != nil || e.Code != protocol.ExitHostrunError {
		t.Fatalf("frame %v %+v, err %v; want an error %d", f.Type, e, err, protocol.ExitHostrunError)
	}
}

// An arm on the container's socket is refused and changes nothing: it
// neither forges the warning of a change not allowed nor ends the one an
// arm of up reported.
func TestAnArmOnTheContainersSocketChangesNoWarning(t *testing.T) {
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	digest := rules.ContentDigest([]byte(echoRules))
	h.armOnContainerSocket(t, protocol.Arm{NotAllowed: &protocol.ArmNotAllowed{Digest: digest, Settings: armedSettings}})
	noWarning(t, h)

	h.armNotAllowed(t, digest, armedSettings)
	h.armOnContainerSocket(t, protocol.Arm{ConfigDigest: digest})
	want := result{stdout: "old\n", stderr: "hostrun: warning: " + changeNotAllowedText(digest) + "\n"}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// armNotAllowedLog is how the daemon logs a change an arm of up reported
// not allowed, of the given digest, whose settings differ from the
// daemon's or not.
func armNotAllowedLog(digest string, settingsDiffer bool) string {
	return "msg=\"hostrunner up reported a change not allowed: the daemon keeps the old rules and settings until `hostrunner allow`\" digest=" +
		digest + " settings_differ=" + strconv.FormatBool(settingsDiffer)
}

// A change an arm of up reports not allowed is logged, with its digest
// and whether its settings differ, and shown in the session serving the
// daemon, in the words of hostrun's warning.
func TestArmedChangeNotAllowedIsLoggedAndShownInTheSession(t *testing.T) {
	digest := rules.ContentDigest([]byte(touchRules))
	for _, tt := range []struct {
		name     string
		settings map[string]string
		differ   bool
		notice   string
	}{
		{"other settings", armedSettings, true, "\n" + changeNotAllowedText(digest) + "\n"},
		{"the daemon's settings", allowSettings, false, notAllowedNotice(digest)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var log lockedBuffer
			a := newAllowStore(t)
			h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
			s := h.startSession(t)
			if !tt.differ {
				// With the daemon's own settings, up reports the rules file
				// as it found it: holding the edit.
				writeRules(t, h.rulesFile, touchRules)
			}
			h.armNotAllowed(t, digest, tt.settings)
			waitForLog(t, &log, armNotAllowedLog(digest, tt.differ))
			s.waitFor(t, tt.notice)
		})
	}
}

// A change an arm reports not allowed whose allow cannot be checked is
// shown in the session as not checked.
func TestArmedChangeNotCheckedIsShownInTheSession(t *testing.T) {
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules, daemon.WithSettings(allowSettings),
		daemon.WithAllowed(func(allow.Scope) (bool, error) { return false, errors.New("the allow store is broken") }))
	s := h.startSession(t)
	digest := rules.ContentDigest([]byte(touchRules))
	h.armNotAllowed(t, digest, armedSettings)
	s.waitFor(t, "\n"+changeNotCheckedText(digest)+"\n")
}

// An edit the daemon refused is shown in the session when it is first
// refused; an arm of up that reports the same edit not allowed, with the
// daemon's own settings, shows it no more, though a later edit is shown.
func TestArmedEditTheDaemonRefusedIsShownInTheSessionOnce(t *testing.T) {
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	s := h.startSession(t)
	writeRules(t, h.rulesFile, touchRules)
	digest := rules.ContentDigest([]byte(touchRules))
	s.waitFor(t, notAllowedNotice(digest))
	h.armNotAllowed(t, digest, allowSettings)
	// A later edit is told after the arm's notice, if there is one.
	writeRules(t, h.rulesFile, handDate)
	got := s.waitFor(t, notAllowedNotice(rules.ContentDigest([]byte(handDate))))
	if strings.Contains(got, digest) {
		t.Fatalf("the session shows the edit again after the arm: %q", got)
	}
}

// An edit the daemon refused while no session served is shown in the
// session that starts serving later, and an arm of up that reports the same
// edit not allowed, with the daemon's own settings, shows it no more: the
// session is shown it once.
func TestArmedEditRefusedBeforeTheSessionStartedIsShownInTheSessionOnce(t *testing.T) {
	h, _, _, digest := pendingEdit(t)
	s := h.startSession(t)
	s.waitFor(t, notAllowedNotice(digest))
	h.armNotAllowed(t, digest, allowSettings)
	// A later edit is told after the arm's notice, if there is one.
	writeRules(t, h.rulesFile, handDate)
	got := s.waitFor(t, notAllowedNotice(rules.ContentDigest([]byte(handDate))))
	if strings.Contains(got, digest) {
		t.Fatalf("the session shows the edit again after the arm: %q", got)
	}
}

// replaceRules replaces the rules file at path, at once, with a symbolic
// link to target, or with a file holding content when target is "", as an
// editor that saves by rename does.
func replaceRules(t *testing.T, path, target, content string) {
	t.Helper()
	tmp := path + ".new"
	var err error
	if target != "" {
		err = os.Symlink(target, tmp)
	} else {
		err = os.WriteFile(tmp, []byte(content), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// An arm of up that reports an edit not allowed, with the daemon's own
// settings, before the daemon refused the edit itself is noticed once: the
// daemon refusing the same edit at a later look notices nothing more. Until
// the arm is made, the rules file is a link to itself, which the daemon
// cannot read, so its look at the edit comes after the arm.
func TestArmedEditTheDaemonRefusesLaterIsNoticedOnce(t *testing.T) {
	var log lockedBuffer
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	conn := helloThenServe(t, h, func() {}, readsNotAllowedOnly)
	replaceRules(t, h.rulesFile, filepath.Base(h.rulesFile), "")
	digest := rules.ContentDigest([]byte(touchRules))
	h.armNotAllowed(t, digest, allowSettings)
	replaceRules(t, h.rulesFile, "", touchRules)
	waitForLog(t, &log, notAllowedLog+h.rulesFile+" digest="+digest)
	// A later edit is noticed after any notice of the first one.
	later := rules.ContentDigest([]byte(handDate))
	writeRules(t, h.rulesFile, handDate)
	checkNotices(t, noticesUntil(t, conn, later), protocol.RulesNotAllowed{Digest: digest}, protocol.RulesNotAllowed{Digest: later})
}

// A change an arm of up reports not allowed while no session serves is
// noticed to a session that starts serving later, unless the allow store
// allows it by then.
func TestArmedChangeIsNoticedToASessionStartingLater(t *testing.T) {
	t.Run("not allowed", func(t *testing.T) {
		h, _, digest := startArmedDaemon(t)
		conn := helloThenServe(t, h, func() {}, readsNotAllowedOnly)
		checkNotices(t, noticesBeforeStandingList(t, conn), protocol.RulesNotAllowed{Digest: digest, Settings: true})
	})
	t.Run("allowed since", func(t *testing.T) {
		h, a, digest := startArmedDaemon(t)
		if _, err := a.store.Allow(a.workspace, allow.Target{RulesFile: h.rulesFile, Scope: allow.Scope{Digest: digest, Settings: armedSettings}}); err != nil {
			t.Fatal(err)
		}
		conn := helloThenServe(t, h, func() {}, readsNotAllowedOnly)
		checkNotices(t, noticesBeforeStandingList(t, conn))
	})
}

// Arms of up that report the same change not allowed again notice it once.
func TestTheSameArmedChangeIsNoticedOnce(t *testing.T) {
	a := newAllowStore(t)
	h := a.startDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	conn := helloThenServe(t, h, func() {}, readsNotAllowedOnly)
	digest := rules.ContentDigest([]byte(echoRules))
	for range 3 {
		h.armNotAllowed(t, digest, armedSettings)
	}
	checkNotices(t, noticesBeforeStandingList(t, conn), protocol.RulesNotAllowed{Digest: digest, Settings: true})
}

// An edit not allowed that the daemon refused and an arm of up reported,
// with the daemon's own settings, is noticed again when it is made again
// after the rules file went back to the rules served.
func TestArmedEditUndoneIsNoticedAgainWhenMadeAgain(t *testing.T) {
	h, _, log, digest := pendingEdit(t)
	conn := helloThenServe(t, h, func() {}, readsNotAllowedOnly)
	checkNotices(t, noticesUntil(t, conn, digest), protocol.RulesNotAllowed{Digest: digest})
	h.armNotAllowed(t, digest, allowSettings)
	writeRules(t, h.rulesFile, echoRules)
	waitForLog(t, log, `msg="rules change undone: the file holds the rules served" file=`+h.rulesFile)
	writeRules(t, h.rulesFile, touchRules)
	checkNotices(t, noticesUntil(t, conn, digest), protocol.RulesNotAllowed{Digest: digest})
}
