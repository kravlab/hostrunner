package client_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// addPromptText is what the session asks once it shows a proposed rule.
const addPromptText = "[w]rite [e]dit in $EDITOR [c]ancel ? "

// ruleAddedText is what the session prints once the daemon reread the
// rules with a rule it added.
const ruleAddedText = "rule added; the daemon reread the rules"

// errNoReread stands for a rules file the daemon fails to reread.
var errNoReread = errors.New("the rules cannot be reread")

// startRulesDaemon starts a daemon serving the rules file content, written
// into the host workspace (none when content is ""), and rereading it when
// a session adds a rule; with failReread, every reread fails. opts are
// the Server options besides its reread and approval socket.
func startRulesDaemon(t *testing.T, log *slog.Logger, content string, failReread bool, opts ...daemon.Option) harness {
	t.Helper()
	return startRulesDaemonWith(t, log, failReread, func(rulesFile string) {
		if content != "" {
			if err := os.WriteFile(rulesFile, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}, opts...)
}

// startRulesDaemonWith is startRulesDaemon with the rules file, in its
// existing directory, made by prepare.
func startRulesDaemonWith(t *testing.T, log *slog.Logger, failReread bool, prepare func(rulesFile string), opts ...daemon.Option) harness {
	t.Helper()
	return startHarness(t, log, func(rulesFile string) (daemon.Policy, []daemon.Option) {
		if err := os.MkdirAll(filepath.Dir(rulesFile), 0o755); err != nil {
			t.Fatal(err)
		}
		prepare(rulesFile)
		p, err := rules.Load(rulesFile)
		if err != nil {
			t.Fatal(err)
		}
		reread := func() (daemon.Policy, string, error) {
			if failReread {
				return nil, "", errNoReread
			}
			p, err := rules.Load(rulesFile)
			if err != nil {
				return nil, "", err
			}
			return p, p.Digest(), nil
		}
		return p, append([]daemon.Option{daemon.WithReread(p.Digest(), reread)}, opts...)
	})
}

// readFile returns the content of the file at path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// addRule answers the shown request "add a rule" and writes the proposed
// rule; it returns the block that showed the proposal.
func (s *session) addRule(t *testing.T) string {
	t.Helper()
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	block := s.waitFor(t, addPromptText)
	s.answer(t, "w")
	return block
}

// A command no rule matches gets a new rule: shown as a diff, written after
// the last rule with the rest of the file kept byte for byte, reread by the
// daemon, and run under it; later runs need no approval.
func TestAddRuleForACommandNoRuleMatches(t *testing.T) {
	const before = "# hostrun rules\nrules:\n  # echo anything\n  - command: echo\n    args: any # any args\n\n# the end\n"
	const rule = "  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n"
	var log lockedBuffer
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), before, false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	block := s.addRule(t)
	diff := "--- .devcontainer/hostrun.yaml\n+++ .devcontainer/hostrun.yaml\n@@ -3,5 +3,10 @@\n" +
		"   # echo anything\n   - command: echo\n     args: any # any args\n" +
		"+  - command: touch\n+    flags:\n+      allow: [-a]\n+    positional:\n+      allow: [marker]\n" +
		" \n # the end\n"
	if !strings.Contains(block, diff) {
		t.Fatalf("the block %q lacks the diff %q", block, diff)
	}
	s.waitFor(t, ruleAddedText+" · request 1 runs under it\n")
	got := awaitResult(t, ch)
	if want := (result{stderr: "hostrun: waiting for approval on the host: no rule allows \"touch -a\"\nhostrun: approved on the host: rule added\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	want := "# hostrun rules\nrules:\n  # echo anything\n  - command: echo\n    args: any # any args\n" + rule + "\n# the end\n"
	if file := readFile(t, h.rulesFile); file != want {
		t.Fatalf("the rules file is\n%s\nwant\n%s", file, want)
	}
	waitForLog(t, &log, `msg="rule added"`)
	waitForLog(t, &log, `rule=touch`)
	waitForLog(t, &log, `msg="rules reread"`)
	waitForLog(t, &log, `approved=rule`)
	if err := os.Remove(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatalf("the approved command did not run: %v", err)
	}
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got != (result{}) {
		t.Fatalf("a later run: got %+v, want it to run without asking", got)
	}
}

// An arm after a rule is added gets the digest of the rules reread with
// it, so `hostrunner up` validating the edited file keeps the daemon.
func TestAddRuleArmGetsTheRereadDigest(t *testing.T) {
	digests := make(chan string, 1)
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "rules:\n  - command: echo\n    args: any\n", false,
		daemon.WithArmHandler(func(_ protocol.Arm, digest string) bool {
			digests <- digest
			return false
		}))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.addRule(t)
	s.waitFor(t, ruleAddedText+" · request 1 runs under it\n")
	awaitResult(t, ch)
	reread, err := rules.Load(h.rulesFile)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := h.transport.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(conn, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: reread.Digest()}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameArmed {
		t.Fatalf("frame %v, err %v; want armed", f.Type, err)
	}
	select {
	case got := <-digests:
		if got != reread.Digest() {
			t.Fatalf("the arm got digest %q, want that of the reread rules %q", got, reread.Digest())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the arm handler was not called")
	}
}

// The proposal reads the argv as the rules do, and widens a matching rule
// by what its allow lists refused, or turns args: none into exact lists;
// only the rule's lines change.
func TestAddRuleProposesTheNarrowestRule(t *testing.T) {
	// anyargs, a program on PATH, takes any argv and succeeds.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "anyargs"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const head, tail = "# rules\nrules:\n  # the rule\n", "  # next\n  - command: echo\n    args: any\n"
	tests := []struct {
		name         string
		rule, argv   string
		want         string
		wantContains string
	}{
		{
			name: "flags, a value and a cluster",
			argv: "touch -ac --date=now marker",
			want: "  - command: touch\n    flags:\n      allow: [-a, -c]\n      values:\n        --date: {allow: [now]}\n    positional:\n      allow: [marker]\n",
		},
		{
			name:         "an argument a glob would match more",
			argv:         "touch -a m*",
			wantContains: "allow_regex:",
		},
		{
			name: "a flag the rule refused",
			rule: "  - command: touch # touch it\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n",
			argv: "touch -a -m marker",
			want: "  - command: touch # touch it\n    flags:\n      allow: [-a, -m]\n    positional:\n      allow: [marker]\n",
		},
		{
			name: "a value the rule refused",
			rule: "  - command: touch\n    flags:\n      allow: []\n      values:\n        --date: {allow: [now]}\n    positional:\n      allow: [marker]\n",
			argv: "touch --date=today marker",
			want: "  - command: touch\n    flags:\n      allow: []\n      values:\n        --date: {allow: [now, today]}\n    positional:\n      allow: [marker]\n",
		},
		{
			name: "an argument the rule refused",
			rule: "  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n",
			argv: "touch -a other",
			want: "  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker, other]\n",
		},
		{
			name: "args: none",
			rule: "  - command: date\n    args: none\n",
			argv: "date -u",
			want: "  - command: date\n    flags:\n      allow: [-u]\n    positional:\n      allow: []\n",
		},
		{
			name: "a flag a go-style rule refused",
			rule: "  - command: date\n    flag_style: go\n    flags:\n      allow: [-R]\n    positional:\n      allow: []\n",
			argv: "date -u",
			want: "  - command: date\n    flag_style: go\n    flags:\n      allow: [-R, -u]\n    positional:\n      allow: []\n",
		},
		{
			name: "a refused flag given a value in its token",
			rule: "  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n",
			argv: "touch --date=now marker",
			want: "  - command: touch\n    flags:\n      allow: [-a]\n      values:\n        --date: {allow: [now]}\n    positional:\n      allow: [marker]\n",
		},
		{
			name: "a single dash is an argument",
			argv: "cat -",
			want: "  - command: cat -\n    args: none\n",
		},
		{
			name: "a comment after the rule stays once",
			rule: "  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n    # note\n",
			argv: "touch -a other",
			want: "  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker, other]\n    # note\n",
		},
		{
			// anyargs, not touch: a touch may refuse a flag given twice.
			name: "a flag refused twice is listed once",
			rule: "  - command: anyargs\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n",
			argv: "anyargs -m -m marker",
			want: "  - command: anyargs\n    flags:\n      allow: [-a, -m]\n    positional:\n      allow: [marker]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startRulesDaemon(t, slog.New(slog.DiscardHandler), head+tt.rule+tail, false)
			s := h.startSession(t)
			argv := strings.Fields(tt.argv)
			ch := h.ask(t, argv...)
			s.addRule(t)
			s.waitFor(t, ruleAddedText)
			if got := awaitResult(t, ch); got.code != 0 {
				t.Fatalf("got %+v, want the approved command to run", got)
			}
			file := readFile(t, h.rulesFile)
			if tt.want != "" {
				want := head + tt.want + tail
				if tt.rule == "" {
					want = head + tail + tt.want
				}
				if file != want {
					t.Fatalf("the rules file is\n%s\nwant\n%s", file, want)
				}
			}
			if !strings.Contains(file, tt.wantContains) {
				t.Fatalf("the rules file\n%s\nlacks %q", file, tt.wantContains)
			}
			if got := h.run(t, containerRoot, "", argv...); got.code != 0 {
				t.Fatalf("a later run: got %+v, want it to run", got)
			}
		})
	}
}

// A guarded refusal gets no rule: the answer is not offered, and typing it
// asks again.
func TestAddRuleIsNotOfferedForGuardedRefusals(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), guardedRules, false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "+marker")
	s.waitFor(t, guardedPromptText)
	s.answer(t, "a")
	s.waitFor(t, guardedPromptText)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
	if file := readFile(t, h.rulesFile); file != guardedRules {
		t.Fatalf("the rules file changed:\n%s", file)
	}
}

