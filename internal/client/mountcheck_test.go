package client_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// mountCheckOffHint is how a human on the host turns the mount check off,
// as the daemon is given it (see daemon.WithMountCheck).
const mountCheckOffHint = "mount_check: false in the host settings file, HOSTRUNNER_MOUNT_CHECK=false or --no-mount-check"

// What hostrun prints for a command the mount check refuses.
const (
	mountCheckPendingText    = "hostrun: the mount check has not run yet: the daemon has not attached to the container; retry\n"
	mountCheckWritableText   = "hostrun: the container can write /workspaces/app/.devcontainer, so it could change the daemon's configuration: every command is refused until it is mounted read-only; a human on the host can turn the mount check off: " + mountCheckOffHint + "\n"
	mountCheckUnreadableText = "hostrun: the daemon cannot read the container's mounts to check them: every command is refused; a human on the host can turn the mount check off: " + mountCheckOffHint + "\n"
	mountCheckFailedNotice   = "the mount check failed, so the daemon refuses every command: "
)

// startMountCheckingDaemon starts a daemon that checks its armed
// container's mounts (daemon.WithMountCheck), serving echoOnly and logging
// to log.
func startMountCheckingDaemon(t *testing.T, log *lockedBuffer) harness {
	t.Helper()
	return startDaemonWith(t, slog.New(slog.NewTextHandler(log, nil)), policy(t, echoOnly), daemon.WithMountCheck(mountCheckOffHint))
}

// writableConfig is the failed check of a container that can write the
// .devcontainer directory of h's workspace.
func writableConfig(h harness) *mountcheck.WritableError {
	dir := filepath.Join(h.hostRoot, ".devcontainer")
	return &mountcheck.WritableError{Container: "c0ffee", Path: dir, Source: dir, ContainerPath: containerRoot + "/.devcontainer"}
}

// refusedByMountCheck runs, dry runs and asks for a command that creates
// marker, which the rules allow, and a run of one they refuse, which would
// hint at --ask with a session serving, and checks each is refused with
// code and stderr and that nothing ran.
func refusedByMountCheck(t *testing.T, h harness, code int, stderr string) {
	t.Helper()
	want := result{code: code, stderr: stderr}
	for _, argv := range [][]string{
		{"sh", "-c", "touch marker"},
		{"touch", "marker"},
		{"--dry-run", "sh", "-c", "touch marker"},
		{"--ask", "sh", "-c", "touch marker"},
		{"--ask", "touch", "marker"},
	} {
		ch := make(chan result, 1)
		go func() { ch <- h.run(t, containerRoot, "", argv...) }()
		if got := awaitResult(t, ch); got != want {
			t.Errorf("%q: got %+v, want %+v", argv, got, want)
		}
	}
	assertNotRun(t, h, "marker")
}

// Until the daemon has attached to its container and checked its mounts,
// it refuses every command as a hostrun failure, which a retry outlives.
func TestMountCheckPendingRefusesEveryCommand(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	refusedByMountCheck(t, h, protocol.ExitHostrunError, mountCheckPendingText)
}

