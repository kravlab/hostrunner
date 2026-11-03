package client_test

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// openTerminal opens a pseudo-terminal and returns its two sides: the
// terminal, which a session reads the human from, and the side the test
// types on. Both are closed when the test ends. A system that cannot open
// one skips the test.
func openTerminal(t *testing.T) (terminal, typing *os.File) {
	t.Helper()
	typing, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { typing.Close() })
	fd := int(typing.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pseudo-terminal: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pseudo-terminal: %v", err)
	}
	terminal, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { terminal.Close() })
	go func() {
		// What the terminal echoes is read and dropped, so it never fills.
		buf := make([]byte, 4096)
		for {
			if _, err := typing.Read(buf); err != nil {
				return
			}
		}
	}()
	return terminal, typing
}

// idleOnTerminal starts a fake daemon that reports approved runs and a
// session serving it that reads the human from a terminal, refuses the
// fake's request 1, and returns once the session shows the idle prompt:
// the session, the fake's connection, and the side of the terminal the
// test types on.
func idleOnTerminal(t *testing.T) (*session, net.Conn, *os.File) {
	t.Helper()
	socket, served := fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Runs: true})
	terminal, typing := openTerminal(t)
	out := &lockedBuffer{}
	s := launchSessionTyping(t, approval.Config{Socket: socket}, terminal, typing, out, out)
	conn := <-served
	s.waitFor(t, answerPromptText)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	answerOnFake(t, s, conn, "")
	sendFrame(t, conn, protocol.FrameAnswered, protocol.Answered{ID: 1})
	s.waitFor(t, "refused\n"+idlePromptText)
	return s, conn, typing
}

// typeOn types text on the terminal through typing.
func typeOn(t *testing.T, typing *os.File, text string) {
	t.Helper()
	if _, err := typing.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// readCommand reads the frame the session sends the fake daemon on conn
// and fails t unless it is of type want; s's output is shown then.
func readCommand(t *testing.T, s *session, conn net.Conn, want protocol.FrameType) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil || f.Type != want {
		t.Fatalf("frame %v, err %v; want %v; the session printed:\n%s", f.Type, err, want, s.out.String())
	}
}

// At the idle prompt a notice is printed at once and the prompt shown
// again, keeping what the human typed after it: a command typed in part
// on a terminal before the notice runs once it is finished.
func TestANoticeAtTheIdlePromptKeepsACommandTypedInPart(t *testing.T) {
	for _, tt := range []struct {
		name string
		typ  protocol.FrameType
		v    any
		text string
	}{
		{"rules reread", protocol.FrameRulesReread, rulesReread, rulesRereadText},
		{"repeat started", protocol.FrameRunStarted, repeatStarted, repeatStartedText},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn, typing := idleOnTerminal(t)
			typeOn(t, typing, "stand")
			sendFrame(t, conn, tt.typ, tt.v)
			s.waitFor(t, "\n"+tt.text+idlePromptText)
			typeOn(t, typing, "ing\n")
			readCommand(t, s, conn, protocol.FrameStanding)
		})
	}
}

// At the idle prompt a command's outcome is printed at once, right below
// the command's line, and the prompt shown again, keeping what the human
// typed after it: a command typed in part on a terminal before the
// outcome runs once it is finished.
func TestACommandsOutcomeAtTheIdlePromptKeepsACommandTypedInPart(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
		sent    protocol.FrameType
		typ     protocol.FrameType
		v       any
		text    string
	}{
		{"standing", "standing", protocol.FrameStanding, protocol.FrameStandingList, protocol.StandingList{Approvals: []protocol.Standing{{N: 3, Argv: []string{"touch", "x"}, Cwd: containerRoot}}}, "  3  touch x   in " + containerRoot + "\n"},
		{"revoke", "revoke 3", protocol.FrameRevoke, protocol.FrameRevoked, protocol.Revoked{N: 3}, "revoked 3\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, conn, typing := idleOnTerminal(t)
			typeOn(t, typing, tt.command+"\n")
			readCommand(t, s, conn, tt.sent)
			typeOn(t, typing, "stand")
			sendFrame(t, conn, tt.typ, tt.v)
			if got := s.waitFor(t, tt.text+idlePromptText); got != tt.text+idlePromptText {
				t.Fatalf("the session printed %q, want the outcome right below the command's line", got)
			}
			typeOn(t, typing, "ing\n")
			readCommand(t, s, conn, protocol.FrameStanding)
		})
	}
}
