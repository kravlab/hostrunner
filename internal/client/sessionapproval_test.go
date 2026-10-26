package client_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// grantSession grants argv a session approval (see grantAs).
func (h harness) grantSession(t *testing.T, s *session, guarded bool, argv ...string) result {
	t.Helper()
	return h.grantAs(t, s, sessionAnswer, guarded, argv...)
}

// approvedSession is the line hostrun prints for the answer "session".
const approvedSession = "hostrun: approved on the host: session (until the approval session ends)\n"

// A session approval lets the same argv from the same container directory
// through, with or without --ask and printing nothing of hostrun's own;
// the log and a dry run say what let it through.
func TestSessionApprovalLetsTheSameCommandThrough(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	if got, want := h.grantSession(t, s, false, "touch", "marker"), (result{stderr: waitingNoRule + approvedSession}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	waitForLog(t, &log, `msg="approval answered" approval_id=1 scope=session`)
	waitForLog(t, &log, `msg="session approval granted" approval=1 argv="[touch marker]"`)
	waitForLog(t, &log, `approved=session approval_id=1`)
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
	waitForLog(t, &log, `approved=session code=0`)
	if got := h.run(t, containerRoot, "", "--dry-run", "touch", "marker"); got != (result{stderr: "hostrun: dry run: allowed by a session approval\n"}) {
		t.Fatalf("dry run: got %+v", got)
	}
}

// A guarded refusal gets a session approval after one "yes", when it is
// granted.
func TestSessionApprovalForAGuardedRefusalConfirmsOnce(t *testing.T) {
	h := startDaemon(t, policy(t, guardedRules))
	s := h.startSession(t)
	if got := h.grantSession(t, s, true, "touch", "+marker"); got.code != 0 {
		t.Fatalf("got %+v, want it approved", got)
	}
	if err := os.Remove(filepath.Join(h.hostRoot, "+marker")); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t, containerRoot, "", "touch", "+marker"); got != (result{}) {
		t.Fatalf("got %+v, want it to run without asking", got)
	}
}

// Granting either kind for an argv and directory the daemon holds an
// approval for keeps its number and takes the new kind.
func TestApprovalGrantedAgainTakesTheNewKind(t *testing.T) {
	tests := []struct {
		name, first, second, want string
	}{
		{"session, then standing", "s", "d", "  1  touch marker   in " + containerRoot + "\n"},
		{"standing, then session", "d", "s", "  1  touch marker   in " + containerRoot + "   session\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, echoOnly))
			s := h.startSession(t)
			first := h.ask(t, "touch", "marker")
			s.waitFor(t, answerPromptText)
			second := h.ask(t, "touch", "marker")
			s.waitFor(t, "\a")
			s.answer(t, tt.first)
			awaitResult(t, first)
			s.waitFor(t, answerPromptText)
			s.answer(t, tt.second)
			awaitResult(t, second)
			s.waitFor(t, "> ")
			s.answer(t, "standing")
			list := s.waitFor(t, "> ")
			if !strings.Contains(list, tt.want) || strings.Contains(list, "  2  ") {
				t.Fatalf("the list %q, want %q alone", list, tt.want)
			}
		})
	}
}

// A command with a session approval whose path check fails only later is
// refused with a hint, and --ask asks the human again, as for a standing
// approval.
func TestSessionApprovalWithANewPathFailureIsAskedAgain(t *testing.T) {
	h := startDaemon(t, policy(t, pathRules))
	inside := filepath.Join(h.hostRoot, "inside")
	if err := os.WriteFile(inside, []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	h.grantSession(t, s, false, "cat", "-n", "inside")
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
	if block := s.waitFor(t, guardedPromptText); !strings.Contains(block, `⚠ guarded: path check: argument "inside"`) {
		t.Fatalf("the block %q lacks the new path failure", block)
	}
	s.refuse(t)
	if got := awaitResult(t, ch); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want the human's refusal", got)
	}
}

