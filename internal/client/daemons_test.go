package client_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// listWait is how long a test waits for the session to notice a daemon,
// which it looks for every 2 seconds.
const listWait = 5 * time.Second

// startEchoDaemon starts a daemon with the echoOnly rules logging to log,
// its runtime directory at base.
func startEchoDaemon(t *testing.T, log *slog.Logger, base string) harness {
	t.Helper()
	return startHarnessAt(t, log, base, func(string) (daemon.Policy, []daemon.Option) {
		return policy(t, echoOnly), nil
	})
}

// newBase returns a fresh runtime directory beside the others of the test,
// in its root, where a listing session looks for approval sockets.
func newBase(t *testing.T) (base, root string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return base, filepath.Dir(base)
}

// waitForListed waits until the session lists daemons and prompts for a
// choice; it returns what it printed.
func (s *session) waitForListed(t *testing.T) string {
	t.Helper()
	return s.waitForWithin(t, "q quit] ? ", listWait)
}

// waitForWithin is waitFor with a deadline of d.
func (s *session) waitForWithin(t *testing.T, text string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		out := s.out.String()
		if i := strings.Index(out[s.seen:], text); i >= 0 {
			got := out[s.seen : s.seen+i+len(text)]
			s.seen += i + len(text)
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session did not print %q; it printed %q", text, out[s.seen:])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Without a socket, the session lists the daemons under its root: the one
// whose workspace holds its directory marked and chosen by default.
func TestSessionListsTheDaemons(t *testing.T) {
	base1, root := newBase(t)
	base2, _ := newBase(t)
	h1 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base1)
	h2 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base2)
	s := startSessionWith(t, approval.Config{Root: root, Cwd: filepath.Join(h2.hostRoot, "sub")})
	list := s.waitForListed(t)
	for _, want := range []string{"daemons:\n", "  1   " + h1.hostRoot, "  2 * " + h2.hostRoot, "(container " + containerRoot + ")", "← current directory", "choose [1-2, Enter = 2, q quit] ? "} {
		if !strings.Contains(list, want) {
			t.Errorf("the list %q lacks %q", list, want)
		}
	}
	s.answer(t, "")
	s.waitFor(t, "serving "+h2.hostRoot)
	ch := h2.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved by the listing session", got)
	}
}

// A daemon another session serves is marked, and one of another approval
// protocol version is marked and cannot be chosen.
func TestSessionListMarksServedDaemonsAndOtherVersions(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	startSessionOn(t, h.approvalSocket)
	other, _ := newBase(t)
	fakeOldDaemon(t, launch.ApprovalSocket(other))
	s := startSessionWith(t, approval.Config{Root: root})
	list := s.waitForListed(t)
	for _, want := range []string{h.hostRoot, "served by another session", "another version: restart its container", "choose [1-2, q quit] ? "} {
		if !strings.Contains(list, want) {
			t.Errorf("the list %q lacks %q", list, want)
		}
	}
	s.answer(t, "2")
	s.waitFor(t, "daemon 2 speaks another approval protocol version: restart its container\n")
	s.waitForListed(t)
	s.answer(t, "q")
	waitEnded(t, s)
}

// fakeOldDaemon answers every hello on socket as a daemon of another
// approval protocol version.
func fakeOldDaemon(t *testing.T, socket string) {
	t.Helper()
	l, err := transport.Unix{Path: socket}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameHello {
					_ = protocol.WriteJSON(c, protocol.FrameDaemonInfo, protocol.DaemonInfo{Version: protocol.ApprovalVersion + 1, Workspace: "/old/workspace"})
				}
			}(c)
		}
	}()
}

