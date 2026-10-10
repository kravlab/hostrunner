// Package watchtest provides a fake container runtime for tests of code
// that follows a devcontainer with package watch.
package watchtest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/watch"
)

// Runtime is a watch.Runtime whose containers a test starts, stops and
// removes, as docker would. Its zero value has no containers.
type Runtime struct {
	mu         sync.Mutex
	containers []watch.Container
	failing    bool
	hold       *hold // the next Containers call waits on it; nil: none
}

type hold struct {
	entered, released chan struct{}
}

// Containers returns the containers in every state, or an error while the
// runtime is failing. A call that Hold catches answers once released, with
// the state at that time.
func (r *Runtime) Containers(context.Context, string) ([]watch.Container, error) {
	r.mu.Lock()
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
	return slices.Clone(r.containers), nil
}

// Start starts the container id, creating it if needed: it runs and its
// start time is now.
func (r *Runtime) Start(id string) {
	r.update(id, func(c *watch.Container) { c.Running, c.StartedAt = true, time.Now() })
}

// Stop stops the container id; it keeps its start time.
func (r *Runtime) Stop(id string) {
	r.update(id, func(c *watch.Container) { c.Running = false })
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
