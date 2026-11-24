package client_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
)

// requestCheck is a per-request mount check (see daemon.WithMountCheck)
// whose results a test sets, and which counts its calls.
type requestCheck struct {
	mu      sync.Mutex
	calls   int
	results []error // the result of each call in turn; the last one repeats; none passes
}

// requirementsPass is the requirements stage of a per-request mount check
// (see daemon.MountCheck) that every rule's mount requirements pass.
func requirementsPass([]rules.MountRequirement) error { return nil }

// check returns the result set for this call as that of its protected-path
// stage, and counts it; its requirements stage passes (see
// requirementsPass).
func (c *requestCheck) check(context.Context) (daemon.MountRequirementsCheck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if len(c.results) == 0 {
		return requirementsPass, nil
	}
	err := c.results[0]
	if len(c.results) > 1 {
		c.results = c.results[1:]
	}
	if err != nil {
		return nil, err
	}
	return requirementsPass, nil
}

// set makes the next calls return results in turn, the last one repeating.
func (c *requestCheck) set(results ...error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = results
}

// count returns how many times the check was called.
func (c *requestCheck) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// startRequestCheckingDaemon starts a daemon that checks its armed
// container's mounts with check before every request, serving echoOnly
// and logging to log; its attach-time check has passed.
func startRequestCheckingDaemon(t *testing.T, log *lockedBuffer, check *requestCheck) harness {
	t.Helper()
	h := startDaemonWith(t, slog.New(slog.NewTextHandler(log, nil)), policy(t, echoOnly), daemon.WithMountCheck(mountCheckOffHint, check.check))
	h.srv.MountChecked(time.Time{}, nil)
	return h
}

// A per-request check that fails refuses every run, dry run and --ask as
// it arrives, naming container paths only; the log names the host path.
func TestRequestMountCheckFailureRefusesEveryCommand(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	failure := writableConfig(h)
	check.set(failure)
	refusedByMountCheck(t, h, protocol.ExitRejected, mountCheckWritableText)
	var refused int
	for line := range strings.SplitSeq(log.String(), "\n") {
		if strings.Contains(line, `msg="request rejected"`) && strings.Contains(line, "code=126") && strings.Contains(line, failure.Path) {
			refused++
		}
	}
	if refused != 5 {
		t.Fatalf("%d refusals with the host path logged, want 5:\n%s", refused, log.String())
	}
}

// A per-request check that cannot run yet, as the latest arm has not
// attached, refuses every command as before the first check: a hostrun
// failure a retry outlives.
func TestRequestMountCheckPendingRefusesEveryCommand(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	check.set(daemon.ErrMountCheckPending)
	refusedByMountCheck(t, h, protocol.ExitHostrunError, mountCheckPendingText)
}

// A per-request check that passes as a run arrives and fails right before
// its command would start refuses it then: nothing runs.
func TestRequestMountCheckFailureBeforeTheStartRefusesTheCommand(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	check.set(nil, writableConfig(h))
	if got, want := h.run(t, containerRoot, "", "sh", "-c", "touch marker"), (result{code: protocol.ExitRejected, stderr: mountCheckWritableText}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if calls := check.count(); calls != 2 {
		t.Errorf("the check ran %d times, want 2", calls)
	}
	assertNotRun(t, h, "marker")
}

// A dry run makes one live check, its last: right before it would answer
// that the command is allowed, so a failure then refuses it, while a dry
// run the rules refuse is refused by them, no live check made.
func TestRequestMountCheckOfADryRunIsItsLastCheck(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	check.set(writableConfig(h))
	want := result{code: protocol.ExitRejected, stderr: mountCheckWritableText}
	if got := h.run(t, containerRoot, "", "--dry-run", "sh", "-c", "touch marker"); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if calls := check.count(); calls != 1 {
		t.Errorf("the check ran %d times for an allowed dry run, want 1", calls)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "touch", "marker"); got.code != protocol.ExitRejected || got.stderr == mountCheckWritableText {
		t.Errorf("got %+v, want it refused by the rules", got)
	}
	if calls := check.count(); calls != 1 {
		t.Errorf("the check ran %d times in all, want 1: none for a dry run the rules refuse", calls)
	}
	assertNotRun(t, h, "marker")
}

// An --ask whose per-request check passes as it arrives and fails after
// the human's answer, right before the approved run starts, is refused as
// a new request would be, once the client was told of the answer, and
// grants nothing: the same command is refused again afterwards.
func TestRequestMountCheckFailureAfterTheAnswerGrantsNothing(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	check.set(nil, writableConfig(h))
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, standingAnswer.key)
	if got, want := awaitResult(t, ch), (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: approved on the host: standing (until the daemon stops)\n" + mountCheckWritableText}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if calls := check.count(); calls != 2 {
		t.Errorf("the check ran %d times, want 2: as the --ask arrived and before its start", calls)
	}
	waitForLog(t, &log, `msg="approval not run" approval_id=1 reason="ended: the mount check failed"`)
	if strings.Contains(log.String(), "approval granted") {
		t.Fatalf("an approval was granted:\n%s", log.String())
	}
	check.set(nil)
	if got := h.run(t, containerRoot, "", "touch", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused by the rules", got)
	}
	assertNotRun(t, h, "marker")
}