// waitEnded waits until the session has ended.
func waitEnded(t *testing.T, s *session) {
	t.Helper()
	select {
	case <-s.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
}

// With no daemon yet, the session waits for one.
func TestSessionWaitsForADaemon(t *testing.T) {
	_, root := newBase(t)
	s := startSessionWith(t, approval.Config{Root: root})
	s.waitFor(t, "no daemons yet: waiting for one")
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	list := s.waitForListed(t)
	if !strings.Contains(list, "  1   "+h.hostRoot) || !strings.Contains(list, "choose [1-1, q quit] ? ") {
		t.Fatalf("the list %q, want the new daemon and no default", list)
	}
	s.answer(t, "1")
	s.waitFor(t, "serving "+h.hostRoot)
}

// Choosing a daemon another session serves asks to take it over: declined,
// the list comes back; accepted, the other session is told and ends, and
// the requests waiting pass to this one with the rest of their limit.
func TestSessionTakesOverADaemon(t *testing.T) {
	var log lockedBuffer
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), base)
	first := startSessionOn(t, h.approvalSocket)
	ch := h.askFor(t, "10m", "touch", "marker")
	first.waitFor(t, answerPromptText)
	// Past a second, the rest of the limit shows under 10:00.
	time.Sleep(1100 * time.Millisecond)
	second := startSessionWith(t, approval.Config{Root: root})
	second.waitForListed(t)
	second.answer(t, "1")
	second.waitFor(t, "Take over? [y/N] ")
	second.answer(t, "")
	second.waitForListed(t)
	second.answer(t, "1")
	second.waitFor(t, "Take over? [y/N] ")
	second.answer(t, "y")
	first.waitFor(t, "taken over by another session\n")
	waitEnded(t, first)
	block := second.waitFor(t, answerPromptText)
	if !strings.Contains(block, "── request 1 ── 9:5") {
		t.Fatalf("the moved request %q, want it with the rest of its limit", block)
	}
	second.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved by the new session", got)
	}
	waitForLog(t, &log, `msg="approval session taken over"`)
}

// When its daemon ends, the session says so and serves the next daemon in
// the same runtime directory.
func TestSessionServesTheNextDaemon(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	s := startSessionOn(t, h.approvalSocket)
	h.stop()
	s.waitFor(t, "the daemon ended: waiting for the next one\n")
	s.answer(t, "x")
	s.waitFor(t, "waiting for the next daemon: q quits\n")
	next := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	s.waitForWithin(t, "serving "+next.hostRoot, listWait)
	ch := next.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved", got)
	}
}

// `daemons` at the idle prompt leaves the daemon and lists them again.
func TestSessionDaemonsCommandReturnsToTheList(t *testing.T) {
	var log lockedBuffer
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), base)
	s := startSessionWith(t, approval.Config{Root: root, Cwd: h.hostRoot})
	s.waitForListed(t)
	s.answer(t, "")
	s.waitFor(t, "serving "+h.hostRoot)
	s.waitFor(t, "> ")
	s.answer(t, "daemons")
	list := s.waitForListed(t)
	if strings.Contains(list, "served by another session") {
		t.Fatalf("the session still serves the daemon it left: %q", list)
	}
	waitForLog(t, &log, `msg="approval session left"`)
}

// A runtime directory without an approval socket has no approvals: the
// session says so.
func TestSessionWithoutAnApprovalSocketSaysSo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := approval.Run(context.Background(), approval.Config{Socket: launch.ApprovalSocket(dir)}, strings.NewReader(""), &lockedBuffer{})
	if err == nil || !strings.Contains(err.Error(), "has no approvals") {
		t.Fatalf("got %v, want it to say the daemon has no approvals", err)
	}
}

// The default daemon another session serves is marked both ways.
func TestSessionListMarksTheDefaultDaemonServed(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	startSessionOn(t, h.approvalSocket)
	s := startSessionWith(t, approval.Config{Root: root, Cwd: h.hostRoot})
	list := s.waitForListed(t)
	if !strings.Contains(list, "← current directory, served by another session") {
		t.Fatalf("the list %q does not mark the default daemon served", list)
	}
}

// A socket nobody answers on is left out of the list.
func TestSessionListSkipsDeadSockets(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	dead, _ := newBase(t)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: launch.ApprovalSocket(dead), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	s := startSessionWith(t, approval.Config{Root: root})
	list := s.waitForListed(t)
	if !strings.Contains(list, "  1   "+h.hostRoot) || !strings.Contains(list, "choose [1-1, q quit] ? ") {
		t.Fatalf("the list %q, want the live daemon alone", list)
	}
}

// Take-overs one after the other: the last wins, and each session replaced
// is told.
func TestSessionTakeOversLastWins(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	startSessionOn(t, h.approvalSocket)
	takeOver := func() *session {
		s := startSessionWith(t, approval.Config{Root: root})
		s.waitForListed(t)
		s.answer(t, "1")
		s.waitFor(t, "Take over? [y/N] ")
		s.answer(t, "y")
		s.waitFor(t, "serving "+h.hostRoot)
		return s
	}
	second := takeOver()
	third := takeOver()
	second.waitFor(t, "taken over by another session\n")
	waitEnded(t, second)
	ch := h.ask(t, "touch", "marker")
	third.waitFor(t, answerPromptText)
	third.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved by the last session", got)
	}
}
