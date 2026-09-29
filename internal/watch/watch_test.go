package watch

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRuntime reports whatever the test stores in present, or fails.
type fakeRuntime struct {
	present atomic.Bool
	fail    atomic.Bool
}

func (f *fakeRuntime) Running(context.Context, string) (bool, error) {
	if f.fail.Load() {
		return false, errors.New("runtime unavailable")
	}
	return f.present.Load(), nil
}

// hangingRuntime never answers until its context ends.
type hangingRuntime struct{}

func (hangingRuntime) Running(ctx context.Context, _ string) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

var testConfig = Config{
	Workspace:      "/home/u/app",
	PollInterval:   5 * time.Millisecond,
	QueryTimeout:   20 * time.Millisecond,
	StartupTimeout: 300 * time.Millisecond,
	Grace:          100 * time.Millisecond,
}

// startWatcher runs a Watcher in the background and returns it with its
// result channel.
func startWatcher(ctx context.Context, log *slog.Logger, runtimes ...Runtime) (*Watcher, <-chan error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := New(runtimes, testConfig, log)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return w, done
}

// startWait runs a Watcher that is never rearmed.
func startWait(ctx context.Context, runtimes ...Runtime) <-chan error {
	_, done := startWatcher(ctx, nil, runtimes...)
	return done
}

// expectRunning fails if the watcher returned within d.
func expectRunning(t *testing.T, done <-chan error, d time.Duration, why string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("watcher returned %v %s", err, why)
	case <-time.After(d):
	}
}

// expectResult waits for the watcher's result and checks it.
func expectResult(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("watcher did not return (want %v)", want)
	}
}

func TestWaitGivesUpWhenContainerNeverStarts(t *testing.T) {
	done := startWait(context.Background(), &fakeRuntime{})
	select {
	case err := <-done:
		if !errors.Is(err, ErrNeverStarted) {
			t.Fatalf("got %v, want ErrNeverStarted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not give up after the startup timeout")
	}
}

func TestWaitReturnsAfterContainerStopsForGracePeriod(t *testing.T) {
	rt := &fakeRuntime{}
	done := startWait(context.Background(), rt)
	rt.present.Store(true)
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v while the container runs", err)
	default:
	}

	stopped := time.Now()
	rt.present.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("got %v, want nil", err)
		}
		if waited := time.Since(stopped); waited < testConfig.Grace {
			t.Fatalf("returned after %v, before the %v grace period", waited, testConfig.Grace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the container stopped")
	}
}

func TestWaitSurvivesShortAbsenceDuringRebuild(t *testing.T) {
	rt := &fakeRuntime{}
	done := startWait(context.Background(), rt)
	rt.present.Store(true)
	time.Sleep(30 * time.Millisecond)
	rt.present.Store(false) // old container removed…
	time.Sleep(testConfig.Grace / 3)
	rt.present.Store(true) // …new one started
	time.Sleep(testConfig.Grace * 2)
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v across a rebuild gap", err)
	default:
	}
}

func TestWaitStaysAttachedWhileAnyRuntimeReportsTheContainer(t *testing.T) {
	broken := &fakeRuntime{}
	broken.fail.Store(true)
	healthy := &fakeRuntime{}
	healthy.present.Store(true)
	done := startWait(context.Background(), broken, healthy)
	time.Sleep(testConfig.StartupTimeout + testConfig.Grace)
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v although a runtime reports the container", err)
	default:
	}
}

func TestWaitStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &fakeRuntime{}
	rt.present.Store(true)
	done := startWait(ctx, rt)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait ignored cancellation")
	}
}

func TestRearmReturnsWatcherToWaiting(t *testing.T) {
	rt := &fakeRuntime{}
	w, done := startWatcher(context.Background(), nil, rt)
	rt.present.Store(true)
	time.Sleep(30 * time.Millisecond)

	// A rebuild: the container goes away and `up` rearms the daemon.
	rt.present.Store(false)
	time.Sleep(testConfig.Grace / 2)
	w.Rearm()
	expectRunning(t, done, testConfig.Grace*3/2, "within the grace period although it was rearmed")

	// The rebuilt container comes up: the watcher is attached again.
	rt.present.Store(true)
	time.Sleep(testConfig.StartupTimeout)
	expectRunning(t, done, 10*time.Millisecond, "while the rebuilt container runs")

	rt.present.Store(false)
	expectResult(t, done, nil)
}

