package client_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// grantStanding asks for argv with --ask and answers "for the daemon's
// lifetime", typing "yes" when the request is guarded; it returns what
// hostrun printed.
func (h harness) grantStanding(t *testing.T, s *session, guarded bool, argv ...string) result {
	t.Helper()
	ch := h.ask(t, argv...)
	prompt := answerPromptText
	if guarded {
		prompt = guardedPromptText
	}
	s.waitFor(t, prompt)
	s.answer(t, "l")
	if guarded {
		s.waitFor(t, `Type "yes" to approve it for the daemon's lifetime: `)
		s.answer(t, "yes")
	}
	got := awaitResult(t, ch)
	s.waitFor(t, "approved for the daemon's lifetime\n")
	return got
}

// A standing approval lets the same argv from the same container
// directory through, with or without --ask and printing nothing of
// hostrun's own; another argv or directory is refused as before.
func TestStandingApprovalLetsTheSameCommandThrough(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	got := h.grantStanding(t, s, false, "touch", "marker")
	if want := (result{stderr: waitingNoRule + "hostrun: approved on the host: for the daemon's lifetime\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	waitForLog(t, &log, `msg="standing approval granted" standing=1 argv="[touch marker]"`)
	waitForLog(t, &log, `approved=lifetime`)
	marker := filepath.Join(h.hostRoot, "marker")
	for _, argv := range [][]string{{"touch", "marker"}, {"--ask", "touch", "marker"}} {
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if got := h.run(t, containerRoot, "", argv...); got != (result{}) {
			t.Fatalf("%v: got %+v, want it to run silently", argv, got)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("%v did not run: %v", argv, err)
		}
	}
	waitForLog(t, &log, `approved=standing`)
	for _, tt := range []struct {
		cwd  string
		argv []string
	}{
		{containerRoot + "/sub", []string{"touch", "marker"}},
		{containerRoot, []string{"touch", "other"}},
	} {
		if got := h.run(t, tt.cwd, "", tt.argv...); got.code != protocol.ExitRejected {
			t.Errorf("%s %v: got %+v, want it refused", tt.cwd, tt.argv, got)
		}
	}
	assertNotRun(t, h, "sub/marker")
	assertNotRun(t, h, "other")
}

// A guarded refusal gets a standing approval after one "yes", when it is
// granted; a dry run reports what lets it through.
func TestStandingApprovalForAGuardedRefusalConfirmsOnce(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, guardedRules))
	s := h.startSession(t)
	if got := h.grantStanding(t, s, true, "printf", "other"); got.code != 0 || got.stdout != "other" {
		t.Fatalf("got %+v, want printf's output", got)
	}
	if got := h.run(t, containerRoot, "", "printf", "other"); got != (result{stdout: "other"}) {
		t.Fatalf("got %+v, want it to run without asking", got)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "printf", "other"); got != (result{stderr: "hostrun: dry run: allowed by a standing approval, injects secrets: token\n"}) {
		t.Fatalf("dry run: got %+v", got)
	}
}

// A standing approval lives in the daemon: it works with no session and
// survives an arm that keeps the daemon.
func TestStandingApprovalWorksWithoutASessionAndSurvivesAnArm(t *testing.T) {
	h := startDaemonWith(t, slog.New(slog.DiscardHandler), policy(t, echoOnly), daemon.WithArmHandler(func(protocol.Arm, string) bool { return false }))
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "marker")
	s.cancel()
	<-s.ended
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.FrameArm, protocol.Arm{Version: protocol.Version}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameArmed {
		t.Fatalf("frame %v, err %v; want armed", f.Type, err)
	}
	if err := os.Remove(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got != (result{}) {
		t.Fatalf("got %+v, want it to run", got)
	}
}

// The session lists the standing approvals and revokes one by its number;
// the log records the revocation.
func TestStandingApprovalsAreListedAndRevoked(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, guardedRules))
	s := h.startSession(t)
	h.grantStanding(t, s, true, "touch", "+marker")
	s.waitFor(t, "> ")
	s.answer(t, "standing")
	s.waitFor(t, `  1  "touch" "+marker"   in `+containerRoot+"   guarded\n")
	s.waitFor(t, "> ")
	s.answer(t, "revoke 2")
	s.waitFor(t, "revoke 2: no standing approval 2\n")
	s.waitFor(t, "> ")
	s.answer(t, "revoke 1")
	s.waitFor(t, "revoked 1\n")
	waitForLog(t, &log, `msg="standing approval revoked" standing=1`)
	s.waitFor(t, "> ")
	s.answer(t, "standing")
	s.waitFor(t, "no standing approvals\n")
	if got := h.run(t, containerRoot, "", "touch", "+marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused once revoked", got)
	}
}

// pathRules allow cat no flags, and check its arguments as workspace
// files.
const pathRules = `
rules:
  - command: cat
    flags:
      allow: []
    positional:
      path: open
`

// A standing approval passes as given only the path failure the human saw:
// the same argument failing for another reason is refused.
func TestStandingApprovalPassesOnlyThePathFailureSeen(t *testing.T) {
	h := startDaemon(t, policy(t, pathRules))
	s := h.startSession(t)
	if got := h.grantStanding(t, s, true, "cat", "-n", "missing"); got.code == 0 {
		t.Fatalf("got %+v, want cat to fail on the missing file", got)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.hostRoot, "missing")); err != nil {
		t.Fatal(err)
	}
	got := h.run(t, containerRoot, "", "cat", "-n", "missing")
	if got.code != protocol.ExitRejected || !strings.Contains(got.stderr, `argument "missing" is not a workspace file: it leads outside the workspace`) || strings.Contains(got.stdout, "secret") {
		t.Fatalf("got %+v, want the new path failure refused", got)
	}
}

