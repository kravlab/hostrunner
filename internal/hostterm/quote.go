// Package hostterm shows what a daemon reports on the human's terminal,
// so that no text from a request can steer the terminal, and what a
// command itself found, such as a warning naming a file, and reads the
// human's answers line by line: approval sessions and `hostrunner stop`
// share it.
package hostterm

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/textcut"
)

// How much of an argv ArgvLine shows (an approval session's `c` shows all
// of a request's).
const (
	MaxShownArgs  = 20               // arguments; more are counted, not shown
	maxShownRunes = textcut.MaxChars // characters of one argument; more are counted
)

// IsLong reports whether arg is longer than ArgvLine shows of it.
func IsLong(arg string) bool { return utf8.RuneCountInString(arg) > maxShownRunes }

// ShownArgs returns the arguments of argv ArgvLine shows.
func ShownArgs(argv []string) []string { return argv[:min(len(argv), MaxShownArgs)] }

// ArgvLine shows argv on one line: the arguments ShownArgs returns,
// each quoted as BashQuote does, so none can steer the terminal or hide
// where it ends, and cut (see textcut.Cut), the arguments left out counted.
func ArgvLine(argv []string) string {
	shown := ShownArgs(argv)
	cut := make([]string, len(shown))
	for i, arg := range shown {
		cut[i] = textcut.Cut(arg, BashQuote)
	}
	line := strings.Join(cut, " ")
	if more := len(argv) - len(shown); more > 0 {
		line += fmt.Sprintf(" …(+%d args)", more)
	}
	return line
}

// BashQuote returns arg quoted so that bash reads it back as it is: as it
// is when every character in it is safe (see bashSafe), in single quotes
// when every character in it is printable and none is ' or a combining
// mark, in ANSI-C quotes otherwise (see ansiCQuote). An empty argument
// shows as two single quotes.
func BashQuote(arg string) string {
	needsQuotes := func(r rune) bool { return !bashSafe(r) }
	switch {
	case arg == "":
		return "''"
	case strings.IndexFunc(arg, needsQuotes) < 0:
		return arg
	case printable(arg) && !strings.ContainsRune(arg, '\'') && strings.IndexFunc(arg, isMark) < 0:
		return "'" + arg + "'"
	}
	return ansiCQuote(arg, false)
}

// ASCIIQuote returns arg's ASCII form: arg in ANSI-C quotes with every
// character outside ASCII escaped, so a look-alike letter shows its code.
func ASCIIQuote(arg string) string { return ansiCQuote(arg, true) }

// bashSafe reports whether r stands for itself in a bash word, unquoted:
// an ASCII letter or digit, or one of _@%+=:,./-. A first word holding =
// still reads as an assignment, which the cmd line leaves as it is.
func bashSafe(r rune) bool {
	return r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_@%+=:,./-", r))
}

// isMark reports whether r is a combining mark (Unicode Mn, Me), which a
// terminal draws over the character before it, a quote included.
func isMark(r rune) bool { return unicode.In(r, unicode.Mn, unicode.Me) }

// ansiCQuote returns arg in bash's ANSI-C quotes, $'…': ' and \ escaped
// with \, newline and tab as \n and \t, any other ASCII control character,
// DEL and a byte of invalid UTF-8 as \xHH, a combining mark and a
// character outside ASCII that is not printable, or any when ascii is set,
// as \uHHHH, or \UHHHHHHHH above U+FFFF, and every other character as it
// is.
func ansiCQuote(arg string, ascii bool) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := 0; i < len(arg); {
		r, size := utf8.DecodeRuneInString(arg[i:])
		escape := r > unicode.MaxASCII && (ascii || isMark(r) || !strconv.IsPrint(r))
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, arg[i])
		case r == '\'' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r <= unicode.MaxASCII && !strconv.IsPrint(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case escape && r > 0xFFFF: // past the four digits \u takes
			fmt.Fprintf(&b, `\U%08x`, r)
		case escape:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	b.WriteByte('\'')
	return b.String()
}

// HoldsNonASCII reports whether arg holds a character outside ASCII.
func HoldsNonASCII(arg string) bool {
	return strings.IndexFunc(arg, func(r rune) bool { return r > unicode.MaxASCII }) >= 0
}

// Display returns text as it may be shown on the terminal: as it is when
// every character in it is printable, Go-quoted otherwise, so control
// characters and terminal escapes appear escaped.
func Display(text string) string {
	if !printable(text) {
		return strconv.Quote(text)
	}
	return text
}

// printable reports whether text is valid UTF-8 and every character in it
// is printable.
func printable(text string) bool {
	return utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool { return !strconv.IsPrint(r) }) < 0
}
