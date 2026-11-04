package client_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

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
	if _, err := a.store.Allow(a.workspace, h.rulesFile, digest, armedSettings); err != nil {
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
	broken := daemon.WithAllowed(func(string, map[string]string) (bool, error) {
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
