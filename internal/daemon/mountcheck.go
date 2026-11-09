package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// Messages of the refusals of the mount check, for the client: they name
// container paths only. Those of a failed check end with mountOffForm.
const (
	mountPendingMessage      = "the mount check has not run yet: the daemon has not attached to the container; retry"
	mountUnreadableMessage   = "the daemon cannot read the container's mounts to check them: every command is refused"
	mountUncheckableMessage  = "the daemon cannot check the container's mounts: every command is refused"
	mountWritableMessageForm = "the container can write %s, so it could change the daemon's configuration: every command is refused until it is mounted read-only"
)

// mountOffForm ends the refusals of a failed mount check with how a human
// on the host turns the check off (the hint WithMountCheck is given) for a
// devcontainer that cannot mount its configuration read-only: the
// container itself cannot.
const mountOffForm = "; a human on the host can turn the mount check off: %s"

// errMountPending is why a command is refused before the first mount
// check of the latest arm.
var errMountPending = errors.New("mount check pending: the daemon has not attached to the container yet")

// mountChecker is what the Server does about the mount check: mountState
// when WithMountCheck turned the check on, noMountCheck, which does
// nothing and refuses nothing, otherwise. New picks one, so the entry
// points do not each ask whether the check is on.
type mountChecker interface {
	// rearmed puts the check back to pending for the arm at armedAt (see
	// Server.MountRearmed).
	rearmed(armedAt time.Time)
	// checked records err, the result of the check for the arm at armedAt
	// (see Server.MountChecked).
	checked(armedAt time.Time, err error)
	// failure returns why the latest check failed, and the count of failed
	// checks so far (see mountState.failure).
	failure() (text string, failures uint64)
	// refusal returns the refusal of every run, dry run and --ask while the
	// check has not passed, or nil (see mountState.refusal).
	refusal() *refusal
	// recheck ends the request id, r, just enqueued, for a failed check
	// (see mountState.recheck).
	recheck(id uint64, r *approvalRequest) (outcome, bool)
	// holdReports makes the report of a failed check to a session wait for
	// the caller (see mountState.reporting), until the returned function is
	// called.
	holdReports() (release func())
}

// noMountCheck is the mountChecker of a Server that does not check the
// mounts (see WithMountCheck): every check passes unseen. Its methods do
// nothing: the result of a check is dropped, none failed and none
// refuses, no request ends for the check, and holding the reports of a
// failed check holds nothing. It has no failure to report and counts no
// failed checks (see mountState.failure).
type noMountCheck struct{}

func (noMountCheck) rearmed(time.Time)             {}
func (noMountCheck) checked(time.Time, error)      {}
func (noMountCheck) failure() (string, uint64)     { return "", 0 }
func (noMountCheck) refusal() *refusal             { return nil }
func (noMountCheck) holdReports() (release func()) { return func() {} }

func (noMountCheck) recheck(uint64, *approvalRequest) (outcome, bool) {
	return outcome{}, false
}