// An --ask approved with standing scope is checked twice: as it arrives,
// and once after the human's answer, right before the approved run
// starts. That check passing, the approval is granted and its repeats run.
func TestRequestMountCheckAfterTheAnswerIsTheOneBeforeTheStart(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	if got := h.grantStanding(t, s, false, "touch", "marker"); got.code != 0 {
		t.Fatalf("got %+v, want it to run", got)
	}
	if calls := check.count(); calls != 2 {
		t.Errorf("the check ran %d times for the --ask, want 2: as it arrived and before its start", calls)
	}
	waitForLog(t, &log, `msg="standing approval granted"`)
	if err := os.Remove(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got != (result{}) {
		t.Fatalf("got %+v, want the repeat to run", got)
	}
	if _, err := os.Stat(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatalf("the repeat did not run: %v", err)
	}
}

// An --ask whose per-request check fails as it arrives is refused before
// it is queued: the session is shown no request.
func TestRequestMountCheckFailureRefusesAnAskBeforeItIsQueued(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	check.set(writableConfig(h))
	if got, want := awaitResult(t, h.ask(t, "touch", "marker")), (result{code: protocol.ExitRejected, stderr: mountCheckWritableText}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if strings.Contains(log.String(), "approval requested") || strings.Contains(s.out.String(), answerPromptText) {
		t.Fatalf("the request was queued:\nlog:\n%s\nsession:\n%s", log.String(), s.out.String())
	}
}

// A repeat a standing approval lets through is checked as any request:
// with or without --ask, a failure as it arrives or right before its start
// refuses it, and nothing runs.
func TestRequestMountCheckFailureRefusesARepeat(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "marker")
	marker := filepath.Join(h.hostRoot, "marker")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	failure := writableConfig(h)
	want := result{code: protocol.ExitRejected, stderr: mountCheckWritableText}
	for _, results := range [][]error{{failure}, {nil, failure}} {
		for _, argv := range [][]string{{"touch", "marker"}, {"--ask", "touch", "marker"}} {
			check.set(results...)
			if got := h.run(t, containerRoot, "", argv...); got != want {
				t.Errorf("%v %q: got %+v, want %+v", results, argv, got, want)
			}
		}
	}
	assertNotRun(t, h, "marker")
}

// The session serving the daemon is told when a per-request check starts
// failing for a path and reason: once, however many requests the same
// failure refuses, and again after a check passed, or for another failure.
func TestRequestMountCheckFailureIsNoticedOncePerChange(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	failure := writableConfig(h)
	other := writableConfig(h)
	other.ContainerPath += "/other"
	for _, err := range []error{failure, failure, nil, failure, other} {
		check.set(err)
		h.run(t, containerRoot, "", "echo", "hi")
	}
	s.waitFor(t, mountCheckFailedNotice+other.Error()+"\n")
	out := s.out.String()
	if n := strings.Count(out, mountCheckFailedNotice+failure.Error()+"\n"); n != 2 {
		t.Fatalf("the session was told of the failure %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, mountCheckFailedNotice); n != 3 {
		t.Fatalf("the session was told of %d failures, want 3:\n%s", n, out)
	}
}

// Two per-request failures for the same container path and reason are the
// same failure, whatever else their text says: the session is told of the
// first alone, and again for a failure at another container path.
func TestRequestMountCheckFailureIsNoticedByItsPathAndReason(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	first := writableConfig(h)
	same := writableConfig(h)
	same.Source += "-elsewhere"
	other := writableConfig(h)
	other.ContainerPath += "/other"
	for _, err := range []error{first, same, other} {
		check.set(err)
		h.run(t, containerRoot, "", "echo", "hi")
	}
	s.waitFor(t, mountCheckFailedNotice+other.Error()+"\n")
	out := s.out.String()
	if strings.Contains(out, same.Error()) {
		t.Fatalf("the session was told again of the same path and reason:\n%s", out)
	}
	if n := strings.Count(out, mountCheckFailedNotice); n != 2 {
		t.Fatalf("the session was told of %d failures, want 2:\n%s", n, out)
	}
}

// A per-request check that fails withdraws no request waiting for an
// answer, nor does it change the result of the attach-time check a new
// session is told of: the waiting request runs once approved and checked.
func TestRequestMountCheckFailureLeavesWaitingRequestsAndTheAttachResult(t *testing.T) {
	var log lockedBuffer
	var check requestCheck
	h := startRequestCheckingDaemon(t, &log, &check)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	failure := writableConfig(h)
	check.set(failure)
	if got := h.run(t, containerRoot, "", "echo", "hi"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused", got)
	}

	conn, err := transport.Unix{Path: h.approvalSocket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var info protocol.DaemonInfo
	if err == nil && f.Type == protocol.FrameDaemonInfo {
		err = protocol.DecodeJSON(f, &info)
	}
	if err != nil || f.Type != protocol.FrameDaemonInfo || info.MountCheckFailed != "" {
		t.Fatalf("frame %v %q, err %v; want a DaemonInfo with no failed check", f.Type, f.Payload, err)
	}

	check.set(nil)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it to run", got)
	}
	if _, err := os.Stat(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatalf("the approved command did not run: %v", err)
	}
}
