package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
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

// ErrMountCheckPending is why a command is refused before the first mount
// check of the latest arm; the per-request check of WithMountCheck returns
// it when it cannot run yet, before the latest arm has attached.
var ErrMountCheckPending = errors.New("mount check pending: the daemon has not attached to the container yet")

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
	// refusal returns the refusal of every run, dry run and --ask by the
	// attach-time state, while the check of the latest arm has not passed,
	// or nil (see mountState.refusal).
	refusal() *refusal
	// requestRefusal returns the refusal of a run, dry run or --ask by the
	// per-request check: that of the attach-time state, then that of the
	// live check made for the request now, or nil (see
	// mountState.requestRefusal).
	requestRefusal(ctx context.Context) *refusal
	// recheck ends the request id, r, just enqueued, for a failed check of
	// the attach-time state (see mountState.recheck).
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

// requestRefusal checks nothing before a request: none is refused.
func (noMountCheck) requestRefusal(context.Context) *refusal { return nil }

// mountState is the result of the latest check of the armed container's
// mounts (see WithMountCheck), the mountChecker of a Server that checks
// them.
type mountState struct {
	log       *slog.Logger // the Server's log
	approvals *approvals   // the Server's approval sessions; nil without WithApprovals
	offHint   string       // how a human on the host turns the check off; see WithMountCheck
	// liveCheck is the check made for every request, on the live mount
	// tables (see WithMountCheck and requestRefusal).
	liveCheck func(ctx context.Context) error
	// requestsStarted counts the live checks started: each takes the next
	// number as it starts, which orders their results (see
	// requestChecked).
	requestsStarted atomic.Uint64

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
	// before it is told of a request ended for it. It also makes the
	// reports of the per-request checks one at a time (see requestChecked).
	reporting sync.Mutex
	// requestFailure is the path and reason of the latest per-request
	// check that failed; the zero key once one passed, or before any
	// failed. Guarded by reporting.
	requestFailure mountFailureKey
	// requestRecorded is the number of the live check whose result
	// requestFailure holds (see requestsStarted). Guarded by reporting.
	requestRecorded uint64
}

// mountFailureKind is the reason a mount check failed, as its refusal
// tells it (see failureRefusal).
type mountFailureKind string

// Reasons a mount check fails.
const (
	mountWritable    mountFailureKind = "writable"    // a container can write a protected path (mountcheck.WritableError)
	mountUnreadable  mountFailureKind = "unreadable"  // a container's mounts cannot be read (mountcheck.UnreadableError)
	mountUncheckable mountFailureKind = "uncheckable" // any other failure
)

// mountFailureKey is the path and reason of a failed mount check: two
// failures with the same key are the same failure, whatever else their
// error text says. Its zero value is no failure.
type mountFailureKey struct {
	kind          mountFailureKind
	container     string // the container ID, of a writable or unreadable container
	containerPath string // where in the container a protected path is writable
}

