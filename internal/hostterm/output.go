package hostterm

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/textcut"
)

// maxPendingSGR is how long a sequence OutputLine waits for to end as an
// SGR one: a longer one is shown as text.
const maxPendingSGR = 64

// OutputLine is a line of a command's output as it arrives, in chunks,
// held to be shown on the terminal: as the terminal shows it, it keeps
// the visible text that follows the line's last \r, a \r right before the
// newline that ends it excepted, which belongs to the line's ending. SGR
// (colour) sequences are removed as they arrive, so they are not text. Of
// the text it holds the first textcut.MaxChars characters, a character
// being a rune or a byte that is not part of one, and counts the bytes of
// the rest, so a line of gigabytes holds a few hundred bytes. Its zero
// value is an empty line.
type OutputLine struct {
	held  []byte
	chars int // characters in held
	over  int // bytes of text past held
	// part is the start of a rune split across chunks (or by an SGR
	// sequence), esc an escape sequence that may yet end as an SGR one:
	// text that comes after part's.
	part, esc []byte
	// cr is whether the last byte written is a \r, which drops what comes
	// before it unless the newline ending the line comes next (End).
	cr bool
}

// Write adds p, which holds no newline, to the line.
func (l *OutputLine) Write(p []byte) {
	for _, b := range p {
		if l.cr {
			*l = OutputLine{held: l.held[:0], part: l.part[:0], esc: l.esc[:0]}
		}
		if b == '\r' {
			l.cr = true
			continue
		}
		l.filter(b)
	}
}

// filter takes byte b of the line apart from SGR sequences, which it
// drops, and passes on the text (see text).
func (l *OutputLine) filter(b byte) {
	switch {
	case len(l.esc) == 0 && b == 0x1b:
		l.esc = append(l.esc, b)
		return
	case len(l.esc) == 0:
		l.text(b)
		return
	case len(l.esc) == 1 && b == '[', len(l.esc) > 1 && len(l.esc) < maxPendingSGR && (b >= '0' && b <= '9' || b == ';' || b == ':'):
		l.esc = append(l.esc, b)
		return
	case len(l.esc) > 1 && b == 'm':
		l.esc = l.esc[:0]
		return
	}
	for _, e := range l.esc { // not an SGR sequence: text
		l.text(e)
	}
	l.esc = l.esc[:0]
	l.filter(b)
}

// text takes byte b of the line's text, keeping each character once it is
// whole: a rune, or a byte that cannot be part of one.
func (l *OutputLine) text(b byte) {
	l.part = append(l.part, b)
	for len(l.part) > 0 && utf8.FullRune(l.part) {
		_, size := utf8.DecodeRune(l.part)
		l.keep(l.part[:size])
		l.part = l.part[:copy(l.part, l.part[size:])]
	}
}

// keep adds character c to the text held, or counts its bytes once the
// text held is as long as it is shown.
func (l *OutputLine) keep(c []byte) {
	if l.chars == textcut.MaxChars {
		l.over += len(c)
		return
	}
	l.held = append(l.held, c...)
	l.chars++
}

// flush keeps what is pending as text, as the line ends: each byte of a
// rune not complete as a character of its own, then an escape sequence
// not complete.
func (l *OutputLine) flush() {
	for _, b := range l.part {
		l.keep([]byte{b})
	}
	for _, b := range l.esc {
		l.keep([]byte{b})
	}
	l.part, l.esc = l.part[:0], l.esc[:0]
}

// End ends the line at a newline: a \r written last is the line's ending.
func (l *OutputLine) End() {
	l.cr = false
	l.flush()
}

// String returns the line as it is shown: its text, what is pending kept
// as at its end (see flush), with every character that is not printable
// but a tab escaped on its own, with its Go escape, such as \x1b, \a or
// \u200b, and \xHH for a byte not part of a rune, no quotes around the
// line, and cut (see textcut.CutCounting), an escape counting as one
// character and the bytes counted being the text's. A line whose last
// byte written is a \r, not ended, shows as empty.
func (l *OutputLine) String() string {
	if l.cr {
		return ""
	}
	shown := OutputLine{held: slices.Clone(l.held), chars: l.chars, over: l.over, part: slices.Clone(l.part), esc: slices.Clone(l.esc)}
	shown.flush()
	return textcut.CutCounting(string(shown.held), shown.over, escapeEach)
}

// escapeEach returns text with every character that is not printable but
// a tab escaped on its own (see OutputLine.String).
func escapeEach(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, text[i])
		case r == '\t' || strconv.IsPrint(r):
			b.WriteRune(r)
		default:
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}
