package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// invalidRules fails to load, so an `up` stops after its warning without
// starting a daemon.
const invalidRules = "rules: [\n"

// devcontainerWith returns a workspace whose .devcontainer holds the
// named rules files with content, and the path of that directory.
func devcontainerWith(t *testing.T, content string, names ...string) (ws, dir string) {
	t.Helper()
	ws = t.TempDir()
	dir = filepath.Join(ws, ".devcontainer")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws, dir
}

// ignoredWarning is the warning for hostrun.yaml left unread in dir.
func ignoredWarning(dir string) string {
	return "hostrunner: warning: " + filepath.Join(dir, "hostrun.yaml") + " is ignored: " + filepath.Join(dir, "hostrunner-rules.yaml") + " is used; remove it\n"
}

// isolate keeps the machine's settings and variables from choosing the
// rules file of a run.
func isolate(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "HOSTRUNNER_SETTINGS", "HOSTRUNNER_WORKSPACE_SETTINGS", "HOSTRUNNER_RULES_FILE"} {
		t.Setenv(name, "")
	}
}

// up and `up --restart` warn once when both rules files exist, and go on
// (here: to the error of invalid rules); the fallback alone and a rules
// file given are silent.
func TestUpWarnsOfAnIgnoredRulesFile(t *testing.T) {
	isolate(t)
	for _, restart := range []bool{false, true} {
		for _, tt := range []struct {
			name      string
			files     []string
			rulesFlag string // given with the path of hostrunner-rules.yaml
			warns     bool
		}{
			{"both", []string{"hostrunner-rules.yaml", "hostrun.yaml"}, "", true},
			{"fallback only", []string{"hostrun.yaml"}, "", false},
			{"new only", []string{"hostrunner-rules.yaml"}, "", false},
			{"given rules file", []string{"hostrunner-rules.yaml", "hostrun.yaml"}, "--rules-file", false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				ws, dir := devcontainerWith(t, invalidRules, tt.files...)
				args := []string{"up", "--dir", filepath.Join(t.TempDir(), "d"), "--workspace", ws, "--container-workspace", "/w"}
				if tt.rulesFlag != "" {
					args = append(args, tt.rulesFlag, filepath.Join(dir, "hostrunner-rules.yaml"))
				}
				if restart {
					args = append(args, "--restart")
				}
				var stderr strings.Builder
				err := run(args, nil, &stderr)
				if err == nil || !strings.Contains(err.Error(), "rules") {
					t.Fatalf("got %v, want the error of invalid rules", err)
				}
				want := ""
				if tt.warns {
					want = ignoredWarning(dir)
				}
				if got := stderr.String(); got != want {
					t.Errorf("stderr %q, want %q", got, want)
				}
			})
		}
	}
}

// test warns the same way and goes on to test the command.
func TestRulesTestWarnsOfAnIgnoredRulesFile(t *testing.T) {
	ws, dir := devcontainerWith(t, testRules, "hostrunner-rules.yaml", "hostrun.yaml")
	t.Chdir(ws)
	code, _, stderr := runTest(t, "git", "push")
	if want := ignoredWarning(dir) + "hostrunner: allowed by rule \"git push\"\n"; code != 0 || stderr != want {
		t.Fatalf("exit code %d, stderr %q, want 0 and %q", code, stderr, want)
	}
}

// The fallback alone and a rules file given are silent.
func TestRulesTestDoesNotWarnWithoutBothFiles(t *testing.T) {
	ws, dir := devcontainerWith(t, testRules, "hostrun.yaml")
	t.Chdir(ws)
	if code, _, stderr := runTest(t, "git", "push"); code != 0 || strings.Contains(stderr, "warning") {
		t.Fatalf("fallback: exit code %d, stderr %q", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostrunner-rules.yaml"), []byte(testRules), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runTest(t, "--rules-file", filepath.Join(dir, "hostrun.yaml"), "git", "push"); code != 0 || strings.Contains(stderr, "warning") {
		t.Fatalf("given: exit code %d, stderr %q", code, stderr)
	}
}

// A missing file stays an error naming hostrunner-rules.yaml.
func TestRulesTestNamesTheNewFileWhenNeitherExists(t *testing.T) {
	ws, dir := devcontainerWith(t, "")
	t.Chdir(ws)
	code, _, stderr := runTest(t, "git", "push")
	if code == 0 || !strings.Contains(stderr, filepath.Join(dir, "hostrunner-rules.yaml")) {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
}
