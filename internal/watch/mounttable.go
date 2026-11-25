package watch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MountPoint is one mount of a live mount table.
type MountPoint struct {
	Path     string // the mount point as the process sees it, in the container
	ReadOnly bool   // whether this mount is read-only: its own options say ro, not those of its filesystem
}

// ErrProcessGone is why a mount table cannot be read: the process whose
// table was asked for does not exist (any more).
var ErrProcessGone = errors.New("the process does not exist")

// ErrNoProcessID is why a mount table cannot be read: the runtime reports
// the container running (restarting) but without a process ID, also at an
// inspection made for the read.
var ErrNoProcessID = errors.New("container runs but reports no process id")

// MountTable returns the live mount table of the process pid, read from
// /proc/<pid>/mountinfo on the host, in the order of mountinfo. docker and
// podman both run their containers' processes there, so it is the same for
// both. Once ctx is done, it reads nothing and returns ctx's error; the read
// itself does not wait, so ctx does not bound it.
func (c CLI) MountTable(ctx context.Context, pid int) ([]MountPoint, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("mount table of process %d: not a process ID", pid)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, err)
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, ErrProcessGone)
	}
	if err != nil {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, err)
	}
	table, err := parseMountInfo(string(raw))
	if err != nil {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, err)
	}
	return table, nil
}

// parseMountInfo reads the text of a /proc/<pid>/mountinfo (see proc(5)),
// one mount per line, in the order given.
func parseMountInfo(text string) ([]MountPoint, error) {
	var table []MountPoint
	for line := range strings.Lines(text) {
		fields := strings.Fields(line)
		// Six fields precede the optional ones, which a "-" ends.
		if len(fields) < 7 || !slices.Contains(fields[6:], "-") {
			return nil, fmt.Errorf("malformed mountinfo line %q", strings.TrimRight(line, "\r\n"))
		}
		table = append(table, MountPoint{
			Path:     unescapeMountInfo(fields[4]),
			ReadOnly: slices.Contains(strings.Split(fields[5], ","), "ro"),
		})
	}
	return table, nil
}

// unescapeMountInfo decodes the octal escapes (a backslash and three
// digits, as \040 for a space) the kernel writes a field of mountinfo with.
// A backslash that starts no escape stays as it is.
func unescapeMountInfo(field string) string {
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+4 <= len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

// Live returns copies of the armed containers of the latest attach (those
// Attached was last called with for the latest arm, containers that joined
// it included), as the newest inspection of each reported it (the attach's,
// a later poll's or a Live call's), each that runs with its live mount
// table read now, or ErrNotAttached before the latest arm has attached:
// from an arm until its attach there are none. A container that does not
// run cannot write, so it is left out. One whose process is gone is
// inspected again at once, and so is one that runs without a process ID
// (restarting: Running, Pid 0), whatever the age of its inspection; one that
// did not run at an inspection at least StoppedInspectionMaxAge (zero:
// PollInterval) old is inspected again too: it is reported as it runs now,
// or left out if it does not run; if no runtime answers, it carries why as
// its Live.Err. One that still has no process ID after that inspection is
// returned with ErrNoProcessID as its Live.Err, and is inspected again at
// each call until it has one. One that did not run at a newer inspection is
// left out without a query: Run's polls inspect it every PollInterval. It
// is safe for concurrent use with Run and other Live calls.
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

// readAttached sets the Live of each of armed, the armed containers of an
// attach, that runs to its live mount table, read through the runtime that
// reported it within QueryTimeout, or to why it could not be read; the
// others keep the Live their runtime reported, a container that runs
// without a process ID (restarting) included: it gets none, here or after the
// fresh inspection, and the request that follows judges it (see Live). A
// table whose process is gone (the container restarted or stopped since its
// inspection) is read again after one fresh inspection of every runtime (see
// refresh).
func (w *Watcher) readAttached(ctx context.Context, armed []tracked) {
	w.readArmed(ctx, armed, armedRead{})
}

// readRequested is readAttached for Live: it also inspects again, in the
// same fresh inspection, the containers that did not run at an inspection
// at least StoppedInspectionMaxAge (zero: PollInterval) old, and, whatever
// the age, those that run without a process ID (Running, Pid 0), whose
// Live.Err is ErrNoProcessID if they still have none then. Those that did
// not run at a later inspection are left without a Live and not inspected:
// Run's polls, which come every PollInterval, inspect them too (see
// takePoll). It reports whether any container was inspected again.
func (w *Watcher) readRequested(ctx context.Context, armed []tracked) (refreshed bool) {
	now := time.Now()
	maxAge := cmp.Or(w.cfg.StoppedInspectionMaxAge, w.cfg.PollInterval)
	// A container that does not run (t.Runs() is false) is inspected again
	// if t.Running holds, which here means it runs without a process ID,
	// or if its inspection is old enough.
	stale := func(t *tracked) bool {
		return t.Running || now.Sub(t.inspectedAt) >= maxAge
	}
	// Leave out the containers that do not run and are not inspected
	// again: they cannot write.
	for i := range armed {
		if t := &armed[i]; !t.Runs() && !stale(t) {
			t.Live = nil
		}
	}
	return w.readArmed(ctx, armed, armedRead{stale: stale, noProcessID: true})
}

// armedRead says what readArmed does beyond reading the mount tables of
// the containers that run.
type armedRead struct {
	// stale reports whether a container that does not run is inspected
	// again; it must not change the container. Nil means none is.
	stale func(*tracked) bool
	// noProcessID is what refresh does for a container that still runs
	// without a process ID after the inspection: it sets ErrNoProcessID
	// as its Live.Err.
	noProcessID bool
}

// readArmed reads the live mount table of each of armed that runs (see
// readTable), then inspects again, in one refresh, the containers whose
// process is gone and those that do not run for which mode.stale returns
// true. It passes mode.noProcessID on to refresh, and reports whether it
// inspected any container again.
func (w *Watcher) readArmed(ctx context.Context, armed []tracked, mode armedRead) (refreshed bool) {
	var again []int
	for i := range armed {
		t := &armed[i]
		var needsInspection bool
		if t.Runs() {
			needsInspection = w.readTable(ctx, t)
		} else {
			needsInspection = mode.stale != nil && mode.stale(t)
		}
		if needsInspection {
			again = append(again, i)
		}
	}
	if len(again) == 0 {
		return false
	}
	w.refresh(ctx, armed, again, mode.noProcessID)
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
