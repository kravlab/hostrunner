package daemon

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// WithRereadEvery makes the Server reread its rules file by itself once
// it changes: every interval it looks at the file WithApprovals names
// (stat, links followed) and, when that changed, reads it and rereads it
// as WithReread does unless its content is that of the rules it serves.
// A file that does not load keeps the rules served and is logged once
// for its content; a missing one loads as rules that deny every command.
// Either way the session serving the daemon is told, if it reads that
// (protocol.Serve.Notices).
// Without WithApprovals or WithReread it does nothing.
func WithRereadEvery(interval time.Duration) Option {
	return func(s *Server) { s.rereadEvery = interval }
}

// WithAllowed makes the Server reread its rules file by itself (see
// WithRereadEvery) only to content allowed reports allowed, by its
// digest and the daemon settings (those of WithSettings): one a human on
// the host allowed for the daemon's workspace with those settings
// (allow.Store.Allowed). A change to content not allowed keeps the rules
// served and is logged, and noticed to the session serving the daemon,
// once for its content. Without it every content is allowed. The Server
// also checks through it a change an arm reports not allowed (see
// protocol.ArmNotAllowed and pendingAllowWarning), with the arm's settings.
func WithAllowed(allowed func(digest string, settings map[string]string) (bool, error)) Option {
	return func(s *Server) { s.allowed = allowed }
}

// WithAllowRequest makes the Server record an allow request through
// request, with the digest of content of its rules file it found not
// allowed (see WithAllowed), once each time it reports that content, so
// that the allow command allows exactly what the Server checks (e.g.
// allow.Store.Request with its workspace, rules file and settings). A
// request that fails is logged and changes nothing else.
func WithAllowRequest(request func(digest string) error) Option {
	return func(s *Server) { s.allowRequest = request }
}

// allowCommand is the command a human on the host runs to allow the
// content of the rules file (see WithAllowed).
const allowCommand = "hostrunner allow"

// errChangedMeanwhile is why rereadRules keeps the old rules when the file
// it rereads is not the one it was asked to.
var errChangedMeanwhile = errors.New("the rules file changed meanwhile")

// rereadsByItself reports whether the Server rereads its rules file once
// it changes (see WithRereadEvery).
func (s *Server) rereadsByItself() bool {
	return s.rereadEvery > 0 && s.rereadFile != nil && s.approvals != nil
}

// fileState is what the Server last saw of its rules file: a change of
// any field is a change of the file, to be read.
type fileState struct {
	err          string // why stat failed; "" when it did not
	dev, ino     uint64
	size         int64
	mtime, ctime int64 // nanoseconds since the epoch
}

// statRules returns the state of the file at path, links followed.
func statRules(path string) fileState {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return fileState{err: err.Error()}
	}
	return fileState{
		dev: st.Dev, ino: st.Ino, size: st.Size,
		mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano(),
	}
}

// watchRules looks at the rules file every rereadEvery until ctx is done
// and rereads it when it changed (see rereadChanged). The first look
// always reads it, so an edit made before Serve started is not missed.
// While content not allowed is pending (see WithAllowed), every look
// reads the file, changed or not, so an allow made meanwhile is taken at
// the next look.
func (s *Server) watchRules(ctx context.Context) {
	path := s.approvals.rulesFile
	var (
		last fileState
		// failedContent is the content whose reread last failed, logged
		// once: its digest, or "unreadable: " and why it could not be
		// read; "" for none.
		failedContent string
	)
	tick := time.NewTicker(s.rereadEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if now := statRules(path); now != last || s.pendingUnallowed() != "" {
			last = now
			failedContent = s.rereadChanged(path, failedContent)
		}
	}
}

