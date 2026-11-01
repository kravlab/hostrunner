package hostterm_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
)

func TestDisplay(t *testing.T) {
	tests := []struct{ text, want string }{
		{"/home/u/app", "/home/u/app"},
		{"über", "über"},
		{"a\x1b[2Jb", `"a\x1b[2Jb"`},
		{"line\nbreak", `"line\nbreak"`},
		{"\xff", `"\xff"`},
	}
	for _, tt := range tests {
		if got := hostterm.Display(tt.text); got != tt.want {
			t.Errorf("Display(%q) = %s, want %s", tt.text, got, tt.want)
		}
	}
}

func TestBashQuote(t *testing.T) {
	tests := []struct{ arg, want string }{
		{"", "''"},
		{"make", "make"},
		{"a b", "'a b'"},
		{"it's", `$'it\'s'`},
		{"a\tb", `$'a\tb'`},
		{"\x1b", `$'\x1b'`},
		{"é", "'é'"},
		{"e\u0301", `$'e\u0301'`},
	}
	for _, tt := range tests {
		if got := hostterm.BashQuote(tt.arg); got != tt.want {
			t.Errorf("BashQuote(%q) = %s, want %s", tt.arg, got, tt.want)
		}
	}
}

func TestASCIIQuote(t *testing.T) {
	if got, want := hostterm.ASCIIQuote("\u0440\u043c"), `$'\u0440\u043c'`; got != want {
		t.Errorf("ASCIIQuote = %s, want %s", got, want)
	}
	if got, want := hostterm.ASCIIQuote("\U0001F600"), `$'\U0001f600'`; got != want {
		t.Errorf("ASCIIQuote = %s, want %s", got, want)
	}
}

func TestHoldsNonASCII(t *testing.T) {
	if hostterm.HoldsNonASCII("rm -rf") {
		t.Error("ASCII text holds non-ASCII")
	}
	if !hostterm.HoldsNonASCII("rм") {
		t.Error("a Cyrillic letter is not non-ASCII")
	}
}

func TestArgvLineCountsWhatItLeavesOut(t *testing.T) {
	argv := make([]string, hostterm.MaxShownArgs+3)
	for i := range argv {
		argv[i] = fmt.Sprint(i)
	}
	argv[0] = "echo hi"
	got := hostterm.ArgvLine(argv)
	if !strings.HasPrefix(got, "'echo hi' 1 2 ") || !strings.HasSuffix(got, " 19 …(+3 args)") {
		t.Errorf("ArgvLine = %q", got)
	}
	if n := len(hostterm.ShownArgs(argv)); n != hostterm.MaxShownArgs {
		t.Errorf("ShownArgs shows %d arguments, want %d", n, hostterm.MaxShownArgs)
	}
	long := strings.Repeat("x", 1000)
	if !hostterm.IsLong(long) || hostterm.IsLong("x") {
		t.Error("IsLong does not tell a long argument")
	}
	if got := hostterm.ArgvLine([]string{long}); len([]rune(got)) >= 1000 {
		t.Errorf("ArgvLine does not cut a long argument: %d runes", len([]rune(got)))
	}
}

func TestReadLinesCountsLinesAsTheyAreRead(t *testing.T) {
	var read atomic.Uint64
	want := make(chan struct{}, 1)
	done := make(chan struct{})
	lines := hostterm.ReadLines(strings.NewReader("k\nw\nq"), &read, want, done)
	want <- struct{}{}
	var got []hostterm.Line
	for l := range lines {
		got = append(got, l)
		if l.Last {
			want <- struct{}{}
		}
	}
	// A last line without a newline is not a line.
	if len(got) != 2 || got[0] != (hostterm.Line{N: 1, Text: "k"}) || got[1] != (hostterm.Line{N: 2, Text: "w", Last: true}) {
		t.Fatalf("got %+v", got)
	}
	if read.Load() != 2 {
		t.Fatalf("read %d lines, want 2", read.Load())
	}
}

func TestStopReadingCutsAReadShort(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	var read atomic.Uint64
	want := make(chan struct{}, 1)
	want <- struct{}{}
	done := make(chan struct{})
	lines := hostterm.ReadLines(r, &read, want, done)
	stopped := make(chan struct{})
	go func() {
		hostterm.StopReading(r, done, lines)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopReading did not cut the read short")
	}
	// The deadline is lifted: the pipe reads again.
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("the pipe does not read after StopReading: %v", err)
	}
}

