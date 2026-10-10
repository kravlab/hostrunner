// Package watch follows a devcontainer's lifecycle through the container
// runtime, so the daemon can exit once its container is gone.
//
// Containers are identified by the devcontainer.local_folder label, which
// the devcontainer tooling puts on every container of a workspace. Keying
// on the label rather than a container ID lets a rebuild (old container
// removed, new one started) keep the same daemon.
package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Errors Run returns when it gives up.
var (
	ErrNeverStarted       = errors.New("devcontainer did not start within the startup timeout")
	ErrRuntimeUnavailable = errors.New("no container runtime answered within the startup timeout")
)

// Container is what a runtime reports about one container of the
// workspace.
type Container struct {
	ID        string
	Running   bool
	StartedAt time.Time // last start; zero if never started
}

// Runtime lists a workspace's containers, stopped ones included: a stopped
// container keeps its start time, so one that ran between two polls is
// still seen. An error means the runtime did not answer.
type Runtime interface {
	Containers(ctx context.Context, workspace string) ([]Container, error)
}

// Config tunes a Watcher.
type Config struct {
	Workspace      string        // host workspace folder (the label value)
	PollInterval   time.Duration // how often runtimes are asked
	QueryTimeout   time.Duration // how long one runtime query may take
	StartupTimeout time.Duration // how long to wait for the armed container (covers image builds)
	Grace          time.Duration // how long the armed container must be seen absent
}

// Watcher follows one workspace's armed container: the container the last
// arm was for.
//
// It is waiting after an arm, attached once it knows the armed container,
// and ends once that container has been observed absent for Grace, counted
// from when it is attached at the earliest. `hostrunner up` arms it on every
// devcontainer start; being created counts as an arm.
//
// devcontainer arms the daemon before it starts a container, and on a
// rebuild before it removes the old one. So a container that started after
// the arm is the armed container, running or already stopped. If none
// starts, `up` found the container running: a container started before the
// arm that still runs Grace after it is the armed container.
type Watcher struct {
	runtimes  []Runtime
	cfg       Config
	log       *slog.Logger
	armSignal chan struct{}    // tells Run about an arm
	failing   map[Runtime]bool // runtimes whose last query failed; Run's goroutine only

	// mu orders arms against Run's decision to end: an arm either comes
	// before it and is followed, or after it and is refused.
	mu      sync.Mutex
	armedAt time.Time // time of the last arm
	ended   error     // why the watch ended; nil while it runs
}

// New returns a Watcher over runtimes, armed now.
func New(runtimes []Runtime, cfg Config, log *slog.Logger) *Watcher {
	return &Watcher{
		runtimes:  runtimes,
		cfg:       cfg,
		log:       log,
		armSignal: make(chan struct{}, 1),
		failing:   make(map[Runtime]bool),
		armedAt:   time.Now(),
	}
}

// Arm makes a running Watcher wait for a container started from now on,
// with a fresh startup timeout. Once the watch has ended, or its context is
// cancelled, Arm refuses and returns why it ended: the watcher will not
// follow the arm, and the daemon must step aside. Arm never waits for
// runtime queries; the arm's time is taken here, before the caller lets the
// container start.
func (w *Watcher) Arm() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended != nil {
		return fmt.Errorf("the watch has ended: %w", w.ended)
	}
	w.armedAt = time.Now()
	select {
	case w.armSignal <- struct{}{}:
	default: // an arm is already pending; it reads the latest time
	}
	return nil
}

// lastArm returns the time of the last arm.
func (w *Watcher) lastArm() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.armedAt
}

// takeArmOrEnd takes an arm that came while Run was deciding to end, and
// returns its time; without one, or once the watch has ended, it ends the
// watch for reason.
func (w *Watcher) takeArmOrEnd(reason error) (armedAt time.Time, ended bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended == nil {
		select {
		case <-w.armSignal:
			return w.armedAt, false
		default:
		}
	}
	w.endLocked(reason)
	return time.Time{}, true
}

