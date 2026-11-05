package daemon

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// Messages of the refusals of the mount check, for the client: they name
// container paths only.
const (
	mountPendingMessage      = "the mount check has not run yet: the daemon has not attached to the container; retry"
	mountUnreadableMessage   = "the daemon cannot read the container's mounts to check them: every command is refused"
	mountUncheckableMessage  = "the daemon cannot check the container's mounts: every command is refused"
	mountWritableMessageForm = "the container can write %s, so it could change the daemon's configuration: every command is refused until it is mounted read-only"
)

// errMountPending is why a command is refused before the first mount
// check of the latest arm.
var errMountPending = errors.New("mount check pending: the daemon has not attached to the container yet")

// mountState is the result of the latest check of the armed container's
// mounts (see WithMountCheck).
type mountState struct {
	mu sync.Mutex
	// armedAt is the arm the latest is for: that of the latest re-arm
	// (MountRearmed) or result (MountChecked), whichever is later. Neither
	// of an earlier arm changes latest.
	armedAt time.Time
	latest  mountSnapshot

	// reporting makes the report of a failed check to the session serving
	// the daemon (see MountChecked), to a session starting to serve it (see
	// serveSession) and the recheck of an --ask request just enqueued (see
	// mountRecheck) one at a time: a session is told of a failure once, and
	// before it is told of a request ended for it.
	reporting sync.Mutex
}

// mountSnapshot is the result of the latest mount check, read at once.
type mountSnapshot struct {
	checked bool  // a check of the latest arm has reported its result
	failure error // why the latest check failed; nil when it passed
	// failures counts the failed checks so far: it tells one failure from
	// the next, whatever their text (see sessionConn.helloMountFailures).
	failures uint64
}

// snapshot returns the result of the latest mount check.
func (m *mountState) snapshot() mountSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest
}

// WithMountCheck makes the Server check the mounts of the container it is
// armed for, as a `serve --watch` daemon does: it refuses every run, dry
// run and --ask until the first result of the check of the latest arm
// arrives (see MountRearmed and MountChecked), as a hostrun failure the
// client may retry, and while the latest result is a failure. Without it
// the Server ignores MountRearmed and MountChecked.
func WithMountCheck() Option {
	return func(s *Server) { s.mountCheck = true }
}

// MountRearmed puts the mount check back to pending for the arm at
// armedAt (watch.Config.Armed), called on every arm before `hostrunner up`
// gets its reply: the container the arm starts, or reuses, is not served
// on the result of the one before, until MountChecked brings the result
// for this arm. An arm no later than the one the latest re-arm or result
// is for changes nothing, as concurrent arms may report out of order. It
// is safe for concurrent use with request handling and MountChecked, and
// does nothing without WithMountCheck.
func (s *Server) MountRearmed(armedAt time.Time) {
	if !s.mountCheck {
		return
	}
	m := &s.mountResult
	m.mu.Lock()
	later := armedAt.After(m.armedAt)
	if later {
		m.armedAt = armedAt
		m.latest.checked, m.latest.failure = false, nil
	}
	m.mu.Unlock()
	if later {
		s.log.Info("mount check pending: re-armed")
	}
}

// MountChecked records err, the result of the check of the mounts of the
// container the Server attached to for the arm at armedAt
// (mountcheck.Check), called on every attach: the result of the latest arm
// wins. A result of an arm earlier than the latest re-arm or result, from
// a poll under way as the daemon was re-armed, is dropped. nil passes, and
// commands are served again. A failure refuses every run, dry run and
// --ask from then on, before the rules are consulted; it is logged with
// its host paths, and the approval session serving the Server, if it reads
// it (protocol.Serve.MountCheck), is told. Every --ask request still
// waiting for an answer ends for it (protocol.WithdrawnMountCheck), its
// client refused as a new request would be. It is safe for concurrent use
// with request handling and MountRearmed, and does nothing without
// WithMountCheck.
func (s *Server) MountChecked(armedAt time.Time, err error) {
	if !s.mountCheck {
		return
	}
	m := &s.mountResult
	m.reporting.Lock()
	defer m.reporting.Unlock()
	m.mu.Lock()
	stale := armedAt.Before(m.armedAt)
	if !stale {
		m.armedAt = armedAt
		m.latest.checked, m.latest.failure = true, err
		if err != nil {
			m.latest.failures++
		}
	}
	m.mu.Unlock()
	switch {
	case stale:
		s.log.Info("mount check result dropped: it is of an earlier arm")
	case err == nil:
		s.log.Info("mount check passed")
	default:
		s.log.Warn("mount check failed: every command is refused", "error", err)
		s.approvals.mountCheckFailed(err)
	}
}

