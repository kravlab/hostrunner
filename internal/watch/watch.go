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
	"maps"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/contain"
	"github.com/kravlab/hostrunner/internal/procgroup"
)

// Errors Run returns when it gives up.
var (
	ErrNeverStarted       = errors.New("devcontainer did not start within the startup timeout")
	ErrRuntimeUnavailable = errors.New("no container runtime answered within the startup timeout")
)

// ErrNotAttached is why Live has no armed containers to read: the watcher
// has not attached since the last arm.
var ErrNotAttached = errors.New("not attached to the armed container since the last arm")

// Container is what a runtime reports about one container of the
// workspace.
type Container struct {
	ID        string
	Running   bool
	Pid       int       // host process ID of the container's init process as the runtime reports it; 0 when it does not run
	StartedAt time.Time // last start; zero if never started
	Mounts    []Mount   // meaningful only if MountsErr is nil
	MountsErr error     // why the mounts could not be read; nil when they were
	// Live is the live mount table of the container's init process, read
	// when the container runs (Runtime.MountTable); nil when it was not
	// read, as for a container that does not run.
	Live *LiveMounts
}

// Runs reports whether c runs, as the inspection that reported it found it,
// with an init process the daemon can read: Running, and Pid > 0.
func (c Container) Runs() bool { return c.Running && c.Pid > 0 }

// LiveMounts is the live mount table of a container's init process, as the
// host kernel holds it, which the runtime's inspection of the container may
// no longer match: a mount can be unmounted, mounted over or remounted
// after the container started.
type LiveMounts struct {
	// Points is the table in mountinfo order: a later point at the same
	// Path is mounted over an earlier one. Meaningful only if Err is nil.
	Points []MountPoint
	// Err is why the table could not be read; nil when it was.
	Err error
}

// The Mount.Type of a volume mount, the Volume.Driver of the runtime's
// own volumes, and the Volume.Options key that names the host path a
// local volume binds.
const (
	MountTypeVolume    = "volume"
	DriverLocal        = "local"
	VolumeOptionDevice = "device"
)

// Mount is one of a container's mounts as its runtime reports it. A
// volume's Source lies in the runtime's storage; where its content comes
// from, a host directory a local volume binds, say, only the volume's
// driver and options tell (Volume).
type Mount struct {
	Type        string // the runtime's mount type: "bind", "volume" (MountTypeVolume) or "tmpfs"; empty if it names none
	Name        string // the volume's name, for a "volume" mount; empty otherwise or if the runtime names none
	Source      string // on the host; empty for a mount without one, as tmpfs
	Destination string // in the container
	ReadOnly    bool
	// Volume is, for a "volume" mount, the volume as the runtime's
	// inspection of it reports it; nil for other mounts and when VolumeErr
	// says why it could not be inspected. Copies of a Mount share it.
	Volume *Volume
	// VolumeErr is, for a "volume" mount, why the volume could not be
	// inspected; nil when it was, and for other mounts.
	VolumeErr error
}

// Point returns the mount point in the container a live mount table lists
// the mount at: its cleaned Destination.
func (m Mount) Point() string { return path.Clean(m.Destination) }

// Volume is a volume as its runtime's inspection reports it: its driver
// (DriverLocal for the runtime's own) and the options it was created with,
// such as a local volume's type, o and device (VolumeOptionDevice); nil
// Options for none.
type Volume struct {
	Driver  string
	Options map[string]string
}

// Runtime lists a workspace's containers, stopped ones included: a stopped
// container keeps its start time, so one that ran between two polls is
// still seen. An error means the runtime did not answer.
type Runtime interface {
	Containers(ctx context.Context, workspace string) ([]Container, error)
	// MountTable returns the live mount table of the process pid, in the
	// order of mountinfo: a later entry is mounted over an earlier one at
	// the same point. The error wraps ErrProcessGone when the process does
	// not exist (any more).
	MountTable(ctx context.Context, pid int) ([]MountPoint, error)
}

