package watch_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kravlab/hostrunner/internal/procgroup"
	"github.com/kravlab/hostrunner/internal/procgroup/procgrouptest"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

// hangingRuntime never answers until its context ends.
type hangingRuntime struct{}

func (hangingRuntime) Containers(ctx context.Context, _ string) ([]watch.Container, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingRuntime) MountTable(ctx context.Context, _ int) ([]watch.MountPoint, error) {
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

const (
	// mountsOfA is a's mounts: a bind mount whose source has a space, a
	// read-only one, a named volume whose source is under the runtime's
	// storage, and a tmpfs without a source.
	mountsOfA = `[{"Type":"bind","Source":"/home/u/my app","Destination":"/workspace","Mode":"","RW":true,"Propagation":"rprivate"},` +
		`{"Type":"bind","Source":"/etc/ro","Destination":"/ro","Mode":"ro","RW":false,"Propagation":"rprivate"},` +
		`{"Type":"volume","Name":"data","Source":"/var/lib/example/volumes/data/_data","Destination":"/data","Driver":"local","Mode":"z","RW":true,"Propagation":""},` +
		`{"Type":"tmpfs","Source":"","Destination":"/tmp","Mode":"","RW":true,"Propagation":""}]`
	noMounts = "null"
	// dataVolume is how the runtime answers the inspection of the volume
	// data that a mounts: a local volume that binds a host directory, as
	// `volume inspect --format {{json .}}` prints it.
	dataVolume = `echo '{"CreatedAt":"2026-10-09T10:00:00Z","Driver":"local","Labels":null,"Mountpoint":"/var/lib/example/volumes/data/_data","Name":"data","Options":{"device":"/home/u/data","o":"bind","type":"none"},"Scope":"local"}'`
)

// fakeCLI writes an executable that answers a container listing for
// workspace with containers a (running, with mountsOfA) and b (stopped, its
// start time with an offset as podman prints it, without mounts), and their
// inspection in the format the watch.CLI runtime asks for. With bErr set,
// inspecting b fails with that message instead, as when it is gone by then.
func fakeCLI(t *testing.T, workspace, bErr string) string {
	t.Helper()
	return fakeCLIMounts(t, workspace, bErr, mountsOfA, noMounts)
}

// fakeCLIMounts is fakeCLI with the mounts field of a and b given verbatim.
func fakeCLIMounts(t *testing.T, workspace, bErr, mountsA, mountsB string) string {
	t.Helper()
	return fakeCLIVolumes(t, workspace, bErr, mountsA, mountsB, dataVolume)
}

// fakeCLIVolumes is fakeCLIMounts with volume, the shell commands that
// answer the inspection of the volume data, given verbatim.
func fakeCLIVolumes(t *testing.T, workspace, bErr, mountsA, mountsB, volume string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakert")
	inspectB := `echo 'b false 0 "2026-10-09T12:00:05.5+02:00" ` + mountsB + `'`
	if bErr != "" {
		inspectB = `echo '` + bErr + `' >&2; status=1`
	}
	script := `#!/bin/sh
status=0
case "$*" in
"ps -aq --filter label=devcontainer.local_folder=` + workspace + `")
	echo a; echo b ;;
"ps -aq --filter label=devcontainer.local_folder="*) ;;
"inspect --type container --format {{.Id}} {{.State.Running}} {{.State.Pid}} {{json .State.StartedAt}} {{json .Mounts}} a b")
	echo 'a true 4242 "2026-10-09T10:00:00.123456789Z" ` + mountsA + `'
	` + inspectB + ` ;;
"volume inspect --format {{json .}} data")
	` + volume + ` ;;
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

func TestCLIRuntimeReportsTheInitProcessOfEachContainer(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLI(t, "/home/u/app", "")}
	got, err := rt.Containers(context.Background(), "/home/u/app")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v; want two containers, nil", got, err)
	}
	if got[0].Pid != 4242 || got[1].Pid != 0 {
		t.Fatalf("got pids %d (running a), %d (stopped b); want 4242, 0", got[0].Pid, got[1].Pid)
	}
}

func TestCLIRuntimeReportsMounts(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLI(t, "/home/u/app", "")}
	got, err := rt.Containers(context.Background(), "/home/u/app")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v; want two containers, nil", got, err)
	}
	want := []watch.Mount{
		{Type: "bind", Source: "/home/u/my app", Destination: "/workspace"},
		{Type: "bind", Source: "/etc/ro", Destination: "/ro", ReadOnly: true},
		{Type: "volume", Name: "data", Source: "/var/lib/example/volumes/data/_data", Destination: "/data",
			Volume: &watch.Volume{Driver: "local", Options: map[string]string{"device": "/home/u/data", "o": "bind", "type": "none"}}},
		{Type: "tmpfs", Destination: "/tmp"},
	}
	if !slices.EqualFunc(got[0].Mounts, want, sameMount) || got[0].MountsErr != nil {
		t.Fatalf("a: got %v, %v; want %v, nil", got[0].Mounts, got[0].MountsErr, want)
	}
	if len(got[1].Mounts) != 0 || got[1].MountsErr != nil {
		t.Fatalf("b (null mounts): got %v, %v; want none, nil", got[1].Mounts, got[1].MountsErr)
	}
}

func TestCLIRuntimeReportsUnparsableMountsPerContainer(t *testing.T) {
	rt := watch.CLI{Binary: fakeCLIMounts(t, "/home/u/app", "", mountsOfA, "{not json")}
	got, err := rt.Containers(context.Background(), "/home/u/app")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v; want two containers, nil", got, err)
	}
	if got[0].MountsErr != nil || len(got[0].Mounts) != 4 {
		t.Fatalf("a: got %v, %v; want its four mounts, nil", got[0].Mounts, got[0].MountsErr)
	}
	b := got[1]
	if b.ID != "b" || b.Running || b.StartedAt.IsZero() {
		t.Fatalf("b: got %+v; want its id, state and start time intact", b)
	}
	if b.MountsErr == nil || !strings.Contains(b.MountsErr.Error(), "b") || len(b.Mounts) != 0 {
		t.Fatalf("b: got %v, %v; want an error naming b, no mounts", b.Mounts, b.MountsErr)
	}
}

func TestCLIRuntimeReportsMissingMountsField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fakert")
	script := `#!/bin/sh
case "$1" in
ps) echo a ;;
inspect) echo 'a true 4242 "2026-10-09T10:00:00Z"' ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := (watch.CLI{Binary: path}).Containers(context.Background(), "/home/u/app")
	if err != nil || len(got) != 1 || got[0].MountsErr == nil || !got[0].Running {
		t.Fatalf("got %+v, %v; want container a with a MountsErr, nil", got, err)
	}
}