// The session's `standing` lists session and standing approvals together,
// a session approval marked, and `revoke` ends either.
func TestSessionAndStandingApprovalsAreListedTogether(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "a")
	s.waitFor(t, "> ")
	h.grantSession(t, s, false, "touch", "b")
	s.waitFor(t, "> ")
	s.answer(t, "standing")
	list := s.waitFor(t, "> ")
	for _, want := range []string{"  1  touch a   in " + containerRoot + "\n", "  2  touch b   in " + containerRoot + "   session\n"} {
		if !strings.Contains(list, want) {
			t.Errorf("the list %q lacks %q", list, want)
		}
	}
	s.answer(t, "revoke 2")
	s.waitFor(t, "revoked 2\n")
	waitForLog(t, &log, `msg="session approval revoked" approval=2`)
	if got := h.run(t, containerRoot, "", "touch", "b"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused once revoked", got)
	}
	if got := h.run(t, containerRoot, "", "touch", "a"); got != (result{}) {
		t.Fatalf("got %+v, want the standing approval to stay", got)
	}
}

// A session that leaves ends the session approvals it granted, and only
// those: a standing approval stays.
func TestSessionApprovalEndsWhenTheSessionLeaves(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	h.grantSession(t, s, false, "touch", "marker")
	s.waitFor(t, "> ")
	h.grantStanding(t, s, false, "touch", "other")
	s.cancel()
	<-s.ended
	waitForLog(t, &log, `msg="session approvals ended" count=1 reason="session left"`)
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused once the session left", got)
	}
	if got := h.run(t, containerRoot, "", "touch", "other"); got != (result{}) {
		t.Fatalf("got %+v, want the standing approval to stay", got)
	}
}

// `daemons` leaves the daemon: the session approvals end, and serving it
// again starts with none.
func TestSessionApprovalEndsWithDaemons(t *testing.T) {
	var log lockedBuffer
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), base)
	s := startSessionWith(t, approval.Config{Root: root, Cwd: h.hostRoot})
	s.waitForListed(t)
	s.answer(t, "")
	s.waitFor(t, "serving "+h.hostRoot)
	h.grantSession(t, s, false, "touch", "marker")
	s.waitFor(t, "> ")
	s.answer(t, "daemons")
	s.waitForListed(t)
	waitForLog(t, &log, `msg="session approvals ended" count=1 reason="session left"`)
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused once the session left the daemon", got)
	}
	s.answer(t, "")
	s.waitFor(t, "serving "+h.hostRoot)
	s.waitFor(t, "> ")
	s.answer(t, "standing")
	s.waitFor(t, "no approvals\n")
}

// A session taken over ends the session approvals it granted; the one
// that took over starts with none.
func TestSessionApprovalEndsOnATakeOver(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	first := h.startSession(t)
	h.grantSession(t, first, false, "touch", "marker")
	second := launchSession(t, approval.Config{Socket: h.approvalSocket})
	second.waitFor(t, takeOverPromptText)
	second.answer(t, "y")
	first.waitFor(t, "taken over by another session\n")
	waitForLog(t, &log, `msg="session approvals ended" count=1 reason="taken over"`)
	second.waitFor(t, "> ")
	second.answer(t, "standing")
	second.waitFor(t, "no approvals\n")
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused once the session was taken over", got)
	}
}

// A session the daemon drops (it stopped reading) ends the session
// approvals it granted.
func TestSessionApprovalEndsWhenTheSessionIsDropped(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	conn := h.serveRaw(t)
	ch := h.ask(t, "touch", "marker")
	f, err := protocol.ReadFrame(conn)
	if err != nil || f.Type != protocol.FrameApprovalRequest {
		t.Fatalf("frame %v, err %v; want the request", f.Type, err)
	}
	var req protocol.ApprovalRequest
	if err := protocol.DecodeJSON(f, &req); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteJSON(conn, protocol.FrameAnswer, protocol.Answer{ID: req.ID, Scope: protocol.ScopeSession}); err != nil {
		t.Fatal(err)
	}
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved", got)
	}
	// The session stops reading: a request large enough to fill the
	// socket's buffer is not taken, and the daemon drops it.
	awaitResult(t, h.ask(t, "touch", strings.Repeat("x", 400*1024)))
	waitForLog(t, &log, `msg="session approvals ended" count=1 reason="session left"`)
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused once the session was dropped", got)
	}
}