// Config tunes a Watcher.
type Config struct {
	Workspace      string        // host workspace folder (the label value)
	PollInterval   time.Duration // how often runtimes are queried; also how old an inspection that found an armed container stopped must be for Live to inspect it again
	QueryTimeout   time.Duration // how long one runtime query may take
	StartupTimeout time.Duration // how long to wait for the armed container (covers image builds)
	Grace          time.Duration // how long the armed container must be seen absent

	// Attached, when set, is called on each attach to an armed container,
	// and again whenever a container that started after the arm joins an
	// attached arm (e.g. one a Compose devcontainer recreated), so its
	// mounts are seen too. It gets all armed containers, the new one
	// included, as the runtimes reported them in the poll that changed the
	// armed set, mounts included, each that runs with its live mount table
	// (Live) read then; Run calls it before it polls again.
	// Polls that bring no new armed container do not call it; they update
	// the armed containers Live reads instead. The containers are copies the callback may keep. armedAt is the time of
	// the arm it attached for (see Armed): a call may come after a later
	// arm, from a poll that was under way when it came.
	Attached func(armedAt time.Time, armed []Container)

	// Armed, when set, is called by each Arm that the watcher follows, with
	// the arm's time, before Arm returns: so before the caller lets the
	// container start. Concurrent arms may call it out of order. The arm
	// New makes is not reported.
	Armed func(armedAt time.Time)
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
	stopped bool      // see ContainerStopped
	// latest is the armed containers of the latest arm's latest attach,
	// which Live reads; nil until the latest arm has attached. Each attach
	// replaces it with a new one, so a Live call can tell whether the one
	// it read is still the latest; Run's later polls and Live calls update
	// its entries.
	latest *attachment
}

// attachment is the armed containers of one attach, each with the
// runtime that reported it, as the newest inspection of it reported it: the
// attach's, a later poll's (see takePoll) or a Live call's.
type attachment struct {
	armed []tracked
}

// ContainerStopped reports whether the watcher is attached and the last
// runtime answer showed its armed container gone: it counts the grace
// before the watch ends. Not while waiting for the armed container, after
// an arm too, nor while it runs.
func (w *Watcher) ContainerStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

