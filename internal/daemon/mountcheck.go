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
	"github.com/kravlab/hostrunner/internal/rules"
)

// Messages of the refusals of the mount check, for the client: they name
// container paths only. Those of a failed check end with mountOffForm.
const (
	mountPendingMessage      = "the mount check has not run yet: the daemon has not attached to the container; retry"
	mountUnreadableMessage   = "the daemon cannot read the container's mounts to check them: every command is refused"
	mountUncheckableMessage  = "the daemon cannot check the container's mounts: every command is refused"
	mountWritableMessageForm = "the container can write %s, so it could change the daemon's configuration: every command is refused until it is mounted read-only"
	// Of a failed mount requirement: the container path, then the rule.
	mountNotMountedMessageForm  = "the container does not have %s as a mount point of its own, which rule %q requires: the command is refused until it is mounted"
	mountNotReadOnlyMessageForm = "the container can write %s, which rule %q requires to be read-only: the command is refused until it is mounted read-only"
	// Of any other failure of the requirements stage: the rule.
	mountRequirementUncheckableMessageForm = "the daemon cannot check the mounts rule %q requires: the command is refused"
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

// MountCheck is the per-request mount check of WithMountCheck, made in two
// stages on one reading of the live mount tables of the armed containers,
// so that the daemon knows which stage failed. The protected-path stage is
// the call itself: it reads the tables and checks the protected paths on
// them, and its error refuses every command. When it passes it returns the
// requirements stage, which checks the mount requirements of the rules
// that start the request on the same tables, and whose error refuses only
// the commands of the rule that declares the requirement that failed. The
// requirements stage it returns with a nil error is never nil: the daemon
// calls it without a check, so a nil one is a programming error.
type MountCheck func(ctx context.Context) (MountRequirementsCheck, error)

// MountRequirementsCheck is the requirements stage of a MountCheck: it
// returns nil when the containers meet every one of reqs, the mount
// requirements one rule of a request declares (the part of
// rules.Allowed.Mounts that names that rule), or why they do not. The
// daemon calls it once per rule that declares requirements, in the order
// of their first requirement.
type MountRequirementsCheck func(reqs []rules.MountRequirement) error

// ruleRequirements are the mount requirements of one rule, reqs, which
// the per-request mount check checks for a request besides the protected
// paths, and rule, the command of the rule that declares them, for the
// refusals and notices that name it: a failed requirement refuses the
// commands of that rule, which are those it starts, whichever rule
// selects the request. A request has those of every rule that starts it,
// from one policy, the rules.Allowed that holds them (see
// requirementsByRule).
type ruleRequirements struct {
	rule string
	reqs []rules.MountRequirement
}

// requirementsByRule groups reqs, the mount requirements of every rule
// that starts a request (rules.Allowed.Mounts), by the rule that declares
// each (rules.MountRequirement.Rule), in the order the first requirement
// of each rule comes in reqs, which keeps its own order; nil for none.
func requirementsByRule(reqs []rules.MountRequirement) []ruleRequirements {
	var groups []ruleRequirements
	for _, req := range reqs {
		i := slices.IndexFunc(groups, func(g ruleRequirements) bool { return g.rule == req.Rule })
		if i < 0 {
			groups = append(groups, ruleRequirements{rule: req.Rule})
			i = len(groups) - 1
		}
		groups[i].reqs = append(groups[i].reqs, req)
	}
	return groups
}

// ruleResult is the result of the requirements stage of a live check for
// one rule: err, why the mount requirements of rule are not met, or nil
// when they are (see mountState.requestRefusal).
type ruleResult struct {
	rule string
	err  error
}

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
	// live check made for the request now, with rrs, the requirements of
	// every rule that starts it, or nil (see mountState.requestRefusal).
	requestRefusal(ctx context.Context, rrs []ruleRequirements) *refusal
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

// requestRefusal checks nothing before a request, the mount requirements
// of its rules included: none is refused.
func (noMountCheck) requestRefusal(context.Context, []ruleRequirements) *refusal { return nil }

// mountState is the result of the latest check of the armed container's
// mounts (see WithMountCheck), the mountChecker of a Server that checks
// them.
type mountState struct {
	log       *slog.Logger // the Server's log
	approvals *approvals   // the Server's approval sessions; nil without WithApprovals
	offHint   string       // how a human on the host turns the check off; see WithMountCheck
	// liveCheck is the check made for every request, on the live mount
	// tables, its requirements stage with the mount requirements of each
	// rule that starts the request (see WithMountCheck and requestRefusal).
	liveCheck MountCheck
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
	// check whose protected-path stage failed, which refuses every
	// command; the zero key once one passed that stage, or before any
	// failed. Guarded by reporting.
	requestFailure recordedFailure
	// ruleFailures are, by rule, the path and reason of the latest failed
	// check of the mount requirements the rule declares, which refuses only
	// its commands; the zero key once a check of a request that has them
	// passed them, none before any was checked. Guarded by reporting.
	ruleFailures map[string]recordedFailure
}

// recordedFailure is the latest result of one stage of the per-request
// check, for the protected paths or for the requirements of one rule (see
// requestChecked): key, the zero key for a pass, and seq, the number of
// the live check it is of (see requestsStarted).
type recordedFailure struct {
	key mountFailureKey
	seq uint64
}

// mountFailureKind is the reason a mount check failed, as its refusal
// tells it (see failureRefusal).
type mountFailureKind string

// Reasons a mount check fails.
const (
	mountWritable    mountFailureKind = "writable"      // a container can write a protected path (mountcheck.WritableError)
	mountUnreadable  mountFailureKind = "unreadable"    // a container's mounts cannot be read (mountcheck.UnreadableError)
	mountNotMounted  mountFailureKind = "not mounted"   // a required path is not a mount of its own (mountcheck.NotMountedError)
	mountNotReadOnly mountFailureKind = "not read-only" // a container can write a path required read-only (mountcheck.ReadOnlyRequirementError)
	mountUncheckable mountFailureKind = "uncheckable"   // any other failure of the protected-path stage
	// mountRequirementUncheckable is any other failure of the requirements
	// stage (see MountCheck), e.g. a *mountcheck.UnresolvableError for a
	// required path.
	mountRequirementUncheckable mountFailureKind = "requirement uncheckable"
)

// mountFailureMessages build the message of the refusal of each kind of
// failed mount check from its key (see failureRefusal).
var mountFailureMessages = map[mountFailureKind]func(key mountFailureKey) string{
	mountWritable: func(key mountFailureKey) string {
		return fmt.Sprintf(mountWritableMessageForm, key.containerPath)
	},
	mountUnreadable:  func(mountFailureKey) string { return mountUnreadableMessage },
	mountUncheckable: func(mountFailureKey) string { return mountUncheckableMessage },
	mountNotMounted: func(key mountFailureKey) string {
		return fmt.Sprintf(mountNotMountedMessageForm, key.containerPath, key.rule)
	},
	mountNotReadOnly: func(key mountFailureKey) string {
		return fmt.Sprintf(mountNotReadOnlyMessageForm, key.containerPath, key.rule)
	},
	mountRequirementUncheckable: func(key mountFailureKey) string {
		return fmt.Sprintf(mountRequirementUncheckableMessageForm, key.rule)
	},
}

// mountStage is the stage of a mount check a failure is of (see
// MountCheck).
type mountStage int

// Stages of a mount check.
const (
	stageProtected    mountStage = iota // the protected paths, or the attach-time check
	stageRequirements                   // the mount requirements of one rule
)

// mountFailureKey is the path and reason of a failed mount check: two
// failures with the same key are the same failure, whatever else their
// error text says. Its zero value is no failure.
type mountFailureKey struct {
	stage         mountStage
	kind          mountFailureKind
	container     string // the container ID, of a writable or unreadable container or one failing a mount requirement
	containerPath string // where in the container a protected path is writable, or a mount requirement fails
	// rule is the rule whose mount requirements failed, for a failure of
	// the requirements stage; "" otherwise.
	rule string
}

// ofRequirements reports whether k is a failure of the requirements stage
// (see MountCheck), which refuses only the commands of k.rule, rather than
// one that refuses every command.
func (k mountFailureKey) ofRequirements() bool { return k.stage == stageRequirements }

// failureKey returns the key of failure, the error of a failed mount
// check of stage, or the zero key for nil. For stageRequirements, rule is
// the rule whose requirements were checked; for stageProtected, which is
// also the attach-time check, it is "".
func failureKey(failure error, stage mountStage, rule string) mountFailureKey {
	switch {
	case failure == nil:
		return mountFailureKey{}
	case stage == stageRequirements:
		if n, ok := errors.AsType[*mountcheck.NotMountedError](failure); ok {
			return mountFailureKey{
				stage:         stageRequirements,
				kind:          mountNotMounted,
				container:     n.Container,
				containerPath: n.ContainerPath,
				rule:          rule,
			}
		}
		if r, ok := errors.AsType[*mountcheck.ReadOnlyRequirementError](failure); ok {
			return mountFailureKey{
				stage:         stageRequirements,
				kind:          mountNotReadOnly,
				container:     r.Container,
				containerPath: r.ContainerPath,
				rule:          rule,
			}
		}
		return mountFailureKey{stage: stageRequirements, kind: mountRequirementUncheckable, rule: rule}
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
// before its command would start; for a dry run as it arrives and as its
// last check; for an --ask as it arrives, before it is queued, and once
// after the human's answer, right before the approved run starts, a
// standing or session approval granted only once it passed; for a repeat
// of a standing or session approval as for a run. Its protected-path
// stage checks the protected paths; its requirements stage (see
// MountCheck), the mount requirements of the rules that start the
// request, those of every rule whose command is a prefix of its argv,
// each rule's apart: as the request arrives, those of the rules served at
// that moment, an argv no rule starts having none; after the other
// checks, those of the rules as they were prepared for the command that
// was allowed or approved, so the rules and their requirements always
// come from one policy. A nil error of both passes; ErrMountCheckPending
// from the protected-path stage, when it cannot run yet, refuses the
// request as before the first check; any other error refuses it as a
// failed check does, without withdrawing the requests waiting for an
// answer or changing the result MountChecked recorded. Any error of the
// requirements stage names the rule in the refusal and the session's
// notice: a *mountcheck.NotMountedError or
// *mountcheck.ReadOnlyRequirementError the requirement that failed, any
// other that the rule's requirements cannot be checked. It is called
// concurrently, under the daemon's context.
func WithMountCheck(offHint string, check MountCheck) Option {
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
// (mountcheck.Mounts.Check), called on every attach: the result of the latest arm
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
		m.approvals.mountCheckFailed(err, m.failureRefusal(err, stageProtected, ""))
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
	return m.failureRefusal(r.failure, stageProtected, "")
}

// pendingRefusal returns the refusal of every run, dry run and --ask
// before the mount check can run: a hostrun failure the client may retry.
func pendingRefusal() *refusal {
	return &refusal{protocol.ExitHostrunError, mountPendingMessage, ErrMountCheckPending}
}

// requestRefusal returns the refusal of a run, dry run or --ask by the
// per-request mount check, or nil: that of the attach-time state while it
// has not passed (see refusal), otherwise that of the live check made for
// the request now with rrs, the requirements of every rule that starts it
// (WithMountCheck), pending while it cannot run yet and failed while it
// fails, the session serving the daemon told of each new failure (see
// requestChecked). The requirements stage checks each rule's requirements
// apart, so that a failure names the rule that declares the requirement
// that failed, the first of rrs when several fail. A live check that ends
// with ctx done, the daemon stopping, refuses the request as a stopping
// daemon does, its result neither recorded nor told. Its message names
// container paths only; its error, for the log, the host's.
func (m *mountState) requestRefusal(ctx context.Context, rrs []ruleRequirements) *refusal {
	if r := m.refusal(); r != nil {
		return r
	}
	seq := m.requestsStarted.Add(1)
	requirements, protected := m.liveCheck(ctx)
	var required []ruleResult // the requirements stage for each of rrs, run only once the protected paths passed
	if protected == nil {
		required = make([]ruleResult, len(rrs))
		for i, rr := range rrs {
			required[i] = ruleResult{rule: rr.rule, err: requirements(rr.reqs)}
		}
	}
	if ctx.Err() != nil {
		// What the check found, cut short, is not the container's state.
		return drainingRefusal()
	}
	pending := func(r ruleResult) bool { return errors.Is(r.err, ErrMountCheckPending) }
	if errors.Is(protected, ErrMountCheckPending) || slices.ContainsFunc(required, pending) {
		return pendingRefusal()
	}
	m.requestChecked(seq, protected, required)
	if protected != nil {
		return m.failureRefusal(protected, stageProtected, "")
	}
	for _, r := range required {
		if r.err != nil {
			return m.failureRefusal(r.err, stageRequirements, r.rule)
		}
	}
	return nil
}

// requestChecked records the result of the live check numbered seq (see
// requestsStarted), made for a request: protected, the result of its
// protected-path stage, and, once that passed, required, that of its
// requirements stage for each rule that starts it. It tells the session serving
// the daemon of a failure unlike the latest one of its stage: a failed
// protected path once per path and reason (failureKey), again after any
// check passed the protected paths (see requestFailure); a failure of the
// requirements of a rule once per path and reason for that rule, again
// after a check of a request that has that same rule's requirements
// passed them (see ruleFailures), whatever the requests of other rules
// found meanwhile. In each, a result of a check that started before the
// one recorded last is stale and dropped, so concurrent checks are
// recorded in the order they started. Unlike MountChecked, it ends no
// request waiting for an answer.
func (m *mountState) requestChecked(seq uint64, protected error, required []ruleResult) {
	m.reporting.Lock()
	defer m.reporting.Unlock()
	m.record(&m.requestFailure, seq, protected, stageProtected, "")
	if protected != nil {
		return
	}
	if m.ruleFailures == nil {
		m.ruleFailures = make(map[string]recordedFailure)
	}
	for _, r := range required {
		// Kept once it passes, so that its seq still drops stale results.
		f := m.ruleFailures[r.rule]
		m.record(&f, seq, r.err, stageRequirements, r.rule)
		m.ruleFailures[r.rule] = f
	}
}

// record records in f the result of the live check numbered seq, failure,
// of stage (of the requirements of rule for stageRequirements, "" for
// stageProtected; see failureKey), unless it is stale, and tells the
// session serving the daemon of a failure unlike the one f holds (see
// requestChecked). The caller holds reporting.
func (m *mountState) record(f *recordedFailure, seq uint64, failure error, stage mountStage, rule string) {
	if seq < f.seq {
		return
	}
	f.seq = seq
	key := failureKey(failure, stage, rule)
	if key == f.key {
		return
	}
	f.key = key
	if failure != nil {
		m.approvals.tellMountCheckFailed(failure, key.rule)
	}
}

// failureRefusal returns the refusal of a run, dry run or --ask for
// failure, the error of a failed mount check of stage (see refusal): of
// every one for stageProtected, of those of rule for stageRequirements,
// the stage checking rule's mount requirements (see failureKey). Its
// message ends with how to turn the check off (see WithMountCheck); its
// error, for the log, names the rule of a failure of the requirements
// stage.
func (m *mountState) failureRefusal(failure error, stage mountStage, rule string) *refusal {
	key := failureKey(failure, stage, rule)
	message := mountFailureMessages[key.kind](key) + fmt.Sprintf(mountOffForm, m.offHint)
	err := fmt.Errorf("mount check failed: %w", failure)
	if key.ofRequirements() {
		err = fmt.Errorf("mount check failed: rule %q: %w", key.rule, failure)
	}
	return &refusal{protocol.ExitRejected, message, err}
}

// recheck ends request id, r, just enqueued, for the failed mount check
// when the attach-time state failed after ask's gate: mountCheckFailed did
// not find it waiting. It makes no live check (see requestRefusal). It
// returns the request's outcome, whatever ended it first, and true, or
// false when the check has not failed.
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
// failed with failure, which the frame names in Rule, not in Error, when
// it is a failure of the requirements stage, of the mount requirements of
// rule; rule is "" for one of the protected paths. Unlike
// mountCheckFailed, it ends no request. A session that does not take the
// frame is gone, as in enqueue.
func (a *approvals) tellMountCheckFailed(failure error, rule string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	c := a.serving
	a.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.sendMountCheckFailed(c.out.writeJSON, failure.Error(), rule); err != nil {
		c.conn.Close()
	}
}

// sendMountCheckFailed sends c, through send, the notice that a mount
// check failed with failed, a failure of the mount requirements of rule
// when rule is not "", if c reads it (protocol.Serve.MountCheck). It
// returns the error of send.
func (c *sessionConn) sendMountCheckFailed(send func(protocol.FrameType, any) error, failed, rule string) error {
	if !c.readsMountCheck.Load() {
		return nil
	}
	return send(protocol.FrameMountCheckFailed, protocol.MountCheckFailed{Error: failed, Rule: rule})
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
		return c.sendMountCheckFailed(send, failure.Error(), "")
	})
}