// The human may edit the proposal: an edit that does not parse or does not
// let the request through is never written, and asks to edit again or
// cancel; a valid one is written as edited.
func TestAddRuleEditedInTheEditor(t *testing.T) {
	const edited = "rules:\n  # added by hand\n  - command: touch\n    args: any\n"
	scriptEditor(t, "rules: [", "rules:\n  - command: touch\n    args: none\n", edited)
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "rules:\n  - command: echo\n    args: any\n", false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	for range 2 {
		s.answer(t, "e")
		s.waitFor(t, "not written: the edited rules do not allow request 1: ")
		s.waitFor(t, "[e]dit again [c]ancel ? ")
	}
	s.answer(t, "w")
	s.waitFor(t, "[e]dit again [c]ancel ? ")
	s.answer(t, "e")
	s.waitFor(t, ruleAddedText)
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the approved command to run", got)
	}
	if file := readFile(t, h.rulesFile); file != edited {
		t.Fatalf("the rules file is\n%s\nwant the edit\n%s", file, edited)
	}
}

// Without $VISUAL or $EDITOR the session says so and asks again; cancel
// goes back to the answer.
func TestAddRuleWithoutAnEditorSaysSo(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	const content = "rules:\n  - command: echo\n    args: any\n"
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), content, false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	s.answer(t, "e")
	s.waitFor(t, "$EDITOR is not set\n"+addPromptText)
	s.answer(t, "c")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	if file := readFile(t, h.rulesFile); file != content {
		t.Fatalf("the rules file changed:\n%s", file)
	}
}

