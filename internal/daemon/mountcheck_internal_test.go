package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"sync"
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

// TestFailureKeyNamesTheStageOfItsFailure checks that failureKey keys a
// failure as of the stage it is given, whatever its error, with the kind
// that stage gives it, and that only the requirements stage keys the rule.
func TestFailureKeyNamesTheStageOfItsFailure(t *testing.T) {
	failure := mountcheck.RequirementFailure{Container: "c0ffee", Path: ".git", ContainerPath: "/workspaces/app/.git"}
	unreadable := &mountcheck.UnreadableError{Container: "c0ffee", Err: errors.New("gone")}
	tests := []struct {
		name     string
		failure  error
		stage    mountStage
		rule     string
		wantKind mountFailureKind
	}{
		{"not mounted", &mountcheck.NotMountedError{RequirementFailure: failure}, stageRequirements, "git", mountNotMounted},
		{
			"not read-only",
			&mountcheck.ReadOnlyRequirementError{RequirementFailure: failure},
			stageRequirements,
			"git",
			mountNotReadOnly,
		},
		{"requirement uncheckable", errors.New("boom"), stageRequirements, "git", mountRequirementUncheckable},
		{"writable", writableAt("/host/app"), stageProtected, "", mountWritable},
		{"unreadable", unreadable, stageProtected, "", mountUnreadable},
		{"uncheckable", errors.New("boom"), stageProtected, "", mountUncheckable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := failureKey(tt.failure, tt.stage, tt.rule)
			if key.kind != tt.wantKind {
				t.Fatalf("got kind %q, want %q", key.kind, tt.wantKind)
			}
			if key.stage != tt.stage {
				t.Fatalf("got stage %v, want %v", key.stage, tt.stage)
			}
			if got := key.ofRequirements(); got != (tt.stage == stageRequirements) {
				t.Fatalf("ofRequirements() = %v, want %v", got, tt.stage == stageRequirements)
			}
			if key.rule != tt.rule {
				t.Fatalf("got rule %q, want %q", key.rule, tt.rule)
			}
		})
	}
}

// TestSendMountCheckFailedSendsTheNoticeToASessionThatReadsIt checks the
// frame sendMountCheckFailed sends, and that it sends none to a session
// that does not read it.
func TestSendMountCheckFailedSendsTheNoticeToASessionThatReadsIt(t *testing.T) {
	type frame struct {
		t protocol.FrameType
		v any
	}
	for _, reads := range []bool{true, false} {
		var c sessionConn
		c.readsMountCheck.Store(reads)
		var sent []frame
		send := func(t protocol.FrameType, v any) error {
			sent = append(sent, frame{t, v})
			return nil
		}
		if err := c.sendMountCheckFailed(send, "boom", "git"); err != nil {
			t.Fatal(err)
		}
		want := []frame{{protocol.FrameMountCheckFailed, protocol.MountCheckFailed{Error: "boom", Rule: "git"}}}
		if !reads {
			want = nil
		}
		if !slices.Equal(sent, want) {
			t.Fatalf("reads %v: sent %+v, want %+v", reads, sent, want)
		}
	}
}

// TestSendMountCheckFailedReturnsTheErrorOfSend checks that a frame the
// session does not take is reported.
func TestSendMountCheckFailedReturnsTheErrorOfSend(t *testing.T) {
	var c sessionConn
	c.readsMountCheck.Store(true)
	gone := errors.New("gone")
	err := c.sendMountCheckFailed(func(protocol.FrameType, any) error { return gone }, "boom", "")
	if !errors.Is(err, gone) {
		t.Fatalf("got %v, want %v", err, gone)
	}
}

// loggedRecords is a slog.Handler that keeps every record logged, for a
// test to read while the server still logs. It keeps no attributes or
// groups added by With.
type loggedRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

// Enabled reports that every level is kept.
func (h *loggedRecords) Enabled(context.Context, slog.Level) bool { return true }

// Handle keeps r.
func (h *loggedRecords) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// WithAttrs returns h, dropping attrs.
func (h *loggedRecords) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup returns h, dropping the group.
func (h *loggedRecords) WithGroup(string) slog.Handler { return h }

