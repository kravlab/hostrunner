package daemon

import (
	"context"
	"net"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// runRepeat grants req a standing approval on s and runs req as a plain
// request, the client reading nothing: it returns the frames the client
// got once the run is over.
func runRepeat(t *testing.T, s *Server, req protocol.Request) []protocol.Frame {
	t.Helper()
	if _, ok := s.approvals.grant(req, false, pathPasses{}, nil); !ok {
		t.Fatal("the standing approval was not granted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonEnd, client := net.Pipe()
	defer client.Close()
	result := collectFrames(client)
	x := exchange{cancel: cancel, conn: daemonEnd, out: &frameWriter{conn: daemonEnd}}
	s.run(ctx, x, req)
	daemonEnd.Close()
	return <-result
}

// assertNoRunsGoing fails t unless s numbered n approved runs and none of
// them is going: noRuns, made as the first started, was closed as the
// last ended.
func assertNoRunsGoing(t *testing.T, s *Server, n uint64) {
	t.Helper()
	s.approvals.mu.Lock()
	defer s.approvals.mu.Unlock()
	if s.approvals.lastRun != n {
		t.Fatalf("the daemon numbered %d approved runs, want %d", s.approvals.lastRun, n)
	}
	if len(s.approvals.going) != 0 || s.approvals.noRuns != nil {
		t.Fatalf("runs %v are left going, noRuns %v", s.approvals.going, s.approvals.noRuns)
	}
}

// A repeat a standing approval lets through runs with no session serving
// the daemon, or with one that did not ask for runs, which gets no run
// frames; it is an approved run all the same, and once it ends none is
// left going, so a stopping daemon does not wait for it.
func TestARepeatWithoutASessionAskingForRunsRunsAndEnds(t *testing.T) {
	for _, serving := range []bool{false, true} {
		name := "no session"
		if serving {
			name = "session without runs"
		}
		t.Run(name, func(t *testing.T) {
			s := newApprovalsServer(t)
			var frames <-chan []protocol.Frame
			if serving {
				daemonEnd, peer := net.Pipe()
				frames = collectFrames(peer)
				s.approvals.serving = &sessionConn{conn: daemonEnd, out: &frameWriter{conn: daemonEnd, timeout: sessionWriteTimeout}}
				defer func() {
					daemonEnd.Close()
					if got := <-frames; len(got) != 0 {
						t.Errorf("the session got %d frames, want none", len(got))
					}
				}()
			}
			got := runRepeat(t, s, protocol.Request{Argv: []string{"true"}, Cwd: "/workspaces/app"})
			if len(got) == 0 || got[len(got)-1].Type != protocol.FrameExit {
				t.Fatalf("the client got %d frames, want them to end with its exit", len(got))
			}
			if e := decoded[protocol.Exit](t, got[len(got)-1], protocol.FrameExit); e.Code != 0 {
				t.Fatalf("the command exited %d, want 0", e.Code)
			}
			assertNoRunsGoing(t, s, 1)
		})
	}
}

// A repeat whose command fails to start tells the session that asked for
// runs of its start, then of its end with the start failure, as the
// client gets it.
func TestARepeatThatFailsToStartEndsNotStarted(t *testing.T) {
	s, c, peer := approvalsServer(t)
	frames := collectFrames(peer)
	req := protocol.Request{Argv: []string{"hostrun-example-missing"}, Cwd: "/workspaces/app"}
	got := runRepeat(t, s, req)
	const message = "command hostrun-example-missing not found on the host"
	if len(got) != 1 {
		t.Fatalf("the client got %d frames, want its start failure alone", len(got))
	}
	if e := decoded[protocol.Error](t, got[0], protocol.FrameError); e.Code != protocol.ExitNotFound || e.Message != message {
		t.Fatalf("the client got %+v, want code %d, %q", e, protocol.ExitNotFound, message)
	}
	s.closeStopping(c)
	runs := <-frames
	if len(runs) != 2 {
		t.Fatalf("the session got %d frames, want the start and the end of the run", len(runs))
	}
	if m := decoded[protocol.RunStarted](t, runs[0], protocol.FrameRunStarted); m.Request != 0 || m.Approval != protocol.ScopeStanding {
		t.Fatalf("the run started as %+v, want a repeat of a standing approval", m)
	}
	if m := decoded[protocol.RunEnded](t, runs[1], protocol.FrameRunEnded); m.Code != protocol.ExitNotFound || m.Error != message || m.Cancelled != "" {
		t.Fatalf("the run ended as %+v, want not started: %q", m, message)
	}
	assertNoRunsGoing(t, s, 1)
}
