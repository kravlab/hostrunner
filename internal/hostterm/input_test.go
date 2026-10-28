package hostterm_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/hostterm"
)

// next returns the next line in reads, failing the test after a second.
func next(t *testing.T, in *hostterm.Input) hostterm.Line {
	t.Helper()
	select {
	case l, ok := <-in.Lines():
		if !ok {
			t.Fatal("the lines ended")
		}
		return l
	case <-time.After(time.Second):
		t.Fatal("no line")
		return hostterm.Line{}
	}
}

// A line typed before a prompt does not answer it; one typed after does,
// trimmed.
func TestInputTakesOnlyLinesTypedAfterThePrompt(t *testing.T) {
	r, w := io.Pipe()
	in := hostterm.NewInput(r)
	defer in.Close()
	go func() { _, _ = io.WriteString(w, "early\n") }()
	early := next(t, in)
	var out strings.Builder
	in.Prompt(&out, "answer? ")
	if out.String() != "answer? " {
		t.Fatalf("printed %q", out.String())
	}
	if text, fresh := in.Take(early); fresh || text != "early" {
		t.Fatalf("early line: %q fresh %v, want not fresh", text, fresh)
	}
	go func() { _, _ = io.WriteString(w, "  k \n") }()
	if text, fresh := in.Take(next(t, in)); !fresh || text != "k" {
		t.Fatalf("got %q fresh %v, want \"k\" fresh", text, fresh)
	}
}

// Close ends the lines once the input does.
func TestInputCloseEndsTheLines(t *testing.T) {
	in := hostterm.NewInput(strings.NewReader(""))
	in.Close()
	select {
	case _, ok := <-in.Lines():
		if ok {
			t.Fatal("a line after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("the lines did not end")
	}
}

func TestIsQuit(t *testing.T) {
	for text, want := range map[string]bool{"q": true, "quit": true, "Q": false, "k": false, "": false} {
		if got := hostterm.IsQuit(text); got != want {
			t.Errorf("IsQuit(%q) = %v, want %v", text, got, want)
		}
	}
}
