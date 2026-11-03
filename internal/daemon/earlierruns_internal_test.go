package daemon

import (
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// newSession returns a session connection whose frames go to the peer
// it returns, with a run feed when it asks for runs; it is closed when
// the test ends.
func newSession(t *testing.T, runs bool) (*sessionConn, net.Conn) {
	t.Helper()
	daemonEnd, peer := net.Pipe()
	t.Cleanup(func() {
		daemonEnd.Close()
		peer.Close()
	})
	out := &frameWriter{conn: daemonEnd, timeout: sessionWriteTimeout}
	c := &sessionConn{conn: daemonEnd, out: out}
	if runs {
		feed := newRunFeed(out)
		c.runs.Store(feed)
		go feed.pump()
		t.Cleanup(feed.stop)
	}
	return c, peer
}

// frameTypes returns the types of frames, in order.
func frameTypes(frames []protocol.Frame) []protocol.FrameType {
	types := make([]protocol.FrameType, len(frames))
	for i, f := range frames {
		types[i] = f.Type
	}
	return types
}

// A session that starts serving the daemon, having asked for runs, is
// told of the approved runs going, right after it is told it serves, in
// the order the daemon numbered them, each as it started, marked as
// started earlier, with how long it has gone; their ends follow. A
// session that did not ask for runs is told of none.
func TestServingASessionAnnouncesTheRunsGoing(t *testing.T) {
	for _, asksForRuns := range []bool{true, false} {
		name := "asking for runs"
		if !asksForRuns {
			name = "not asking for runs"
		}
		t.Run(name, func(t *testing.T) {
			s := newApprovalsServer(t)
			fresh := protocol.RunStarted{Argv: []string{"make", "test"}, Cwd: "/workspaces/app", Approval: protocol.ScopeOnce, Request: 3}
			repeat := protocol.RunStarted{Argv: []string{"make", "logs"}, Cwd: "/workspaces/app/sub", Approval: protocol.ScopeStanding}
			first := s.approvals.startRun(fresh)
			second := s.approvals.startRun(repeat)
			elapsed := []time.Duration{time.Minute, 2 * time.Second}
			first.begun = first.begun.Add(-elapsed[0])
			second.begun = second.begun.Add(-elapsed[1])
			c, peer := newSession(t, asksForRuns)
			frames := collectFrames(peer)
			if why := s.serveSession(c, false); why != "" {
				t.Fatalf("serving refused: %s", why)
			}
			second.exited(0)
			first.exited(1)
			s.closeStopping(c)
			got := <-frames

			want := []protocol.FrameType{protocol.FrameServing}
			if asksForRuns {
				want = append(want, protocol.FrameRunStarted, protocol.FrameRunStarted, protocol.FrameRunEnded, protocol.FrameRunEnded)
			}
			if types := frameTypes(got); !slices.Equal(types, want) {
				t.Fatalf("the session got frames %v, want %v", types, want)
			}
			if !asksForRuns {
				return
			}
			fresh.Run, fresh.Earlier = 1, true
			repeat.Run, repeat.Earlier = 2, true
			for i, w := range []protocol.RunStarted{fresh, repeat} {
				m := decoded[protocol.RunStarted](t, got[1+i], protocol.FrameRunStarted)
				if !slices.Equal(m.Argv, w.Argv) || m.Cwd != w.Cwd || m.Approval != w.Approval || m.Request != w.Request || m.Run != w.Run || !m.Earlier {
					t.Fatalf("run announced as %+v, want %+v", m, w)
				}
				// The announcement follows the run's start by a little more
				// than elapsed[i], the time the test set it back by.
				if d := time.Duration(m.ElapsedMs) * time.Millisecond; d < elapsed[i] || d > elapsed[i]+10*time.Second {
					t.Fatalf("run %d announced as gone for %v, want %v or a little more", m.Run, d, elapsed[i])
				}
			}
			for i, w := range []protocol.RunEnded{{Run: 2}, {Run: 1, Code: 1}} {
				if m := decoded[protocol.RunEnded](t, got[3+i], protocol.FrameRunEnded); m.Run != w.Run || m.Code != w.Code {
					t.Fatalf("run ended as %+v, want %+v", m, w)
				}
			}
		})
	}
}

// A run whose end is on its way to the session serving the daemon as
// another takes it over, here held up by the session's run feed, is not
// announced to the new session, whose take-over waits for the end: the
// new session is told of no run it is not told the end of.
func TestARunEndingAsASessionTakesOverIsNotLeftWithoutAnEnd(t *testing.T) {
	s, old, oldPeer := approvalsServer(t)
	collectFrames(oldPeer)
	run := s.approvals.startRun(protocol.RunStarted{Argv: []string{"true"}, Approval: protocol.ScopeOnce, Request: 1})
	oldFeed := old.runs.Load()
	oldFeed.mu.Lock()
	release := sync.OnceFunc(oldFeed.mu.Unlock)
	defer release() // should the test fail before it lets the end go
	ended := make(chan struct{})
	go func() {
		run.exited(0)
		close(ended)
	}()
	waitHeld(t, &s.approvals.mu, "the run's end did not hold the approvals' lock while it waited for the old session's feed")
	c, peer := newSession(t, true)
	frames := collectFrames(peer)
	served := make(chan string, 1)
	go func() { served <- s.serveSession(c, true) }()
	release()
	<-ended
	if why := <-served; why != "" {
		t.Fatalf("taking over refused: %s", why)
	}
	s.closeStopping(c)
	got := <-frames
	if len(got) != 1 || got[0].Type != protocol.FrameServing {
		t.Fatalf("the new session got frames %v, want %v alone", frameTypes(got), protocol.FrameServing)
	}
}

// waitHeld waits until another goroutine holds mu, failing the test with
// why if none does within a few seconds.
func waitHeld(t *testing.T, mu *sync.Mutex, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for mu.TryLock() {
		mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(time.Millisecond)
	}
}
