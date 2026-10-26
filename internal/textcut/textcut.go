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
func Cut(s string, quote func(string) string) string {
	n := 0
	for i := range s {
		if n == MaxChars {
			return fmt.Sprintf("%s…(+%d bytes)", quote(s[:i]), len(s)-i)
		}
		n++
	}
	return quote(s)
}
