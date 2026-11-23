package stop

import (
	"errors"
	"testing"
)

func TestCheckServe(t *testing.T) {
	const socket = "/run/u/hostrunner/abc/hostrunner.sock"
	tests := []struct {
		argv []string
		ok   bool
	}{
		{[]string{"/home/u/.local/bin/hostrunner", "serve", "--socket", socket, "--watch"}, true},
		{[]string{"hostrunner", "serve", "--workspace", "/w", "-socket", socket}, true},
		{[]string{"hostrunner", "serve", "--socket=" + socket}, true},
		{[]string{"hostrunner", "serve", "-socket=/run/u/hostrunner/abc/../abc/hostrunner.sock"}, true},
		{[]string{"hostrunner", "serve", "--socket", "/run/u/hostrunner/other/hostrunner.sock"}, false},
		{[]string{"hostrunner", "approve", "--socket", socket}, false},
		{[]string{"sh", "serve", "--socket", socket}, false},
		{[]string{"hostrunner", "serve", "--socket"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if err := checkServe(tt.argv, socket); (err == nil) != tt.ok {
			t.Errorf("checkServe(%q) = %v, want ok %v", tt.argv, err, tt.ok)
		}
	}
}

// A shared process is told apart from a daemon of another socket by its
// own error, as it is never signalled; its subcommand names it, whatever
// the program is called.
func TestCheckServeNamesASharedProcess(t *testing.T) {
	const socket = "/run/u/hostrunner/abc/hostrunner.sock"
	tests := []struct {
		name   string
		argv   []string
		shared bool
	}{
		{"shared process", []string{"/home/u/.local/bin/hostrunner", "shared", "--socket", "/run/u/hostrunner/shared-1.sock"}, true},
		{"daemon of another socket", []string{"hostrunner", "serve", "--socket", "/run/u/hostrunner/other/hostrunner.sock"}, false},
		{"shared of a renamed binary", []string{"/opt/x/hr-copy", "shared", "--socket", "/run/u/hostrunner/shared-1.sock"}, true},
		{"shared of a wrapper", []string{"sh", "shared", "--socket", "/run/u/hostrunner/shared-1.sock"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkServe(tt.argv, socket)
			if got := errors.Is(err, errSharedProcess); got != tt.shared {
				t.Errorf("checkServe(%q) = %v, want errSharedProcess %v", tt.argv, err, tt.shared)
			}
		})
	}
}