// A command with a standing approval whose path check fails only later is
// refused with a hint, and --ask asks the human again.
func TestStandingApprovalWithANewPathFailureIsAskedAgain(t *testing.T) {
	h := startDaemon(t, policy(t, pathRules))
	inside := filepath.Join(h.hostRoot, "inside")
	if err := os.WriteFile(inside, []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	h.grantStanding(t, s, false, "cat", "-n", "inside")
	if err := os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), inside); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t, containerRoot, "", "cat", "-n", "inside"); got.code != protocol.ExitRejected || !strings.Contains(got.stderr, "(ask for an approval: hostrun --ask …; guarded") {
		t.Fatalf("got %+v, want a refusal hinting at --ask", got)
	}
	ch := h.ask(t, "cat", "-n", "inside")
	block := s.waitFor(t, guardedPromptText)
	if !strings.Contains(block, `⚠ guarded: path check: argument "inside"`) {
		t.Fatalf("the block %q lacks the new path failure", block)
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	if got := awaitResult(t, ch); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want the human's refusal", got)
	}
}

// Granting a standing approval the daemon holds already keeps the one it
// has, so revoking it ends it.
func TestStandingApprovalIsGrantedOnce(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	first := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	second := h.ask(t, "touch", "marker")
	s.waitFor(t, "\a")
	s.answer(t, "l")
	awaitResult(t, first)
	s.waitFor(t, answerPromptText)
	s.answer(t, "l")
	awaitResult(t, second)
	s.waitFor(t, "> ")
	s.answer(t, "standing")
	list := s.waitFor(t, "> ")
	if !strings.Contains(list, `  1  "touch" "marker"`) || strings.Contains(list, "  2  ") {
		t.Fatalf("the list %q, want approval 1 alone", list)
	}
}

// Revoking a standing approval leaves the command it let through running.
func TestStandingRevokeLeavesARunningCommand(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	argv := []string{"sh", "-x", "-c", "sleep 0.5; echo done"}
	h.grantStanding(t, s, false, argv...)
	ch := make(chan result, 1)
	go func() { ch <- h.run(t, containerRoot, "", argv...) }()
	time.Sleep(100 * time.Millisecond)
	s.waitFor(t, "> ")
	s.answer(t, "revoke 1")
	s.waitFor(t, "revoked 1\n")
	if got := awaitResult(t, ch); got.code != 0 || got.stdout != "done\n" {
		t.Fatalf("got %+v, want the command to finish", got)
	}
}

// A command's outcome that arrives while a request is shown is printed,
// and the request's prompt asked again; a refused list does not end the
// session.
func TestSessionReprintsThePromptAfterACommandsOutcome(t *testing.T) {
	socket, served := fakeDaemon(t)
	s := startSessionOn(t, socket)
	conn := <-served
	s.waitFor(t, answerPromptText)
	_ = protocol.WriteJSON(conn, protocol.FrameStandingList, protocol.StandingList{Approvals: []protocol.Standing{{N: 3, Argv: []string{"touch", "x"}, Cwd: containerRoot}}})
	s.waitFor(t, `  3  "touch" "x"   in `+containerRoot+"\n"+answerPromptText)
	_ = protocol.WriteJSON(conn, protocol.FrameStandingList, protocol.StandingList{Error: "not now"})
	s.waitFor(t, "standing: not now\n"+answerPromptText)
	s.answer(t, "o")
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameAnswer {
		t.Fatalf("frame %v, err %v; want the answer", f.Type, err)
	}
}

// A standing approval goes with the daemon: once it steps aside, the next
// daemon for the devcontainer refuses the command again.
func TestStandingApprovalIsGoneAfterTheDaemonStepsAside(t *testing.T) {
	h := startDaemonWith(t, slog.New(slog.DiscardHandler), policy(t, echoOnly), daemon.WithArmHandler(func(protocol.Arm, string) bool { return true }))
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "marker")
	if err := os.Remove(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatal(err)
	}
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.FrameArm, protocol.Arm{Version: protocol.Version}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(conn)
	var armed protocol.Armed
	if err != nil || f.Type != protocol.FrameArmed || protocol.DecodeJSON(f, &armed) != nil || !armed.Restart {
		t.Fatalf("frame %v %+v, err %v; want the daemon to step aside", f.Type, armed, err)
	}
	// Stepping aside, the daemon stops; stop waits for it.
	h.stop()
	next := startHarnessAt(t, slog.New(slog.DiscardHandler), h.runtimeDir, func(string) (daemon.Policy, []daemon.Option) {
		return policy(t, echoOnly), nil
	})
	if got, want := next.run(t, containerRoot, "", "touch", "marker"), (result{code: protocol.ExitRejected, stderr: "hostrun: no rule allows \"touch marker\"\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertNotRun(t, next, "marker")
}

// A session that takes the daemon over sees the standing approvals the
// one before it granted: they live in the daemon.
func TestStandingApprovalsAreListedAfterATakeOver(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	first := h.startSession(t)
	h.grantStanding(t, first, false, "touch", "marker")
	second := launchSession(t, approval.Config{Socket: h.approvalSocket})
	second.waitFor(t, takeOverPromptText)
	second.answer(t, "y")
	first.waitFor(t, "taken over by another session\n")
	second.waitFor(t, "> ")
	second.answer(t, "standing")
	second.waitFor(t, `  1  "touch" "marker"   in `+containerRoot+"\n")
}
