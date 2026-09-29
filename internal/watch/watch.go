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
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Errors Run returns when it gives up.
var (
	ErrNeverStarted       = errors.New("devcontainer did not start within the startup timeout")
	ErrRuntimeUnavailable = errors.New("no container runtime answered within the startup timeout")
)

// Runtime answers whether a container of the workspace is running.
type Runtime interface {
	Running(ctx context.Context, workspace string) (bool, error)
}

// Config tunes a Watcher.
type Config struct {
	Workspace      string        // host workspace folder (the label value)
	PollInterval   time.Duration // how often runtimes are asked
	QueryTimeout   time.Duration // how long one runtime query may take
	StartupTimeout time.Duration // how long to wait for a container (covers image builds)
	Grace          time.Duration // how long the container must be seen absent
}

// Watcher follows one workspace's devcontainer.
//
// It starts out waiting for a container, is attached while one runs, and
// ends once the container has been observed absent for Grace. Rearm puts it
// back into waiting, which `hostrunner up` uses on every devcontainer start:
// a rebuild removes the old container before building the image, and the
// build may take far longer than Grace.
//
// devcontainer arms the daemon before it removes the old container, so the
// old one is still visible right after Rearm. For Grace after a Rearm the
// watcher therefore does not attach to what it sees: if the container
// disappears in that window it is a rebuild, and the watcher keeps waiting
// (up to StartupTimeout) for the new one; if it is still there afterwards,
// the watcher attaches to it as usual.
type Watcher struct {
	runtimes []Runtime
	cfg      Config
	log      *slog.Logger
	rearm    chan struct{}
	failing  map[Runtime]bool // runtimes whose last query failed; Run's goroutine only
}

// New returns a Watcher over runtimes.
func New(runtimes []Runtime, cfg Config, log *slog.Logger) *Watcher {
	return &Watcher{
		runtimes: runtimes,
		cfg:      cfg,
		log:      log,
		rearm:    make(chan struct{}, 1),
		failing:  make(map[Runtime]bool),
	}
}

// Rearm makes a running Watcher wait for a container again, with a fresh
// startup timeout. It never blocks.
func (w *Watcher) Rearm() {
	select {
	case w.rearm <- struct{}{}:
	default: // a rearm is already pending
	}
}

// presence is what the runtimes collectively know about the container.
type presence int

const (
	unknown presence = iota // no runtime answered
	absent                  // some runtime answered, none reports it
	present                 // some runtime reports it running
)

// Run blocks until the devcontainer is gone and returns nil, or gives up:
// ErrNeverStarted if no container appeared within StartupTimeout of the
// start or of the last Rearm, ErrRuntimeUnavailable if no runtime answered
// for StartupTimeout while attached, or ctx's error if cancelled.
//
// Only observed absence counts toward Grace: while no runtime can answer,
// the container's state is unknown and the daemon stays.
func (w *Watcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	var (
		waitingSince = time.Now() // start of the current waiting period
		rearmedAt    time.Time    // last Rearm; zero if never rearmed
		attached     bool
		lastAnswer   time.Time // last time any runtime answered
		absentSince  time.Time // zero unless absence is being observed
	)
	for {
		now := time.Now()
		switch w.query(ctx) {
		case present:
			lastAnswer, absentSince = now, time.Time{}
			if !attached && (rearmedAt.IsZero() || now.Sub(rearmedAt) >= w.cfg.Grace) {
				w.log.Info("devcontainer is running", "workspace", w.cfg.Workspace)
				attached = true
			}
		case absent:
			lastAnswer = now
			if absentSince.IsZero() {
				absentSince = now
			}
		case unknown:
			absentSince = time.Time{}
		}
		switch {
		case !attached && now.Sub(waitingSince) >= w.cfg.StartupTimeout:
			return ErrNeverStarted
		case attached && !absentSince.IsZero() && now.Sub(absentSince) >= w.cfg.Grace:
			w.log.Info("devcontainer stopped", "workspace", w.cfg.Workspace)
			return nil
		case attached && now.Sub(lastAnswer) >= w.cfg.StartupTimeout:
			return ErrRuntimeUnavailable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.rearm:
			w.log.Info("rearmed: waiting for the devcontainer", "workspace", w.cfg.Workspace)
			waitingSince, rearmedAt, attached, absentSince = time.Now(), time.Now(), false, time.Time{}
		case <-ticker.C:
		}
	}
}

// query asks every runtime, each within QueryTimeout. A failing runtime is
// logged when it starts and stops failing, not on every poll.
func (w *Watcher) query(ctx context.Context) presence {
	result := unknown
	for _, rt := range w.runtimes {
		qctx, cancel := context.WithTimeout(ctx, w.cfg.QueryTimeout)
		running, err := rt.Running(qctx, w.cfg.Workspace)
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
		if running {
			return present
		}
		result = absent
	}
	return result
}

// CLI is a Runtime backed by a docker-compatible command line (docker or
// podman), which both accept the same `ps` filters.
type CLI struct {
	Binary string
}

// Running lists running containers carrying the workspace's
// devcontainer.local_folder label.
func (c CLI) Running(ctx context.Context, workspace string) (bool, error) {
	cmd := exec.CommandContext(ctx, c.Binary, "ps", "-q",
		"--filter", "label=devcontainer.local_folder="+workspace,
		"--filter", "status=running")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("%s ps: %w: %s", c.Binary, err, strings.TrimSpace(stderr.String()))
	}
	return len(bytes.TrimSpace(out)) > 0, nil
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