// mountState is the result of the latest check of the armed container's
// mounts (see WithMountCheck), the mountChecker of a Server that checks
// them.
type mountState struct {
	log       *slog.Logger // the Server's log
	approvals *approvals   // the Server's approval sessions; nil without WithApprovals
	offHint   string       // how a human on the host turns the check off; see WithMountCheck

	mu sync.Mutex
	// armedAt is the arm the latest is for: that of the latest re-arm
	// (MountRearmed) or result (MountChecked), whichever is later. Neither
	// of an earlier arm changes latest.
	armedAt time.Time
	latest  mountSnapshot

	// reporting makes the report of a failed check to the session serving
	// the daemon (see MountChecked), to a session starting to serve it (see
	// serveSession) and the recheck of an --ask request just enqueued (see
	// recheck) one at a time: a session is told of a failure once, and
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
// client may retry, and while the latest result is a failure; offHint,
// how a human on the host turns the check off (`mount_check: false in the
// host settings file, …`), ends the refusals of a failed check. Without it
// the Server ignores MountRearmed and MountChecked. New builds the check
// once every option is applied, so that it reports to the approval
// sessions of WithApprovals whichever option comes first.
func WithMountCheck(offHint string) Option {
	return func(s *Server) { s.mountOffHint = &offHint }
}

// MountRearmed puts the mount check back to pending for the arm at
// armedAt (watch.Config.Armed), called on every arm before `hostrunner up`
// gets its reply: the container the arm starts, or reuses, is not served
// on the result of the one before, until MountChecked brings the result
// for this arm. An arm no later than the one the latest re-arm or result
// is for changes nothing, as concurrent arms may report out of order. It
// is safe for concurrent use with request handling and MountChecked, and
// does nothing without WithMountCheck.
func (s *Server) MountRearmed(armedAt time.Time) { s.mount.rearmed(armedAt) }

// rearmed is MountRearmed of a Server that checks the mounts: it puts the
// check back to pending when armedAt is later than the arm the latest
// result is for, and logs it.
func (m *mountState) rearmed(armedAt time.Time) {
	m.mu.Lock()
	later := armedAt.After(m.armedAt)
	if later {
		m.armedAt = armedAt
		m.latest.checked, m.latest.failure = false, nil
	}
	m.mu.Unlock()
	if later {
		m.log.Info("mount check pending: re-armed")
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
func (s *Server) MountChecked(armedAt time.Time, err error) { s.mount.checked(armedAt, err) }

// checked is MountChecked of a Server that checks the mounts: it records
// err unless the arm at armedAt is earlier than the latest, logs the
// result and, for a failure, tells the approval session and ends the
// waiting requests (see approvals.mountCheckFailed). It reports one
// result at a time (see mountState.reporting).
func (m *mountState) checked(armedAt time.Time, err error) {
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
		m.log.Info("mount check result dropped: it is of an earlier arm")
	case err == nil:
		m.log.Info("mount check passed")
	default:
		m.log.Warn("mount check failed: every command is refused", "error", err)
		m.approvals.mountCheckFailed(err, m.failureRefusal(err))
	}
}

// failure returns why the latest mount check failed, as
// protocol.DaemonInfo.MountCheckFailed carries it, or "" when it passed or
// none has run yet. It also returns the count of failed checks so far,
// which names that failure (see mountSnapshot.failures).
func (m *mountState) failure() (text string, failures uint64) {
	r := m.snapshot()
	if r.failure == nil {
		return "", r.failures
	}
	return r.failure.Error(), r.failures
}

// refusal returns the refusal of every run, dry run and --ask while the
// mount check has not passed (see WithMountCheck), or nil. Its message
// names container paths only; its error, for the log, the host's.
func (m *mountState) refusal() *refusal {
	r := m.snapshot()
	switch {
	case !r.checked:
		return &refusal{protocol.ExitHostrunError, mountPendingMessage, errMountPending}
	case r.failure == nil:
		return nil
	}
	return m.failureRefusal(r.failure)
}

// failureRefusal returns the refusal of every run, dry run and --ask for
// failure, the error of a failed mount check (see refusal); its message
// ends with how to turn the check off (see WithMountCheck).
func (m *mountState) failureRefusal(failure error) *refusal {
	message := mountUncheckableMessage
	if w, ok := errors.AsType[*mountcheck.WritableError](failure); ok {
		message = fmt.Sprintf(mountWritableMessageForm, w.ContainerPath)
	} else if _, ok := errors.AsType[*mountcheck.UnreadableError](failure); ok {
		message = mountUnreadableMessage
	}
	message += fmt.Sprintf(mountOffForm, m.offHint)
	return &refusal{protocol.ExitRejected, message, fmt.Errorf("mount check failed: %w", failure)}
}

// recheck ends request id, r, just enqueued, for the failed mount check
// when the check failed after ask's gate: mountCheckFailed did not find it
// waiting. It returns the request's outcome, whatever ended it first, and
// true, or false when the check has not failed.
func (m *mountState) recheck(id uint64, r *approvalRequest) (outcome, bool) {
	m.reporting.Lock()
	defer m.reporting.Unlock()
	refused := m.refusal()
	if refused == nil {
		return outcome{}, false
	}
	return m.approvals.endWith(id, r, outcome{ended: endedMountCheck, refused: refused}), true
}

// holdReports makes the report of a failed check to a session wait until
// the returned function is called (see mountState.reporting).
func (m *mountState) holdReports() (release func()) {
	m.reporting.Lock()
	return m.reporting.Unlock
}

// mountCheckFailed tells the session serving the daemon, if it reads the
// frame (protocol.Serve.MountCheck), that the mount check failed with
// failure, and ends every request waiting for an answer for it, their
// clients refused with refused, as a new request would be
// (mountState.failureRefusal); the session is told of their ends after
// the failure, in the order of their ids. A session that does not take the
// frames is gone, as in enqueue.
func (a *approvals) mountCheckFailed(failure error, refused *refusal) {
	if a == nil {
		return
	}
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