// With no rules file, adding a rule creates it.
func TestAddRuleCreatesAMissingRulesFile(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "", false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	block := s.addRule(t)
	if !strings.Contains(block, "+rules:\n+  - command: touch\n") {
		t.Fatalf("the block %q does not create the file", block)
	}
	s.waitFor(t, ruleAddedText)
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the approved command to run", got)
	}
	want := "rules:\n  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n"
	if file := readFile(t, h.rulesFile); file != want {
		t.Fatalf("the rules file is\n%s\nwant\n%s", file, want)
	}
}

// A failed reread keeps the daemon's old rules: the request runs once, the
// session says so, and the log warns.
func TestAddRuleWithAFailedRereadRunsOnce(t *testing.T) {
	var log lockedBuffer
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), "rules:\n  - command: echo\n    args: any\n", true)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.addRule(t)
	s.waitFor(t, "rule added, but the daemon kept its old rules: "+errNoReread.Error()+" · request 1 runs once\n")
	got := awaitResult(t, ch)
	if !strings.HasSuffix(got.stderr, "hostrun: approved on the host: this request only\n") || got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	waitForLog(t, &log, `level=WARN msg="rules reread failed: the old rules stay"`)
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got.code != protocol.ExitRejected {
		t.Fatalf("a later run: got %+v, want the old rules to refuse it", got)
	}
}

