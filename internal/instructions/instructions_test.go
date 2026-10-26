package instructions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	begin = BeginMarker + "\n"
	end   = EndMarker + "\n"
)

// The block carries its own markers, so the printed text and the written
// one are the same bytes.
func TestBlockStartsAndEndsWithTheMarkers(t *testing.T) {
	b := Block()
	if !strings.HasPrefix(b, begin+"## Host commands (hostrun)\n") {
		t.Fatalf("block does not start with the begin marker and the heading: %q", b[:min(len(b), 80)])
	}
	if !strings.HasSuffix(b, "\n"+end) {
		t.Fatalf("block does not end with the end marker and one newline: %q", b[max(0, len(b)-80):])
	}
	if strings.Count(b, BeginMarker) != 1 || strings.Count(b, EndMarker) != 1 {
		t.Fatal("block holds more than one pair of markers")
	}
}

// A marker counts only as a whole line, so a mention of it in prose is
// text, and a block left open is refused rather than guessed at.
func TestHas(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
		wantErr error
	}{
		{"empty", "", false, nil},
		{"no markers", "# Agents\n\ntext\n", false, nil},
		{"block", "a\n" + Block() + "b\n", true, nil},
		{"block at end without final newline", "a\n" + begin + "x\n" + EndMarker, true, nil},
		{"crlf markers", "a\r\n" + BeginMarker + "\r\nx\r\n" + EndMarker + "\r\n", true, nil},
		{"begin marker inside a line", "see " + BeginMarker + "\n" + end, false, nil},
		{"begin marker with trailing text", BeginMarker + " x\n" + end, false, nil},
		{"begin marker with leading space", " " + begin + end, false, nil},
		{"end marker alone", "a\n" + end, false, nil},
		{"end marker inside a line", begin + "see " + EndMarker + "\n", false, ErrNoEndMarker},
		{"end marker with trailing text", begin + EndMarker + " x\n" + end, true, nil},
		{"begin marker without end marker", "a\n" + begin + "x\n", false, ErrNoEndMarker},
		{"later block without end marker", Block() + "\n" + begin + "x\n", false, ErrNoEndMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Has([]byte(tt.content))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got error %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// Append separates the block from earlier text by exactly one blank line.
func TestAppend(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", Block()},
		{"final newline", "# Agents\n", "# Agents\n\n" + Block()},
		{"no final newline", "# Agents", "# Agents\n\n" + Block()},
		{"crlf final newline", "# Agents\r\n", "# Agents\r\n\n" + Block()},
		{"final blank line", "# Agents\n\n", "# Agents\n\n" + Block()},
		{"crlf final blank line", "# Agents\r\n\r\n", "# Agents\r\n\r\n" + Block()},
		{"next to an existing block", "a\n\n" + Block(), "a\n\n" + Block() + "\n" + Block()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(Append([]byte(tt.content))); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Replace leaves one current block where the first one was and keeps all
// other text.
func TestReplace(t *testing.T) {
	old := begin + "old\n" + end
	crlfOld := BeginMarker + "\r\nold\r\n" + EndMarker + "\r\n"
	tests := []struct {
		name    string
		content string
		want    string
		wantErr error
	}{
		{"block in the middle", "before\n\n" + old + "\nafter\n", "before\n\n" + Block() + "\nafter\n", nil},
		{"block alone", old, Block(), nil},
		{"block without final newline", "a\n" + begin + "old\n" + EndMarker, "a\n" + Block(), nil},
		{"crlf block", "a\r\n" + BeginMarker + "\r\nold\r\n" + EndMarker + "\r\nb\r\n", "a\r\n" + Block() + "b\r\n", nil},
		{"several blocks become one", "a\n\n" + old + "\n" + old + "b\n" + old, "a\n\n" + Block() + "b\n", nil},
		{"crlf later block with blank line before it", "a\r\n\r\n" + crlfOld + "\r\n" + crlfOld + "b\r\n", "a\r\n\r\n" + Block() + "b\r\n", nil},
		{"later block without blank line before it", old + old + "b\n", Block() + "b\n", nil},
		{"begin marker inside the block is replaced", begin + begin + "old\n" + end + "b\n", Block() + "b\n", nil},
		{"no block", "a\n" + end, "a\n" + end, nil},
		{"begin marker without end marker", "a\n" + begin + "old\n", "", ErrNoEndMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Replace([]byte(tt.content))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got error %v, want %v", err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// The result of Replace never shares memory with its input, so a caller
// may change one without the other.
func TestReplaceWithoutBlockReturnsACopy(t *testing.T) {
	content := []byte("a\n")
	got, err := Replace(content)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 'b'
	if string(content) != "a\n" {
		t.Fatalf("changing the result changed the input to %q", content)
	}
}

// This repository's AGENTS.md holds the block as hostrunner writes it, so
// the instructions agents here read are the ones hostrunner hands out.
func TestAgentsMDHoldsTheBlock(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	spans, err := blocks(content)
	if err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("AGENTS.md holds %d blocks, want 1", len(spans))
	}
	got := strings.SplitAfter(string(content[spans[0].start:spans[0].end]), "\n")
	want := strings.SplitAfter(Block(), "\n")
	for i := range max(len(got), len(want)) {
		gotLine, wantLine := lineAt(got, i), lineAt(want, i)
		if gotLine != wantLine {
			t.Fatalf("the block in AGENTS.md differs from internal/instructions/instructions.md at line %d of the block:\n got: %q\nwant: %q\n"+
				"edit instructions.md, then from the repository root run `go run ./cmd/hostrunner instructions --append` in a terminal and answer r (or copy instructions.md over the block)",
				i+1, gotLine, wantLine)
		}
	}
}

// lineAt is lines[i], or "" past the end of lines.
func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
