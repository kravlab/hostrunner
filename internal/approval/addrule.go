package approval

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/ruleedit"
	"github.com/kravlab/hostrunner/internal/rules"
)

// addition is a rule the human adds for request id: rule, the command of
// the rule added or widened; data, the rules file to write, the
// proposal's or the human's edit of it (edited); and base, the digest of
// the file the proposal was computed against, which the file must still
// have when it is written. invalid is set while an edit is not a rules
// file allowing the request, which is never written. file is the rules
// file as the proposal read it, where it is written.
type addition struct {
	id      uint64
	rule    string
	data    []byte
	base    string
	edited  bool
	invalid bool
	file    *rulesFileAt
}

// dropAddition ends the rule being added, if any, releasing its file's
// directory.
func (s *session) dropAddition() {
	if s.adding != nil {
		s.adding.file.close()
		s.adding = nil
	}
}

// rulesFileAt is the rules file name in dir, a directory opened once, when
// a proposal reads the file: the file is checked and written there, so
// neither the file nor its directory swapped for a symbolic link
// meanwhile redirects the write. path is where the file lies, symbolic
// links resolved when dir was opened.
type rulesFileAt struct {
	dir  *os.Root
	name string
	path string
}

// openRules opens the directory of the rules file at path, its symbolic
// links resolved (a missing file's in its directory's path).
func openRules(path string) (*rulesFileAt, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		var dir string
		if dir, err = filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			resolved = filepath.Join(dir, filepath.Base(path))
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("the rules file's directory does not exist")
	}
	if err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot(filepath.Dir(resolved))
	if err != nil {
		return nil, err
	}
	return &rulesFileAt{dir: dir, name: filepath.Base(resolved), path: resolved}, nil
}

// close releases the file's directory.
func (f *rulesFileAt) close() { f.dir.Close() }

// digest returns the file's digest and content (see rules.FileDigest). A
// file swapped for a link out of its directory cannot be read: os.Root
// reports that escape without an errno.
func (f *rulesFileAt) digest() (string, []byte, error) {
	digest, data, err := rules.FileDigest(f.dir.FS(), f.name)
	var errno syscall.Errno
	if err != nil && !errors.As(err, &errno) {
		return "", nil, errors.New("the rules file was swapped for a link out of its directory")
	}
	return digest, data, err
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
// disk, which must be the one the daemon serves: for a file changed by
// hand since the daemon loaded it, the session asks whether the daemon
// rereads it, making that change live, and proposes the rule against the
// rules it then serves (see rereadTyped and rereadThenPropose); a daemon
// that rereads by itself rereads it without the question. When there is
// no edit, it says why and asks for the answer again.
func (s *session) proposeRule(r waiting) {
	var (
		digest string
		data   []byte
	)
	file, err := openRules(s.daemon.RulesFile)
	if err == nil {
		digest, data, err = file.digest()
	}
	if err == nil && s.daemon.RulesDigest != "" && digest != s.daemon.RulesDigest {
		// Changed by hand: once the human agrees, the daemon rereads the
		// file first, so the proposal is computed against the rules it
		// then serves (see rereadTyped). A daemon that rereads by itself
		// would soon anyway: it is asked to at once, without a question.
		file.close()
		if s.daemon.Rereads {
			s.askReread(digest)
			return
		}
		s.awaiting, s.rereadDigest = awaitRereadChoice, digest
		s.prompt(rereadPrompt)
		return
	}
	var p *ruleedit.Proposal
	if err == nil {
		p, err = ruleedit.Propose(data, s.rulesName(), r.Argv)
	}
	if err != nil {
		if file != nil {
			file.close()
		}
		fmt.Fprintf(s.out, "cannot add a rule: %s\n", hostterm.Display(err.Error()))
		s.askAnswer()
		return
	}
	s.printDiff(p.Diff)
	if file.path != s.daemon.RulesFile {
		// A link on the way: the diff names the file the daemon reads.
		fmt.Fprintf(s.out, "writes %s\n", hostterm.Display(s.shownPath(file.path)))
	}
	s.awaiting, s.adding = awaitAdd, &addition{id: r.ID, rule: p.Rule, data: p.Data, base: digest, file: file}
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
		s.dropAddition()
		s.awaiting = awaitAnswer
		s.askAnswer()
	default:
		s.prompt(s.asking)
	}
}