// A rule added that the daemon's checks after the rules still refuse (here
// a fixed directory that does not exist) lets the request run once: the
// agent is told, and the log records, an approval of this request only.
func TestAddRuleThatStillRefusesRunsOnce(t *testing.T) {
	const edited = "rules:\n  - command: echo\n    args: any\n  - command: touch\n    args: any\n    dir: /nonexistent-hostrunner-dir\n"
	scriptEditor(t, edited)
	var log lockedBuffer
	h := startRulesDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), "rules:\n  - command: echo\n    args: any\n", false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	s.answer(t, "e")
	s.waitFor(t, ruleAddedText+" · request 1 runs once: fixed directory /nonexistent-hostrunner-dir does not exist\n")
	got := awaitResult(t, ch)
	if !strings.HasSuffix(got.stderr, "hostrun: approved on the host: this request only\n") || got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	waitForLog(t, &log, `msg="approval answered" approval_id=1 scope=once`)
	waitForLog(t, &log, "approved=once")
	if strings.Contains(log.String(), "=rule") {
		t.Errorf("the log records the rule scope: %s", log.String())
	}
	if _, err := os.Stat(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Errorf("the request did not run in the mirrored directory: %v", err)
	}
}

// scriptEditor makes $VISUAL an editor that, opened the n-th time, leaves
// the file it is given with edits[n-1].
func scriptEditor(t *testing.T, edits ...string) {
	t.Helper()
	dir := t.TempDir()
	for i, e := range edits {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i+1)), []byte(e), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "n"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "edit")
	body := "#!/bin/sh\nd=" + strconv.Quote(dir) + "\nn=$(cat \"$d/n\")\ncp \"$d/$n\" \"$1\"\necho $((n + 1)) > \"$d/n\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", script)
}

// An invalid edit can be cancelled, back to the answer; nothing is written.
func TestAddRuleCancelledAfterAnInvalidEdit(t *testing.T) {
	scriptEditor(t, "rules: [")
	const content = "rules:\n  - command: echo\n    args: any\n"
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), content, false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	s.answer(t, "e")
	s.waitFor(t, "[e]dit again [c]ancel ? ")
	s.answer(t, "c")
	s.waitFor(t, answerPromptText)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
	if file := readFile(t, h.rulesFile); file != content {
		t.Fatalf("the rules file changed:\n%s", file)
	}
}