func TestCLIRuntimeReportsUnparsablePidAsInspectionError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fakert")
	script := `#!/bin/sh
case "$1" in
ps) echo a ;;
inspect) echo 'a true not-a-pid "2026-10-09T10:00:00Z" null' ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := (watch.CLI{Binary: path}).Containers(context.Background(), "/home/u/app"); err == nil {
		t.Fatalf("got %v, nil; want an error for the unparsable pid", got)
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

// A volume the runtime cannot inspect, or whose inspection leaves it out,
// is reported on its mount, naming it; the container's other mounts stay.
func TestCLIRuntimeReportsAVolumeItCannotInspect(t *testing.T) {
	for _, tt := range []struct{ name, volume string }{
		{"an inspection that fails", `echo 'Error: no such volume data' >&2; exit 1`},
		{"an inspection that leaves it out", `:`},
		{"an inspection it cannot parse", `echo '{not json'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := watch.CLI{Binary: fakeCLIVolumes(t, "/home/u/app", "", mountsOfA, noMounts, tt.volume)}
			got, err := rt.Containers(context.Background(), "/home/u/app")
			if err != nil || len(got) != 2 || got[0].MountsErr != nil || len(got[0].Mounts) != 4 {
				t.Fatalf("got %+v, %v; want a with its four mounts, nil", got, err)
			}
			m := got[0].Mounts[2]
			if m.Name != "data" || m.Volume != nil || m.VolumeErr == nil || !strings.Contains(m.VolumeErr.Error(), "data") {
				t.Fatalf("the volume mount: got %+v; want no volume and an error naming data", m)
			}
			if got[0].Mounts[0].VolumeErr != nil {
				t.Fatalf("the bind mount: got %+v; want no volume error", got[0].Mounts[0])
			}
		})
	}
}

// A volume mount that names no volume cannot be inspected: it is reported
// as such, and nothing is asked of the runtime for it.
func TestCLIRuntimeReportsAVolumeMountWithoutAName(t *testing.T) {
	mounts := `[{"Type":"volume","Source":"/var/lib/example/volumes/x/_data","Destination":"/x","RW":true}]`
	rt := watch.CLI{Binary: fakeCLIVolumes(t, "/home/u/app", "", mounts, noMounts, `echo unexpected >&2; exit 2`)}
	got, err := rt.Containers(context.Background(), "/home/u/app")
	if err != nil || len(got) != 2 || len(got[0].Mounts) != 1 {
		t.Fatalf("got %+v, %v; want a with its mount, nil", got, err)
	}
	if m := got[0].Mounts[0]; m.Volume != nil || m.VolumeErr == nil {
		t.Fatalf("got %+v; want no volume and an error", m)
	}
}

// sameMount reports whether a and b are the same mount, their volumes
// compared by value.
func sameMount(a, b watch.Mount) bool {
	va, vb := a.Volume, b.Volume
	a.Volume, b.Volume = nil, nil
	if a != b || (va == nil) != (vb == nil) {
		return false
	}
	return va == nil || va.Driver == vb.Driver && maps.Equal(va.Options, vb.Options)
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

// containersResult is what CLI.Containers returned.
type containersResult struct {
	containers []watch.Container
	err        error
}

// queryContainers asks the runtime binary for the containers of a workspace
// in the background, and returns the channel its result arrives on.
func queryContainers(ctx context.Context, binary string) <-chan containersResult {
	result := make(chan containersResult, 1)
	go func() {
		containers, err := (watch.CLI{Binary: binary}).Containers(ctx, "/home/u/app")
		result <- containersResult{containers, err}
	}()
	return result
}

func TestCLIRuntimeCancelledQueryEndsWithinWaitDelayAndKillsTheRuntimesChildren(t *testing.T) {
	binary, pidFile := procgrouptest.LeakyCommand(t, nil, "wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := queryContainers(ctx, binary)
	child := procgrouptest.WaitPID(t, pidFile)

	cancel()
	select {
	case got := <-result:
		if got.err == nil {
			t.Fatalf("got %v, nil; want an error from a cancelled query", got.containers)
		}
	case <-time.After(procgroup.WaitDelay + procgrouptest.Margin):
		t.Fatal("the cancelled query did not return")
	}
	if !procgrouptest.Gone(child) {
		t.Fatalf("the runtime's child %d still runs", child)
	}
}

// A runtime that answers and exits, but whose child keeps its output open,
// fails the query: the answer may be cut short, so it is never taken.
func TestCLIRuntimeQueryOfARuntimeThatExitsLeavingItsOutputOpenFails(t *testing.T) {
	binary, _ := procgrouptest.LeakyCommand(t, []string{"a"}, "exit 0")
	result := queryContainers(context.Background(), binary)
	select {
	case got := <-result:
		if got.err == nil || got.containers != nil {
			t.Fatalf("got %v, %v; want no containers and an error from a runtime whose output the bound closed", got.containers, got.err)
		}
	case <-time.After(procgroup.WaitDelay + procgrouptest.Margin):
		t.Fatal("the query did not return while the runtime's child held its output open")
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

// eventually waits until cond holds, polling, and fails with why if it
// does not within a second.
func eventually(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(time.Millisecond)
	}
}

// expectNotStopped fails the test if ContainerStopped holds at any time
// during d.
func expectNotStopped(t *testing.T, w *watch.Watcher, d time.Duration, while string) {
	t.Helper()
	for until := time.Now().Add(d); time.Now().Before(until); time.Sleep(time.Millisecond) {
		if w.ContainerStopped() {
			t.Fatalf("ContainerStopped true %s", while)
		}
	}
}

// ContainerStopped holds while the watcher is attached and saw its armed
// container stopped, counting the grace before it ends: not while waiting
// for it, after an arm too, nor while it runs.
func TestContainerStoppedFollowsTheArmedContainer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &watchtest.Runtime{}
	w, done := startWatcher(ctx, nil, rt)
	expectNotStopped(t, w, 10*testConfig.PollInterval, "while waiting for the first container")
	rt.Start("c")
	expectNotStopped(t, w, 10*testConfig.PollInterval, "while the armed container runs")
	w.Arm()
	// The container that still runs becomes the armed one a grace after
	// the arm; waiting until then.
	expectNotStopped(t, w, 2*testConfig.Grace, "while waiting after an arm, or attached again")
	rt.Stop("c")
	eventually(t, w.ContainerStopped, "ContainerStopped false while the armed container is stopped")
	select {
	case err := <-done:
		t.Fatalf("the watcher ended (%v) before its grace", err)
	default:
	}
	expectResult(t, done, nil)
}

// attachRecorder collects what Config.Attached is called with.
type attachRecorder struct {
	mu      sync.Mutex
	calls   [][]watch.Container
	armedAt []time.Time // the arm of each call
}

func (r *attachRecorder) attached(armedAt time.Time, armed []watch.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, armed)
	r.armedAt = append(r.armedAt, armedAt)
}

// arms returns the arm time of each call.
func (r *attachRecorder) arms() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.armedAt)
}

