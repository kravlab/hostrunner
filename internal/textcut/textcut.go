// Package textcut cuts a long value for a terminal line, as hostrun's
// refusals and an approval session show arguments.
package textcut

import "fmt"

// MaxChars is how many characters of a value Cut shows.
const MaxChars = 200

// Cut shows s quoted by quote, cut after MaxChars characters, an invalid
// UTF-8 byte counting as one, the bytes left out counted. The cut falls
// between two characters, so a value of megabytes shows in a few hundred
// bytes.
func Cut(s string, quote func(string) string) string { return CutCounting(s, 0, quote) }

// CutCounting is Cut for a value of which dropped bytes were already left
// out before s reached it: dropped is the number of those bytes, counted
// with the bytes the cut leaves out of s, and shown even when s itself is
// not cut.
func CutCounting(s string, dropped int, quote func(string) string) string {
	n := 0
	for i := range s {
		if n == MaxChars {
			return fmt.Sprintf("%s…(+%d bytes)", quote(s[:i]), len(s)-i+dropped)
		}
		n++
	}
	if dropped > 0 {
		return fmt.Sprintf("%s…(+%d bytes)", quote(s), dropped)
	}
	return quote(s)
}