// markEnded ends the watch for reason, refusing every later arm.
func (w *Watcher) markEnded(reason error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.endLocked(reason)
}

func (w *Watcher) endLocked(reason error) {
	if w.ended == nil {
		w.ended = reason
	}
}

// errArmedContainerStopped is why a watch ends normally.
var errArmedContainerStopped = errors.New("the armed container stopped")

// Run blocks until the armed container is gone and returns nil, or gives
// up: ErrNeverStarted if no armed container was found within StartupTimeout
// of the last arm, ErrRuntimeUnavailable if no runtime answered for
// StartupTimeout while attached, or ctx's error if cancelled. An arm that
// comes while Run decides to give up wins: Run waits for its container
// instead.
//
// Only observed absence counts toward Grace: while no runtime can answer,
// the container's state is unknown and the daemon stays.
func (w *Watcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	// Cancellation ends the watch at once, not when the loop notices it,
	// so no arm is accepted while the daemon stops.
	stop := context.AfterFunc(ctx, func() { w.markEnded(context.Cause(ctx)) })
	defer stop()

	var (
		arm        = w.waitFor(w.lastArm())
		lastAnswer time.Time // last time any runtime answered
	)
	for {
		now := time.Now()
		containers, answered := w.query(ctx)
		if answered {
			lastAnswer = now
			if arm.observe(containers, now, w.cfg.Grace) {
				w.log.Info("attached to the armed container", "workspace", w.cfg.Workspace)
			}
		} else {
			arm.absentSince = time.Time{} // unknown does not count
		}
		var giveUp error
		switch {
		case !arm.attached() && now.Sub(arm.at) >= w.cfg.StartupTimeout:
			giveUp = ErrNeverStarted
		case arm.attached() && !arm.absentSince.IsZero() && now.Sub(arm.absentSince) >= w.cfg.Grace:
			giveUp = errArmedContainerStopped
		case arm.attached() && now.Sub(lastAnswer) >= w.cfg.StartupTimeout:
			giveUp = ErrRuntimeUnavailable
		}
		if giveUp != nil {
			armedAt, ended := w.takeArmOrEnd(giveUp)
			switch {
			case !ended:
				arm = w.waitFor(armedAt)
				continue
			case errors.Is(giveUp, errArmedContainerStopped):
				w.log.Info("armed container stopped", "workspace", w.cfg.Workspace)
				return nil
			default:
				return giveUp
			}
		}
		select {
		case <-ctx.Done():
			w.markEnded(context.Cause(ctx))
			return ctx.Err()
		case <-w.armSignal:
			arm = w.waitFor(w.lastArm())
		case <-ticker.C:
		}
	}
}

// waitFor starts waiting for the container of the arm at armedAt.
func (w *Watcher) waitFor(armedAt time.Time) arming {
	w.log.Info("armed: waiting for the armed container", "workspace", w.cfg.Workspace)
	return arming{at: armedAt}
}

// arming is what the watcher knows since one arm: waiting until it finds
// the armed container, attached from then on.
type arming struct {
	at          time.Time       // when the arm happened
	ids         map[string]bool // the armed container's IDs; nil while waiting
	absentSince time.Time       // zero unless the armed container is seen absent
}

func (a *arming) attached() bool { return a.ids != nil }

// observe takes the containers the runtimes answered with and reports
// whether the watcher has just attached. Absence counts from the attach at
// the earliest.
func (a *arming) observe(containers []Container, now time.Time, grace time.Duration) (justAttached bool) {
	if !a.attached() {
		a.ids = findArmed(containers, a.at, now, grace)
		justAttached = a.attached()
	}
	if !a.attached() {
		return false
	}
	// A container started after the arm is the armed container too, e.g.
	// one a Compose devcontainer recreated.
	for _, c := range containers {
		if c.StartedAt.After(a.at) {
			a.ids[c.ID] = true
		}
	}
	running := slices.ContainsFunc(containers, func(c Container) bool { return c.Running && a.ids[c.ID] })
	switch {
	case running:
		a.absentSince = time.Time{}
	case a.absentSince.IsZero():
		a.absentSince = now
	}
	return justAttached
}

