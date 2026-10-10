package watch_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

// hangingRuntime never answers until its context ends.
type hangingRuntime struct{}

func (hangingRuntime) Containers(ctx context.Context, _ string) ([]watch.Container, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

var testConfig = watch.Config{
	Workspace:      "/home/u/app",
	PollInterval:   5 * time.Millisecond,
	QueryTimeout:   20 * time.Millisecond,
	StartupTimeout: 300 * time.Millisecond,
	Grace:          100 * time.Millisecond,
}

// startWatcher runs a watch.Watcher with testConfig in the background and
// returns it with its result channel.
func startWatcher(ctx context.Context, log *slog.Logger, runtimes ...watch.Runtime) (*watch.Watcher, <-chan error) {
	return startWatcherWith(ctx, testConfig, log, runtimes...)
}

// startWatcherWith is startWatcher with another config.
func startWatcherWith(ctx context.Context, cfg watch.Config, log *slog.Logger, runtimes ...watch.Runtime) (*watch.Watcher, <-chan error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := watch.New(runtimes, cfg, log)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return w, done
}

// startWait runs a watch.Watcher that is armed only by being created.
func startWait(ctx context.Context, runtimes ...watch.Runtime) <-chan error {
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

func TestArmedContainerStoppedBeforeAnyPollEndsWatch(t *testing.T) {
	cfg := testConfig
	cfg.PollInterval = cfg.Grace * 2 // the container lives between two polls
	rt := &watchtest.Runtime{}
	w, done := startWatcherWith(context.Background(), cfg, nil, rt)
	time.Sleep(10 * time.Millisecond) // the first poll has seen nothing

	w.Arm()
	rt.Start("abc")
	time.Sleep(10 * time.Millisecond)
	rt.Stop("abc")
	expectResult(t, done, nil)
}

func TestArmedContainerStoppedWithinGraceOfArmEndsWatch(t *testing.T) {
	rt := &watchtest.Runtime{}
	w, done := startWatcher(context.Background(), nil, rt)
	w.Arm()
	rt.Start("abc")
	time.Sleep(testConfig.Grace / 2) // polls see it running
	rt.Stop("abc")
	expectResult(t, done, nil)
}

func TestArmedContainerStoppedBeforeAttachStillGetsGrace(t *testing.T) {
	cfg := testConfig
	cfg.PollInterval = cfg.Grace * 3 / 2
	cfg.StartupTimeout = 2 * time.Second
	rt := &watchtest.Runtime{}
	w, done := startWatcherWith(context.Background(), cfg, nil, rt)
	w.Arm() // polls see nothing while the image builds

	time.Sleep(cfg.Grace)
	rt.Start("c") // the new container lives between two polls
	rt.Stop("c")
	stopped := time.Now()
	expectResult(t, done, nil)
	if waited := time.Since(stopped); waited < cfg.Grace {
		t.Fatalf("exited %v after the armed container stopped, before the %v grace period", waited, cfg.Grace)
	}
}

func TestOnlyArmedContainerKeepsWatch(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Start("other") // started before the arm and left running
	w, done := startWatcher(context.Background(), nil, rt)
	w.Arm()
	rt.Start("armed")
	time.Sleep(testConfig.Grace / 2) // attached to "armed"

	rt.Stop("armed")
	expectResult(t, done, nil)
}

func TestContainersOfEveryAnsweringRuntimeCount(t *testing.T) {
	empty, docker := &watchtest.Runtime{}, &watchtest.Runtime{}
	w, done := startWatcher(context.Background(), nil, docker, empty)
	w.Arm()
	docker.Start("c")
	expectRunning(t, done, testConfig.Grace*2, "while one runtime reports the armed container and the other none")

	docker.Stop("c")
	expectResult(t, done, nil)
}

func TestArmedContainerStartedAgainUnderSameIDIsFollowed(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Start("c")
	rt.Stop("c")
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)

	// `up` on a stopped container starts the same ID again after the arm,
	// and it is stopped before a grace has passed.
	w.Arm()
	rt.Start("c")
	time.Sleep(testConfig.Grace / 2)
	rt.Stop("c")
	expectResult(t, done, nil)
}

func TestArmedContainerRestartShorterThanGraceKeepsWatch(t *testing.T) {
	rt := &watchtest.Runtime{}
	w, done := startWatcher(context.Background(), nil, rt)
	w.Arm()
	rt.Start("c")
	time.Sleep(30 * time.Millisecond)

	rt.Stop("c") // docker restart
	time.Sleep(testConfig.Grace / 3)
	rt.Start("c")
	expectRunning(t, done, testConfig.Grace*2, "across a restart shorter than the grace period")
}

// raceConfig polls less often than the grace period, so a test can act
// between the poll that sees the armed container stopped and the one that
// ends the watch. The race tests run in a synctest bubble, where these
// durations are exact.
func raceConfig() watch.Config {
	cfg := testConfig
	cfg.PollInterval = 150 * time.Millisecond
	cfg.Grace = 100 * time.Millisecond
	cfg.StartupTimeout = 2 * time.Second
	return cfg
}

// holdNextQuery holds rt's next query and returns once the watcher is in
// it, i.e. once a poll has started and before it decides anything.
func holdNextQuery(t *testing.T, rt *watchtest.Runtime) (release func()) {
	t.Helper()
	entered, release := rt.Hold()
	select {
	case <-entered:
	case <-time.After(time.Minute):
		release()
		t.Fatal("no query reached the runtime")
	}
	return release
}

// expectArmAccepted arms w and fails if it refuses.
func expectArmAccepted(t *testing.T, w *watch.Watcher) {
	t.Helper()
	if err := w.Arm(); err != nil {
		t.Fatalf("arm refused: %v", err)
	}
}

// expectFollowsNewContainer checks that the watch goes on for the arm just
// made: it attaches to a container started now and ends a grace after that
// container stops.
func expectFollowsNewContainer(t *testing.T, cfg watch.Config, rt *watchtest.Runtime, done <-chan error) {
	t.Helper()
	time.Sleep(cfg.PollInterval / 10)
	rt.Start("new")
	time.Sleep(cfg.PollInterval)
	rt.Stop("new")
	expectResult(t, done, nil)
}

func TestArmDuringPollThatEndsWatchWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := raceConfig()
		rt := &watchtest.Runtime{}
		w, done := startWatcherWith(context.Background(), cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 5) // after the first poll
		rt.Start("old")
		rt.Stop("old")               // the poll at 150 ms attaches and sees it stopped
		time.Sleep(cfg.PollInterval) // the poll at 300 ms ends the watch

		release := holdNextQuery(t, rt)
		expectArmAccepted(t, w)
		release()
		synctest.Wait() // the poll has decided
		expectRunning(t, done, cfg.PollInterval, "although armed before it decided to end")
		expectFollowsNewContainer(t, cfg, rt, done)
	})
}