func (r *attachRecorder) snapshot() [][]watch.Container {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *attachRecorder) count() int { return len(r.snapshot()) }

// ids returns the IDs of one call's containers.
func ids(armed []watch.Container) []string {
	var out []string
	for _, c := range armed {
		out = append(out, c.ID)
	}
	return out
}

func TestAttachedGetsTheArmedContainerWithItsMounts(t *testing.T) {
	rt := &watchtest.Runtime{}
	rt.Start("other") // runs, but started before the arm: not the armed container
	rt.SetMounts("other", watch.Mount{Source: "/home/u/other", Destination: "/other"})
	rec := &attachRecorder{}
	cfg := testConfig
	cfg.Attached = rec.attached
	_, done := startWatcherWith(context.Background(), cfg, nil, rt)
	time.Sleep(testConfig.PollInterval)
	want := []watch.Mount{{Source: "/home/u/app", Destination: "/workspace"}, {Destination: "/tmp", ReadOnly: true}}
	rt.SetMounts("c", want...)
	rt.Start("c")
	eventually(t, func() bool { return rec.count() > 0 }, "Attached was not called")
	got := rec.snapshot()[0]
	if len(got) != 1 || got[0].ID != "c" || !got[0].Running || !slices.Equal(got[0].Mounts, want) {
		t.Fatalf("got %+v; want only the armed container c with mounts %v", got, want)
	}
	rt.Stop("c")
	expectResult(t, done, nil)
}

func TestAttachedGetsEveryArmedContainerInPollOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &watchtest.Runtime{}
		rec := &attachRecorder{}
		cfg := testConfig
		cfg.Attached = rec.attached
		_, done := startWatcherWith(context.Background(), cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 2)
		rt.Start("b")
		rt.Start("a") // both start between two polls
		time.Sleep(cfg.PollInterval)
		calls := rec.snapshot()
		if len(calls) != 1 || !slices.Equal(ids(calls[0]), []string{"b", "a"}) {
			t.Fatalf("got %+v; want one call with b and a in the poll's order", calls)
		}
		rt.Stop("a")
		rt.Stop("b")
		expectResult(t, done, nil)
	})
}

func TestAttachedIsCalledOncePerAttachNotPerPoll(t *testing.T) {
	rt := &watchtest.Runtime{}
	rec := &attachRecorder{}
	cfg := testConfig
	cfg.Attached = rec.attached
	_, done := startWatcherWith(context.Background(), cfg, nil, rt)
	rt.Start("c")
	eventually(t, func() bool { return rec.count() > 0 }, "Attached was not called")
	time.Sleep(10 * testConfig.PollInterval)
	if n := rec.count(); n != 1 {
		t.Fatalf("Attached called %d times over many polls; want once", n)
	}
	rt.Stop("c")
	expectResult(t, done, nil)
}

func TestAttachedIsCalledAgainAfterRearmAndNewContainer(t *testing.T) {
	rt := &watchtest.Runtime{}
	rec := &attachRecorder{}
	cfg := testConfig
	cfg.Attached = rec.attached
	w, done := startWatcherWith(context.Background(), cfg, nil, rt)
	rt.SetMounts("old", watch.Mount{Destination: "/old"})
	rt.Start("old")
	eventually(t, func() bool { return rec.count() == 1 }, "Attached was not called for the first container")

	// A rebuild: the old container stops, `up` arms, a new one starts.
	rt.Stop("old")
	w.Arm()
	rt.SetMounts("new", watch.Mount{Destination: "/new"})
	rt.Start("new")
	eventually(t, func() bool { return rec.count() == 2 }, "Attached was not called again for the new container")
	got := rec.snapshot()[1]
	if len(got) != 1 || got[0].ID != "new" || !slices.Equal(got[0].Mounts, []watch.Mount{{Destination: "/new"}}) {
		t.Fatalf("got %+v; want only the new container with its mounts", got)
	}
	time.Sleep(5 * testConfig.PollInterval)
	if n := rec.count(); n != 2 {
		t.Fatalf("Attached called %d times; want 2", n)
	}
	rt.Stop("new")
	expectResult(t, done, nil)
}

