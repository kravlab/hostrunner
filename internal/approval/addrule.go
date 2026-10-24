package approval

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// addition is a rule the human adds for request id: rule, the command of
// the rule added or widened; data, the rules file to write, the
// proposal's or the human's edit of it (edited); and base, the digest of
// the file the proposal was computed against, which the file must still
// have when it is written. invalid is set while an edit is not a rules
// file allowing the request, which is never written.
type addition struct {
	id      uint64
	rule    string
	data    []byte
	base    string
	edited  bool
	invalid bool
}

// guarded reports whether approving w needs a second confirmation; a
// guarded request gets no rule.
func (w waiting) guarded() bool { return len(w.Guards) > 0 }

// askAnswer asks for the answer to the shown request.
func (s *session) askAnswer() {
	if s.queue[0].guarded() {
		s.prompt(guardedAnswerPrompt)
		return
	}
	s.prompt(answerPrompt)
}

// proposeRule shows the narrowest edit of the rules file that lets r
// through and asks what to do with it. It is computed against the file on
// disk, which must be the one the daemon serves: an edit nobody reviewed
// waits for the next container start, rather than going live with the
// rule. When there is no edit, it says why and asks for the answer again.
func (s *session) proposeRule(r waiting) {
	digest, data, err := rules.FileDigest(s.rulesFile)
	if err == nil && s.rulesDigest != "" && digest != s.rulesDigest {
		err = errors.New("the rules file changed since the daemon loaded it; the change takes effect at the next container start")
	}
	var p *rules.Proposal
	if err == nil {
		p, err = rules.Propose(data, s.rulesName(), r.Argv)
	}
	if err != nil {
		fmt.Fprintf(s.out, "cannot add a rule: %s\n", display(err.Error()))
		s.askAnswer()
		return
	}
	s.printDiff(p.Diff)
	s.adding = &addition{id: r.ID, rule: p.Rule, data: p.Data, base: digest}
	s.prompt(addPrompt)
}

// addTyped handles a line typed while a rule is being added for r: write
// it, edit it, or cancel, back to the answer.
func (s *session) addTyped(r waiting, text string) {
	switch text {
	case "w":
		if s.adding.invalid {
			s.prompt(s.asking)
			return
		}
		s.writeRule()
	case "e":
		s.editRule(r)
	case "c":
		s.adding = nil
		s.askAnswer()
	default:
		s.prompt(s.asking)
	}
}

// writeRule writes the rule being added into the rules file and asks the
// daemon to reread it; the daemon's result answers the request (see
// reread). A file that changed since the proposal is not overwritten, and
// a failed write goes back to the answer.
func (s *session) writeRule() {
	a := s.adding
	digest, _, err := rules.FileDigest(s.rulesFile)
	if err == nil && digest != a.base {
		err = errors.New("the rules file changed since the proposal")
	}
	if err == nil {
		err = writeRules(s.rulesFile, a.data)
	}
	if err != nil {
		fmt.Fprintf(s.out, "nothing written: %s\n", display(err.Error()))
		s.adding = nil
		s.askAnswer()
		return
	}
	s.rereading = true
	_ = protocol.WriteJSON(s.conn, protocol.FrameReread, protocol.Reread{Rule: a.rule, Edited: a.edited, Digest: rules.ContentDigest(a.data), ID: a.id})
}

// editRule opens the rules file to write in $VISUAL or $EDITOR and writes
// the result when it parses and lets r through; otherwise it says why and
// asks to edit again or cancel. Nothing invalid is written.
func (s *session) editRule(r waiting) {
	a := s.adding
	tmp, err := os.CreateTemp("", "hostrun-*.yaml")
	if err == nil {
		_, err = tmp.Write(a.data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		defer os.Remove(tmp.Name())
	}
	if err == nil {
		err = EditInEditor(tmp.Name())
	}
	var data []byte
	if err == nil {
		data, err = os.ReadFile(tmp.Name())
	}
	switch {
	case errors.Is(err, ErrNoEditor):
		fmt.Fprintln(s.out, err)
		s.prompt(s.asking)
		return
	case err != nil:
		fmt.Fprintf(s.out, "the editor failed: %s\n", display(err.Error()))
		s.prompt(s.asking)
		return
	}
	a.data, a.edited = data, true
	if err := rules.Allows(data, r.Argv); err != nil {
		fmt.Fprintf(s.out, "not written: the edited rules do not allow request %d: %s\n", r.ID, display(err.Error()))
		a.invalid = true
		s.prompt(editAgainPrompt)
		return
	}
	a.invalid = false
	s.writeRule()
}

// reread reports the daemon's reread of the rules file a rule was written
// to, and answers the request it was added for: under the new rule, or
// once, when the daemon kept its old rules or its checks still refuse the
// request under the new ones. A request that ended meanwhile gets no
// answer.
func (s *session) reread(res protocol.RereadResult) {
	a := s.adding
	s.adding, s.rereading = nil, false
	if a == nil {
		return
	}
	waiting := len(s.queue) > 0 && s.queue[0].ID == a.id
	scope := protocol.ScopeRule
	if res.Error != "" {
		scope = protocol.ScopeOnce
		fmt.Fprintf(s.out, "rule added, but the daemon kept its old rules: %s", display(res.Error))
		if waiting {
			fmt.Fprintf(s.out, " · request %d runs once", a.id)
		}
	} else {
		s.rulesDigest = res.Digest
		fmt.Fprint(s.out, "rule added; the daemon reloaded the rules")
		switch {
		case !waiting:
		case res.RunsOnce != "":
			scope = protocol.ScopeOnce
			fmt.Fprintf(s.out, " · request %d runs once: %s", a.id, display(res.RunsOnce))
		default:
			fmt.Fprintf(s.out, " · request %d runs under it", a.id)
		}
	}
	fmt.Fprintln(s.out)
	if !waiting {
		s.idle()
		return
	}
	s.send(protocol.Answer{ID: a.id, Scope: scope}, scopeTexts[scope].outcome)
}

// printDiff prints diff, a proposed edit, a line at a time; a line holding
// what cannot be shown as it is, from an argument of the request, is shown
// quoted.
func (s *session) printDiff(diff string) {
	for _, l := range strings.SplitAfter(strings.TrimSuffix(diff, "\n"), "\n") {
		fmt.Fprintln(s.out, display(strings.TrimSuffix(l, "\n")))
	}
}

// rulesName names the rules file as the diff shows it: relative to the
// daemon's host workspace when it lies inside it.
func (s *session) rulesName() string {
	if rel, err := filepath.Rel(s.workspace, s.rulesFile); err == nil && filepath.IsLocal(rel) {
		return rel
	}
	return s.rulesFile
}

// writeRules replaces the rules file at path, or the file a symbolic link
// there points to, with data at once: written to a temporary file beside
// it, with its mode, synced and renamed over it, so the daemon never reads
// half a file.
func writeRules(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	for _, step := range []func() error{
		func() error { _, err := tmp.Write(data); return err },
		func() error { return tmp.Chmod(mode) },
		tmp.Sync,
	} {
		if err := step(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