// failureKey returns the key of failure, the error of a failed mount
// check, or the zero key for nil.
func failureKey(failure error) mountFailureKey {
	if failure == nil {
		return mountFailureKey{}
	}
	if w, ok := errors.AsType[*mountcheck.WritableError](failure); ok {
		return mountFailureKey{kind: mountWritable, container: w.Container, containerPath: w.ContainerPath}
	}
	if u, ok := errors.AsType[*mountcheck.UnreadableError](failure); ok {
		return mountFailureKey{kind: mountUnreadable, container: u.Container}
	}
	return mountFailureKey{kind: mountUncheckable}
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
// the Server ignores MountRearmed and MountChecked, and checks nothing
// before a request. New builds the check once every option is applied,
// so that it reports to the approval sessions of WithApprovals whichever
// option comes first.
//
// Once the check of the latest arm has passed, check, which must not be
// nil, checks the mounts again for every request, on the live mount
// tables of the armed containers: for a run as it arrives and right
// before its command would start; for a dry run once, as its last check;
// for an --ask as it arrives, before it is queued, and once after the
// human's answer, right before the approved run starts, a standing or
// session approval granted only once it passed. Its nil passes; ErrMountCheckPending, when it
// cannot run yet, refuses the request as before the first check; any
// other error refuses it as a failed check does, without withdrawing the
// requests waiting for an answer or changing the result MountChecked
// recorded. It is called concurrently, under the daemon's context.
func WithMountCheck(offHint string, check func(ctx context.Context) error) Option {
	return func(s *Server) { s.mountOffHint, s.mountLiveCheck = &offHint, check }
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

// refusal returns the refusal of every run, dry run and --ask by the
// attach-time state: while the mount check of the latest arm has not
// passed (see WithMountCheck), or nil. Unlike requestRefusal, it checks
// nothing now. Its message names container paths only; its error, for the
// log, the host's.
func (m *mountState) refusal() *refusal {
	r := m.snapshot()
	switch {
	case !r.checked:
		return pendingRefusal()
	case r.failure == nil:
		return nil
	}
	return m.failureRefusal(r.failure)
}

// pendingRefusal returns the refusal of every run, dry run and --ask
// before the mount check can run: a hostrun failure the client may retry.
func pendingRefusal() *refusal {
	return &refusal{protocol.ExitHostrunError, mountPendingMessage, ErrMountCheckPending}
}

// requestRefusal returns the refusal of a run, dry run or --ask by the
// per-request mount check, or nil: that of the attach-time state while it
// has not passed (see refusal), otherwise that of the live check made for
// the request now (WithMountCheck), pending while it cannot run yet and
// failed while it fails, the session serving the daemon told of each new
// failure (see requestChecked). A live check that ends with ctx done, the
// daemon stopping, refuses the request as a stopping daemon does, its
// result neither recorded nor told. Its message names container paths
// only; its error, for the log, the host's.
func (m *mountState) requestRefusal(ctx context.Context) *refusal {
	if r := m.refusal(); r != nil {
		return r
	}
	seq := m.requestsStarted.Add(1)
	err := m.liveCheck(ctx)
	if ctx.Err() != nil {
		// What the check found, cut short, is not the container's state.
		return drainingRefusal()
	}
	if errors.Is(err, ErrMountCheckPending) {
		return pendingRefusal()
	}
	m.requestChecked(seq, err)
	if err == nil {
		return nil
	}
	return m.failureRefusal(err)
}

// requestChecked records err, the result of the live check numbered seq
// (see requestsStarted), and tells the session serving the daemon of a
// failure unlike the latest one (see requestFailure): once per path and
// reason (failureKey), again after a check passed. A result of a check
// that started before the one recorded last is stale and dropped, so
// concurrent checks are recorded in the order they started. Unlike
// MountChecked, it ends no request waiting for an answer.
func (m *mountState) requestChecked(seq uint64, err error) {
	key := failureKey(err)
	m.reporting.Lock()
	defer m.reporting.Unlock()
	if seq < m.requestRecorded {
		return
	}
	m.requestRecorded = seq
	if key == m.requestFailure {
		return
	}
	m.requestFailure = key
	if err != nil {
		m.approvals.tellMountCheckFailed(err)
	}
}

// failureRefusal returns the refusal of every run, dry run and --ask for
// failure, the error of a failed mount check (see refusal); its message
// ends with how to turn the check off (see WithMountCheck).
func (m *mountState) failureRefusal(failure error) *refusal {
	key := failureKey(failure)
	message := mountUncheckableMessage
	switch key.kind {
	case mountWritable:
		message = fmt.Sprintf(mountWritableMessageForm, key.containerPath)
	case mountUnreadable:
		message = mountUnreadableMessage
	}
	message += fmt.Sprintf(mountOffForm, m.offHint)
	return &refusal{protocol.ExitRejected, message, fmt.Errorf("mount check failed: %w", failure)}
}

// recheck ends request id, r, just enqueued, for the failed mount check
// when the attach-time state failed after ask's gate: mountCheckFailed did
// not find it waiting. It makes no live check (see requestRefusal). It returns the request's outcome, whatever ended it first, and
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

// tellMountCheckFailed tells the session serving the daemon, if it reads
// the frame (protocol.Serve.MountCheck), that a per-request mount check
// failed with failure; unlike mountCheckFailed, it ends no request. A
// session that does not take the frame is gone, as in enqueue.
func (a *approvals) tellMountCheckFailed(failure error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	c := a.serving
	a.mu.Unlock()
	if c == nil || !c.readsMountCheck.Load() {
		return
	}
	if err := c.out.writeJSON(protocol.FrameMountCheckFailed, protocol.MountCheckFailed{Error: failure.Error()}); err != nil {
		c.conn.Close()
	}
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
