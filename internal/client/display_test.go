package client_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/daemon"
)

// An argument holding non-ASCII is shown as it is, readable, and marked
// with its ASCII form, so a look-alike letter in a flag stands out.
func TestAskSessionMarksNonASCIIArguments(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "-а", "привет")
	block := s.waitFor(t, answerPromptText)
	for _, want := range []string{
		`argv  "touch" "-а" "привет"`,
		`⚠ non-ASCII: argument 2 "-а" is "-\u0430"`,
		`⚠ non-ASCII: argument 3 "привет" is "\u043f\u0440\u0438\u0432\u0435\u0442"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block %q lacks %q", block, want)
		}
	}
	if strings.Contains(block, `argument 1`) {
		t.Errorf("an ASCII argument is marked: %q", block)
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
}

// A long argument and a long argv are cut, and marked so; `?` shows every
// argument whole, one numbered per line, and asks again, and the request
// can still be answered.
func TestAskSessionCutsLongArgvAndShowsItAll(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	long := strings.Repeat("x", 250)
	argv := []string{"touch", long}
	for i := range 23 {
		argv = append(argv, "f"+strings.Repeat("y", i%3)+string(rune('a'+i)))
	}
	ch := h.ask(t, argv...)
	block := s.waitFor(t, answerPromptText)
	for _, want := range []string{
		`"` + strings.Repeat("x", 200) + `"…(+50 bytes)`,
		"…(+5 args)\n",
		"  argv truncated: ? shows all\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block %q lacks %q", block, want)
		}
	}
	if strings.Contains(block, argv[24]) {
		t.Errorf("the block shows an argument past the 20th: %q", block)
	}
	s.answer(t, "?")
	all := s.waitFor(t, answerPromptText)
	for i, arg := range argv {
		if want := "  " + strconv.Itoa(i+1) + "  \"" + arg + "\"\n"; !strings.Contains(all, want) {
			t.Errorf("? lacks %q", want)
		}
	}
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the truncated request approved", got)
	}
}

// A note with control characters reaches the agent escaped, never raw.
func TestAskNoteReachesTheAgentEscaped(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "use \x1b[2Jecho")
	got := awaitResult(t, ch)
	if strings.Contains(got.stderr, "\x1b") || !strings.Contains(got.stderr, `refused on the host: "use \x1b[2Jecho"`) {
		t.Fatalf("got %q, want the note escaped", got.stderr)
	}
}

// A long argument holding non-ASCII is cut in its ASCII form too, so a
// megabyte argument never floods the block.
func TestAskSessionCutsTheASCIIFormOfALongArgument(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", strings.Repeat("я", 250))
	block := s.waitFor(t, answerPromptText)
	shown := `"` + strings.Repeat("я", 200) + `"…(+100 bytes)`
	ascii := `"` + strings.Repeat("\x5cu044f", 200) + `"…(+100 bytes)`
	for _, want := range []string{
		`argv  "touch" ` + shown + "\n",
		"⚠ non-ASCII: argument 2 " + shown + " is " + ascii + "\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block %q lacks %q", block, want)
		}
	}
	if strings.Contains(block, strings.Repeat("\x5cu044f", 201)) {
		t.Errorf("the block shows the ASCII form past 200 characters: %q", block)
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
}

// The block cuts an argument past 200 characters and an argv past 20
// arguments, and no sooner; a cut never splits a character.
func TestAskSessionCutsAtTheBoundaries(t *testing.T) {
	args := func(n int) []string {
		argv := []string{"touch"}
		for i := 1; i < n; i++ {
			argv = append(argv, "f"+strconv.Itoa(i))
		}
		return argv
	}
	x200 := strings.Repeat("x", 200)
	tests := []struct {
		name      string
		argv      []string
		want      string
		truncated bool
	}{
		{"200 characters whole", []string{"touch", x200}, `"` + x200 + `"` + "\n", false},
		{"201 characters cut", []string{"touch", x200 + "y"}, `"` + x200 + `"…(+1 bytes)` + "\n", true},
		{"cut after a two-byte character", []string{"touch", x200[1:] + "я" + strings.Repeat("z", 10)}, `"` + x200[1:] + `я"…(+10 bytes)` + "\n", true},
		{"20 arguments whole", args(20), ` "f19"` + "\n", false},
		{"21 arguments cut", args(21), ` "f19" …(+1 args)` + "\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, echoOnly))
			s := h.startSession(t)
			ch := h.ask(t, tt.argv...)
			block := s.waitFor(t, answerPromptText)
			if !strings.Contains(block, tt.want) {
				t.Errorf("the block %q lacks %q", block, tt.want)
			}
			if got := strings.Contains(block, "  argv truncated: ? shows all\n"); got != tt.truncated {
				t.Errorf("argv truncated shown: %v, want %v, in %q", got, tt.truncated, block)
			}
			s.answer(t, "o")
			awaitResult(t, ch)
		})
	}
}

// `?` shows all of a guarded request too, and asks the guarded answer
// again.
func TestAskSessionShowsAllOfAGuardedRequest(t *testing.T) {
	h := startDaemon(t, policy(t, guardedRules))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "+marker")
	s.waitFor(t, guardedPromptText)
	s.answer(t, "?")
	all := s.waitFor(t, guardedPromptText)
	for _, want := range []string{"  1  \"touch\"\n", "  2  \"+marker\"\n"} {
		if !strings.Contains(all, want) {
			t.Errorf("? lacks %q in %q", want, all)
		}
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
}

// Reasons and guards naming an argument with a terminal escape show it
// escaped: nothing from the container steers the terminal.
func TestAskSessionEscapesReasonsAndGuards(t *testing.T) {
	h := startDaemon(t, policy(t, guardedRules))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "+\x1b[2J")
	block := s.waitFor(t, guardedPromptText)
	if strings.Contains(block, "\x1b") {
		t.Errorf("the block holds a raw escape: %q", block)
	}
	for _, want := range []string{
		"why   denied by rule \"touch\": argument \"+\x5cx1b[2J\" is denied (deny list)\n",
		"⚠ guarded: explicitly denied: argument \"+\x5cx1b[2J\" is denied\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block %q lacks %q", block, want)
		}
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
}

// A bidi control or a zero-width character in an argument shows escaped,
// so an argument cannot reorder or hide what the block shows.
func TestAskSessionEscapesInvisibleCharacters(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "a\xe2\x80\xaeb\xe2\x80\x8bc")
	block := s.waitFor(t, answerPromptText)
	if strings.Contains(block, "\xe2\x80\xae") || strings.Contains(block, "\xe2\x80\x8b") {
		t.Errorf("the block holds a raw invisible character: %q", block)
	}
	escaped := "\"a\x5cu202eb\x5cu200bc\""
	for _, want := range []string{
		`argv  "touch" ` + escaped + "\n",
		"⚠ non-ASCII: argument 2 " + escaped + " is " + escaped + "\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block %q lacks %q", block, want)
		}
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
}

// The list of standing approvals shows an argv as a request's block does:
// cut, and an argument holding non-ASCII marked.
func TestStandingListCutsAndMarksArgv(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	argv := []string{"touch", strings.Repeat("я", 250)}
	for i := 1; i <= 20; i++ {
		argv = append(argv, "f"+strconv.Itoa(i))
	}
	h.grantStanding(t, s, false, argv...)
	s.answer(t, "standing")
	shown := `"` + strings.Repeat("я", 200) + `"…(+100 bytes)`
	ascii := `"` + strings.Repeat("\x5cu044f", 200) + `"…(+100 bytes)`
	mark := "⚠ non-ASCII: argument 2 " + shown + " is " + ascii + "\n"
	list := s.waitFor(t, mark)
	if want := `  1  "touch" ` + shown + ` "f1" `; !strings.Contains(list, want) {
		t.Errorf("the list %q lacks %q", list, want)
	}
	if want := ` "f18" …(+2 args)   in ` + containerRoot + "\n"; !strings.Contains(list, want) {
		t.Errorf("the list %q lacks %q", list, want)
	}
}

// The serving line shows a host workspace holding a control character
// escaped, as the list of daemons does.
func TestSessionEscapesTheServedWorkspace(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(tmp, "w\x1bx")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	h := startHarnessAt(t, slog.New(slog.DiscardHandler), base, func(string) (daemon.Policy, []daemon.Option) {
		return policy(t, echoOnly), nil
	})
	s := h.startSession(t)
	out := s.out.String()
	if want := "serving " + strconv.Quote(h.hostRoot) + " (container " + containerRoot + ")\n"; !strings.Contains(out, want) {
		t.Errorf("the session printed %q, lacking %q", out, want)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("the session printed a raw escape: %q", out)
	}
}
