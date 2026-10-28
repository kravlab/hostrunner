package stop

import "testing"

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