// Armed reports each arm's time before Arm returns, and Attached carries the
// time of the arm it attached for: New's, then that of each later arm, so a
// callback can tell an attach of an earlier arm from one of the latest.
func TestArmedAndAttachedCarryTheArmTime(t *testing.T) {
	rt := &watchtest.Runtime{}
	rec := &attachRecorder{}
	var (
		mu    sync.Mutex
		armed []time.Time
	)
	cfg := testConfig
	cfg.Attached = rec.attached
	cfg.Armed = func(armedAt time.Time) {
		mu.Lock()
		defer mu.Unlock()
		armed = append(armed, armedAt)
	}
	before := time.Now()
	w, done := startWatcherWith(context.Background(), cfg, nil, rt)
	rt.Start("old")
	eventually(t, func() bool { return rec.count() == 1 }, "Attached was not called for the first container")
	first := rec.arms()[0]
	if first.Before(before) {
		t.Fatalf("the first attach carries %v; want New's time, not before %v", first, before)
	}
	mu.Lock()
	if len(armed) != 0 {
		t.Fatalf("Armed called %d times before any Arm; want none: New does not report its arm", len(armed))
	}
	mu.Unlock()

	rt.Stop("old")
	if err := w.Arm(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	reported := slices.Clone(armed)
	mu.Unlock()
	if len(reported) != 1 || !reported[0].After(first) {
		t.Fatalf("Armed got %v by the time Arm returned; want one time after the first arm %v", reported, first)
	}
	rt.Start("new")
	eventually(t, func() bool { return rec.count() == 2 }, "Attached was not called for the new container")
	if second := rec.arms()[1]; !second.Equal(reported[0]) {
		t.Fatalf("the second attach carries %v; want the time Armed reported, %v", second, reported[0])
	}
	rt.Stop("new")
	expectResult(t, done, nil)
}

// A container that starts after the arm and joins an already attached arm,
// e.g. one a Compose devcontainer recreated, is an armed container whose
// mounts have not been seen: Attached gets every armed container again.
func TestAttachedIsCalledAgainWhenANewContainerJoinsTheArm(t *testing.T) {
	rt := &watchtest.Runtime{}
	rec := &attachRecorder{}
	cfg := testConfig
	cfg.Attached = rec.attached
	_, done := startWatcherWith(context.Background(), cfg, nil, rt)
	rt.SetMounts("first", watch.Mount{Destination: "/first"})
	rt.Start("first")
	eventually(t, func() bool { return rec.count() == 1 }, "Attached was not called for the first container")

	// No arm: the new container joins the one the arm is attached to.
	rt.SetMounts("joined", watch.Mount{Source: "/home/u/app", Destination: "/workspace"})
	rt.Start("joined")
	eventually(t, func() bool { return rec.count() == 2 }, "Attached was not called again for the joined container")
	got := rec.snapshot()[1]
	if !slices.Equal(ids(got), []string{"first", "joined"}) ||
		!slices.Equal(got[0].Mounts, []watch.Mount{{Destination: "/first"}}) ||
		!slices.Equal(got[1].Mounts, []watch.Mount{{Source: "/home/u/app", Destination: "/workspace"}}) {
		t.Fatalf("got %+v; want first and joined in poll order, with their mounts", got)
	}
	time.Sleep(5 * testConfig.PollInterval)
	if n := rec.count(); n != 2 {
		t.Fatalf("Attached called %d times; want 2: polls without a new armed container do not call it", n)
	}
	rt.Stop("first")
	rt.Stop("joined")
	expectResult(t, done, nil)
}

// sharedRuntime reports one container whose Mounts slice is the same on
// every call, as a runtime that does not copy it would.
type sharedRuntime struct {
	mu sync.Mutex
	c  watch.Container
}

func (r *sharedRuntime) Containers(context.Context, string) ([]watch.Container, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []watch.Container{r.c}, nil
}

func (r *sharedRuntime) MountTable(context.Context, int) ([]watch.MountPoint, error) {
	return nil, nil
}

func (r *sharedRuntime) set(running bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.c.Running = running
	if running {
		r.c.StartedAt = time.Now()
	}
}

func TestAttachedContainersAreCopiesTheCallbackMayChange(t *testing.T) {
	want := []watch.Mount{{Source: "/home/u/app", Destination: "/workspace"}, {Destination: "/tmp"},
		{Type: "volume", Name: "v", Destination: "/v", Volume: &watch.Volume{Driver: "local", Options: map[string]string{"device": "/home/u/v"}}}}
	wantLive := []watch.MountPoint{{Path: "/workspace"}, {Path: "/tmp", ReadOnly: true}}
	rt := &sharedRuntime{c: watch.Container{ID: "c", Mounts: slices.Clone(want),
		Live: &watch.LiveMounts{Points: slices.Clone(wantLive)}}}
	var mu sync.Mutex
	var seen [][]watch.Mount
	var seenLive [][]watch.MountPoint
	cfg := testConfig
	cfg.Attached = func(_ time.Time, armed []watch.Container) {
		mu.Lock()
		defer mu.Unlock()
		mounts := slices.Clone(armed[0].Mounts)
		v := *mounts[2].Volume
		v.Options = maps.Clone(v.Options)
		mounts[2].Volume = &v
		seen = append(seen, mounts)
		seenLive = append(seenLive, slices.Clone(armed[0].Live.Points))
		armed[0].Live.Points[0].Path = "/changed by the callback"
		armed[0].Live.Points = append(armed[0].Live.Points, watch.MountPoint{Path: "/added by the callback"})
		armed[0].Mounts[0].Destination = "/changed by the callback"
		armed[0].Mounts[2].Volume.Options["device"] = "/changed by the callback"
		armed[0].Mounts[2].Volume.Driver = "changed by the callback"
		armed[0].Mounts = append(armed[0].Mounts, watch.Mount{Destination: "/added by the callback"})
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }
	w, done := startWatcherWith(context.Background(), cfg, nil, rt)
	rt.set(true)
	eventually(t, func() bool { return count() == 1 }, "Attached was not called")

	// The same container starts again after a re-arm: a second attach.
	rt.set(false)
	w.Arm()
	rt.set(true)
	eventually(t, func() bool { return count() == 2 }, "Attached was not called again")
	mu.Lock()
	got := seen[1]
	mu.Unlock()
	if !slices.EqualFunc(got, want, sameMount) {
		t.Fatalf("second attach got mounts %v; want %v unchanged by the first callback", got, want)
	}
	mu.Lock()
	gotLive := seenLive[1]
	mu.Unlock()
	if !slices.Equal(gotLive, wantLive) {
		t.Fatalf("second attach got live mount points %v; want %v unchanged by the first callback", gotLive, wantLive)
	}
	rt.set(false)
	expectResult(t, done, nil)
}

// Attached gets the live mount table of each armed container that runs,
// read through the runtime that reported it; one that does not run gets
// none, and is checked from its inspection alone.
func TestAttachedGetsTheLiveTablesOfRunningArmedContainers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &watchtest.Runtime{}
		rec := &attachRecorder{}
		cfg := testConfig
		cfg.Attached = rec.attached
		_, done := startWatcherWith(context.Background(), cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 2)
		table := []watch.MountPoint{{Path: "/workspace"}, {Path: "/workspace/.devcontainer", ReadOnly: true}}
		rt.SetMountTable("running", table...)
		rt.Start("stopped")
		rt.Stop("stopped")
		rt.Start("running") // both start between two polls
		time.Sleep(cfg.PollInterval)
		calls := rec.snapshot()
		if len(calls) != 1 || !slices.Equal(ids(calls[0]), []string{"stopped", "running"}) {
			t.Fatalf("got %+v; want one call with stopped and running", calls)
		}
		if l := calls[0][0].Live; l != nil {
			t.Fatalf("the stopped container got live mounts %+v; want none", l)
		}
		if l := calls[0][1].Live; l == nil || l.Err != nil || !slices.Equal(l.Points, table) {
			t.Fatalf("the running container got live mounts %+v; want %v", l, table)
		}
		rt.Stop("running")
		expectResult(t, done, nil)
	})
}