func TestArmDuringPollThatHitsStartupTimeoutWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := raceConfig()
		cfg.StartupTimeout = 200 * time.Millisecond // the poll at 300 ms gives up
		rt := &watchtest.Runtime{}
		w, done := startWatcherWith(context.Background(), cfg, nil, rt)
		time.Sleep(cfg.PollInterval + cfg.PollInterval/5)

		release := holdNextQuery(t, rt)
		expectArmAccepted(t, w)
		release()
		synctest.Wait() // the poll has decided
		expectFollowsNewContainer(t, cfg, rt, done)
	})
}

func TestArmDuringPollWithoutRuntimeAnswerWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := raceConfig()
		cfg.StartupTimeout = 200 * time.Millisecond
		rt := &watchtest.Runtime{}
		w, done := startWatcherWith(context.Background(), cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 5)
		rt.Start("c")                // the poll at 150 ms attaches
		time.Sleep(cfg.PollInterval) // at 180 ms
		rt.Fail(true)                // no answer from 300 ms; the poll at 450 ms gives up
		time.Sleep(cfg.PollInterval)

		release := holdNextQuery(t, rt)
		expectArmAccepted(t, w)
		release()
		synctest.Wait() // the poll has decided
		rt.Fail(false)
		expectFollowsNewContainer(t, cfg, rt, done)
	})
}

func TestArmWhileDaemonStopsIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		rt := &watchtest.Runtime{}
		w, done := startWatcherWith(ctx, raceConfig(), nil, rt)
		release := holdNextQuery(t, rt)
		defer release()

		cancel() // SIGTERM while a poll runs
		synctest.Wait()
		if err := w.Arm(); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want the arm refused as the daemon stops", err)
		}
		release()
		expectResult(t, done, context.Canceled)
	})
}