func TestDiscardTypedAheadLeavesAPipeAlone(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := w.Write([]byte("k\n")); err != nil {
		t.Fatal(err)
	}
	hostterm.DiscardTypedAhead(r)
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "k\n" {
		t.Fatalf("read %q, %v after DiscardTypedAhead on a pipe", buf, err)
	}
}

func daemons() []daemonlist.Daemon {
	return []daemonlist.Daemon{
		{Socket: "/run/u/hostrunner/a.approval.sock", Info: protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: "/home/u/a", ContainerWorkspace: "/workspaces/a", Served: true}},
		{Socket: "/run/u/hostrunner/old.approval.sock", Info: protocol.DaemonInfo{Version: protocol.ApprovalVersion - 1, Workspace: "/home/u/old", ContainerWorkspace: "/workspaces/old"}},
		{Socket: "/run/u/hostrunner/b.approval.sock", Info: protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: "/home/u/b", ContainerWorkspace: "/w/b"}},
	}
}

func TestListPrintsTheDaemonsNumbered(t *testing.T) {
	var out bytes.Buffer
	prompt := hostterm.List{Daemons: daemons(), Cwd: "/home/u/b/src"}.Print(&out)
	want := "daemons:\n" +
		"  1   /home/u/a     (container /workspaces/a)     served by another session\n" +
		"  2   /home/u/old   another version: restart its container\n" +
		"  3 * /home/u/b     (container /w/b)              ← current directory\n"
	if out.String() != want {
		t.Errorf("list:\n%s\nwant:\n%s", out.String(), want)
	}
	if prompt != "choose [1-3, Enter = 3, q quit] ? " {
		t.Errorf("prompt %q", prompt)
	}
}

func TestListOfAnyVersionLetsAnOlderDaemonBeTheDefault(t *testing.T) {
	l := hostterm.List{Daemons: daemons(), Cwd: "/home/u/old", AnyVersion: true}
	if l.Default() != 1 {
		t.Fatalf("Default = %d, want 1", l.Default())
	}
	var out bytes.Buffer
	l.Print(&out)
	if !strings.Contains(out.String(), "  2 * /home/u/old   (container /workspaces/old)   ← current directory\n") {
		t.Errorf("list:\n%s", out.String())
	}
	if (hostterm.List{Daemons: daemons(), Cwd: "/home/u/old"}).Default() != -1 {
		t.Error("an older daemon is the default without AnyVersion")
	}
}

func TestListPick(t *testing.T) {
	l := hostterm.List{Daemons: daemons(), Cwd: "/home/u/a"}
	tests := []struct {
		text string
		want int
		ok   bool
	}{
		{"", 0, true},
		{"3", 2, true},
		{"0", 0, false},
		{"4", 0, false},
		{"x", 0, false},
	}
	for _, tt := range tests {
		if got, ok := l.Pick(tt.text); got != tt.want || ok != tt.ok {
			t.Errorf("Pick(%q) = %d, %v, want %d, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := (hostterm.List{Daemons: daemons(), Cwd: "/srv"}).Pick(""); ok {
		t.Error("Enter picks a daemon without a default")
	}
}

func TestListHideServedLeavesOutTheServingSession(t *testing.T) {
	var out bytes.Buffer
	hostterm.List{Daemons: daemons(), HideServed: true}.Print(&out)
	if strings.Contains(out.String(), "served by another session") {
		t.Errorf("list:\n%s", out.String())
	}
}

// The warning names both files, shows an unsafe path quoted, and is
// silent when nothing is ignored.
func TestWarnIgnoredRules(t *testing.T) {
	for _, tt := range []struct {
		name          string
		ignored, used string
		want          string
	}{
		{"nothing ignored", "", "/home/u/app/.devcontainer/hostrunner-rules.yaml", ""},
		{
			"an older file", "/home/u/app/.devcontainer/hostrun.yaml", "/home/u/app/.devcontainer/hostrunner-rules.yaml",
			"hostrunner: warning: /home/u/app/.devcontainer/hostrun.yaml is ignored: /home/u/app/.devcontainer/hostrunner-rules.yaml is used; remove it\n",
		},
		{
			"a path with an escape", "/w/a\x1b[2J", "/w/b",
			`hostrunner: warning: "/w/a\x1b[2J" is ignored: /w/b is used; remove it` + "\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			hostterm.WarnIgnoredRules(&buf, tt.ignored, tt.used)
			if buf.String() != tt.want {
				t.Errorf("got %q, want %q", buf.String(), tt.want)
			}
		})
	}
}