// When no edit lets the request through, the session says why and asks
// for the answer again: a rule sharing YAML anchors, an argument a glob
// list cannot match exactly.
func TestAddRuleSaysWhyItCannot(t *testing.T) {
	const loaded = "rules:\n  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n"
	tests := []struct {
		name    string
		rules   string
		changed string // the file on disk, when it differs from the rules loaded
		argv    []string
		why     string
	}{
		{"anchors", "rules:\n  - command: touch\n    flags:\n      allow: &common [-a]\n    positional:\n      allow: [marker]\n  - command: date\n    flags:\n      allow: *common\n    positional:\n      allow: []\n", "", []string{"touch", "-m", "marker"}, `rule "touch" uses YAML anchors or aliases`},
		{"a glob argument", loaded, "", []string{"touch", "-a", "m*"}, `"m*" holds a glob character`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startRulesDaemon(t, slog.New(slog.DiscardHandler), tt.rules, false)
			if tt.changed != "" {
				if err := os.WriteFile(h.rulesFile, []byte(tt.changed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := h.startSession(t)
			ch := h.ask(t, tt.argv...)
			s.waitFor(t, answerPromptText)
			s.answer(t, "a")
			s.waitFor(t, "cannot add a rule: "+tt.why)
			s.waitFor(t, answerPromptText)
			s.answer(t, "o")
			awaitResult(t, ch)
		})
	}
}

// rereadPromptText is what the session asks before the daemon rereads a
// rules file changed by hand since it loaded it.
const rereadPromptText = "the rules file changed since the daemon loaded it; reread it, making that change live now? [r]eread [c]ancel ? "

// handEditedRules starts a daemon serving loaded rules, then changes the
// rules file by hand to changed, and asks for argv with a session
// serving the daemon; it returns once the session asks to reread the file
// for "add a rule".
func handEditedRules(t *testing.T, log *slog.Logger, loaded, changed string, argv ...string) (harness, *session, <-chan result) {
	t.Helper()
	h := startRulesDaemon(t, log, loaded, false)
	if err := os.WriteFile(h.rulesFile, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := h.ask(t, argv...)
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, rereadPromptText)
	return h, s, ch
}

const (
	loadedTouch = "rules:\n  - command: touch\n    flags:\n      allow: [-a]\n    positional:\n      allow: [marker]\n"
	handDate    = loadedTouch + "  - command: date\n    args: any\n"
)

// A rules file changed by hand since the daemon loaded it is reread, once
// the human agrees, before a rule is proposed: the proposal is computed
// against the rules the daemon then serves, and the hand edit goes live.
func TestAddRuleRereadsAFileChangedSinceLoaded(t *testing.T) {
	var log lockedBuffer
	h, s, ch := handEditedRules(t, slog.New(slog.NewTextHandler(&log, nil)), loadedTouch, handDate, "touch", "-m", "marker")
	s.answer(t, "r")
	if block := s.waitFor(t, addPromptText); !strings.Contains(block, "+      allow: [-a, -m]\n") {
		t.Errorf("the proposal %q does not widen the rule", block)
	}
	waitForLog(t, &log, `msg="hand-edited rules reread"`)
	waitForLog(t, &log, `msg="rules reread"`)
	s.answer(t, "w")
	s.waitFor(t, ruleAddedText+" · request 1 runs under it\n")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it to run under the rule", got)
	}
	if file := readFile(t, h.rulesFile); !strings.Contains(file, "  - command: date\n") || !strings.Contains(file, "allow: [-a, -m]") {
		t.Fatalf("the rules file lacks the hand edit or the rule:\n%s", file)
	}
	if got := h.run(t, containerRoot, "", "date"); got.code != 0 {
		t.Fatalf("a command of the hand edit: got %+v, want it to run", got)
	}
}

// Cancelling the reread of a rules file changed by hand keeps the hand
// edit out until the next start: no rule is proposed, and the session
// asks for the answer again.
func TestAddRuleRereadCancelledKeepsTheHandEditOut(t *testing.T) {
	h, s, ch := handEditedRules(t, slog.New(slog.DiscardHandler), loadedTouch, handDate, "touch", "-m", "marker")
	s.answer(t, "c")
	s.waitFor(t, "cannot add a rule: the rules file changed since the daemon loaded it; the change takes effect at the next container start\n")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	if got := h.run(t, containerRoot, "", "date"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the hand edit: got %+v, want it refused", got)
	}
}

// A rules file changed by hand that the daemon cannot reread gets no
// rule: the session says why, asks for the answer again, and the old
// rules stay.
func TestAddRuleWithAChangedFileThatDoesNotLoad(t *testing.T) {
	h, s, ch := handEditedRules(t, slog.New(slog.DiscardHandler), loadedTouch, "rules: [", "touch", "-m", "marker")
	s.answer(t, "r")
	s.waitFor(t, "cannot add a rule: the rules file changed since the daemon loaded it, and the daemon cannot reread it: ")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	if got := h.run(t, containerRoot, "", "touch", "-a", "marker"); got.code != 0 {
		t.Fatalf("a command of the old rules: got %+v, want it to run", got)
	}
}

// A request that ends while the session asks to reread the rules file is
// dropped with the question: the file is not reread.
func TestAddRuleRereadQuestionEndsWithItsRequest(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), loadedTouch, false)
	if err := os.WriteFile(h.rulesFile, []byte(handDate), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := h.askFor(t, "2s", "touch", "-m", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, rereadPromptText)
	s.waitFor(t, "request 1 expired\n")
	awaitResult(t, ch)
	s.answer(t, "r")
	if got := h.run(t, containerRoot, "", "date"); got.code != protocol.ExitRejected {
		t.Fatalf("a command of the hand edit: got %+v, want it refused", got)
	}
}