// findArmed returns the IDs of the armed container among containers, or
// nil if it is not there yet: containers started after the arm, running or
// not, or, once grace has passed since the arm without one, the running
// containers started before it.
func findArmed(containers []Container, armedAt, now time.Time, grace time.Duration) map[string]bool {
	var startedAfterArm, runningSinceBefore []string
	for _, c := range containers {
		switch {
		case c.StartedAt.After(armedAt):
			startedAfterArm = append(startedAfterArm, c.ID)
		case c.Running:
			runningSinceBefore = append(runningSinceBefore, c.ID)
		}
	}
	ids := startedAfterArm
	if len(ids) == 0 && now.Sub(armedAt) >= grace {
		ids = runningSinceBefore
	}
	if len(ids) == 0 {
		return nil
	}
	armed := make(map[string]bool, len(ids))
	for _, id := range ids {
		armed[id] = true
	}
	return armed
}

// query asks every runtime, each within QueryTimeout, and merges the
// containers of those that answered. A failing runtime is logged when it
// starts and stops failing, not on every poll.
func (w *Watcher) query(ctx context.Context) (containers []Container, answered bool) {
	for _, rt := range w.runtimes {
		qctx, cancel := context.WithTimeout(ctx, w.cfg.QueryTimeout)
		found, err := rt.Containers(qctx, w.cfg.Workspace)
		cancel()
		if err != nil {
			if !w.failing[rt] && ctx.Err() == nil {
				w.log.Warn("container runtime query failed", "runtime", rt, "error", err)
				w.failing[rt] = true
			}
			continue
		}
		if w.failing[rt] {
			w.log.Info("container runtime answers again", "runtime", rt)
			w.failing[rt] = false
		}
		containers, answered = append(containers, found...), true
	}
	return containers, answered
}

// CLI is a Runtime backed by a docker-compatible command line (docker or
// podman), which both accept the same `ps` filters.
type CLI struct {
	Binary string
}

// Containers lists the containers carrying the workspace's
// devcontainer.local_folder label, in every state, then inspects them.
func (c CLI) Containers(ctx context.Context, workspace string) ([]Container, error) {
	out, _, err := c.run(ctx, "ps", "-aq", "--filter", "label=devcontainer.local_folder="+workspace)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	// json renders docker's string and podman's time.Time alike, as RFC 3339.
	args := append([]string{"inspect", "--type", "container", "--format",
		"{{.Id}} {{.State.Running}} {{json .State.StartedAt}}"}, ids...)
	out, stderr, err := c.run(ctx, args...)
	// A container removed since the listing fails the inspection but not
	// the others: it is absent. Any other failure is the runtime's.
	if err != nil && !strings.Contains(strings.ToLower(stderr), "no such container") {
		return nil, err
	}
	var found []Container
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s inspect: unexpected line %q", c.Binary, line)
		}
		started, perr := parseStartedAt(fields[2])
		if perr != nil {
			return nil, fmt.Errorf("%s inspect: %w", c.Binary, perr)
		}
		found = append(found, Container{ID: fields[0], Running: fields[1] == "true", StartedAt: started})
	}
	return found, nil
}

// run runs the runtime's CLI and returns its stdout and stderr.
func (c CLI) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	stderr = strings.TrimSpace(errBuf.String())
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", c.Binary, args[0], err, stderr)
	}
	return string(out), stderr, err
}

// parseStartedAt reads a start time as {{json}} renders it.
func parseStartedAt(raw string) (time.Time, error) {
	var t time.Time
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return time.Time{}, fmt.Errorf("start time %s: %w", raw, err)
	}
	return t, nil
}

// String names the runtime in logs.
func (c CLI) String() string { return c.Binary }

// Detect returns a CLI runtime for each of docker and podman found in PATH.
func Detect() []Runtime {
	var found []Runtime
	for _, name := range []string{"docker", "podman"} {
		if path, err := exec.LookPath(name); err == nil {
			found = append(found, CLI{Binary: path})
		}
	}
	return found
}
