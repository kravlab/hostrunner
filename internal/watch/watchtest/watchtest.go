// Package watchtest provides a fake container runtime for tests of code
// that follows a devcontainer with package watch.
package watchtest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/watch"
)

// Runtime is a watch.Runtime whose containers a test starts, stops and
// removes, as docker would, and whose volumes it sets. Each start gives a
// container a new pid, that of its init process, whose live mount table a
// test sets (see MountTable). Its zero value has no containers and no
// volumes.
type Runtime struct {
	mu         sync.Mutex
	containers []watch.Container
	mounts     map[string][]watch.Mount      // by container ID; outlives Remove
	mountsErr  map[string]bool               // IDs whose mounts fail to read
	tables     map[string][]watch.MountPoint // by container ID; outlives Remove
	tablesErr  map[string]bool               // IDs whose mount tables fail to read
	volumes    map[string]watch.Volume       // by name
	volumesErr map[string]bool               // names of the volumes that fail to inspect
	failing    bool
	lastPid    int   // the pid Start gave last; 0 before any
	hold       *hold // the next Containers call waits on it; nil: none
	panicValue any   // the next Containers call panics with it; nil: none
}

// pidBase is what the pids Start gives count up from.
const pidBase = 1000

type hold struct {
	entered, released chan struct{}
}

// Containers returns the containers in every state, or an error while the
// runtime is failing. A call that Panic catches panics instead. A call
// that Hold catches answers once released, with the state at that time.
// Each "volume" mount carries a copy of the volume SetVolume set for its
// name, or, for a volume none set or one FailVolume fails, a VolumeErr, as
// watch.CLI reports a volume it cannot inspect.
func (r *Runtime) Containers(context.Context, string) ([]watch.Container, error) {
	r.mu.Lock()
	if v := r.panicValue; v != nil {
		r.panicValue = nil
		r.mu.Unlock()
		panic(v)
	}
	h := r.hold
	r.hold = nil
	r.mu.Unlock()
	if h != nil {
		close(h.entered)
		<-h.released
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failing {
		return nil, errors.New("runtime unavailable")
	}
	out := slices.Clone(r.containers)
	for i := range out {
		out[i].Mounts = slices.Clone(r.mounts[out[i].ID])
		if r.mountsErr[out[i].ID] {
			out[i].Mounts, out[i].MountsErr = nil, errors.New("mounts unavailable")
		}
		for j, m := range out[i].Mounts {
			if m.Type == watch.MountTypeVolume {
				out[i].Mounts[j].Volume, out[i].Mounts[j].VolumeErr = r.volume(m.Name)
			}
		}
	}
	return out, nil
}

// MountTable returns a copy of the live mount table of the running
// container whose pid is pid. Any other pid, as the old one of a restarted
// container, one of a stopped or removed container, or 0, is a process that
// is gone: the error wraps watch.ErrProcessGone. Until SetMountTable is
// called for the container, its table has one point per mount SetMounts set
// for it, at the cleaned destination and with the ReadOnly flag, in that
// order, as a container just made has.
func (r *Runtime) MountTable(_ context.Context, pid int) ([]watch.MountPoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.containers, func(c watch.Container) bool { return c.Runs() && c.Pid == pid })
	if i < 0 {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, watch.ErrProcessGone)
	}
	id := r.containers[i].ID
	if r.tablesErr[id] {
		return nil, fmt.Errorf("mount table of process %d: unreadable", pid)
	}
	if table, ok := r.tables[id]; ok {
		return slices.Clone(table), nil
	}
	var table []watch.MountPoint
	for _, m := range r.mounts[id] {
		table = append(table, watch.MountPoint{Path: m.PointPath(), ReadOnly: m.ReadOnly})
	}
	return table, nil
}

// SetMountTable sets the live mount table of the init process of the
// container id, which MountTable reports. It is kept across Start and Stop;
// for an id that does not exist yet it is remembered and applies once it
// starts. A test can change it between requests.
func (r *Runtime) SetMountTable(id string, points ...watch.MountPoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tables == nil {
		r.tables = make(map[string][]watch.MountPoint)
	}
	r.tables[id] = slices.Clone(points)
}

