package client_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/client"
)

// rulesWorkspace creates a workspace with .devcontainer holding the named
// files (hostrunner-rules.yaml when none is named) and a subdirectory, and
// returns the subdirectory and the .devcontainer path.
func rulesWorkspace(t *testing.T, names ...string) (sub, dir string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, ".devcontainer")
	sub = filepath.Join(root, "a", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		names = []string{"hostrunner-rules.yaml"}
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("rules: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sub, dir
}

// skipIfRoot skips tests relying on permission bits, which root ignores.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
}

func TestWarnWritableRulesWarnsAboutWritableDirectory(t *testing.T) {
	sub, dir := rulesWorkspace(t)
	var stderr bytes.Buffer
	client.WarnWritableRules(sub, &stderr)
	if got := stderr.String(); !strings.HasPrefix(got, "hostrun: warning: "+dir+" is writable") {
		t.Fatalf("stderr = %q, want a warning about %s", got, dir)
	}
}

func TestWarnWritableRulesWarnsAboutWritableFile(t *testing.T) {
	skipIfRoot(t)
	sub, dir := rulesWorkspace(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	var stderr bytes.Buffer
	client.WarnWritableRules(sub, &stderr)
	file := filepath.Join(dir, "hostrunner-rules.yaml")
	if got := stderr.String(); !strings.HasPrefix(got, "hostrun: warning: "+file+" is writable") {
		t.Fatalf("stderr = %q, want a warning about %s", got, file)
	}
}

// The file checked is the one the daemon uses: hostrun.yaml when it alone
// exists, hostrunner-rules.yaml when both do.
func TestWarnWritableRulesChecksTheFileTheDaemonUses(t *testing.T) {
	skipIfRoot(t)
	tests := []struct {
		name     string
		files    []string
		readOnly []string // files made read-only
		want     string   // the file warned about, empty for silence
	}{
		{"fallback only", []string{"hostrun.yaml"}, nil, "hostrun.yaml"},
		{"both, the fallback ignored", []string{"hostrunner-rules.yaml", "hostrun.yaml"}, []string{"hostrunner-rules.yaml"}, ""},
		{"both, the new one writable", []string{"hostrunner-rules.yaml", "hostrun.yaml"}, []string{"hostrun.yaml"}, "hostrunner-rules.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, dir := rulesWorkspace(t, tt.files...)
			for _, n := range tt.readOnly {
				if err := os.Chmod(filepath.Join(dir, n), 0o444); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			var stderr bytes.Buffer
			client.WarnWritableRules(sub, &stderr)
			if tt.want == "" {
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q, want nothing", stderr.String())
				}
				return
			}
			if want := "hostrun: warning: " + filepath.Join(dir, tt.want) + " is writable"; !strings.HasPrefix(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
		})
	}
}

func TestWarnWritableRulesSilentWhenReadOnly(t *testing.T) {
	skipIfRoot(t)
	sub, dir := rulesWorkspace(t)
	if err := os.Chmod(filepath.Join(dir, "hostrunner-rules.yaml"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	var stderr bytes.Buffer
	client.WarnWritableRules(sub, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing", stderr.String())
	}
}

func TestWarnWritableRulesSilentWithoutDevcontainer(t *testing.T) {
	var stderr bytes.Buffer
	client.WarnWritableRules(t.TempDir(), &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing", stderr.String())
	}
}