// rereadChanged reads the rules file at path, which changed, and returns
// the failedContent of the next look (see watchRules):
//   - the rules served: nothing to do; a pending change not allowed ends,
//     logged as undone unless a session's reread served it;
//   - failedContent: nothing to do, it was logged;
//   - content not allowed: the rules served stay (see refuseUnallowed);
//   - otherwise it is reread, and a failure is logged and kept as
//     failedContent.
//
// A file that changes again while it is reread is left to the next look,
// which sees it changed; one a session had reread meanwhile changes
// nothing.
func (s *Server) rereadChanged(path, failedContent string) string {
	content, readErr := rulesFileDigest(path)
	if readErr != nil {
		content = "unreadable: " + readErr.Error()
	}
	if content == s.rulesDigest() {
		if pending := s.endUnallowed(); pending != "" && pending != content {
			s.log.Info("rules change undone: the file holds the rules served", "file", path)
		}
		return ""
	}
	if content == failedContent {
		return failedContent
	}
	if readErr == nil {
		if first, err := s.refuseUnallowed(content); err != nil {
			if first {
				s.approvals.notice(protocol.RulesReread{Error: err.Error()})
			}
			return ""
		}
	}
	digest, changed, err := s.rereadRules(content)
	if errors.Is(err, errChangedMeanwhile) || err == nil && !changed {
		return ""
	}
	s.logReread(digest, err)
	if err != nil {
		s.approvals.notice(protocol.RulesReread{Error: err.Error()})
		return content
	}
	s.approvals.notice(protocol.RulesReread{Digest: digest})
	return ""
}

// logReread logs a reread of the rules file: the digest of the rules it
// serves since, or err, why the old rules stay.
func (s *Server) logReread(digest string, err error) {
	if err != nil {
		s.log.Warn("rules reread failed: the old rules stay", "file", s.approvals.rulesFile, "error", err)
		return
	}
	s.log.Info("rules reread", "file", s.approvals.rulesFile, "digest", digest)
}

// errNotAllowed is why the Server keeps its rules against content of
// its rules file no human on the host allowed (see WithAllowed).
var errNotAllowed = errors.New("not allowed")

// unallowedError is why the Server keeps its rules against content of
// its rules file, of digest: err is errNotAllowed, or why the allow
// could not be checked, which allows nothing. Its message is what the
// approval session shows.
type unallowedError struct {
	digest string
	err    error
}

// Error says why the content is refused and, when it is not allowed, how
// a human on the host allows it.
func (e *unallowedError) Error() string {
	if errors.Is(e.err, errNotAllowed) {
		return "its content (digest " + e.digest + ") is not allowed; run `" + allowCommand + "` on the host"
	}
	return "the allow of its content (digest " + e.digest + ") cannot be checked: " + e.err.Error()
}

// Unwrap returns errNotAllowed or the error of the check.
func (e *unallowedError) Unwrap() error { return e.err }

// unallowedReport is the content of the rules file the Server last
// refused as not allowed (see refuseUnallowed), pending until the file
// holds the rules served or a reread serves new ones.
type unallowedReport struct {
	digest     string // the content's digest; "" for none
	notAllowed bool   // the check found it not allowed; false: the check failed
}

// refuseUnallowed returns nil when the Server may serve rules of the
// given digest (see WithAllowed), or why not, an *unallowedError. The
// refusal becomes the one pending; when its content or its reason (not
// allowed, or not checked) differs from the one pending before, it is
// logged, content not allowed gets an allow request (see
// WithAllowRequest), and first is true, for the caller to tell the
// session. Both the
// watcher and a session's reread of a hand edit check through it, so
// each refusal is reported once.
func (s *Server) refuseUnallowed(digest string) (first bool, err error) {
	if s.allowed == nil {
		return false, nil
	}
	s.unallowedMu.Lock()
	defer s.unallowedMu.Unlock()
	ok, checkErr := s.allowed(digest, s.settings)
	if checkErr == nil && ok {
		return false, nil
	}
	refusal := &unallowedError{digest: digest, err: errNotAllowed}
	if checkErr != nil {
		refusal.err = checkErr
	}
	report := unallowedReport{digest: digest, notAllowed: checkErr == nil}
	if s.unallowed == report {
		return false, refusal
	}
	s.unallowed = report
	if checkErr != nil {
		s.log.Warn("rules change not checked: the old rules stay", "file", s.approvals.rulesFile, "digest", digest, "error", checkErr)
		return true, refusal
	}
	s.log.Warn("rules changed but not allowed: the old rules stay until `"+allowCommand+"`", "file", s.approvals.rulesFile, "digest", digest)
	if s.allowRequest != nil {
		if err := s.allowRequest(digest); err != nil {
			s.log.Warn("allow request not recorded", "file", s.approvals.rulesFile, "digest", digest, "error", err)
		}
	}
	return true, refusal
}

// pendingUnallowed returns the digest of the content refuseUnallowed
// last refused, while it is pending; "" for none.
func (s *Server) pendingUnallowed() string {
	s.unallowedMu.Lock()
	defer s.unallowedMu.Unlock()
	return s.unallowed.digest
}