// writeRule writes the rule being added into the rules file and asks the
// daemon to reread it; the daemon's result answers the request (see
// reread). A file that changed since the proposal is not overwritten, and
// a failed write goes back to the answer. The file is checked and written
// in the directory the proposal read it from (see addition).
func (s *session) writeRule() {
	a := s.adding
	digest, _, err := a.file.digest()
	if err == nil && digest != a.base {
		err = errors.New("the rules file changed since the proposal")
	}
	if err == nil {
		err = a.file.write(a.data)
	}
	if err != nil {
		fmt.Fprintf(s.out, "nothing written: %s\n", hostterm.Display(err.Error()))
		s.dropAddition()
		s.awaiting = awaitAnswer
		s.askAnswer()
		return
	}
	s.awaiting = awaitReread
	s.write(protocol.FrameReread, protocol.Reread{Rule: a.rule, Edited: a.edited, Digest: rules.ContentDigest(a.data), ID: a.id})
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
		err = editInEditor(tmp.Name())
	}
	var data []byte
	if err == nil {
		data, err = os.ReadFile(tmp.Name())
	}
	switch {
	case errors.Is(err, errNoEditor):
		fmt.Fprintln(s.out, err)
		s.prompt(s.asking)
		return
	case err != nil:
		fmt.Fprintf(s.out, "the editor failed: %s\n", hostterm.Display(err.Error()))
		s.prompt(s.asking)
		return
	}
	a.data, a.edited = data, true
	if err := ruleedit.Allows(data, r.Argv); err != nil {
		fmt.Fprintf(s.out, "not written: the edited rules do not allow request %d: %s\n", r.ID, hostterm.Display(err.Error()))
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
// answer. A reread of a file changed by hand, before a proposal, goes to
// rereadThenPropose.
func (s *session) reread(res protocol.RereadResult) {
	if s.awaiting == awaitProposeReread {
		s.rereadThenPropose(res)
		return
	}
	a := s.adding
	s.dropAddition()
	s.awaiting = awaitAnswer
	if a == nil {
		return
	}
	waiting := len(s.queue) > 0 && s.queue[0].ID == a.id
	scope := protocol.ScopeRule
	if res.Error != "" {
		scope = protocol.ScopeOnce
		fmt.Fprintf(s.out, "rule added, but the daemon kept its old rules: %s", hostterm.Display(res.Error))
		if waiting {
			fmt.Fprintf(s.out, " · request %d runs once", a.id)
		}
	} else {
		s.daemon.RulesDigest = res.Digest
		fmt.Fprint(s.out, "rule added; the daemon reloaded the rules")
		switch {
		case !waiting:
		case res.RunsOnce != "":
			scope = protocol.ScopeOnce
			fmt.Fprintf(s.out, " · request %d runs once: %s", a.id, hostterm.Display(res.RunsOnce))
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

// rereadTyped handles a line typed while the session asks whether the
// daemon rereads a rules file changed by hand (see proposeRule): r has it
// reread the file it found, c keeps the change out until the next
// container start, back to the answer.
func (s *session) rereadTyped(text string) {
	switch text {
	case "r":
		s.askReread(s.rereadDigest)
	case "c":
		fmt.Fprintln(s.out, "cannot add a rule: the rules file changed since the daemon loaded it; the change takes effect at the next container start")
		s.awaiting = awaitAnswer
		s.askAnswer()
	default:
		s.prompt(s.asking)
		return
	}
	s.rereadDigest = ""
}

// askReread asks the daemon to reread the rules file changed by hand,
// found with digest, so a rule for the request shown is proposed against
// the rules it then serves (see rereadThenPropose).
func (s *session) askReread(digest string) {
	s.awaiting, s.rereadFor = awaitProposeReread, s.queue[0].ID
	s.write(protocol.FrameReread, protocol.Reread{Digest: digest})
}

// rereadThenPropose takes the daemon's reread of a rules file changed by
// hand, asked for to propose a rule for request rereadFor (see
// rereadTyped): it proposes the rule against the rules the daemon now
// serves, or says why it cannot. A request that ended meanwhile gets no
// proposal; the one shown is asked again, as what was typed meanwhile was
// not taken.
func (s *session) rereadThenPropose(res protocol.RereadResult) {
	id := s.rereadFor
	s.awaiting, s.rereadFor = awaitAnswer, 0
	if res.Error == "" {
		s.daemon.RulesDigest = res.Digest
	}
	switch {
	case len(s.queue) == 0:
		s.idle()
	case s.queue[0].ID != id:
		s.askAnswer()
	case res.Error != "":
		fmt.Fprintf(s.out, "cannot add a rule: the rules file changed since the daemon loaded it, and the daemon cannot reread it: %s\n", hostterm.Display(res.Error))
		s.askAnswer()
	default:
		s.proposeRule(s.queue[0])
	}
}

// printDiff prints diff, a proposed edit, a line at a time; a line holding
// what cannot be shown as it is, from an argument of the request, is shown
// quoted.
func (s *session) printDiff(diff string) {
	for _, l := range strings.SplitAfter(strings.TrimSuffix(diff, "\n"), "\n") {
		fmt.Fprintln(s.out, hostterm.Display(strings.TrimSuffix(l, "\n")))
	}
}

// rulesName names the rules file as the diff shows it (see shownPath).
func (s *session) rulesName() string { return s.shownPath(s.daemon.RulesFile) }

// shownPath shows path relative to the daemon's host workspace when it
// lies inside it.
func (s *session) shownPath(path string) string {
	if rel, err := filepath.Rel(s.daemon.Workspace, path); err == nil && filepath.IsLocal(rel) {
		return rel
	}
	return path
}

// write replaces the file with data at once: written to a temporary file
// beside it, with its mode, synced and renamed over it, so the daemon
// never reads half a file. A symbolic link there is replaced, not
// followed out of the directory; the mode is that of the file it points
// to in the directory.
func (f *rulesFileAt) write(data []byte) error {
	mode := fs.FileMode(0o644)
	if fi, err := f.dir.Stat(f.name); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, tmpName, err := f.createTemp()
	if err != nil {
		return err
	}
	defer f.dir.Remove(tmpName)
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
	return f.dir.Rename(tmpName, f.name)
}

// createTemp creates a new temporary file beside the file, trying another
// name while one is taken.
func (f *rulesFileAt) createTemp() (*os.File, string, error) {
	for range 10 {
		name := "." + f.name + "." + strconv.FormatUint(rand.Uint64(), 36)
		tmp, err := f.dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			return tmp, name, err
		}
	}
	return nil, "", errors.New("no free name for a temporary file beside the rules file")
}