// A container that can write the daemon's configuration gets every command
// refused, with no hint at --ask and no host path; the log names the host
// path, and the session serving the daemon says why.
func TestMountCheckFailedRefusesEveryCommand(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	s := h.startSession(t)
	failure := writableConfig(h)
	h.srv.MountChecked(time.Time{}, failure)
	s.waitFor(t, mountCheckFailedNotice+failure.Error()+"\n")
	waitForLog(t, &log, `msg="mount check failed: every command is refused"`)
	if !strings.Contains(log.String(), failure.Path) {
		t.Fatalf("the log lacks the host path %s:\n%s", failure.Path, log.String())
	}
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

// A session that connects while the check has failed says so at once.
func TestMountCheckFailureIsShownToALaterSession(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	failure := writableConfig(h)
	h.srv.MountChecked(time.Time{}, failure)
	s := h.startSession(t)
	if out := s.out.String(); !strings.Contains(out, mountCheckFailedNotice+failure.Error()+"\n") {
		t.Fatalf("the session does not show the failed check:\n%s", out)
	}
}

// Mounts the daemon cannot read refuse every command too.
func TestMountCheckUnreadableRefusesCommands(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	h.srv.MountChecked(time.Time{}, &mountcheck.UnreadableError{Container: "c0ffee", Err: errors.New("no such container")})
	got := h.run(t, containerRoot, "", "sh", "-c", "touch marker")
	if want := (result{code: protocol.ExitRejected, stderr: mountCheckUnreadableText}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertNotRun(t, h, "marker")
}

// A mount the daemon cannot resolve, or any other failure, refuses every
// command.
func TestMountCheckOtherFailuresRefuseCommands(t *testing.T) {
	const text = "hostrun: the daemon cannot check the container's mounts: every command is refused; a human on the host can turn the mount check off: " + mountCheckOffHint + "\n"
	for _, failure := range []error{
		&mountcheck.UnresolvableError{Container: "c0ffee", Path: "/srv/missing", Err: os.ErrNotExist},
		errors.New("the runtime failed"),
	} {
		var log lockedBuffer
		h := startMountCheckingDaemon(t, &log)
		h.srv.MountChecked(time.Time{}, failure)
		got := h.run(t, containerRoot, "", "sh", "-c", "touch marker")
		if want := (result{code: protocol.ExitRejected, stderr: text}); got != want {
			t.Errorf("%v: got %+v, want %+v", failure, got, want)
		}
		assertNotRun(t, h, "marker")
	}
}

// A passed check serves commands, and is logged.
func TestMountCheckPassedServesCommands(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	h.srv.MountChecked(time.Time{}, nil)
	waitForLog(t, &log, `msg="mount check passed"`)
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want it to run", got)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "echo", "hi"); got != (result{stderr: "hostrun: dry run: allowed by rule \"echo\"\n"}) {
		t.Fatalf("dry run: got %+v, want it allowed", got)
	}
	if got := awaitResult(t, h.ask(t, "echo", "hi")); got != (result{stdout: "hi\n"}) {
		t.Fatalf("--ask: got %+v, want it to run", got)
	}
}

// The latest check wins: a container attached again with its mounts fixed
// is served again.
func TestMountCheckPassedAfterAFailureServesCommandsAgain(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	h.srv.MountChecked(time.Time{}, writableConfig(h))
	if got := h.run(t, containerRoot, "", "echo", "hi"); got.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want it refused", got)
	}
	h.srv.MountChecked(time.Time{}, nil)
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want it to run again", got)
	}
}

// Each re-arm puts the check back to pending until the daemon attaches for
// it: the container started for the arm is not served on the result of the
// one before.
func TestMountCheckIsPendingAgainAfterARearm(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	first := time.Now()
	h.srv.MountChecked(first, nil)
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want it to run once the check passed", got)
	}
	rearmed := first.Add(time.Second)
	h.srv.MountRearmed(rearmed)
	refusedByMountCheck(t, h, protocol.ExitHostrunError, mountCheckPendingText)
	waitForLog(t, &log, `msg="mount check pending: re-armed"`)
	h.srv.MountChecked(rearmed, nil)
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want it to run once the check of the re-arm passed", got)
	}
}

// A result of an earlier arm, as a poll under way during the re-arm brings,
// leaves the check pending, and a failed one is neither logged nor shown;
// a re-arm that arrives after a result of a later arm changes nothing.
func TestMountCheckResultOfAnEarlierArmIsIgnored(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	s := h.startSession(t)
	first := time.Now()
	rearmed := first.Add(time.Second)
	h.srv.MountRearmed(rearmed)
	h.srv.MountChecked(first, nil)
	h.srv.MountChecked(first, writableConfig(h))
	refusedByMountCheck(t, h, protocol.ExitHostrunError, mountCheckPendingText)
	if strings.Contains(log.String(), "mount check failed") || strings.Contains(s.out.String(), mountCheckFailedNotice) {
		t.Fatalf("a failure of an earlier arm was reported:\nlog:\n%s\nsession:\n%s", log.String(), s.out.String())
	}

	h.srv.MountChecked(rearmed, nil)
	h.srv.MountRearmed(first)
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want it to run: the late re-arm is older than the passed check", got)
	}
}

// `hostrunner up` arms the daemon whatever the check: the arm is what
// leads to the attach that runs it.
func TestMountCheckDoesNotRefuseAnArm(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	h.arm(t, protocol.Arm{})
	h.srv.MountChecked(time.Time{}, writableConfig(h))
	h.arm(t, protocol.Arm{})
}

