package client_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// rereadEvery is how often the daemons of these tests look at their
// rules file, short to keep the tests fast.
const rereadEvery = 10 * time.Millisecond

const (
	echoRules  = "rules:\n  - command: echo\n    args: any\n  - command: sh\n    flags:\n      allow: [-c]\n"
	touchRules = echoRules + "  - command: touch\n    args: any\n"
)

// startRereadingDaemon starts a daemon serving the rules file content
// that rereads the file by itself once it changes.
func startRereadingDaemon(t *testing.T, log *slog.Logger, content string, opts ...daemon.Option) harness {
	t.Helper()
	return startRulesDaemon(t, log, content, false, append([]daemon.Option{daemon.WithRereadEvery(rereadEvery)}, opts...)...)
}

// writeRules replaces the rules file at path with content, as an editor
// writing in place does.
func writeRules(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitForCode runs argv on h until hostrun exits with code, for 5 s at
// most, and returns the last result.
func (h harness) waitForCode(t *testing.T, code int, argv ...string) result {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := h.run(t, containerRoot, "", argv...)
		if got.code == code {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q: got %+v, want code %d", argv, got, code)
		}
		time.Sleep(rereadEvery)
	}
}

// A rule added to the rules file by hand takes effect by itself, and the
// daemon logs the reread with the new digest.
func TestRereadTakesAHandEdit(t *testing.T) {
	var log lockedBuffer
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("before the edit: got %+v, want it refused", got)
	}
	writeRules(t, h.rulesFile, touchRules)
	h.waitForCode(t, 0, "touch", "marker")
	digest := rules.ContentDigest([]byte(touchRules))
	waitForLog(t, &log, `msg="rules reread" file=`+h.rulesFile+" digest="+digest)
}

// A file replaced by a rename, as many editors and git save it, is
// reread.
func TestRereadTakesAFileReplacedByRename(t *testing.T) {
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	tmp := filepath.Join(filepath.Dir(h.rulesFile), "hostrun.yaml.tmp")
	writeRules(t, tmp, touchRules)
	if err := os.Rename(tmp, h.rulesFile); err != nil {
		t.Fatal(err)
	}
	h.waitForCode(t, 0, "touch", "marker")
}

// A rules file reached through a symbolic link is reread when the link
// is swapped for one to another file.
func TestRereadFollowsALinkSwapped(t *testing.T) {
	h := startRulesDaemonWith(t, slog.New(slog.DiscardHandler), false, func(rulesFile string) {
		dir := filepath.Dir(rulesFile)
		writeRules(t, filepath.Join(dir, "a.yaml"), echoRules)
		if err := os.Symlink("a.yaml", rulesFile); err != nil {
			t.Fatal(err)
		}
	}, daemon.WithRereadEvery(rereadEvery))
	dir := filepath.Dir(h.rulesFile)
	writeRules(t, filepath.Join(dir, "b.yaml"), touchRules)
	if err := os.Symlink("b.yaml", filepath.Join(dir, "link.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "link.tmp"), h.rulesFile); err != nil {
		t.Fatal(err)
	}
	h.waitForCode(t, 0, "touch", "marker")
}

// A rules file reached through a symbolic link is reread when the file
// the link names is edited in place.
func TestRereadFollowsALinkTargetEdited(t *testing.T) {
	h := startRulesDaemonWith(t, slog.New(slog.DiscardHandler), false, func(rulesFile string) {
		writeRules(t, filepath.Join(filepath.Dir(rulesFile), "a.yaml"), echoRules)
		if err := os.Symlink("a.yaml", rulesFile); err != nil {
			t.Fatal(err)
		}
	}, daemon.WithRereadEvery(rereadEvery))
	writeRules(t, filepath.Join(filepath.Dir(h.rulesFile), "a.yaml"), touchRules)
	h.waitForCode(t, 0, "touch", "marker")
}

// An invalid rules file keeps the old rules and is reported once, however
// often the file is touched without changing; the next valid save is
// reread.
func TestRereadKeepsTheOldRulesOfAnInvalidFile(t *testing.T) {
	var log lockedBuffer
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	writeRules(t, h.rulesFile, "rules: [")
	const failed = `msg="rules reread failed: the old rules stay" file=`
	waitForLog(t, &log, failed+h.rulesFile)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(h.rulesFile, later, later); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * rereadEvery)
	if n := strings.Count(log.String(), failed); n != 1 {
		t.Fatalf("the failure is logged %d times, want once:\n%s", n, log.String())
	}
	if got := h.run(t, containerRoot, "", "echo", "old"); got != (result{stdout: "old\n"}) {
		t.Fatalf("a command of the old rules: got %+v, want it to run", got)
	}
	writeRules(t, h.rulesFile, touchRules)
	h.waitForCode(t, 0, "touch", "marker")
}

// A deleted rules file denies every command, as a daemon started without
// one does.
func TestRereadOfADeletedFileDeniesEverything(t *testing.T) {
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	if err := os.Remove(h.rulesFile); err != nil {
		t.Fatal(err)
	}
	h.waitForCode(t, protocol.ExitRejected, "echo", "hi")
}

// Commands running when the rules are reread keep running.
func TestRereadLeavesRunningCommands(t *testing.T) {
	var log lockedBuffer
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	h.startSleeper(t)
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, `msg="rules reread"`)
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	if got := c.running(t).Commands; len(got) != 1 || got[0].Argv[0] != "sh" {
		t.Fatalf("the daemon runs %+v after the reread, want the sleeper", got)
	}
}

