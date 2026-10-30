package daemon

import (
	"context"
	"errors"
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
func (s *Server) watchRules(ctx context.Context) {
	path := s.approvals.rulesFile
	var (
		last          fileState
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
		if now := statRules(path); now != last {
			last = now
			failedContent = s.rereadChanged(path, failedContent)
		}
	}
}

// rereadChanged rereads the rules file at path, which changed, unless it
// holds the rules served or failedContent, the content whose reread last
// failed (its digest, or why it could not be read). It returns
// failedContent from then on: this content when its reread fails, ""
// otherwise. A file that changes again while it is reread is left to the
// next look, which sees it changed; one a session had reread meanwhile
// changes nothing.
func (s *Server) rereadChanged(path, failedContent string) string {
	content, _, err := rules.FileDigest(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	if err != nil {
		content = "unreadable: " + err.Error()
	}
	if content == s.rulesDigest() {
		return ""
	}
	if content == failedContent {
		return failedContent
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