// mountFailure returns why the latest mount check failed, as
// protocol.DaemonInfo.MountCheckFailed carries it, or "": when it passed,
// none has run yet, or the Server does not check (see WithMountCheck). It
// also returns the count of failed checks so far, which names that failure
// (see mountSnapshot.failures).
func (s *Server) mountFailure() (text string, failures uint64) {
	if !s.mountCheck {
		return "", 0
	}
	r := s.mountResult.snapshot()
	if r.failure == nil {
		return "", r.failures
	}
	return r.failure.Error(), r.failures
}

// mountRefusal returns the refusal of every run, dry run and --ask while
// the mount check has not passed (see WithMountCheck), or nil. Its message
// names container paths only; its error, for the log, the host's.
func (s *Server) mountRefusal() *refusal {
	if !s.mountCheck {
		return nil
	}
	r := s.mountResult.snapshot()
	switch {
	case !r.checked:
		return &refusal{protocol.ExitHostrunError, mountPendingMessage, errMountPending}
	case r.failure == nil:
		return nil
	}
	return mountFailureRefusal(r.failure)
}

// mountFailureRefusal returns the refusal of every run, dry run and --ask
// for failure, the error of a failed mount check (see mountRefusal).
func mountFailureRefusal(failure error) *refusal {
	message := mountUncheckableMessage
	if w, ok := errors.AsType[*mountcheck.WritableError](failure); ok {
		message = fmt.Sprintf(mountWritableMessageForm, w.ContainerPath)
	} else if _, ok := errors.AsType[*mountcheck.UnreadableError](failure); ok {
		message = mountUnreadableMessage
	}
	return &refusal{protocol.ExitRejected, message, fmt.Errorf("mount check failed: %w", failure)}
}

// mountRecheck ends request id, r, just enqueued, for the failed mount
// check when the check failed after ask's gate: mountCheckFailed did not
// find it waiting. It returns the request's outcome, whatever ended it
// first, and true, or false when the check has not failed.
func (s *Server) mountRecheck(id uint64, r *approvalRequest) (outcome, bool) {
	if !s.mountCheck {
		return outcome{}, false
	}
	s.mountResult.reporting.Lock()
	defer s.mountResult.reporting.Unlock()
	refused := s.mountRefusal()
	if refused == nil {
		return outcome{}, false
	}
	return s.approvals.endWith(id, r, outcome{ended: endedMountCheck, refused: refused}), true
}

// mountCheckFailed tells the session serving the daemon, if it reads the
// frame (protocol.Serve.MountCheck), that the mount check failed with
// failure, and ends every request waiting for an answer for it, their
// clients refused as a new request would be (mountFailureRefusal); the
// session is told of their ends after the failure, in the order of their
// ids. A session that does not take the frames is gone, as in enqueue.
func (a *approvals) mountCheckFailed(failure error) {
	if a == nil {
		return
	}
	refused := mountFailureRefusal(failure)
	told, _ := endedMountCheck.withdrawn()
	var ids []uint64
	a.mu.Lock()
	c := a.serving
	for id, r := range a.waiting {
		delete(a.waiting, id)
		r.done <- outcome{ended: endedMountCheck, refused: refused}
		if c != nil && r.session == c {
			ids = append(ids, id)
			a.telling[id] = told
		}
	}
	a.mu.Unlock()
	if c == nil {
		return
	}
	slices.Sort(ids)
	a.tellWithdrawn(c, told, ids, func(send func(protocol.FrameType, any) error) error {
		if !c.readsMountCheck.Load() {
			return nil
		}
		return send(protocol.FrameMountCheckFailed, protocol.MountCheckFailed{Error: failure.Error()})
	})
}
