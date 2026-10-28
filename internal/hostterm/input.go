package hostterm

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

// Input is the human's side of a terminal: the lines typed on in, read
// (see ReadLines) only once the lines of the last read are handled, and
// the prompt they answer. A line typed before the current
// prompt does not answer it, so a key meant for something else never
// does.
type Input struct {
	in    io.Reader
	lines <-chan Line
	read  atomic.Uint64 // lines read from in so far
	// fresh is the number of the first line typed after the current
	// prompt.
	fresh uint64
	want  chan struct{} // lets ReadLines read on
	done  chan struct{} // closed by Close
}

// NewInput starts reading the lines typed on in; Close stops it.
func NewInput(in io.Reader) *Input {
	i := &Input{in: in, want: make(chan struct{}, 1), done: make(chan struct{})}
	i.want <- struct{}{}
	i.lines = ReadLines(in, &i.read, i.want, i.done)
	return i
}

// Lines delivers the lines typed, each to be passed to Take or Handled;
// it is closed once in ends or Close is called.
func (i *Input) Lines() <-chan Line { return i.lines }

// Prompt prints p to out and discards what the human typed before it.
// The lines counted before p is printed are the ones discarded, so an
// answer typed once p shows always counts.
func (i *Input) Prompt(out io.Writer, p string) {
	DiscardTypedAhead(i.in)
	i.fresh = i.read.Load() + 1
	fmt.Fprint(out, p)
}

// Fresh reports whether l was typed after the current prompt.
func (i *Input) Fresh(l Line) bool { return l.N >= i.fresh }

// Handled lets the reading go on once the lines of its last read are
// handled, l being one of them.
func (i *Input) Handled(l Line) {
	if l.Last {
		i.want <- struct{}{}
	}
}

// Take handles l and returns its text, trimmed, and whether it was typed
// after the current prompt, which it answers.
func (i *Input) Take(l Line) (string, bool) {
	i.Handled(l)
	return strings.TrimSpace(l.Text), i.Fresh(l)
}

// Close stops reading in (see StopReading).
func (i *Input) Close() { StopReading(i.in, i.done, i.lines) }

// IsQuit reports whether text answers a question by quitting.
func IsQuit(text string) bool { return text == "q" || text == "quit" }