func TestLiveBeforeTheAttachIsNotAttached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &watchtest.Runtime{}
	w, _ := startWatcher(ctx, nil, rt)
	time.Sleep(3 * testConfig.PollInterval) // polls find no armed container
	if got, err := w.Live(ctx); !errors.Is(err, watch.ErrNotAttached) {
		t.Fatalf("Live before the attach got %+v, %v; want %v", got, err, watch.ErrNotAttached)
	}
}

// expectLive waits for Live to answer for an attach, and returns its
// containers.
func expectLive(t *testing.T, w *watch.Watcher) []watch.Container {
	t.Helper()
	var (
		got []watch.Container
		err error
	)
	eventually(t, func() bool {
		got, err = w.Live(context.Background())
		return !errors.Is(err, watch.ErrNotAttached)
	}, "Live did not attach")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	return got
}

// livePoints returns the live mount points of the container id among
// containers, failing if it is not there or its table was not read.
func livePoints(t *testing.T, containers []watch.Container, id string) []watch.MountPoint {
	t.Helper()
	i := slices.IndexFunc(containers, func(c watch.Container) bool { return c.ID == id })
	if i < 0 {
		t.Fatalf("got %+v; want container %s", containers, id)
	}
	l := containers[i].Live
	if l == nil || l.Err != nil {
		t.Fatalf("container %s got live mounts %+v; want a table", id, l)
	}
	return l.Points
}

func TestLiveReadsTheCurrentMountTable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &watchtest.Runtime{}
	before := []watch.MountPoint{{Path: "/workspace"}, {Path: "/workspace/.devcontainer", ReadOnly: true}}
	rt.SetMountTable("c", before...)
	w, _ := startWatcher(ctx, nil, rt)
	rt.Start("c")
	if got := livePoints(t, expectLive(t, w), "c"); !slices.Equal(got, before) {
		t.Fatalf("first Live got %v; want %v", got, before)
	}

	after := []watch.MountPoint{{Path: "/workspace"}, {Path: "/workspace/.devcontainer"}} // remounted writable
	rt.SetMountTable("c", after...)
	got, err := w.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if points := livePoints(t, got, "c"); !slices.Equal(points, after) {
		t.Fatalf("second Live got %v; want the changed table %v", points, after)
	}
}

func TestLiveAfterARearmIsNotAttachedUntilTheNextAttach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &watchtest.Runtime{}
	w, _ := startWatcher(ctx, nil, rt)
	rt.Start("old")
	expectLive(t, w)

	rt.Stop("old")
	expectArmAccepted(t, w)
	if got, err := w.Live(ctx); !errors.Is(err, watch.ErrNotAttached) {
		t.Fatalf("Live right after a re-arm got %+v, %v; want %v", got, err, watch.ErrNotAttached)
	}
	time.Sleep(3 * testConfig.PollInterval) // polls find no container started since the arm
	if got, err := w.Live(ctx); !errors.Is(err, watch.ErrNotAttached) {
		t.Fatalf("Live before the re-arm attached got %+v, %v; want %v", got, err, watch.ErrNotAttached)
	}
	rt.Start("new")
	if got := expectLive(t, w); !slices.Equal(ids(got), []string{"new"}) {
		t.Fatalf("Live after the re-arm attached got %v; want only the new container", ids(got))
	}
}

// countingRuntime is a watchtest.Runtime that counts its Containers calls.
type countingRuntime struct {
	*watchtest.Runtime
	calls atomic.Int32
}

func (r *countingRuntime) Containers(ctx context.Context, workspace string) ([]watch.Container, error) {
	r.calls.Add(1)
	return r.Runtime.Containers(ctx, workspace)
}

// A restart gives the armed container a new process: Live finds the old
// one gone, inspects again and reads the table of the new one, whose pid
// the next call reads directly.
func TestLiveAfterARestartReadsTheNewProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := raceConfig() // the test acts between two polls
		rt := &countingRuntime{Runtime: &watchtest.Runtime{}}
		w, _ := startWatcherWith(ctx, cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 10)
		rt.Start("c")
		time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches
		attached, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}

		rt.Stop("c") // docker restart
		rt.Start("c")
		table := []watch.MountPoint{{Path: "/workspace"}, {Path: "/after-the-restart", ReadOnly: true}}
		rt.SetMountTable("c", table...)
		before := rt.calls.Load()
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if points := livePoints(t, got, "c"); !slices.Equal(points, table) {
			t.Fatalf("Live after the restart got %v; want the new process's table %v", points, table)
		}
		if got[0].Pid == attached[0].Pid {
			t.Fatalf("Live after the restart reports pid %d; want the new process's, not the attach's", got[0].Pid)
		}
		if n := rt.calls.Load() - before; n != 1 {
			t.Fatalf("Live after the restart inspected %d times; want once", n)
		}

		before = rt.calls.Load()
		again, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if points := livePoints(t, again, "c"); !slices.Equal(points, table) || again[0].Pid != got[0].Pid {
			t.Fatalf("the next Live got pid %d, %v; want pid %d, %v", again[0].Pid, points, got[0].Pid, table)
		}
		if n := rt.calls.Load() - before; n != 0 {
			t.Fatalf("the next Live inspected %d times; want none: it reads the new pid", n)
		}
	})
}

