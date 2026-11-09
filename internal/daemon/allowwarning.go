package daemon

import (
	"errors"
	"maps"
	"slices"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// allowCheck is the outcome of checking the allow of a change to the rules
// file or the daemon settings that is not allowed: it was found not
// allowed, or the check itself failed, which allows nothing. The zero
// value is neither.
type allowCheck int

const (
	checkNotAllowed allowCheck = iota + 1 // the allow store does not allow the change
	checkNotChecked                       // the allow store could not be asked
)

// allowCheckOf returns the allowCheck of a check that did not allow: err
// is nil, or errNotAllowed (also as the cause of a *notAllowedError),
// when it found the change not allowed, and any other error when the
// check failed.
func allowCheckOf(err error) allowCheck {
	if err == nil || errors.Is(err, errNotAllowed) {
		return checkNotAllowed
	}
	return checkNotChecked
}

// changeScope is what a change that is not allowed changed: the rules
// file, or the rules file or the daemon settings, the latter when the
// settings of an arm differ from the daemon's (see armChange). The zero
// value is neither.
type changeScope int

const (
	rulesChanged           changeScope = iota + 1 // the rules file changed
	rulesOrSettingsChanged                        // the rules file or the daemon settings changed
)

// askAllowed asks the allow store, as it is asked for (see WithAllowed),
// whether it allows sc, which a Server with WithAllowed does: allowed is
// true once it does. A check that fails allows nothing: allowed is false
// and err is why it failed.
func (s *Server) askAllowed(sc allow.Scope) (allowed bool, err error) {
	ok, err := s.allowed(sc)
	return err == nil && ok, err
}

// recheckAllowed asks the allow store again, as it is asked for (see
// WithAllowed), whether it allows sc. allowed is true once it does. Else
// check says why not: checkNotAllowed, or checkNotChecked when the check
// fails. A Server without WithAllowed cannot ask, and returns unchecked as
// the check of what it cannot allow.
func (s *Server) recheckAllowed(sc allow.Scope, unchecked allowCheck) (check allowCheck, allowed bool) {
	if s.allowed == nil {
		return unchecked, false
	}
	ok, err := s.askAllowed(sc)
	if ok {
		return 0, true
	}
	return allowCheckOf(err), false
}

// changeNotice returns the notice that the rules file, or with
// rulesOrSettingsChanged the rules or the daemon settings, changed to
// content of digest, but the change is not allowed, or with
// checkNotChecked its allow cannot be checked: for the session serving
// the daemon (see approvals.noticeNotAllowed), and with Text for the
// warning to hostrun. It is where allowCheck and changeScope turn back
// into the booleans of the wire, the boundary of the daemon's own types
// (protocol.RulesNotAllowed).
func changeNotice(digest string, scope changeScope, check allowCheck) protocol.RulesNotAllowed {
	return protocol.RulesNotAllowed{
		Digest:     digest,
		Settings:   scope == rulesOrSettingsChanged,
		NotChecked: check != checkNotAllowed,
	}
}

// pendingChanges returns the changes to the rules file or the daemon
// settings that are not allowed and pending, each as its notice (see
// changeNotice), checked as they are asked for: the refusal of the rules
// file's content pending (see refuseNotAllowed), then the change the last
// arm reported not allowed (see protocol.ArmNotAllowed). It is the one
// source of what is pending: the warning to hostrun names these changes
// (see pendingAllowWarning), and the session serving the daemon is told of
// them (see noticeChanges and serveSession), so both say the same. The
// rule they are merged by:
//   - the refusal is a change of the rules file, named until it ends (see
//     endNotAllowed) or the allow store allows it with the daemon
//     settings (see WithSettings); a check that fails names it as not
//     checked;
//   - the arm's change is named until the store allows it with the arm's
//     settings (a Server without WithAllowed cannot check it, and keeps
//     it); a check that fails names it as not checked. With settings that
//     differ from the daemon's it is a change of the rules or the daemon
//     settings. With the daemon's own it is a change of the rules file,
//     and not named when its digest is that of the refusal pending, which
//     names it already, nor once the rules file holds the rules served
//     again (see rulesFileServed): that edit was undone.
//
// Arms reach the Server only on its approval socket (see WithApprovals),
// so a Server without one has no arm's change.
func (s *Server) pendingChanges() []protocol.RulesNotAllowed {
	s.notAllowedMu.Lock()
	pending := s.notAllowed
	s.notAllowedMu.Unlock()
	s.armMu.Lock()
	na := s.armNotAllowed
	s.armMu.Unlock()
	var changes []protocol.RulesNotAllowed
	if pending.digest != "" {
		if check, allowed := s.recheckAllowed(allow.Scope{Digest: pending.digest, Settings: s.settings}, pending.check); !allowed {
			changes = append(changes, changeNotice(pending.digest, rulesChanged, check))
		}
	}
	if na != nil {
		if n, ok := s.armChange(*na, pending.digest); ok {
			changes = append(changes, n)
		}
	}
	return changes
}

// armChange returns the notice of na, the change the last arm reported not
// allowed, while the refusal of content of digest pending is pending ("" for
// none), as pendingChanges names it; ok is false when it names none.
func (s *Server) armChange(na protocol.ArmNotAllowed, pending string) (n protocol.RulesNotAllowed, ok bool) {
	check, allowed := s.recheckAllowed(allow.Scope{Digest: na.Digest, Settings: na.Settings}, checkNotAllowed)
	if allowed {
		return protocol.RulesNotAllowed{}, false
	}
	if !maps.Equal(na.Settings, s.settings) {
		return changeNotice(na.Digest, rulesOrSettingsChanged, check), true
	}
	if pending != "" && na.Digest == pending || s.rulesFileServed() {
		return protocol.RulesNotAllowed{}, false
	}
	return changeNotice(na.Digest, rulesChanged, check), true
}

// noticeChanges brings what the session serving the daemon was told of the
// changes not allowed pending (Server.noticed) up to date with
// pendingChanges, and tells that session, if it reads the notice (see
// approvals.noticeNotAllowed), of each change pending it was not told of:
// a new digest, scope or check. A change no longer pending is dropped, so
// the same change made again later is told again. Every source of a change
// not allowed calls it once it changed what is pending: a refusal of the
// rules file's content, by the watcher or by a session's reread (see
// refuseNotAllowed), the end of a refusal (see endNotAllowed) and an arm
// (see Server.arm). It holds s.noticing, which a session starting to
// serve holds as it is told what is pending (see serveSession), so each
// change reaches a session once: as the session starts, or here. Its
// caller holds none of notAllowedMu, armMu and mu (see Server for the
// order of the locks).
func (s *Server) noticeChanges() {
	s.noticing.Lock()
	defer s.noticing.Unlock()
	changes := s.pendingChanges()
	for _, n := range changes {
		if !slices.Contains(s.noticed, n) {
			s.approvals.noticeNotAllowed(n)
		}
	}
	s.noticed = changes
}

// rulesFileServed reports whether the rules file (see WithApprovals)
// holds the rules the Server serves; false without a rules file, so
// without WithApprovals a change with the daemon's own settings is not
// seen undone (see armChange), or when it cannot be read.
func (s *Server) rulesFileServed() bool {
	if s.approvals == nil || s.approvals.rulesFile == "" {
		return false
	}
	content, err := rulesFileDigest(s.approvals.rulesFile)
	return err == nil && content == s.rulesDigest()
}