// setStopped records what ContainerStopped reports, as observed since the
// arm at armedAt; an observation from before a later arm is dropped, so a
// poll in flight during an arm does not undo it.
func (w *Watcher) setStopped(armedAt time.Time, stopped bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if armedAt.Equal(w.armedAt) {
		w.stopped = stopped
	}
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
// container start, and reported to Config.Armed before Arm returns.
func (w *Watcher) Arm() error {
	armedAt, err := w.arm()
	if err != nil {
		return err
	}
	// Outside w.mu: the callback must not hold up Run or ContainerStopped.
	if w.cfg.Armed != nil {
		w.cfg.Armed(armedAt)
	}
	return nil
}

// arm records an arm now and signals Run, or refuses once the watch has
// ended; it returns the arm's time.
func (w *Watcher) arm() (time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended != nil {
		return time.Time{}, fmt.Errorf("the watch has ended: %w", w.ended)
	}
	w.armedAt = time.Now()
	w.stopped = false // waiting from the arm on
	w.latest = nil    // the arm has not attached yet
	select {
	case w.armSignal <- struct{}{}:
	default: // an arm is already pending; it reads the latest time
	}
	return w.armedAt, nil
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
	stop := contain.AfterFunc(ctx, func() { w.markEnded(context.Cause(ctx)) })
	defer stop()

	var (
		arm        = w.waitFor(w.lastArm())
		lastAnswer time.Time // last time any runtime answered
	)
	for {
		now := time.Now()
		ins := w.query(ctx)
		if ins.answered {
			lastAnswer = now
			if justAttached, changed := arm.observe(ins.containers, now, w.cfg.Grace); changed {
				if justAttached {
					w.log.Info("attached to the armed container", "workspace", w.cfg.Workspace)
				} else {
					w.log.Info("a container joined the armed containers", "workspace", w.cfg.Workspace)
				}
				armed := arm.armed(ins)
				w.readAttached(ctx, armed)
				w.setAttachment(arm.at, armed)
				if w.cfg.Attached != nil {
					w.cfg.Attached(arm.at, containersOf(armed))
				}
			} else if arm.attached() {
				w.takePoll(arm.at, ins)
			}
			w.setStopped(arm.at, arm.attached() && !arm.absentSince.IsZero())
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

// setAttachment makes copies of armed, the armed containers of an attach
// for the arm at armedAt, what Live reads; an attach for an arm before the
// latest, from a poll in flight during an arm, is dropped.
func (w *Watcher) setAttachment(armedAt time.Time, armed []tracked) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if armedAt.Equal(w.armedAt) {
		w.latest = &attachment{armed: copyTracked(armed)}
	}
}

// takePoll makes what ins, a poll for the arm at armedAt that changed no
// armed set, reports of the armed containers of the latest attach what Live
// reads, so that Live follows the runtimes without inspecting them itself
// (see readRequested). One ins does not report any more keeps its data,
// inspected at ins.at. An entry that holds a newer inspection, a Live
// call's that started after the poll, is kept as it is; a poll for an arm
// before the latest, in flight during an arm, changes nothing.
func (w *Watcher) takePoll(armedAt time.Time, ins inspection) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !armedAt.Equal(w.armedAt) || w.latest == nil {
		return
	}
	for i := range w.latest.armed {
		t := &w.latest.armed[i]
		if t.inspectedAt.After(ins.at) {
			continue
		}
		if j := ins.index(t.ID); j >= 0 {
			*t = ins.tracked(j)
		} else {
			t.inspectedAt = ins.at
		}
	}
}

// copyTracked returns copies of ts that share nothing with them but their
// runtimes.
func copyTracked(ts []tracked) []tracked {
	out := make([]tracked, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.copy())
	}
	return out
}

// Live returns copies of the armed containers of the latest attach (those
// Attached was last called with for the latest arm, containers that joined
// it included), as the newest inspection of each reported it (the attach's,
// a later poll's or a Live call's), each that runs with its live mount
// table read now, or ErrNotAttached before the latest arm has attached:
// from an arm until its attach there are none. A container that does not
// run cannot write, so it is left out. One whose process is gone is
// inspected again at once, and one that did not run at an inspection at
// least PollInterval old is inspected again too: it is reported as it runs
// now, or left out if it does not run; if no runtime answers, it carries
// why as its Live.Err. One that did not run at a newer inspection is left
// out without a query: Run's polls inspect it every PollInterval. It is
// safe for concurrent use with Run and other Live calls.
func (w *Watcher) Live(ctx context.Context) ([]Container, error) {
	w.mu.Lock()
	latest := w.latest
	var armed []tracked
	if latest != nil {
		armed = copyTracked(latest.armed)
	}
	w.mu.Unlock()
	if latest == nil {
		return nil, ErrNotAttached
	}
	if w.readRequested(ctx, armed) {
		w.updateAttachment(latest, armed)
	}
	// After readRequested, a container without Live does not run: it cannot
	// write, so it is left out.
	return containersOf(slices.DeleteFunc(armed, func(t tracked) bool { return t.Live == nil })), nil
}

// updateAttachment makes copies of armed, read again since Live copied them
// from a, the containers of a, so that the next Live call reads the
// processes they name now. Each replaces its entry only if that entry holds
// no newer inspection: a concurrent Live call or poll that inspected later
// and wrote first keeps what it found. a no longer the latest, as after an
// arm or another attach meanwhile, is left as it is.
func (w *Watcher) updateAttachment(a *attachment, armed []tracked) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.latest != a {
		return
	}
	for i, t := range armed {
		if !a.armed[i].inspectedAt.After(t.inspectedAt) {
			a.armed[i] = t.copy()
		}
	}
}