// A session older than the frame of a failed check takes an unknown frame
// for an error, so it is sent none.
func TestMountCheckFailureIsNotSentToAnOlderSession(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	conn := h.serveRaw(t)
	h.srv.MountChecked(time.Time{}, writableConfig(h))
	waitForLog(t, &log, `msg="mount check failed: every command is refused"`)
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStanding}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameStandingList {
		t.Fatalf("frame %v, err %v; want the standing list", f.Type, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if f, err := protocol.ReadFrame(conn); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("frame %v, err %v; want none", f.Type, err)
	}
}

// A daemon that does not check mounts serves commands before any check.
func TestWithoutMountCheckCommandsRun(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	if got := h.run(t, containerRoot, "", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want it to run", got)
	}
}

// A check that fails while --ask requests wait for the human refuses each
// of them as a new one would be refused, and the session drops them all,
// announcing each as it is shown.
func TestMountCheckFailureRefusesWaitingRequests(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	h.srv.MountChecked(time.Time{}, nil)
	s := h.startSession(t)
	current := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	queued := h.ask(t, "touch", "queued")
	s.waitFor(t, "\a")
	h.srv.MountChecked(time.Time{}, writableConfig(h))
	for ch, waiting := range map[<-chan result]string{current: waitingNoRule, queued: strings.Replace(waitingNoRule, "marker", "queued", 1)} {
		got := awaitResult(t, ch)
		if want := (result{code: protocol.ExitRejected, stderr: waiting + mountCheckWritableText}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
	s.waitFor(t, "request 1 ended: the mount check failed\n")
	s.waitFor(t, "request 2 ended: the mount check failed\n")
	s.waitFor(t, idlePromptText)
	assertNotRun(t, h, "marker")
	assertNotRun(t, h, "queued")
}

// helloThenServe connects to h's approval socket, says Hello, calls between,
// then asks to serve as sv says and reads the daemon's confirmation.
func helloThenServe(t *testing.T, h harness, between func(), sv protocol.Serve) net.Conn {
	t.Helper()
	conn, err := transport.Unix{Path: h.approvalSocket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameDaemonInfo {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	between()
	if err := protocol.WriteJSON(conn, protocol.FrameServe, sv); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameServing {
		t.Fatalf("serve: frame %v, err %v", f.Type, err)
	}
	return conn
}

// assertNoFrameBeforeStandingList checks that the daemon sends conn
// nothing before the standing list it asks for.
func assertNoFrameBeforeStandingList(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStanding}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameStandingList {
		t.Fatalf("frame %v, err %v; want the standing list", f.Type, err)
	}
}

// A session that said Hello before the check failed learns of the failure
// as it starts serving the daemon, when it reads the frame.
func TestMountCheckFailureIsSentAsASessionStartsServing(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	failure := writableConfig(h)
	conn := helloThenServe(t, h, func() { h.srv.MountChecked(time.Time{}, failure) }, protocol.Serve{MountCheck: true})
	f, err := protocol.ReadFrame(conn)
	var m protocol.MountCheckFailed
	if err == nil && f.Type == protocol.FrameMountCheckFailed {
		err = protocol.DecodeJSON(f, &m)
	}
	if err != nil || f.Type != protocol.FrameMountCheckFailed || m.Error != failure.Error() {
		t.Fatalf("frame %v %q, err %v; want the failed check %q", f.Type, f.Payload, err, failure.Error())
	}
	assertNoFrameBeforeStandingList(t, conn)
}

// A session that does not read the frame of a failed check is sent none
// as it starts serving the daemon, nor is one that learned of the failure
// from the daemon's hello.
func TestMountCheckFailureIsNotSentTwiceOrToAnOlderSession(t *testing.T) {
	var log lockedBuffer
	h := startMountCheckingDaemon(t, &log)
	failure := writableConfig(h)
	older := helloThenServe(t, h, func() { h.srv.MountChecked(time.Time{}, failure) }, protocol.Serve{Notices: true, Runs: true})
	assertNoFrameBeforeStandingList(t, older)
	told := helloThenServe(t, h, func() {}, protocol.Serve{TakeOver: true, MountCheck: true})
	assertNoFrameBeforeStandingList(t, told)
}
