package hostterm_test

import (
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/hostterm"
)

func TestOutputLine(t *testing.T) {
	long := strings.Repeat("x", 5000)
	tests := []struct {
		name   string
		chunks []string
		ended  bool // by a newline
		want   string
	}{
		{"plain", []string{"plain"}, true, "plain"},
		{"in chunks", []string{"in ", "chu", "nks"}, false, "in chunks"},
		{"colour", []string{"\x1b[1;31mred\x1b[0m \x1b[38:5:196mx\x1b[m"}, true, "red x"},
		{"colour across chunks", []string{"\x1b[3", "1mred\x1b", "[0m"}, true, "red"},
		{"other sequence", []string{"a\x1b[2Kb"}, true, `a\x1b[2Kb`},
		{"rewritten", []string{"10%\r50%\r100%"}, true, "100%"},
		{"rewritten across chunks", []string{"old\r", "new"}, true, "new"},
		{"crlf", []string{"whole\r"}, true, "whole"},
		{"cr twice before newline", []string{"gone\r\r"}, true, ""},
		{"cr unfinished", []string{"gone\r"}, false, ""},
		{"control", []string{"a\x00b\x7fc\x07"}, true, `a\x00b\x7fc\a`},
		{"not printable", []string{"zero\u200bwidth"}, true, `zero\u200bwidth`},
		{"invalid", []string{"\xffok\xe2\x82"}, true, `\xffok\xe2\x82`},
		{"tab", []string{"a\tb"}, true, "a\tb"},
		{"backslash and quotes", []string{`"a\b'`}, true, `"a\b'`},
		{"cut", []string{strings.Repeat("é", 201)}, true, strings.Repeat("é", 200) + "…(+2 bytes)"},
		{"escapes cut whole", []string{strings.Repeat("\x00", 201)}, true, strings.Repeat(`\x00`, 200) + "…(+1 bytes)"},
		{"colour not counted", []string{strings.Repeat("\x1b[31mx", 200) + "y"}, true, strings.Repeat("x", 200) + "…(+1 bytes)"},
		{"long", []string{long[:3000], long[3000:]}, true, long[:200] + "…(+4800 bytes)"},
		{"long rewritten", []string{long, "\rnew"}, true, "new"},
		{"kilobytes of colour first", []string{strings.Repeat("\x1b[0m", 1200) + "text"}, true, "text"},
		{"colour in one-byte chunks", strings.Split("\x1b[31mred\x1b[0m", ""), true, "red"},
		{"rune past kilobytes of colour", []string{strings.Repeat("\x1b[m", 1365) + "éx"}, true, "éx"},
		{"rune split across chunks", []string{"\xc3", "\xa9"}, true, "é"},
		{"colour past kilobytes of colour", []string{strings.Repeat("\x1b[m", 1364) + "\x1b[31mred"}, true, "red"},
		{"colour past the cut not counted", []string{long + strings.Repeat("\x1b[31m", 10)}, true, long[:200] + "…(+4800 bytes)"},
		{"incomplete rune at the end", []string{"ok\xe2\x82"}, false, `ok\xe2\x82`},
		{"incomplete colour at the end", []string{"ok\x1b[31"}, false, `ok\x1b[31`},
		{"too long for colour", []string{"\x1b[" + strings.Repeat("1", 70) + "mx"}, true, `\x1b[` + strings.Repeat("1", 70) + "mx"},
		{"incomplete colour before crlf", []string{"ok\x1b[31\r"}, true, `ok\x1b[31`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l hostterm.OutputLine
			for _, c := range tt.chunks {
				l.Write([]byte(c))
			}
			if tt.ended {
				l.End()
			}
			if got := l.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A whole line holds all of its text, not cut, sanitised as any line, and
// stays whole when a \r rewrites it.
func TestWholeOutputLine(t *testing.T) {
	long := strings.Repeat("é", 5000)
	for _, tt := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"long", []string{long[:3001], long[3001:]}, long},
		{"sanitised", []string{long + "\x1b[31mred\x1b[0m\x00"}, long + `red\x00`},
		{"rewritten", []string{"old\r", long}, long},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := hostterm.WholeOutputLine()
			for _, c := range tt.chunks {
				l.Write([]byte(c))
			}
			l.End()
			if got := l.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