// waitFor starts waiting for the container of the arm at armedAt.
func (w *Watcher) waitFor(armedAt time.Time) arming {
	w.setStopped(armedAt, false)
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
// whether the watcher has just attached, and whether the armed set changed:
// that attach, or a container that joined it. Absence counts from the attach
// at the earliest.
func (a *arming) observe(containers []Container, now time.Time, grace time.Duration) (justAttached, changed bool) {
	if !a.attached() {
		a.ids = findArmed(containers, a.at, now, grace)
		justAttached = a.attached()
	}
	if !a.attached() {
		return false, false
	}
	// A container started after the arm is the armed container too, e.g.
	// one a Compose devcontainer recreated.
	for _, c := range containers {
		if c.StartedAt.After(a.at) && !a.ids[c.ID] {
			a.ids[c.ID] = true
			changed = true
		}
	}
	changed = changed || justAttached
	running := slices.ContainsFunc(containers, func(c Container) bool { return c.Running && a.ids[c.ID] })
	switch {
	case running:
		a.absentSince = time.Time{}
	case a.absentSince.IsZero():
		a.absentSince = now
	}
	return justAttached, changed
}

// armed returns copies of the armed containers among those ins found, in
// their order, each with the runtime that reported it.
func (a *arming) armed(ins inspection) []tracked {
	var out []tracked
	for i, c := range ins.containers {
		if a.ids[c.ID] {
			out = append(out, ins.tracked(i))
		}
	}
	return out
}

// copyContainer returns a copy of c that shares nothing with it: its
// mounts, their volumes and its live mount points are copied too.
func copyContainer(c Container) Container {
	c.Mounts = slices.Clone(c.Mounts)
	for i, m := range c.Mounts {
		if m.Volume != nil {
			v := *m.Volume
			v.Options = maps.Clone(v.Options)
			c.Mounts[i].Volume = &v
		}
	}
	if c.Live != nil {
		l := *c.Live
		l.Points = slices.Clone(l.Points)
		c.Live = &l
	}
	return c
}

// tracked is a container together with the runtime that reported it, the
// one that can read its live mount table, and when the inspection that
// reported it started.
type tracked struct {
	Container
	rt          Runtime
	inspectedAt time.Time
}

// copy returns a copy of t that shares nothing with it but its runtime.
func (t tracked) copy() tracked {
	t.Container = copyContainer(t.Container)
	return t
}

// containersOf returns the containers of ts, in their order.
func containersOf(ts []tracked) []Container {
	out := make([]Container, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Container)
	}
	return out
}

// readAttached sets the Live of each of armed, the armed containers of an
// attach, that runs to its live mount table, read through the runtime that
// reported it within QueryTimeout, or to why it could not be read; the
// others keep the Live their runtime reported. A table whose process is gone
// (the container restarted or stopped since its inspection) is read again
// after one fresh inspection of every runtime (see refresh).
func (w *Watcher) readAttached(ctx context.Context, armed []tracked) {
	var gone []int
	for i := range armed {
		if armed[i].Runs() && w.readTable(ctx, &armed[i]) {
			gone = append(gone, i)
		}
	}
	if len(gone) > 0 {
		w.refresh(ctx, armed, gone)
	}
}

// readRequested is readAttached for Live: it also inspects again, in the
// same fresh inspection, the containers that did not run at an inspection
// at least PollInterval old. Those that did not run at a later one are left
// without a Live and not inspected: Run's polls, which come every
// PollInterval, inspect them too (see takePoll). It reports whether any
// container was inspected again.
func (w *Watcher) readRequested(ctx context.Context, armed []tracked) (refreshed bool) {
	now := time.Now()
	var stale []int
	for i := range armed {
		switch {
		case armed[i].Runs():
			if w.readTable(ctx, &armed[i]) {
				stale = append(stale, i)
			}
		case now.Sub(armed[i].inspectedAt) >= w.cfg.PollInterval:
			stale = append(stale, i)
		default:
			armed[i].Live = nil // left out: it did not run, and cannot write
		}
	}
	if len(stale) == 0 {
		return false
	}
	w.refresh(ctx, armed, stale)
	return true
}

// readTable sets t's Live to its live mount table (see mountTable) and
// reports whether its process is gone, so that t must be inspected again.
func (w *Watcher) readTable(ctx context.Context, t *tracked) (gone bool) {
	t.Live = w.mountTable(ctx, *t)
	if !errors.Is(t.Live.Err, ErrProcessGone) {
		return false
	}
	w.log.Info("armed container's process is gone: inspecting again", "container", t.ID)
	return true
}