// startWithStoppedB runs a watcher whose polls are too far apart to refresh
// anything while the test runs and whose StoppedInspectionMaxAge is 100 ms,
// attaches it to the running a and b, stops b and calls Live: b's process is
// gone, so b is inspected again, found stopped and left out. It then starts b
// again, which only a later inspection can see.
func startWithStoppedB(t *testing.T) (context.Context, *watch.Watcher, *countingRuntime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := raceConfig()
	cfg.PollInterval = 10 * time.Second
	cfg.StoppedInspectionMaxAge = 100 * time.Millisecond
	rt := &countingRuntime{Runtime: &watchtest.Runtime{}}
	w, _ := startWatcherWith(ctx, cfg, nil, rt)
	time.Sleep(cfg.PollInterval / 10)
	rt.Start("a")
	rt.Start("b")
	time.Sleep(cfg.PollInterval) // the poll at 10 s attaches to both
	rt.Stop("b")
	got, err := w.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []string{"a"}) {
		t.Fatalf("Live with b stopped got %v; want only a", ids(got))
	}
	rt.Start("b")
	return ctx, w, rt
}

// StoppedInspectionMaxAge, not PollInterval, says how old the inspection of
// a stopped armed container must be for Live to inspect it again: once it
// has passed, a container that has started since is seen running.
func TestLiveInspectsAStoppedArmedContainerAgainAfterStoppedInspectionMaxAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, w, _ := startWithStoppedB(t)

		time.Sleep(100 * time.Millisecond)
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), []string{"a", "b"}) {
			t.Fatalf("Live a StoppedInspectionMaxAge after b's inspection got %v; want a and b", ids(got))
		}
		livePoints(t, got, "b")
	})
}

// Within StoppedInspectionMaxAge of that inspection, Live leaves the
// stopped container out without a query.
func TestLiveLeavesOutAStoppedArmedContainerWithinStoppedInspectionMaxAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, w, rt := startWithStoppedB(t)

		time.Sleep(50 * time.Millisecond)
		before := rt.calls.Load()
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), []string{"a"}) {
			t.Fatalf("Live within StoppedInspectionMaxAge of b's inspection got %v; want only a", ids(got))
		}
		if n := rt.calls.Load() - before; n != 0 {
			t.Fatalf("Live within StoppedInspectionMaxAge of b's inspection inspected %d times; want none", n)
		}
	})
}

// A container that does not run cannot write: Live leaves out an armed
// container that stopped. Live inspects it again only once its last
// inspection is a poll interval old, and then takes it in again if it runs:
// until then, it is left out without a query.
func TestLiveInspectsAStoppedArmedContainerAgainAfterAPollInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := raceConfig()
		rt := &countingRuntime{Runtime: &watchtest.Runtime{}}
		w, _ := startWatcherWith(ctx, cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 10)
		rt.Start("a")
		rt.Start("b")
		time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches to both

		rt.Stop("b")
		got, err := w.Live(ctx) // at 165 ms: b's process is gone, b is inspected again
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), []string{"a"}) {
			t.Fatalf("Live with b stopped got %v; want only a", ids(got))
		}
		livePoints(t, got, "a")

		rt.Start("b")
		release := holdNextQuery(t, rt.Runtime) // the poll at 300 ms waits: only Live inspects
		defer release()
		before := rt.calls.Load()
		got, err = w.Live(ctx) // b's inspection is 135 ms old
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), []string{"a"}) {
			t.Fatalf("Live within a poll interval of b's inspection got %v; want only a", ids(got))
		}
		if n := rt.calls.Load() - before; n != 0 {
			t.Fatalf("Live within a poll interval of b's inspection inspected %d times; want none", n)
		}

		time.Sleep(cfg.PollInterval / 10) // b's inspection is 150 ms old
		before = rt.calls.Load()
		got, err = w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), []string{"a", "b"}) {
			t.Fatalf("Live a poll interval after b's inspection got %v; want a and b", ids(got))
		}
		livePoints(t, got, "b")
		if n := rt.calls.Load() - before; n != 1 {
			t.Fatalf("Live a poll interval after b's inspection inspected %d times; want once", n)
		}
	})
}

// Run's polls keep what Live reads current: an armed container that
// started again is seen through the poll's inspection, its new process's
// table read, without Live inspecting it.
func TestLiveReadsAContainerStartedAgainThroughThePollsInspection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := raceConfig()
		rt := &countingRuntime{Runtime: &watchtest.Runtime{}}
		w, _ := startWatcherWith(ctx, cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 10)
		rt.Start("a")
		rt.Start("b")
		time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches to both

		rt.Stop("b")
		if got, err := w.Live(ctx); err != nil || !slices.Equal(ids(got), []string{"a"}) {
			t.Fatalf("Live with b stopped got %v, %v; want only a", ids(got), err)
		}
		rt.Start("b")
		table := []watch.MountPoint{{Path: "/workspace"}, {Path: "/started-again", ReadOnly: true}}
		rt.SetMountTable("b", table...)
		time.Sleep(cfg.PollInterval) // the poll at 300 ms sees b running

		before := rt.calls.Load()
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if points := livePoints(t, got, "b"); !slices.Equal(points, table) {
			t.Fatalf("Live after the poll got %v for b; want its new process's table %v", points, table)
		}
		if n := rt.calls.Load() - before; n != 0 {
			t.Fatalf("Live after the poll inspected %d times; want none: the poll's inspection names b's new process", n)
		}
	})
}