// waitReason waits until a record with message msg is logged and returns
// its "reason" attribute, failing the test if none is within a few
// seconds.
func (h *loggedRecords) waitReason(t *testing.T, msg string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		for _, r := range h.records {
			if r.Message != msg {
				continue
			}
			reason := ""
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "reason" {
					reason = a.Value.String()
					return false
				}
				return true
			})
			h.mu.Unlock()
			return reason
		}
		h.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no %q was logged", msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// approvedRunLastCheck approves an --ask of `echo hi` on a server whose
// per-request mount check passes the --ask's own check and makes last the
// check right before the approved run starts, under the daemon context,
// which stop cancels. It returns the records the server logged.
func approvedRunLastCheck(
	t *testing.T,
	last func(ctx context.Context) error,
	stop func(cancel context.CancelFunc),
) *loggedRecords {
	t.Helper()
	var calls atomic.Int32
	s, peer := mountCheckServer(t, protectedStage(func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return nil
		}
		return last(ctx)
	}))
	collectFrames(peer)
	logged := &loggedRecords{}
	s.log = slog.New(logged)
	daemonCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	daemonEnd, client := net.Pipe()
	t.Cleanup(func() { client.Close() })
	go s.handle(daemonCtx, daemonEnd)
	if err := protocol.WriteJSON(client, protocol.FrameAsk, protocol.Ask{Request: echoRequest("echo", "hi")}); err != nil {
		t.Fatal(err)
	}
	frames := collectFrames(client)
	id := waitingID(t, s)
	if why, _ := s.approvals.answer(s.approvals.serving, protocol.Answer{ID: id, Scope: protocol.ScopeOnce}); why != "" {
		t.Fatalf("the answer was not taken: %s", why)
	}
	stop(cancel)
	<-frames
	return logged
}

// TestAnApprovedRunWhoseLastCheckAStopCutsShortIsLoggedAsDraining checks
// that an approved run that does not start because the daemon stops while
// its last mount check runs is logged as such, not as a failed check.
func TestAnApprovedRunWhoseLastCheckAStopCutsShortIsLoggedAsDraining(t *testing.T) {
	started := make(chan struct{})
	logged := approvedRunLastCheck(t, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, func(cancel context.CancelFunc) {
		<-started
		cancel()
	})
	if got := logged.waitReason(t, "approval not run"); got != string(endedDraining) {
		t.Fatalf("logged reason %q, want %q", got, endedDraining)
	}
}

// TestAnApprovedRunWhoseLastCheckFailsIsLoggedAsAFailedCheck checks that an
// approved run its last mount check refuses is logged as refused by the
// check.
func TestAnApprovedRunWhoseLastCheckFailsIsLoggedAsAFailedCheck(t *testing.T) {
	logged := approvedRunLastCheck(t, func(context.Context) error {
		return writableAt("/host/app")
	}, func(context.CancelFunc) {})
	if got := logged.waitReason(t, "approval not run"); got != string(endedMountCheck) {
		t.Fatalf("logged reason %q, want %q", got, endedMountCheck)
	}
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
	r := s.mount.requestRefusal(ctx, nil)
	if r == nil || r.code != protocol.ExitHostrunError || r.message != drainingMessage || !errors.Is(r.err, errDraining) {
		t.Fatalf("got %+v, want the refusal of a stopping daemon", r)
	}
	if r := s.mount.requestRefusal(context.Background(), nil); r == nil || r.code != protocol.ExitRejected {
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
	go func() { first <- s.mount.requestRefusal(context.Background(), nil) }()
	<-started
	if r := s.mount.requestRefusal(context.Background(), nil); r != nil {
		t.Fatalf("got %+v, want the later check to pass", r)
	}
	close(release)
	if r := <-first; r == nil || r.code != protocol.ExitRejected {
		t.Fatalf("got %+v, want the stale check to refuse its own request", r)
	}
	if r := s.mount.requestRefusal(context.Background(), nil); r == nil {
		t.Fatal("the next check passed, want it to fail")
	}
	if got := noticed(t, s, frames); len(got) != 1 || got[0] != later.Error() {
		t.Fatalf("the session was told of %q, want of %q alone", got, later.Error())
	}
}
