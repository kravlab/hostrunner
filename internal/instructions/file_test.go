package instructions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	createQuestion = " does not exist; create it? [y/N] "
	chooseQuestion = " already has hostrunner instructions: [r]eplace, [a]ppend, [c]ancel? "
)

// writeFile creates a file named name in a new temporary directory and
// returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readFile returns the content of path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// terminal is a Prompt answering with answers, as a human at a terminal.
func terminal(answers string, out *strings.Builder) Prompt {
	return Prompt{In: strings.NewReader(answers), Out: out, Terminal: true}
}

// A missing file is created only on a yes, with the block alone and mode
// 0644 before umask.
func TestAppendFileCreates(t *testing.T) {
	old := unix.Umask(0)
	t.Cleanup(func() { unix.Umask(old) })
	tests := []struct {
		name    string
		answers string
		want    Outcome
	}{
		{"y", "y\n", Created},
		{"yes in upper case", "YES\n", Created},
		{"yes without newline", "yes", Created},
		{"yes with crlf", "yes\r\n", Created},
		{"y with spaces and crlf", " Y \r\n", Created},
		{"n", "n\n", Unchanged},
		{"empty line", "\n", Unchanged},
		{"end of input", "", Unchanged},
		{"other answer", "sure\n", Unchanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "AGENTS.md")
			var out strings.Builder
			got, err := AppendFile(path, terminal(tt.answers, &out))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got outcome %v, want %v", got, tt.want)
			}
			if out.String() != path+createQuestion {
				t.Fatalf("asked %q, want %q", out.String(), path+createQuestion)
			}
			info, err := os.Stat(path)
			if tt.want == Unchanged {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("file exists after a no (stat: %v)", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("got mode %v, want 0644", info.Mode().Perm())
			}
			if got := readFile(t, path); got != Block() {
				t.Fatalf("got %q, want the block", got)
			}
		})
	}
}

// A file without a block gets one without a question, from a terminal or
// not.
func TestAppendFileAppendsWithoutQuestion(t *testing.T) {
	for _, isTerminal := range []bool{true, false} {
		path := writeFile(t, "AGENTS.md", "# Agents\n")
		var out strings.Builder
		got, err := AppendFile(path, Prompt{In: strings.NewReader(""), Out: &out, Terminal: isTerminal})
		if err != nil {
			t.Fatal(err)
		}
		if got != Appended {
			t.Fatalf("terminal %v: got outcome %v, want Appended", isTerminal, got)
		}
		if out.Len() != 0 {
			t.Fatalf("terminal %v: asked %q", isTerminal, out.String())
		}
		if got, want := readFile(t, path), string(Append([]byte("# Agents\n"))); got != want {
			t.Fatalf("terminal %v: got %q, want %q", isTerminal, got, want)
		}
	}
}

// A file with a block asks what to do and does exactly that.
func TestAppendFileWithBlockAsks(t *testing.T) {
	content := "# Agents\n\n" + begin + "old\n" + end
	replaced, _ := Replace([]byte(content))
	tests := []struct {
		name      string
		answers   string
		want      Outcome
		wantFile  string
		questions int
	}{
		{"r", "r\n", Replaced, string(replaced), 1},
		{"replace in upper case", "REPLACE\n", Replaced, string(replaced), 1},
		{"r with spaces and crlf", " r \r\n", Replaced, string(replaced), 1},
		{"a", "a\n", Appended, string(Append([]byte(content))), 1},
		{"append", "append\n", Appended, string(Append([]byte(content))), 1},
		{"c", "c\n", Unchanged, content, 1},
		{"cancel", "cancel\n", Unchanged, content, 1},
		{"empty line", "\n", Unchanged, content, 1},
		{"end of input", "", Unchanged, content, 1},
		{"unknown answer asks again", "x\nr\n", Replaced, string(replaced), 2},
		{"unknown answer then end of input", "x\n", Unchanged, content, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, "AGENTS.md", content)
			var out strings.Builder
			got, err := AppendFile(path, terminal(tt.answers, &out))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got outcome %v, want %v", got, tt.want)
			}
			if want := strings.Repeat(path+chooseQuestion, tt.questions); out.String() != want {
				t.Fatalf("asked %q, want %q", out.String(), want)
			}
			if got := readFile(t, path); got != tt.wantFile {
				t.Fatalf("got %q, want %q", got, tt.wantFile)
			}
		})
	}
}

// Without a terminal nobody can answer, so a question is refused instead
// of asked, and nothing is written.
func TestAppendFileRefusesQuestionsWithoutTerminal(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "AGENTS.md")
		var out strings.Builder
		_, err := AppendFile(path, Prompt{In: strings.NewReader("y\n"), Out: &out})
		if want := path + " does not exist; run in a terminal to create it"; err == nil || err.Error() != want {
			t.Fatalf("got error %v, want %q", err, want)
		}
		if out.Len() != 0 {
			t.Fatalf("asked %q", out.String())
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("file created (stat: %v)", err)
		}
	})
	t.Run("file with block", func(t *testing.T) {
		content := Block()
		path := writeFile(t, "AGENTS.md", content)
		var out strings.Builder
		_, err := AppendFile(path, Prompt{In: strings.NewReader("r\n"), Out: &out})
		if want := path + " already has hostrunner instructions; run in a terminal to choose"; err == nil || err.Error() != want {
			t.Fatalf("got error %v, want %q", err, want)
		}
		if out.Len() != 0 {
			t.Fatalf("asked %q", out.String())
		}
		if got := readFile(t, path); got != content {
			t.Fatalf("file changed to %q", got)
		}
	})
}