// A rules file that changed between the proposal and the write is not
// overwritten.
func TestAddRuleWritesNothingWhenTheFileChangedSinceTheProposal(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "rules:\n  - command: echo\n    args: any\n", false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	const changed = "rules:\n  - command: echo\n    args: any\n# edited meanwhile\n"
	if err := os.WriteFile(h.rulesFile, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	s.answer(t, "w")
	s.waitFor(t, "nothing written: the rules file changed since the proposal\n")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
	if file := readFile(t, h.rulesFile); file != changed {
		t.Fatalf("the rules file is\n%s\nwant the change kept", file)
	}
}

// Standing approvals stay through a reread of the rules.
func TestAddRuleKeepsStandingApprovals(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "rules:\n  - command: echo\n    args: any\n", false)
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "-a", "one")
	ch := h.ask(t, "touch", "-m", "two")
	s.addRule(t)
	s.waitFor(t, ruleAddedText)
	awaitResult(t, ch)
	if got := h.run(t, containerRoot, "", "touch", "-a", "one"); got != (result{}) {
		t.Fatalf("got %+v, want the standing approval to let it through", got)
	}
}

// A file with CRLF line endings keeps them in the lines added.
func TestAddRuleKeepsCRLFLineEndings(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "rules:\r\n  - command: echo\r\n    args: any\r\n", false)
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.addRule(t)
	s.waitFor(t, ruleAddedText)
	awaitResult(t, ch)
	want := "rules:\r\n  - command: echo\r\n    args: any\r\n  - command: touch\r\n    flags:\r\n      allow: [-a]\r\n    positional:\r\n      allow: [marker]\r\n"
	if file := readFile(t, h.rulesFile); file != want {
		t.Fatalf("the rules file is %q, want %q", file, want)
	}
}

// A rules file that is a symbolic link stays one: the file it points to
// gets the rule.
func TestAddRuleWritesThroughASymbolicLink(t *testing.T) {
	var target string
	h := startRulesDaemonWith(t, slog.New(slog.DiscardHandler), false, func(rulesFile string) {
		target = filepath.Join(filepath.Dir(rulesFile), "rules.yaml")
		if err := os.WriteFile(target, []byte("rules:\n  - command: echo\n    args: any\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("rules.yaml", rulesFile); err != nil {
			t.Fatal(err)
		}
	})
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.addRule(t)
	s.waitFor(t, ruleAddedText)
	awaitResult(t, ch)
	if fi, err := os.Lstat(h.rulesFile); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the rules file is no longer a symbolic link: %v", err)
	}
	if file := readFile(t, target); !strings.Contains(file, "- command: touch") {
		t.Fatalf("the target lacks the rule:\n%s", file)
	}
}

// A rules file swapped for a symbolic link after the proposal, to a file
// outside its directory with the same content, is not written through:
// the file the link points to stays as it was.
func TestAddRuleWritesNothingThroughAFileSwappedForALink(t *testing.T) {
	const content = "rules:\n  - command: echo\n    args: any\n"
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), content, false)
	outside := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(outside, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	if err := os.Remove(h.rulesFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, h.rulesFile); err != nil {
		t.Fatal(err)
	}
	s.answer(t, "w")
	s.waitFor(t, "nothing written: the rules file was swapped for a link out of its directory\n")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
	if file := readFile(t, outside); file != content {
		t.Fatalf("the file the link points to is\n%s\nwant it untouched", file)
	}
}