// volume returns a copy of the volume name as Containers reports it, or
// why it cannot be inspected. The caller holds r.mu.
func (r *Runtime) volume(name string) (*watch.Volume, error) {
	v, ok := r.volumes[name]
	switch {
	case r.volumesErr[name]:
		return nil, fmt.Errorf("volume %s: inspection failed", name)
	case !ok:
		return nil, fmt.Errorf("volume %s: no such volume", name)
	}
	v.Options = maps.Clone(v.Options)
	return &v, nil
}

// Panic makes the next Containers call panic with v, which is not nil,
// as a bug in a runtime query would; a Hold catches the call after it. A
// test uses it to see what a panic in a watch ends.
func (r *Runtime) Panic(v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.panicValue = v
}

// SetVolume sets the volume name, which the "volume" mounts naming it
// carry (see Containers).
func (r *Runtime) SetVolume(name string, v watch.Volume) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.volumes == nil {
		r.volumes = make(map[string]watch.Volume)
	}
	v.Options = maps.Clone(v.Options)
	r.volumes[name] = v
}

// FailVolume makes the volume name fail to inspect, or inspect again.
func (r *Runtime) FailVolume(name string, failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.volumesErr == nil {
		r.volumesErr = make(map[string]bool)
	}
	r.volumesErr[name] = failing
}

// SetMounts sets the mounts Containers reports for the container id. They
// are kept across Start and Stop; for an id that does not exist yet they are
// remembered and apply once it starts.
func (r *Runtime) SetMounts(id string, mounts ...watch.Mount) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mounts == nil {
		r.mounts = make(map[string][]watch.Mount)
	}
	r.mounts[id] = slices.Clone(mounts)
}

// FailMounts makes Containers report the container id with a MountsErr
// instead of its mounts, or report them again.
func (r *Runtime) FailMounts(id string, failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mountsErr == nil {
		r.mountsErr = make(map[string]bool)
	}
	r.mountsErr[id] = failing
}

// FailMountTable makes MountTable fail, with an error that does not say the
// process is gone, for the process of the container id, or read its table
// again.
func (r *Runtime) FailMountTable(id string, failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tablesErr == nil {
		r.tablesErr = make(map[string]bool)
	}
	r.tablesErr[id] = failing
}

// Start starts the container id, creating it if needed: it runs with a new
// pid, distinct from every pid given before, and its start time is now.
func (r *Runtime) Start(id string) {
	r.update(id, func(c *watch.Container) {
		r.lastPid++
		c.Running, c.StartedAt, c.Pid = true, time.Now(), pidBase+r.lastPid
	})
}

// Stop stops the container id; it keeps its start time and no longer has a
// pid.
func (r *Runtime) Stop(id string) {
	r.update(id, func(c *watch.Container) { c.Running, c.Pid = false, 0 })
}

// Restarting makes the container id, creating it if needed, run without a
// process, as a runtime reports a container that is restarting: Running,
// with pid 0 and the start time it had. The next Start gives it a pid.
func (r *Runtime) Restarting(id string) {
	r.update(id, func(c *watch.Container) { c.Running, c.Pid = true, 0 })
}

// Remove removes the container id.
func (r *Runtime) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers = slices.DeleteFunc(r.containers, func(c watch.Container) bool { return c.ID == id })
}

// Hold makes the next Containers call wait until release is called, even
// if its context is cancelled, as a runtime CLI may take a while to die;
// entered is closed once that call is waiting. A test uses it to act while
// a watcher's query is in flight.
func (r *Runtime) Hold() (entered <-chan struct{}, release func()) {
	h := &hold{entered: make(chan struct{}), released: make(chan struct{})}
	r.mu.Lock()
	r.hold = h
	r.mu.Unlock()
	return h.entered, sync.OnceFunc(func() { close(h.released) })
}

// Fail makes the runtime fail to answer, or answer again.
func (r *Runtime) Fail(failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing = failing
}

// update applies f to the container id, creating it if needed.
func (r *Runtime) update(id string, f func(*watch.Container)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.containers, func(c watch.Container) bool { return c.ID == id })
	if i < 0 {
		r.containers = append(r.containers, watch.Container{ID: id})
		i = len(r.containers) - 1
	}
	f(&r.containers[i])
}
