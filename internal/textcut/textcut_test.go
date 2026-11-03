package textcut_test

import (
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/textcut"
)

// bracket marks what the quote function was given.
func bracket(s string) string { return "[" + s + "]" }

func TestCut(t *testing.T) {
	tests := []struct{ name, s, want string }{
		{"under the limit", "short", "[short]"},
		{"at the limit", strings.Repeat("a", 200), "[" + strings.Repeat("a", 200) + "]"},
		{"over it", strings.Repeat("é", 203), "[" + strings.Repeat("é", 200) + "]…(+6 bytes)"},
		{"invalid bytes count as one character", strings.Repeat("\xff", 201) + "é", "[" + strings.Repeat("\xff", 200) + "]…(+3 bytes)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := textcut.Cut(tt.s, bracket); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCutCounting(t *testing.T) {
	tests := []struct {
		name    string
		s       string
		dropped int
		want    string
	}{
		{"nothing dropped", "short", 0, "[short]"},
		{"dropped, no cut", "short", 7, "[short]…(+7 bytes)"},
		{"dropped at the limit", strings.Repeat("a", 200), 1, "[" + strings.Repeat("a", 200) + "]…(+1 bytes)"},
		{"dropped and cut", strings.Repeat("é", 201), 5, "[" + strings.Repeat("é", 200) + "]…(+7 bytes)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := textcut.CutCounting(tt.s, tt.dropped, bracket); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