// A rules file's directory swapped for a symbolic link after the
// proposal, to a directory holding a file of the same content, is not
// written through: the rule goes to the directory the proposal read.
func TestAddRuleWritesNothingThroughADirectorySwappedForALink(t *testing.T) {
	const content = "rules:\n  - command: echo\n    args: any\n"
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), content, false)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, filepath.Base(h.rulesFile)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	dir := filepath.Dir(h.rulesFile)
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	s.answer(t, "w")
	s.waitFor(t, "rule added, but the daemon kept its old rules: the rules file changed meanwhile · request 1 runs once\n")
	if got := awaitResult(t, ch); !strings.HasSuffix(got.stderr, "hostrun: approved on the host: this request only\n") || got.code != 0 {
		t.Fatalf("got %+v, want it approved once", got)
	}
	if file := readFile(t, filepath.Join(other, filepath.Base(h.rulesFile))); file != content {
		t.Fatalf("the file in the directory linked to is\n%s\nwant it untouched", file)
	}
	if file := readFile(t, filepath.Join(dir+".real", filepath.Base(h.rulesFile))); !strings.Contains(file, "- command: touch") {
		t.Fatalf("the directory the proposal read lacks the rule:\n%s", file)
	}
}

// A rules file swapped after the proposal for a link to another file in
// its directory is replaced by a regular file with the mode of the file
// linked to; that file stays as it was.
func TestAddRuleKeepsTheModeThroughALinkSwappedInItsDirectory(t *testing.T) {
	const content = "rules:\n  - command: echo\n    args: any\n"
	var sibling string
	h := startRulesDaemonWith(t, slog.New(slog.DiscardHandler), false, func(rulesFile string) {
		if err := os.WriteFile(rulesFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		sibling = filepath.Join(filepath.Dir(rulesFile), "copy.yaml")
		if err := os.WriteFile(sibling, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	if err := os.Remove(h.rulesFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("copy.yaml", h.rulesFile); err != nil {
		t.Fatal(err)
	}
	s.answer(t, "w")
	s.waitFor(t, ruleAddedText+" · request 1 runs under it\n")
	awaitResult(t, ch)
	fi, err := os.Lstat(h.rulesFile)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("the rules file has mode %v, want a regular file with mode 0600", fi.Mode())
	}
	if file := readFile(t, sibling); file != content {
		t.Errorf("the file linked to is\n%s\nwant it untouched", file)
	}
}

// A rules file reached through a symbolic link shows, with the proposal,
// the file it writes; one reached directly does not.
func TestAddRuleShowsTheFileItWritesThroughALink(t *testing.T) {
	h := startRulesDaemonWith(t, slog.New(slog.DiscardHandler), false, func(rulesFile string) {
		target := filepath.Join(filepath.Dir(rulesFile), "rules.yaml")
		if err := os.WriteFile(target, []byte("rules:\n  - command: echo\n    args: any\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("rules.yaml", rulesFile); err != nil {
			t.Fatal(err)
		}
	})
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	if block := s.waitFor(t, addPromptText); !strings.Contains(block, "writes .devcontainer/rules.yaml\n") {
		t.Errorf("the block %q does not show the file it writes", block)
	}
	s.answer(t, "c")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)

	direct := startRulesDaemon(t, slog.New(slog.DiscardHandler), "rules:\n  - command: echo\n    args: any\n", false)
	ds := direct.startSession(t)
	dch := direct.ask(t, "touch", "-a", "marker")
	ds.waitFor(t, answerPromptText)
	ds.answer(t, "a")
	if block := ds.waitFor(t, addPromptText); strings.Contains(block, "writes ") {
		t.Errorf("the block %q shows the file it writes, which the diff names", block)
	}
	ds.answer(t, "c")
	ds.waitFor(t, answerPromptText)
	ds.answer(t, "o")
	awaitResult(t, dch)
}

// Without the rules file's directory no rule is proposed: the session says
// why and asks for the answer again.
func TestAddRuleWithoutTheRulesDirectorySaysSo(t *testing.T) {
	h := startRulesDaemon(t, slog.New(slog.DiscardHandler), "", false)
	if err := os.Remove(filepath.Dir(h.rulesFile)); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-a", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, "cannot add a rule: the rules file's directory does not exist\n")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
}
