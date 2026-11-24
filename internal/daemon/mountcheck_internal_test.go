package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// mountCheckServer returns a Server with approvals that makes check the
// live check of every request (see WithMountCheck), its attach-time check
// passed, served by a session that reads the notices of failed checks, its
// frames going to the peer it returns. Its rules allow echo with no
// arguments, requiring echoRequirements, and `echo plain` with any,
// requiring none.
func mountCheckServer(t *testing.T, check MountCheck) (*Server, net.Conn) {
	t.Helper()
	mapper, err := workspace.NewMapper(t.TempDir(), "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	p, err := rules.Parse([]byte("rules:\n" +
		"  - command: echo\n    args: none\n    mounts:\n      .git: mounted\n      .git/config: read-only\n" +
		"  - command: echo plain\n    args: any\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(mapper, p, slog.New(slog.DiscardHandler), WithApprovals(nil, ""), WithMountCheck("turn it off", check))
	s.MountChecked(time.Time{}, nil)
	c, peer := newSession(t, false)
	c.readsMountCheck.Store(true)
	s.approvals.serving = c
	return s, peer
}

// protectedStage returns the per-request mount check whose protected-path
// stage returns what check does, and whose requirements stage passes.
func protectedStage(check func(ctx context.Context) error) MountCheck {
	return func(ctx context.Context) (MountRequirementsCheck, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		return func([]rules.MountRequirement) error { return nil }, nil
	}
}

// writableAt returns the failure of a check that finds the container path
// /workspaces/app/.devcontainer writable, its host source source: failures
// with different sources have the same path and reason.
func writableAt(source string) *mountcheck.WritableError {
	return &mountcheck.WritableError{Container: "c0ffee", Path: "/host/app/.devcontainer", Source: source, ContainerPath: "/workspaces/app/.devcontainer"}
}

// noticedFailures closes the session of s and returns the failed checks it
// was told of, in order.
func noticedFailures(t *testing.T, s *Server, frames <-chan []protocol.Frame) []protocol.MountCheckFailed {
	t.Helper()
	s.approvals.serving.conn.Close()
	var told []protocol.MountCheckFailed
	for _, f := range <-frames {
		told = append(told, decoded[protocol.MountCheckFailed](t, f, protocol.FrameMountCheckFailed))
	}
	return told
}

// noticed closes the session of s and returns the error texts of the
// failed checks it was told of, in order.
func noticed(t *testing.T, s *Server, frames <-chan []protocol.Frame) []string {
	t.Helper()
	var texts []string
	for _, m := range noticedFailures(t, s, frames) {
		texts = append(texts, m.Error)
	}
	return texts
}

// A live check made under a done context, as the daemon stops, refuses
// the request as a stopping daemon does: its result is neither recorded
// nor told to the session, which is told of the next failure, with the
// same path and reason, as of a new one.
func TestARequestMountCheckAsTheDaemonStopsRefusesItAsStopping(t *testing.T) {
	var calls atomic.Int32
	stopping, next := writableAt("/host/first"), writableAt("/host/next")
	s, peer := mountCheckServer(t, protectedStage(func(context.Context) error {
		if calls.Add(1) == 1 {
			return stopping
		}
		return next
	}))
	frames := collectFrames(peer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := s.mount.requestRefusal(ctx, ruleRequirements{})
	if r == nil || r.code != protocol.ExitHostrunError || r.message != drainingMessage || !errors.Is(r.err, errDraining) {
		t.Fatalf("got %+v, want the refusal of a stopping daemon", r)
	}
	if r := s.mount.requestRefusal(context.Background(), ruleRequirements{}); r == nil || r.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want the refusal of a failed check", r)
	}
	if got := noticed(t, s, frames); len(got) != 1 || got[0] != next.Error() {
		t.Fatalf("the session was told of %q, want of %q alone", got, next.Error())
	}
}

// Concurrent live checks are recorded in the order they started: a
// failure that ends after a later check passed is stale, so the session
// is not told of it, and the next failure with its path and reason is
// told as a new one. The stale check still refuses its own request.
func TestAStaleRequestMountCheckIsNotRecorded(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	stale, later := writableAt("/host/stale"), writableAt("/host/later")
	s, peer := mountCheckServer(t, protectedStage(func(context.Context) error {
		switch calls.Add(1) {
		case 1:
			close(started)
			<-release
			return stale
		case 2:
			return nil
		}
		return later
	}))
	frames := collectFrames(peer)
	first := make(chan *refusal, 1)
	go func() { first <- s.mount.requestRefusal(context.Background(), ruleRequirements{}) }()
	<-started
	if r := s.mount.requestRefusal(context.Background(), ruleRequirements{}); r != nil {
		t.Fatalf("got %+v, want the later check to pass", r)
	}
	close(release)
	if r := <-first; r == nil || r.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want the stale check to refuse its own request", r)
	}
	if r := s.mount.requestRefusal(context.Background(), ruleRequirements{}); r == nil {
		t.Fatal("the next check passed, want it to fail")
	}
	if got := noticed(t, s, frames); len(got) != 1 || got[0] != later.Error() {
		t.Fatalf("the session was told of %q, want of %q alone", got, later.Error())
	}
}
