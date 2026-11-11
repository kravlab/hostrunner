package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
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
// WithRereadEvery) only to content allowed reports allowed, by the
// allow.Scope of its digest and the daemon settings (those of
// WithSettings): one a human on the host allowed for the daemon's
// workspace with those settings (allow.Store.Allowed). A change to content
// not allowed keeps the rules served and is logged, and noticed to the
// session serving the daemon, once for its content. Without it every
// content is allowed. The Server also checks through it a change an arm
// reports not allowed (see protocol.ArmNotAllowed and
// pendingAllowWarning), with the arm's settings.
func WithAllowed(allowed func(allow.Scope) (bool, error)) Option {
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

// WithAllowRecorder makes the Server record an allow of the rules file
// content of the given digest through record (e.g.
// allow.Store.AllowAddedRule with its runtime directory), as the human who
// approved a rule added from the approval session allows what that session
// writes. writtenOver is the Scope the Server found allowed for the content
// the session wrote over, its digest with the daemon settings, so that
// record can allow the content written wherever that content was allowed.
// A record that returns an *allow.RequestsNotRemovedError recorded the
// allow: the Server serves the content and logs the error as a warning. It
// is not WithAllowRequest, which records what a human is asked to allow.
func WithAllowRecorder(record func(writtenOver allow.Scope, digest string) error) Option {
	return func(s *Server) { s.allowRecorder = record }
}

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
		if now := statRules(path); now != last || s.pendingNotAllowed() != "" {
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
//   - content not allowed: the rules served stay, and the session is told
//     once (see refuseNotAllowed);
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
		if pending := s.endNotAllowed(); pending != "" && pending != content {
			s.log.Info("rules change undone: the file holds the rules served", "file", path)
		}
		return ""
	}
	if content == failedContent {
		return failedContent
	}
	if readErr == nil {
		if err := s.refuseNotAllowed(content); err != nil {
			return ""
		}
	}
	digest, changed, err := s.rereadRules(content, nil)
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

// notAllowedError is why the Server keeps its rules against content of
// its rules file, of digest: err is errNotAllowed, or why the allow
// could not be checked, which allows nothing. So it covers both "not
// allowed" and "allow not checked" (see allowCheck). Its message is what the
// approval session shows.
type notAllowedError struct {
	digest string
	err    error
}

// Error says why the content is refused and, when it is not allowed, how
// a human on the host allows it.
func (e *notAllowedError) Error() string {
	if errors.Is(e.err, errNotAllowed) {
		return "its content (digest " + e.digest + ") is not allowed; run `" + protocol.AllowCommand + "` on the host"
	}
	return "the allow of its content (digest " + e.digest + ") cannot be checked: " + e.err.Error()
}

// Unwrap returns errNotAllowed or the error of the check.
func (e *notAllowedError) Unwrap() error { return e.err }

// notAllowedReport is the content of the rules file the Server last
// refused as not allowed (see refuseNotAllowed), pending until the file
// holds the rules served or a reread serves new ones.
type notAllowedReport struct {
	digest string     // the content's digest; "" for none
	check  allowCheck // why the content was refused: not allowed, or not checked
}

// refuseNotAllowed returns nil when the Server may serve rules of the
// given digest (see WithAllowed), or why not, a *notAllowedError. The
// refusal becomes the one pending; when its content or its reason (not
// allowed, or not checked) differs from the one pending before, it is
// logged, content not allowed gets an allow request (see
// WithAllowRequest), and the session serving the daemon is told of it
// unless it was told already (see noticeChanges). The watcher, a session's
// reread of a hand edit or of a rule it added, and an allow that cannot be
// recorded all refuse through it, so each refusal is reported once.
func (s *Server) refuseNotAllowed(digest string) error {
	if s.allowed == nil {
		return nil
	}
	refusal, changed := s.recordRefusal(digest)
	if changed {
		s.noticeChanges()
	}
	if refusal == nil {
		return nil
	}
	return refusal
}

// recordRefusal is refuseNotAllowed but for the notice: it returns the
// refusal, nil when the content is allowed, and whether the refusal pending
// changed. It holds notAllowedMu, which it releases before it returns.
func (s *Server) recordRefusal(digest string) (refusal *notAllowedError, changed bool) {
	s.notAllowedMu.Lock()
	defer s.notAllowedMu.Unlock()
	ok, checkErr := s.askAllowed(allow.Scope{Digest: digest, Settings: s.settings})
	if ok {
		return nil, false
	}
	refusal = &notAllowedError{digest: digest, err: errNotAllowed}
	if checkErr != nil {
		refusal.err = checkErr
	}
	report := notAllowedReport{digest: digest, check: allowCheckOf(checkErr)}
	if s.notAllowed == report {
		return refusal, false
	}
	s.notAllowed = report
	if checkErr != nil {
		s.log.Warn("rules change not checked: the old rules stay", "file", s.approvals.rulesFile, "digest", digest, "error", checkErr)
		return refusal, true
	}
	s.log.Warn("rules changed but not allowed: the old rules stay until `"+protocol.AllowCommand+"`", "file", s.approvals.rulesFile, "digest", digest)
	if s.allowRequest != nil {
		if err := s.allowRequest(digest); err != nil {
			s.log.Warn("allow request not recorded", "file", s.approvals.rulesFile, "digest", digest, "error", err)
		}
	}
	return refusal, true
}

// allowAddedRule decides whether the Server rereads the rules file a
// session wrote the rule rr.Rule into: it returns why not, or nil with the
// function that records the allow of rr.Digest for the reread to call (see
// rereadRules), nil when none is to be recorded. Without WithAllowed every
// content is allowed, and none is recorded. A rule added to a file the
// Server allowed (rr.WrittenOver, the content the session wrote over, is
// allowed with the daemon settings) is allowed too, as the human approving
// it reviewed it: its allow is recorded through WithAllowRecorder, given
// that Scope, and only once the reread has read the content of rr.Digest,
// so no allow of content the Server never loaded is left. A failure to
// record it keeps the old rules, is logged as a warning, and leaves the
// written content pending as a change not allowed (see refuseNotAllowed).
// In any other case (the file written over was not allowed or its allow
// cannot be checked, no written-over digest was given, or no allow can be
// recorded) the written content is treated as a hand edit: it is reread
// only if it is allowed by itself (see refuseNotAllowed), and no allow is
// recorded.
func (s *Server) allowAddedRule(rr protocol.Reread) (record func(digest string) error, err error) {
	if s.allowed == nil {
		return nil, nil
	}
	if s.allowRecorder != nil && rr.WrittenOver != "" {
		// A failed check of the file written over is one not allowed:
		// refuseNotAllowed checks the written content too, and reports a
		// check that fails.
		writtenOver := allow.Scope{Digest: rr.WrittenOver, Settings: s.settings}
		if _, ok := s.recheckAllowed(writtenOver, checkNotAllowed); ok {
			return func(digest string) error { return s.recordAllow(writtenOver, digest) }, nil
		}
	}
	return nil, s.refuseNotAllowed(rr.Digest)
}

// recordAllow records through WithAllowRecorder an allow of the rules file
// content of the given digest, which a session wrote over content allowed
// with the Scope writtenOver and the reread loaded. A failure is logged as
// a warning, and the content is marked pending as a change not allowed, so
// it is reported as any other (see refuseNotAllowed). An allow recorded
// whose answered requests could not all be removed is no failure: the
// error is logged as a warning and the content is served.
func (s *Server) recordAllow(writtenOver allow.Scope, digest string) error {
	err := s.allowRecorder(writtenOver, digest)
	if _, ok := errors.AsType[*allow.RequestsNotRemovedError](err); ok {
		s.log.Warn("allow request not removed", "file", s.approvals.rulesFile, "digest", digest, "error", err)
		err = nil
	}
	if err != nil {
		s.log.Warn("allow not recorded", "file", s.approvals.rulesFile, "digest", digest, "error", err)
		_ = s.refuseNotAllowed(digest)
		return err
	}
	s.log.Info("rule allowed", "file", s.approvals.rulesFile, "digest", digest)
	return nil
}

// pendingNotAllowed returns the digest of the content refuseNotAllowed
// last refused, while it is pending; "" for none.
func (s *Server) pendingNotAllowed() string {
	s.notAllowedMu.Lock()
	defer s.notAllowedMu.Unlock()
	return s.notAllowed.digest
}

// rulesFileDigest returns the digest of the rules file at path, as
// rules.FileDigest does for the file in its directory.
func rulesFileDigest(path string) (string, error) {
	digest, _, err := rules.FileDigest(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	return digest, err
}

// endNotAllowed ends the pending refusal, if any, and returns the digest
// it was pending for; "" for none. What the session serving the daemon was
// told then drops the refusal (see noticeChanges), so the same content
// refused again is told again; the session is not told of the end.
func (s *Server) endNotAllowed() string {
	s.notAllowedMu.Lock()
	pending := s.notAllowed.digest
	s.notAllowed = notAllowedReport{}
	s.notAllowedMu.Unlock()
	if pending != "" {
		s.noticeChanges()
	}
	return pending
}
