package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRules is a rules file for the Rules test: bare git, and git push
// with -u as its only flag.
const testRules = `rules:
  - command: git
    args: none
  - command: git push
    flags:
      allow: [-u]
`

// writeRules writes content as a rules file in a new directory and
// returns its path.
func writeRules(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hostrun.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runTest runs `hostrunner test` with args and returns the exit code main
// would use, stdout and stderr.
func runTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	err := run(append([]string{"test"}, args...), &stdout, &stderr)
	if err != nil {
		stderr.WriteString("hostrunner: " + err.Error() + "\n")
	}
	return exitCode(err), stdout.String(), stderr.String()
}

func TestRulesTestReportsTheRuleThatAllowsACommand(t *testing.T) {
	code, stdout, stderr := runTest(t, "--config", writeRules(t, testRules), "git", "push", "-u")
	if code != 0 {
		t.Fatalf("exit code %d, want 0 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: allowed by rule \"git push\"\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	if stdout != "" {
		t.Fatalf("stdout %q, want nothing", stdout)
	}
}

func TestRulesTestDeniesACommandAsTheDaemonWould(t *testing.T) {
	code, stdout, stderr := runTest(t, "--config", writeRules(t, testRules), "git", "push", "--force")
	if code != 126 {
		t.Fatalf("exit code %d, want 126 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: denied by rule \"git push\": flag --force is not allowed\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	if stdout != "" {
		t.Fatalf("stdout %q, want nothing", stdout)
	}
}

func TestRulesTestDeniesACommandNoRuleMatches(t *testing.T) {
	code, _, stderr := runTest(t, "--config", writeRules(t, testRules), "gh", "pr", "list")
	if code != 126 {
		t.Fatalf("exit code %d, want 126 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: no rule allows \"gh pr\"\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

func TestRulesTestReadsTheRulesFileUnderTheCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "hostrun.yaml"), []byte(testRules), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	code, _, stderr := runTest(t, "git", "push")
	if code != 0 {
		t.Fatalf("exit code %d, want 0 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: allowed by rule \"git push\"\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

// A missing file is an error, not "deny everything" as in the daemon, so
// a typo in the path is not mistaken for a denied command.
func TestRulesTestFailsWithoutTheRulesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	code, _, stderr := runTest(t, "--config", path, "git", "push")
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: rules file: open " + path + ": no such file or directory\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

func TestRulesTestFailsOnAnInvalidRulesFile(t *testing.T) {
	path := writeRules(t, "rules:\n  - command: git\n")
	code, _, stderr := runTest(t, "--config", path, "git")
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (stderr %q)", code, stderr)
	}
	// The same error as `hostrunner up` reports for the file.
	if !strings.HasPrefix(stderr, "hostrunner: rules file: "+path+": ") {
		t.Fatalf("stderr %q, want the error naming %s", stderr, path)
	}
}

func TestRulesTestRequiresACommand(t *testing.T) {
	code, _, stderr := runTest(t, "--config", writeRules(t, testRules))
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: " + testUsage + "\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

// After `--`, or the first non-flag argument, everything is the command,
// even what looks like a flag of `hostrunner test`.
func TestRulesTestTakesEverythingAfterTheFlagsAsTheCommand(t *testing.T) {
	config := writeRules(t, testRules)
	code, _, stderr := runTest(t, "--config", config, "--", "--config", config)
	if code != 126 {
		t.Fatalf("exit code %d, want 126 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: no rule allows \"--config " + config + "\"\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	code, _, stderr = runTest(t, "--config", config, "git", "push", "--config")
	if want := "hostrunner: denied by rule \"git push\": flag --config is not allowed\n"; code != 126 || stderr != want {
		t.Fatalf("exit code %d, stderr %q; want 126, %q", code, stderr, want)
	}
}