// A begin marker without an end marker is refused before any question.
func TestAppendFileRefusesOpenBlock(t *testing.T) {
	content := "a\n" + begin + "old\n"
	path := writeFile(t, "AGENTS.md", content)
	var out strings.Builder
	_, err := AppendFile(path, terminal("r\n", &out))
	if want := path + ": " + ErrNoEndMarker.Error(); err == nil || err.Error() != want || !errors.Is(err, ErrNoEndMarker) {
		t.Fatalf("got error %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Fatalf("asked %q", out.String())
	}
	if got := readFile(t, path); got != content {
		t.Fatalf("file changed to %q", got)
	}
}

// The file is written in place: a symlink is followed and stays a
// symlink, and the file keeps its mode.
func TestAppendFileWritesInPlace(t *testing.T) {
	content := "x\n" + begin + "old\n" + end
	replaced, _ := Replace([]byte(content))
	tests := []struct {
		answer string
		want   string
	}{
		{"r\n", string(replaced)},
		{"a\n", string(Append([]byte(content)))},
	}
	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.answer), func(t *testing.T) {
			target := writeFile(t, "real.md", content)
			if err := os.Chmod(target, 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "AGENTS.md")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if _, err := AppendFile(link, terminal(tt.answer, &out)); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&fs.ModeSymlink == 0 {
				t.Fatal("the link was replaced")
			}
			info, err = os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("got mode %v, want 0600", info.Mode().Perm())
			}
			if got := readFile(t, target); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// editDuringQuestion is an answer source that sets the file at path to
// edited when the question is answered, as an agent editing the file
// while the human thinks would.
type editDuringQuestion struct {
	t       *testing.T
	path    string
	edited  string
	answers *strings.Reader
}

// Read writes the edit, then reads the answers.
func (e *editDuringQuestion) Read(p []byte) (int, error) {
	if err := os.WriteFile(e.path, []byte(e.edited), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return e.answers.Read(p)
}

// An answer applies to the file as it is when the human answers, so an
// edit made while the question was open is kept.
func TestAppendFileReadsAgainAfterAnswer(t *testing.T) {
	content := "# Agents\n\n" + begin + "old\n" + end
	edited := content + "added meanwhile"
	replacedEdited, _ := Replace([]byte(edited))
	tests := []struct {
		name     string
		answer   string
		edited   string
		want     Outcome
		wantFile string
		wantErr  error
	}{
		{"replace keeps the edit", "r\n", edited, Replaced, string(replacedEdited), nil},
		{"append keeps the edit", "a\n", edited, Appended, string(Append([]byte(edited))), nil},
		{"replace after the block was removed appends", "r\n", "# Agents\n", Appended, string(Append([]byte("# Agents\n"))), nil},
		{"open block left meanwhile", "r\n", "# Agents\n" + begin, 0, "# Agents\n" + begin, ErrNoEndMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, "AGENTS.md", content)
			var out strings.Builder
			in := &editDuringQuestion{t: t, path: path, edited: tt.edited, answers: strings.NewReader(tt.answer)}
			got, err := AppendFile(path, Prompt{In: in, Out: &out, Terminal: true})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got error %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got outcome %v, want %v", got, tt.want)
			}
			if got := readFile(t, path); got != tt.wantFile {
				t.Fatalf("got %q, want %q", got, tt.wantFile)
			}
		})
	}
}

// A symlink to a missing file cannot be created through, so it is refused
// before any question, from a terminal or not.
func TestAppendFileRefusesDanglingSymlink(t *testing.T) {
	for _, isTerminal := range []bool{true, false} {
		dir := t.TempDir()
		target := filepath.Join(dir, "missing.md")
		link := filepath.Join(dir, "AGENTS.md")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		got, err := AppendFile(link, Prompt{In: strings.NewReader("y\n"), Out: &out, Terminal: isTerminal})
		if want := link + " is a symlink to a missing file"; err == nil || err.Error() != want {
			t.Fatalf("terminal %v: got error %v, want %q", isTerminal, err, want)
		}
		if got != 0 {
			t.Fatalf("terminal %v: got outcome %v with the error", isTerminal, got)
		}
		if out.Len() != 0 {
			t.Fatalf("terminal %v: asked %q", isTerminal, out.String())
		}
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("terminal %v: target created (lstat: %v)", isTerminal, err)
		}
	}
}

// A path that cannot hold the file fails with the OS error before any
// question, from a terminal or not, and creates nothing.
func TestAppendFileFailsOnUnusablePath(t *testing.T) {
	for _, isTerminal := range []bool{true, false} {
		t.Run(fmt.Sprintf("directory, terminal %v", isTerminal), func(t *testing.T) {
			var out strings.Builder
			got, err := AppendFile(t.TempDir(), Prompt{In: strings.NewReader("y\n"), Out: &out, Terminal: isTerminal})
			if !errors.Is(err, syscall.EISDIR) {
				t.Fatalf("got error %v, want EISDIR", err)
			}
			if got != 0 {
				t.Fatalf("got outcome %v with the error", got)
			}
			if out.Len() != 0 {
				t.Fatalf("asked %q", out.String())
			}
		})
		t.Run(fmt.Sprintf("missing parent directory, terminal %v", isTerminal), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing", "AGENTS.md")
			var out strings.Builder
			got, err := AppendFile(path, Prompt{In: strings.NewReader("y\n"), Out: &out, Terminal: isTerminal})
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("got error %v, want one wrapping fs.ErrNotExist", err)
			}
			if got != 0 {
				t.Fatalf("got outcome %v with the error", got)
			}
			if out.Len() != 0 {
				t.Fatalf("asked %q", out.String())
			}
			if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("parent directory created (stat: %v)", err)
			}
		})
	}
}