// refresh inspects every runtime once more and, for each of armed at the
// indexes stale, takes the container as its runtime reports it now, with
// that runtime and the inspection's time, and its live mount table read
// once more if it runs (any failure is its Live.Err, a process gone again
// included). One that does not run gets no Live; one no runtime reports any
// more keeps its data, inspected at the inspection's time, and gets no Live
// either. If no runtime answers, each keeps its data and gets why as its
// Live.Err.
func (w *Watcher) refresh(ctx context.Context, armed []tracked, stale []int) {
	ins := merge(time.Now(), w.pollRuntimes(ctx))
	for _, i := range stale {
		if !ins.answered {
			armed[i].Live = &LiveMounts{Err: ins.err}
			continue
		}
		armed[i].Live = nil
		j := ins.index(armed[i].ID)
		if j < 0 {
			armed[i].inspectedAt = ins.at
			continue
		}
		armed[i] = ins.tracked(j)
		if armed[i].Runs() {
			armed[i].Live = w.mountTable(ctx, armed[i])
		}
	}
}

// mountTable reads the live mount table of t's process through t's
// runtime, within QueryTimeout.
func (w *Watcher) mountTable(ctx context.Context, t tracked) *LiveMounts {
	qctx, cancel := context.WithTimeout(ctx, w.cfg.QueryTimeout)
	defer cancel()
	points, err := t.rt.MountTable(qctx, t.Pid)
	if err != nil {
		return &LiveMounts{Err: err}
	}
	return &LiveMounts{Points: points}
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

// inspection is what one query of every runtime found: the containers of
// those that answered, merged in the order of the runtimes.
type inspection struct {
	containers []Container
	runtimes   []Runtime // runtimes[i] reported containers[i]
	answered   bool      // whether any runtime answered
	err        error     // why none answered; nil when one did
	at         time.Time // when the query started
}

// index returns the index of the container id among those ins found, or -1
// if it found none.
func (ins inspection) index(id string) int {
	return slices.IndexFunc(ins.containers, func(c Container) bool { return c.ID == id })
}

// tracked returns a copy of the container ins found at i, with the runtime
// that reported it and when ins started.
func (ins inspection) tracked(i int) tracked {
	return tracked{Container: copyContainer(ins.containers[i]), rt: ins.runtimes[i], inspectedAt: ins.at}
}

// runtimeAnswer is one runtime's answer to a query: its containers, or why
// it did not answer.
type runtimeAnswer struct {
	rt         Runtime     // the runtime queried
	containers []Container // the workspace's containers it reported; nil when err is set
	err        error       // why it did not answer; nil when it did
}

// pollRuntimes queries every runtime, each within QueryTimeout, and returns
// their answers in the order of the runtimes. It keeps no state, so it is
// safe for concurrent use.
func (w *Watcher) pollRuntimes(ctx context.Context) []runtimeAnswer {
	answers := make([]runtimeAnswer, 0, len(w.runtimes))
	for _, rt := range w.runtimes {
		qctx, cancel := context.WithTimeout(ctx, w.cfg.QueryTimeout)
		found, err := rt.Containers(qctx, w.cfg.Workspace)
		cancel()
		answers = append(answers, runtimeAnswer{rt: rt, containers: found, err: err})
	}
	return answers
}

// merge merges the containers of the answers that came, each with the
// runtime that reported it, of a query that started at at.
func merge(at time.Time, answers []runtimeAnswer) inspection {
	var (
		ins  = inspection{at: at}
		errs []error
	)
	for _, a := range answers {
		if a.err != nil {
			errs = append(errs, a.err)
			continue
		}
		ins.containers = append(ins.containers, a.containers...)
		for range a.containers {
			ins.runtimes = append(ins.runtimes, a.rt)
		}
		ins.answered = true
	}
	switch {
	case ins.answered:
	case len(errs) == 0:
		ins.err = errors.New("no container runtime to ask")
	default:
		ins.err = fmt.Errorf("no container runtime answered: %w", errors.Join(errs...))
	}
	return ins
}

// query is pollRuntimes and merge for Run's polls. A failing runtime is
// logged when it starts and stops failing, not on every poll; w.failing
// makes it Run's goroutine's own.
func (w *Watcher) query(ctx context.Context) inspection {
	at := time.Now()
	answers := w.pollRuntimes(ctx)
	for _, a := range answers {
		if a.err != nil {
			if !w.failing[a.rt] && ctx.Err() == nil {
				w.log.Warn("container runtime query failed", "runtime", a.rt, "error", a.err)
				w.failing[a.rt] = true
			}
			continue
		}
		if w.failing[a.rt] {
			w.log.Info("container runtime answers again", "runtime", a.rt)
			w.failing[a.rt] = false
		}
	}
	return merge(at, answers)
}

// Environment is the environment a runtime's command line runs in, as a
// daemon holds it (see daemon.Environment).
type Environment interface {
	// Entries returns the NAME=value entries of the environment.
	Entries() []string
	// LookPath returns the path of the executable file the program name
	// stands for in the PATH of the environment.
	LookPath(name string) (string, error)
}

// CLI is a Runtime backed by a docker-compatible command line (docker or
// podman), which both accept the same `ps` filters. Without an env, it
// runs with the environment of the process; DetectIn gives it another.
type CLI struct {
	Binary string
	env    Environment
}

// Containers lists the containers carrying the workspace's
// devcontainer.local_folder label, in every state, then inspects them and
// the volumes their mounts name (see inspectVolumes).
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
		"{{.Id}} {{.State.Running}} {{.State.Pid}} {{json .State.StartedAt}} {{json .Mounts}}"}, ids...)
	out, stderr, err := c.run(ctx, args...)
	// A container removed since the listing fails the inspection but not
	// the others: it is absent. Any other failure is the runtime's.
	if err != nil && !strings.Contains(strings.ToLower(stderr), "no such container") {
		return nil, err
	}
	var found []Container
	for line := range strings.Lines(out) {
		// The mounts are JSON and may hold spaces, so they are the rest of the line.
		fields := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 5)
		if len(fields) < 4 {
			return nil, fmt.Errorf("%s inspect: unexpected line %q", c.Binary, line)
		}
		pid, perr := strconv.Atoi(fields[2])
		if perr != nil {
			return nil, fmt.Errorf("%s inspect: pid %s: %w", c.Binary, fields[2], perr)
		}
		started, perr := parseStartedAt(fields[3])
		if perr != nil {
			return nil, fmt.Errorf("%s inspect: %w", c.Binary, perr)
		}
		container := Container{ID: fields[0], Running: fields[1] == "true", Pid: pid, StartedAt: started}
		if len(fields) == 5 {
			container.Mounts, container.MountsErr = parseMounts(fields[4])
		} else {
			container.MountsErr = errors.New("no mounts reported")
		}
		if container.MountsErr != nil {
			container.MountsErr = fmt.Errorf("%s inspect: mounts of %s: %w", c.Binary, container.ID, container.MountsErr)
		}
		found = append(found, container)
	}
	c.inspectVolumes(ctx, found)
	return found, nil
}