// notAllowedWarning returns the warning that the rules file changed to
// content refuseUnallowed refused, while that refusal is pending, with
// the digest of the content and what a human on the host does about it;
// "" for none. The allow of the content is checked as the warning is
// asked for (see WithAllowed), not taken from the refusal: once the store
// allows it there is no warning, even before the next look of the
// watcher; a check that fails gives the warning of a change not checked.
func (s *Server) notAllowedWarning() string {
	s.unallowedMu.Lock()
	pending := s.unallowed
	s.unallowedMu.Unlock()
	if pending.digest == "" {
		return ""
	}
	notAllowed := pending.notAllowed
	if s.allowed != nil {
		ok, err := s.allowed(pending.digest, s.settings)
		if err == nil && ok {
			return ""
		}
		notAllowed = err == nil
	}
	return changeWarning(pending.digest, false, notAllowed)
}

// changeWarning returns the warning that the rules file, or with
// withSettings the rules or the daemon settings, changed to content of
// digest, but the change is not allowed, or with !notAllowed its allow
// cannot be checked, so the daemon keeps the old rules, or ones, and what
// a human on the host does about it. It defines the texts of all four
// variants.
func changeWarning(digest string, withSettings, notAllowed bool) string {
	subject, kept := "the rules file", "rules"
	if withSettings {
		subject, kept = "the rules or the daemon settings", "ones"
	}
	cause, remedy := "the change is not allowed", "runs "+allowCommand+" to allow it"
	if !notAllowed {
		cause, remedy = "its allow cannot be checked", "looks at the daemon log"
	}
	return subject + " changed (digest " + digest + "), but " + cause + ", so the daemon keeps the old " + kept + "; a human on the host " + remedy
}

// armWarning returns the warning of the change the last arm reported not
// allowed (see protocol.ArmNotAllowed), checked as it is asked for; ""
// for none. The change is no longer warned of once the allow store allows
// it (see WithAllowed; a Server without WithAllowed cannot check it, and
// keeps it), nor, when it has the daemon's own settings (see
// WithSettings), once the rules file holds the rules served again. A check
// that fails keeps the change, warned of as one not checked. With the
// daemon's own settings it is a change of the rules file, warned of as
// notAllowedWarning does, and not at all when refusedDigest, the digest
// of the refusal pending (see pendingUnallowed), is its digest, which
// notAllowedWarning warns of already. Without WithApprovals there is no
// rules file (see rulesFileServed), so such a change is not seen undone:
// it ends only on an allow or the next arm.
func (s *Server) armWarning(refusedDigest string) string {
	s.armMu.Lock()
	na := s.armNotAllowed
	s.armMu.Unlock()
	if na == nil {
		return ""
	}
	notAllowed := true
	if s.allowed != nil {
		ok, err := s.allowed(na.Digest, na.Settings)
		if err == nil && ok {
			return ""
		}
		notAllowed = err == nil
	}
	if !maps.Equal(na.Settings, s.settings) {
		return changeWarning(na.Digest, true, notAllowed)
	}
	if na.Digest == refusedDigest || s.rulesFileServed() {
		return ""
	}
	return changeWarning(na.Digest, false, notAllowed)
}

// rulesFileServed reports whether the rules file (see WithApprovals)
// holds the rules the Server serves; false without a rules file, so
// without WithApprovals a change with the daemon's own settings is not
// seen undone (see armWarning), or when it cannot be read.
func (s *Server) rulesFileServed() bool {
	if s.approvals == nil || s.approvals.rulesFile == "" {
		return false
	}
	content, err := rulesFileDigest(s.approvals.rulesFile)
	return err == nil && content == s.rulesDigest()
}

// rulesFileDigest returns the digest of the rules file at path, as
// rules.FileDigest does for the file in its directory.
func rulesFileDigest(path string) (string, error) {
	digest, _, err := rules.FileDigest(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	return digest, err
}

// endUnallowed ends the pending refusal, if any, and returns the digest
// it was pending for; "" for none.
func (s *Server) endUnallowed() string {
	s.unallowedMu.Lock()
	defer s.unallowedMu.Unlock()
	pending := s.unallowed.digest
	s.unallowed = unallowedReport{}
	return pending
}