// Standing and session approvals stay through a reread.
func TestRereadKeepsStandingAndSessionApprovals(t *testing.T) {
	var log lockedBuffer
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "-a", "one")
	h.grantSession(t, s, false, "touch", "-a", "two")
	writeRules(t, h.rulesFile, echoRules+"# edited\n")
	waitForLog(t, &log, `msg="rules reread"`)
	for _, name := range []string{"one", "two"} {
		if got := h.run(t, containerRoot, "", "touch", "-a", name); got != (result{}) {
			t.Fatalf("touch -a %s: got %+v, want its approval to let it through", name, got)
		}
	}
}

// A dry run answers against the rules reread.
func TestRereadAnswersDryRuns(t *testing.T) {
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	writeRules(t, h.rulesFile, touchRules)
	got := h.waitForCode(t, 0, "--dry-run", "touch", "marker")
	if want := "hostrun: dry run: allowed by rule \"touch\"\n"; got.stderr != want {
		t.Fatalf("got %+v, want stderr %q", got, want)
	}
	assertNotRun(t, h, "marker")
}

// An arm after a reread gets the digest of the rules reread, so
// `hostrunner up` validating the edited file keeps the daemon.
func TestRereadArmGetsTheNewDigest(t *testing.T) {
	digests := make(chan string, 1)
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules,
		daemon.WithArmHandler(func(_ protocol.Arm, digest string) bool {
			digests <- digest
			return false
		}))
	writeRules(t, h.rulesFile, touchRules)
	h.waitForCode(t, 0, "touch", "marker")
	conn, err := h.transport.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	want := rules.ContentDigest([]byte(touchRules))
	if err := protocol.WriteJSON(conn, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: want}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameArmed {
		t.Fatalf("frame %v, err %v; want armed", f.Type, err)
	}
	select {
	case got := <-digests:
		if got != want {
			t.Fatalf("the arm got digest %q, want that of the reread rules %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the arm handler was not called")
	}
}

// The session serving the daemon says when the rules were reread, and
// when the file changed but does not load.
func TestRereadIsShownInTheSession(t *testing.T) {
	h := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	s := h.startSession(t)
	writeRules(t, h.rulesFile, "rules: [")
	s.waitFor(t, "\nthe rules file changed, but the daemon cannot reread it, so the old rules stay: ")
	s.waitFor(t, "> ")
	writeRules(t, h.rulesFile, touchRules)
	s.waitFor(t, "\nthe rules file changed; the daemon reread it\n")
	s.waitFor(t, "> ")
	h.waitForCode(t, 0, "touch", "marker")
}

// A session that does not say it reads the notices of a reread, as one
// older than them, gets none: the next frame it gets answers what it
// asked, and none follows. The notice goes out just after the reread is
// logged, so the test waits a while for one before it is sure.
func TestRereadIsNotSentToAnOlderSession(t *testing.T) {
	var log lockedBuffer
	h := startRereadingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), echoRules)
	conn := h.serveRaw(t)
	writeRules(t, h.rulesFile, touchRules)
	waitForLog(t, &log, `msg="rules reread"`)
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStanding}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameStandingList {
		t.Fatalf("frame %v, err %v; want the standing list", f.Type, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * rereadEvery))
	if f, err := protocol.ReadFrame(conn); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("frame %v, err %v; want none", f.Type, err)
	}
}

// Adding a rule after a hand edit the daemon, which rereads by itself, has
// not reread yet, has it reread the file without asking: the proposal is
// computed against the rules it then serves, and the hand edit goes live.
func TestAddRuleRereadsAHandEditWithoutAsking(t *testing.T) {
	var log lockedBuffer
	// Rereading by itself, but not before the test is over.
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), loadedTouch, false, daemon.WithRereadEvery(time.Hour))
	writeRules(t, h.rulesFile, handDate)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-m", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	block := s.waitFor(t, addPromptText)
	if strings.Contains(block, rereadPromptText) {
		t.Errorf("the session asked to reread:\n%s", block)
	}
	if !strings.Contains(block, "+      allow: [-a, -m]\n") {
		t.Errorf("the proposal %q does not widen the rule", block)
	}
	waitForLog(t, &log, `msg="hand-edited rules reread"`)
	s.answer(t, "w")
	s.waitFor(t, ruleAddedText+" · request 1 runs under it\n")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it to run under the rule", got)
	}
	if got := h.run(t, containerRoot, "", "date"); got.code != 0 {
		t.Fatalf("a command of the hand edit: got %+v, want it to run", got)
	}
}

// A daemon without WithRereadEvery leaves a hand edit out.
func TestNoRereadLeavesAHandEditOut(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoRules, false)
	writeRules(t, h.rulesFile, touchRules)
	time.Sleep(20 * rereadEvery)
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused", got)
	}
}

// DaemonInfo says whether the daemon rereads its rules by itself, and
// whether it does not: a daemon older than both says neither.
func TestDaemonInfoSaysWhetherTheDaemonRereads(t *testing.T) {
	rereading := startRereadingDaemon(t, slog.New(slog.DiscardHandler), echoRules)
	if _, info := rereading.stopper(t, protocol.ApprovalVersion); !info.Rereads || info.NoReread {
		t.Fatalf("a rereading daemon: got Rereads=%v NoReread=%v, want true and false", info.Rereads, info.NoReread)
	}
	notRereading := startRulesDaemon(t, slog.New(slog.DiscardHandler), echoRules, false)
	if _, info := notRereading.stopper(t, protocol.ApprovalVersion); info.Rereads || !info.NoReread {
		t.Fatalf("a daemon that does not reread: got Rereads=%v NoReread=%v, want false and true", info.Rereads, info.NoReread)
	}
}