func TestArmAfterWatchEndedIsRefused(t *testing.T) {
	tests := []struct {
		name string
		end  func(rt *watchtest.Runtime, cancel context.CancelFunc)
		want error // from Run
	}{
		{"armed container stopped", func(rt *watchtest.Runtime, _ context.CancelFunc) {
			rt.Start("c")
			time.Sleep(20 * time.Millisecond)
			rt.Stop("c")
		}, nil},
		{"startup timeout", func(*watchtest.Runtime, context.CancelFunc) {}, watch.ErrNeverStarted},
		{"no runtime answered", func(rt *watchtest.Runtime, _ context.CancelFunc) {
			rt.Start("c")
			time.Sleep(20 * time.Millisecond)
			rt.Fail(true)
		}, watch.ErrRuntimeUnavailable},
		{"cancelled", func(_ *watchtest.Runtime, cancel context.CancelFunc) { cancel() }, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rt := &watchtest.Runtime{}
			w, done := startWatcher(ctx, nil, rt)
			tt.end(rt, cancel)
			expectResult(t, done, tt.want)
			err := w.Arm()
			if err == nil {
				t.Fatal("arm accepted after the watch ended")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("arm refused with %v, want the reason %v", err, tt.want)
			}
		})
	}
}

func TestArmWhileWatchingIsAccepted(t *testing.T) {
	rt := &watchtest.Runtime{}
	w, done := startWatcher(context.Background(), nil, rt)
	expectArmAccepted(t, w) // waiting
	rt.Start("c")
	time.Sleep(testConfig.Grace / 2)
	expectArmAccepted(t, w) // attached
	expectRunning(t, done, 10*time.Millisecond, "after being armed")
}

func TestWaitGivesUpWhenContainerNeverStarts(t *testing.T) {
	done := startWait(context.Background(), &watchtest.Runtime{})
	select {
	case err := <-done:
		if !errors.Is(err, watch.ErrNeverStarted) {
			t.Fatalf("got %v, want watch.ErrNeverStarted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not give up after the startup timeout")
	}
}

func TestWaitReturnsAfterContainerStopsForGracePeriod(t *testing.T) {
	rt := &watchtest.Runtime{}
	done := startWait(context.Background(), rt)
	rt.Start("c")
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v while the container runs", err)
	default:
	}

	stopped := time.Now()
	rt.Stop("c")
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
	rt := &watchtest.Runtime{}
	done := startWait(context.Background(), rt)
	rt.Start("old")
	time.Sleep(30 * time.Millisecond)
	rt.Remove("old") // old container removed…
	time.Sleep(testConfig.Grace / 3)
	rt.Start("new") // …new one started
	time.Sleep(testConfig.Grace * 2)
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v across a rebuild gap", err)
	default:
	}
}

func TestWaitStaysAttachedWhileAnyRuntimeReportsTheContainer(t *testing.T) {
	broken := &watchtest.Runtime{}
	broken.Fail(true)
	healthy := &watchtest.Runtime{}
	healthy.Start("c")
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
	rt := &watchtest.Runtime{}
	rt.Start("c")
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

func TestArmReturnsWatcherToWaiting(t *testing.T) {
	rt := &watchtest.Runtime{}
	w, done := startWatcher(context.Background(), nil, rt)
	rt.Start("old")
	time.Sleep(30 * time.Millisecond)

	// A rebuild: the container goes away and `up` arms the daemon.
	rt.Remove("old")
	time.Sleep(testConfig.Grace / 2)
	w.Arm()
	expectRunning(t, done, testConfig.Grace*3/2, "within the grace period although it was armed")

	// The rebuilt container comes up: the watcher is attached again.
	rt.Start("new")
	time.Sleep(testConfig.StartupTimeout)
	expectRunning(t, done, 10*time.Millisecond, "while the rebuilt container runs")

	rt.Stop("new")
	expectResult(t, done, nil)
}

func TestArmSurvivesRebuildThatRemovesContainerAfterArming(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Start("old")
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)

	// devcontainer's order on a rebuild: initializeCommand arms the daemon
	// while the old container still runs, then the container is removed and
	// the image builds for longer than the grace period.
	w.Arm()
	time.Sleep(testConfig.Grace / 5)
	rt.Remove("old")
	expectRunning(t, done, testConfig.Grace*2, "during an image build after being armed")

	rt.Start("new") // the rebuilt container starts
	time.Sleep(testConfig.StartupTimeout)
	expectRunning(t, done, 10*time.Millisecond, "while the rebuilt container runs")
	rt.Stop("new")
	expectResult(t, done, nil)
}

func TestArmOnRunningContainerKeepsFollowingIt(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Start("c")
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)
	w.Arm() // `up` on a running container: it keeps running, nothing starts
	time.Sleep(testConfig.StartupTimeout + testConfig.Grace)
	expectRunning(t, done, 10*time.Millisecond, "while the container still runs")
	rt.Stop("c")
	expectResult(t, done, nil)
}

func TestArmedWatcherGivesUpIfNoContainerStarts(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Start("c")
	w, done := startWatcher(context.Background(), nil, rt)
	time.Sleep(30 * time.Millisecond)
	rt.Stop("c")
	w.Arm()
	expectResult(t, done, watch.ErrNeverStarted)
}