// inspectVolumes sets the Volume of every "volume" mount of containers
// whose mounts were read, from one inspection of all the volumes they
// name, each once; docker and podman both name a volume's keys Name,
// Driver and Options. A volume the inspection does not report (one
// removed meanwhile, which makes it fail), or a mount that names none,
// gets a VolumeErr instead: the caller decides what an unknown volume
// means.
func (c CLI) inspectVolumes(ctx context.Context, containers []Container) {
	volumes, inspectErr := c.inspectVolumeNames(ctx, volumeNames(containers))
	c.setVolumes(containers, volumes, inspectErr)
}

// volumeNames returns the names of the volumes the "volume" mounts of
// containers name, each once, in the order they are first named; a mount
// that names none adds nothing.
func volumeNames(containers []Container) []string {
	var names []string
	for _, ct := range containers {
		for _, m := range ct.Mounts {
			if m.Type == MountTypeVolume && m.Name != "" && !slices.Contains(names, m.Name) {
				names = append(names, m.Name)
			}
		}
	}
	return names
}

// inspectVolumeNames runs one `volume inspect` of the volumes named and
// returns those it reports, by name, and its failure, err. A line of its
// output that is not a volume is skipped. A failed command still returns
// the volumes it reported: one removed meanwhile makes it fail, and the
// others are reported all the same. No names run nothing.
func (c CLI) inspectVolumeNames(ctx context.Context, names []string) (volumes map[string]Volume, err error) {
	volumes = make(map[string]Volume, len(names))
	if len(names) == 0 {
		return volumes, nil
	}
	out, _, err := c.run(ctx, append([]string{"volume", "inspect", "--format", "{{json .}}"}, names...)...)
	for line := range strings.Lines(out) {
		var v struct {
			Name    string
			Driver  string
			Options map[string]string
		}
		if json.Unmarshal([]byte(line), &v) == nil && v.Name != "" {
			volumes[v.Name] = Volume{Driver: v.Driver, Options: v.Options}
		}
	}
	return volumes, err
}