func TestLiveReportsAnUnreadableTableOnItsContainerOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &watchtest.Runtime{}
	w, _ := startWatcher(ctx, nil, rt)
	rt.Start("readable")
	rt.Start("unreadable")
	// The two may attach in two polls.
	eventually(t, func() bool { return len(expectLive(t, w)) == 2 }, "Live did not get both containers")

	rt.FailMountTable("unreadable", true)
	got, err := w.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	livePoints(t, got, "readable")
	i := slices.IndexFunc(got, func(c watch.Container) bool { return c.ID == "unreadable" })
	if i < 0 || got[i].Live == nil || got[i].Live.Err == nil || errors.Is(got[i].Live.Err, watch.ErrProcessGone) {
		t.Fatalf("got %+v; want unreadable with a Live.Err that is not ErrProcessGone", got)
	}
}

// A container whose process is gone cannot be inspected again while no
// runtime answers: it carries why, for the caller to refuse.
func TestLiveReportsAGoneProcessWhileNoRuntimeAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := raceConfig()
		rt := &watchtest.Runtime{}
		w, _ := startWatcherWith(ctx, cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 10)
		rt.Start("c")
		time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches

		rt.Stop("c") // docker restart
		rt.Start("c")
		rt.Fail(true)
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "c" || got[0].Live == nil || got[0].Live.Err == nil ||
			errors.Is(got[0].Live.Err, watch.ErrProcessGone) {
			t.Fatalf("got %+v; want c with why it could not be inspected again as its Live.Err", got)
		}
	})
}

// startRestartingC runs a watcher with a runtime counting its inspections,
// attaches it to the running c, makes c restart (running, without a process)
// and lets a poll inspect it, so that Live starts from a cached container
// that runs with pid 0 and whose inspection is younger than
// StoppedInspectionMaxAge.
func startRestartingC(t *testing.T) (context.Context, *watch.Watcher, *countingRuntime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := raceConfig()
	rt := &countingRuntime{Runtime: &watchtest.Runtime{}}
	w, _ := startWatcherWith(ctx, cfg, nil, rt)
	time.Sleep(cfg.PollInterval / 10)
	rt.Start("c")
	time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches
	rt.Restarting("c")
	time.Sleep(cfg.PollInterval) // the poll at 300 ms sees c without a process
	return ctx, w, rt
}

// A container the runtime reports running without a process (restarting) is
// not taken as stopped: Live inspects it again once, even though its last
// inspection is young, and reads the table of the process it has then.
func TestLiveInspectsAgainAContainerThatRunsWithoutAProcessID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, w, rt := startRestartingC(t)
		rt.Start("c")
		table := []watch.MountPoint{{Path: "/workspace"}, {Path: "/after-the-restart", ReadOnly: true}}
		rt.SetMountTable("c", table...)
		before := rt.calls.Load()
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if points := livePoints(t, got, "c"); !slices.Equal(points, table) {
			t.Fatalf("Live got %v; want the new process's table %v", points, table)
		}
		if n := rt.calls.Load() - before; n != 1 {
			t.Fatalf("Live inspected %d times; want once", n)
		}
	})
}

// A container that still has no process at the fresh inspection is not
// readable: Live returns it with a Live.Err saying why, for the caller to
// refuse, and inspects it again once at each call until it has a process.
func TestLiveReportsAContainerThatStillRunsWithoutAProcessID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, w, rt := startRestartingC(t)
		for range 2 {
			before := rt.calls.Load()
			got, err := w.Live(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ID != "c" || got[0].Live == nil || !errors.Is(got[0].Live.Err, watch.ErrNoProcessID) {
				t.Fatalf("Live got %+v; want c with ErrNoProcessID as its Live.Err", got)
			}
			if n := rt.calls.Load() - before; n != 1 {
				t.Fatalf("Live inspected %d times; want once", n)
			}
		}
		if msg := watch.ErrNoProcessID.Error(); msg != "container runs but reports no process id" {
			t.Fatalf("ErrNoProcessID says %q", msg)
		}

		rt.Start("c")
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		livePoints(t, got, "c") // a later inspection finds its process
	})
}

// A process that is gone and has not come back by the fresh inspection, the
// container running without a process ID, is reported the same way.
func TestLiveReportsAGoneProcessOfAContainerThatRunsWithoutAProcessID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := raceConfig()
		rt := &watchtest.Runtime{}
		w, _ := startWatcherWith(ctx, cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 10)
		rt.Start("c")
		time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches
		rt.Restarting("c")           // docker restart: the attach's pid is gone
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Live == nil || !errors.Is(got[0].Live.Err, watch.ErrNoProcessID) {
			t.Fatalf("Live got %+v; want c with ErrNoProcessID as its Live.Err", got)
		}
	})
}

// racingRuntime is a watchtest.Runtime whose container id restarts, or
// stops, when its first mount table is asked for: between the inspection
// that reported it and the read of its table.
type racingRuntime struct {
	*watchtest.Runtime
	id      string
	restart bool // start it again after stopping it
	// restarting leaves it running without a process after the stop, as a
	// container that is restarting; restart is then ignored.
	restarting bool
	once       sync.Once
}

func (r *racingRuntime) MountTable(ctx context.Context, pid int) ([]watch.MountPoint, error) {
	r.once.Do(func() {
		r.Stop(r.id)
		switch {
		case r.restarting:
			r.Restarting(r.id)
		case r.restart:
			r.Start(r.id)
		}
	})
	return r.Runtime.MountTable(ctx, pid)
}