func TestFailingQueriesDoNotEndAttachedWatch(t *testing.T) {
	rt := &watchtest.Runtime{}
	_, done := startWatcher(context.Background(), nil, rt)
	rt.Start("c")
	time.Sleep(30 * time.Millisecond)
	rt.Fail(true) // runtime down: the container's state is unknown
	expectRunning(t, done, testConfig.Grace*2, "while the runtime could not answer")
	expectResult(t, done, watch.ErrRuntimeUnavailable)
}

func TestOnlyFailingRuntimeNeverStarts(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Fail(true)
	expectResult(t, startWait(context.Background(), rt), watch.ErrNeverStarted)
}

func TestHangingRuntimeDoesNotStallWatch(t *testing.T) {
	rt := &watchtest.Runtime{}
	done := startWait(context.Background(), hangingRuntime{}, rt)
	rt.Start("c")
	time.Sleep(50 * time.Millisecond)
	rt.Stop("c")
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
	broken := &watchtest.Runtime{}
	broken.Fail(true)
	healthy := &watchtest.Runtime{}
	healthy.Start("c")
	ctx, cancel := context.WithCancel(context.Background())
	_, done := startWatcher(ctx, slog.New(h), broken, healthy)
	time.Sleep(20 * testConfig.PollInterval)
	cancel()
	<-done
	if n := h.warnings.Load(); n != 1 {
		t.Fatalf("got %d warnings for one failing runtime, want 1", n)
	}
}

// fakeCLI writes an executable that answers a container listing for
// workspace with containers a (running) and b (stopped, its start time with
// an offset as podman prints it), and their inspection in the format the
// watch.CLI runtime asks for. With bErr set, inspecting b fails with that message
// instead, as when it is gone by then.
func fakeCLI(t *testing.T, workspace, bErr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakert")
	inspectB := `echo 'b false "2026-10-09T12:00:05.5+02:00"'`
	if bErr != "" {
		inspectB = `echo '` + bErr + `' >&2; status=1`
	}
	script := `#!/bin/sh
status=0
case "$*" in
"ps -aq --filter label=devcontainer.local_folder=` + workspace + `")
	echo a; echo b ;;
"ps -aq --filter label=devcontainer.local_folder="*) ;;
"inspect --type container --format {{.Id}} {{.State.Running}} {{json .State.StartedAt}} a b")
	echo 'a true "2026-10-09T10:00:00.123456789Z"'
	` + inspectB + ` ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
exit $status
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRuntimeListsWorkspaceContainersInEveryState(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLI(t, "/home/u/app", "")}
	got, err := rt.Containers(context.Background(), "/home/u/app")
	want := []watch.Container{
		{ID: "a", Running: true, StartedAt: time.Date(2026, 10, 9, 10, 0, 0, 123456789, time.UTC)},
		{ID: "b", Running: false, StartedAt: time.Date(2026, 10, 9, 10, 0, 5, 500000000, time.UTC)},
	}
	if err != nil || !slices.EqualFunc(got, want, sameContainer) {
		t.Fatalf("got %v, %v; want %v, nil", got, err, want)
	}
}

func TestCLIRuntimeSkipsInspectionWithoutContainers(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLI(t, "/home/u/app", "")}
	got, err := rt.Containers(context.Background(), "/home/u/other")
	if err != nil || len(got) != 0 {
		t.Fatalf("other workspace: got %v, %v; want none, nil", got, err)
	}
}

func TestCLIRuntimeTreatsContainerGoneBeforeInspectionAsAbsent(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLI(t, "/home/u/app", "Error: No such container: b")}
	got, err := rt.Containers(context.Background(), "/home/u/app")
	onlyA := len(got) == 1 && got[0].ID == "a"
	if err != nil || !onlyA {
		t.Fatalf("got %v, %v; want only a, nil", got, err)
	}
}

func TestCLIRuntimeReportsOtherInspectionFailure(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLI(t, "/home/u/app", "Error: open /var/lib/containers/x: no such file or directory")}
	if got, err := rt.Containers(context.Background(), "/home/u/app"); err == nil {
		t.Fatalf("got %v, nil; want the runtime's error", got)
	}
}

func sameContainer(a, b watch.Container) bool {
	return a.ID == b.ID && a.Running == b.Running && a.StartedAt.Equal(b.StartedAt)
}

func TestCLIRuntimeReportsFailingBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'daemon down' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (watch.CLI{Binary: path}).Containers(context.Background(), "/home/u/app"); err == nil {
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
	got := watch.Detect()
	if len(got) != 1 || got[0] != (watch.CLI{Binary: docker}) {
		t.Fatalf("got %v, want only %s", got, docker)
	}
}
