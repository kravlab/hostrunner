package client_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/client"
)

// rulesWorkspace creates a workspace with .devcontainer/hostrun.yaml and a
// subdirectory, and returns the subdirectory and the .devcontainer path.
func rulesWorkspace(t *testing.T) (sub, dir string) {
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
	if err := os.WriteFile(filepath.Join(dir, "hostrun.yaml"), []byte("rules: []\n"), 0o644); err != nil {
		t.Fatal(err)
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
	file := filepath.Join(dir, "hostrun.yaml")
	if got := stderr.String(); !strings.HasPrefix(got, "hostrun: warning: "+file+" is writable") {
		t.Fatalf("stderr = %q, want a warning about %s", got, file)
	}
}

func TestWarnWritableRulesSilentWhenReadOnly(t *testing.T) {
	skipIfRoot(t)
	sub, dir := rulesWorkspace(t)
	if err := os.Chmod(filepath.Join(dir, "hostrun.yaml"), 0o444); err != nil {
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