func TestRearmSurvivesRebuildThatRemovesContainerAfterArming(t *testing.T) {
	rt := &fakeRuntime{}
	rt.present.Store(true)
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)

	// devcontainer's order on a rebuild: initializeCommand arms the daemon
	// while the old container still runs, then the container is removed and
	// the image builds for longer than the grace period.
	w.Rearm()
	time.Sleep(testConfig.Grace / 5)
	rt.present.Store(false)
	expectRunning(t, done, testConfig.Grace*2, "during an image build after being armed")

	rt.present.Store(true) // the rebuilt container starts
	time.Sleep(testConfig.StartupTimeout)
	expectRunning(t, done, 10*time.Millisecond, "while the rebuilt container runs")
	rt.present.Store(false)
	expectResult(t, done, nil)
}

func TestRearmWithoutRebuildKeepsFollowingTheContainer(t *testing.T) {
	rt := &fakeRuntime{}
	rt.present.Store(true)
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)
	w.Rearm() // a plain re-up: the container keeps running
	time.Sleep(testConfig.StartupTimeout + testConfig.Grace)
	expectRunning(t, done, 10*time.Millisecond, "while the container still runs")
	rt.present.Store(false)
	expectResult(t, done, nil)
}

func TestRearmedWatcherGivesUpIfNoContainerReturns(t *testing.T) {
	rt := &fakeRuntime{}
	rt.present.Store(true)
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)
	rt.present.Store(false)
	w.Rearm()
	expectResult(t, done, ErrNeverStarted)
}

func TestFailingQueriesDoNotEndAttachedWatch(t *testing.T) {
	rt := &fakeRuntime{}
	rt.present.Store(true)
	_, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)
	rt.fail.Store(true) // runtime down: the container's state is unknown
	expectRunning(t, done, testConfig.Grace*2, "while the runtime could not answer")
	expectResult(t, done, ErrRuntimeUnavailable)
}

func TestOnlyFailingRuntimeNeverStarts(t *testing.T) {
	rt := &fakeRuntime{}
	rt.fail.Store(true)
	expectResult(t, startWait(context.Background(), rt), ErrNeverStarted)
}

func TestHangingRuntimeDoesNotStallWatch(t *testing.T) {
	rt := &fakeRuntime{}
	rt.present.Store(true)
	done := startWait(context.Background(), hangingRuntime{}, rt)
	time.Sleep(50 * time.Millisecond)
	rt.present.Store(false)
	expectResult(t, done, nil)
}

// countingHandler counts warnings.
type countingHandler struct {
	slog.Handler
	warnings atomic.Int32
}

func (h *countingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warnings.Add(1)
	}
	return nil
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func TestFailingRuntimeIsLoggedOnce(t *testing.T) {
	h := &countingHandler{Handler: slog.DiscardHandler}
	broken := &fakeRuntime{}
	broken.fail.Store(true)
	healthy := &fakeRuntime{}
	healthy.present.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	_, done := startWatcher(ctx, slog.New(h), broken, healthy)
	time.Sleep(20 * testConfig.PollInterval)
	cancel()
	<-done
	if n := h.warnings.Load(); n != 1 {
		t.Fatalf("got %d warnings for one failing runtime, want 1", n)
	}
}

// fakeCLI writes an executable that prints "abc123" only when invoked with
// exactly the arguments a running-container query must use.
func fakeCLI(t *testing.T, workspace string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakert")
	script := `#!/bin/sh
[ "$*" = "ps -q --filter label=devcontainer.local_folder=` + workspace + ` --filter status=running" ] && echo abc123
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRuntimeQueriesContainersByWorkspaceLabel(t *testing.T) {
	rt := CLI{Binary: fakeCLI(t, "/home/u/app")}
	running, err := rt.Running(context.Background(), "/home/u/app")
	if err != nil || !running {
		t.Fatalf("got %v, %v; want true, nil", running, err)
	}
	running, err = rt.Running(context.Background(), "/home/u/other")
	if err != nil || running {
		t.Fatalf("other workspace: got %v, %v; want false, nil", running, err)
	}
}

func TestCLIRuntimeReportsFailingBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'daemon down' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (CLI{Binary: path}).Running(context.Background(), "/home/u/app"); err == nil {
		t.Fatal("expected an error from a failing runtime binary")
	}
}

func TestDetectFindsRuntimesInPath(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := Detect()
	if len(got) != 1 || got[0] != (CLI{Binary: docker}) {
		t.Fatalf("got %v, want only %s", got, docker)
	}
}