func TestAttachedInspectsAgainAContainerWhoseProcessIsGone(t *testing.T) {
	table := []watch.MountPoint{{Path: "/workspace"}}
	for _, tt := range []struct {
		name    string
		restart bool
		check   func(c watch.Container) bool
	}{
		{"restarted: the new process's table", true, func(c watch.Container) bool {
			return c.Running && c.Live != nil && c.Live.Err == nil && slices.Equal(c.Live.Points, table)
		}},
		{"stopped: no table", false, func(c watch.Container) bool { return !c.Running && c.Live == nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := &racingRuntime{Runtime: &watchtest.Runtime{}, id: "c", restart: tt.restart}
				rt.SetMountTable("c", table...)
				rec := &attachRecorder{}
				cfg := raceConfig()
				cfg.Attached = rec.attached
				_, done := startWatcherWith(context.Background(), cfg, nil, rt)
				time.Sleep(cfg.PollInterval / 10)
				rt.Start("c")
				time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches
				calls := rec.snapshot()
				if len(calls) != 1 || len(calls[0]) != 1 || !tt.check(calls[0][0]) {
					t.Fatalf("got %+v", calls)
				}
				rt.Stop("c")
				expectResult(t, done, nil)
			})
		})
	}
}

// A container that runs without a process ID (restarting) at the attach, or
// still at the fresh inspection of the attach, is not an error of the
// attach: it gets no live table, as one that does not run, and the request
// that follows judges it (see Live).
func TestAttachedGetsNoLiveTableForAContainerThatRunsWithoutAProcessID(t *testing.T) {
	for _, tt := range []struct {
		name string
		gone bool // its process is gone when its table is read, and it restarts without one
	}{
		{"restarting at the inspection of the attach", false},
		{"restarting at the fresh inspection after a gone process", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := &watchtest.Runtime{}
				var rt watch.Runtime = fake
				if tt.gone {
					rt = &racingRuntime{Runtime: fake, id: "c", restarting: true}
				}
				rec := &attachRecorder{}
				cfg := raceConfig()
				cfg.Attached = rec.attached
				_, done := startWatcherWith(context.Background(), cfg, nil, rt)
				time.Sleep(cfg.PollInterval / 10)
				fake.Start("c")
				if !tt.gone {
					fake.Restarting("c") // between two polls: the attach finds it so
				}
				time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches
				calls := rec.snapshot()
				if len(calls) != 1 || len(calls[0]) != 1 ||
					!calls[0][0].Running || calls[0][0].Pid != 0 || calls[0][0].Live != nil {
					t.Fatalf("got %+v; want one call with c running without a pid and without live mounts", calls)
				}
				fake.Stop("c")
				expectResult(t, done, nil)
			})
		})
	}
}

// lateRuntime is a watchtest.Runtime that counts its Containers calls and
// can make one answer late: it answers with the state at its call, once
// released, as a slow runtime CLI does after the containers changed.
type lateRuntime struct {
	*watchtest.Runtime
	calls atomic.Int32
	mu    sync.Mutex
	late  *hold // the next Containers call answers late; nil: none
}

type hold struct {
	entered, released chan struct{}
}

// Late makes the next Containers call read the state, close entered, and
// answer with that state once release is called.
func (r *lateRuntime) Late() (entered <-chan struct{}, release func()) {
	h := &hold{entered: make(chan struct{}), released: make(chan struct{})}
	r.mu.Lock()
	r.late = h
	r.mu.Unlock()
	return h.entered, sync.OnceFunc(func() { close(h.released) })
}

func (r *lateRuntime) Containers(ctx context.Context, workspace string) ([]watch.Container, error) {
	r.calls.Add(1)
	r.mu.Lock()
	h := r.late
	r.late = nil
	r.mu.Unlock()
	found, err := r.Runtime.Containers(ctx, workspace)
	if h != nil {
		close(h.entered)
		<-h.released
	}
	return found, err
}

// Two Live calls that both inspect the armed container again may finish
// out of order: the one whose inspection is older must not replace what
// the newer one found, or the next Live would read a process that is gone
// and inspect again.
func TestLiveKeepsTheNewerOfTwoConcurrentInspections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := raceConfig() // the test acts between two polls
		rt := &lateRuntime{Runtime: &watchtest.Runtime{}}
		w, _ := startWatcherWith(ctx, cfg, nil, rt)
		time.Sleep(cfg.PollInterval / 10)
		rt.Start("c")
		time.Sleep(cfg.PollInterval) // the poll at 150 ms attaches

		rt.Stop("c") // docker restart: the attach's process is gone
		rt.Start("c")
		entered, release := rt.Late()
		older := make(chan error, 1)
		go func() {
			_, err := w.Live(ctx)
			older <- err
		}()
		<-entered // the older Live has inspected, and answers late

		time.Sleep(time.Millisecond)
		rt.Stop("c") // restarted again: the older inspection's process is gone too
		rt.Start("c")
		newer, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if err := <-older; err != nil {
			t.Fatal(err)
		}

		before := rt.calls.Load()
		got, err := w.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Pid != newer[0].Pid {
			t.Fatalf("the next Live got %+v; want pid %d, the newer inspection's", got, newer[0].Pid)
		}
		if n := rt.calls.Load() - before; n != 0 {
			t.Fatalf("the next Live inspected %d times; want none: it reads the newer pid", n)
		}
	})
}

// Live calls run alongside Run's polls, arms, restarts and each other; run
// under -race, the test checks they share no state unguarded.
func TestLiveIsSafeForConcurrentUseWithRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig
	cfg.Attached = func(time.Time, []watch.Container) {}
	rt := &watchtest.Runtime{}
	w, done := startWatcherWith(ctx, cfg, nil, rt)
	rt.Start("c")
	expectLive(t, w)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := w.Live(ctx)
				if err != nil && !errors.Is(err, watch.ErrNotAttached) {
					t.Errorf("Live: %v", err)
					return
				}
				for _, c := range got {
					if c.Live != nil && c.Live.Err == nil {
						c.Live.Points = append(c.Live.Points, watch.MountPoint{Path: "/added by the caller"})
					}
				}
			}
		})
	}
	for i := range 20 {
		rt.Stop("c") // docker restart
		rt.Start("c")
		if i%5 == 0 {
			expectArmAccepted(t, w)
			rt.Stop("c")
			rt.Start("c")
		}
		time.Sleep(testConfig.PollInterval)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-done:
		t.Fatalf("the watcher ended (%v) while its armed container ran", err)
	default:
	}
}