// setVolumes sets the Volume of every "volume" mount of containers from
// volumes, the volumes the inspection reported, or its VolumeErr: that
// the mount names no volume, or, for a volume volumes lacks, inspectErr,
// the failure of the inspection, when there was one, else that the
// inspection did not report it.
func (c CLI) setVolumes(containers []Container, volumes map[string]Volume, inspectErr error) {
	for i := range containers {
		for j := range containers[i].Mounts {
			m := &containers[i].Mounts[j]
			if m.Type != MountTypeVolume {
				continue
			}
			v, ok := volumes[m.Name]
			switch {
			case m.Name == "":
				m.VolumeErr = fmt.Errorf("%s inspect: a volume mount of %s at %s names no volume", c.Binary, containers[i].ID, m.Destination)
			case ok:
				m.Volume = &v
			case inspectErr != nil:
				m.VolumeErr = fmt.Errorf("volume %s: %w", m.Name, inspectErr)
			default:
				m.VolumeErr = fmt.Errorf("%s volume inspect: volume %s not reported", c.Binary, m.Name)
			}
		}
	}
}

// run runs the runtime's CLI and returns its stdout and stderr. The query is
// bounded: once ctx ends, the CLI's process group is killed, and a CLI that
// exits or is killed, but leaves its output open (through a process of its
// own) fails the query procgroup.WaitDelay later.
func (c CLI) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	procgroup.InOwnGroup(cmd)
	if c.env != nil {
		cmd.Env = c.env.Entries()
	}
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

// parseMounts reads mounts as {{json .Mounts}} renders them: docker and
// podman both name a mount's keys Type, Name (a volume's), Source,
// Destination and RW. null is no mounts.
func parseMounts(raw string) ([]Mount, error) {
	var parsed []struct {
		Type        string
		Name        string
		Source      string
		Destination string
		RW          bool
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	var mounts []Mount
	for _, m := range parsed {
		mount := Mount{Type: m.Type, Source: m.Source, Destination: m.Destination, ReadOnly: !m.RW}
		if m.Type == MountTypeVolume {
			mount.Name = m.Name
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

// String names the runtime in logs.
func (c CLI) String() string { return c.Binary }

// Detect returns a CLI runtime for each of docker and podman found in the
// PATH of the process, to run with its environment.
func Detect() []Runtime {
	return detect(exec.LookPath, nil)
}

// DetectIn returns a CLI runtime for each of docker and podman found in the
// PATH of env, to run with env, not with the environment of the process.
func DetectIn(env Environment) []Runtime {
	return detect(env.LookPath, env)
}

// detect returns a CLI runtime of env for each of docker and podman that
// lookPath finds.
func detect(lookPath func(string) (string, error), env Environment) []Runtime {
	var found []Runtime
	for _, name := range []string{"docker", "podman"} {
		if path, err := lookPath(name); err == nil {
			found = append(found, CLI{Binary: path, env: env})
		}
	}
	return found
}
