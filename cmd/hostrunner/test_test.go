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

func TestRulesTestDeniesACommandAndNamesItsClass(t *testing.T) {
	code, stdout, stderr := runTest(t, "--config", writeRules(t, testRules), "git", "push", "--force")
	if code != 126 {
		t.Fatalf("exit code %d, want 126 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: denied by rule \"git push\": flag --force is not allowed (approvable)\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	if stdout != "" {
		t.Fatalf("stdout %q, want nothing", stdout)
	}
}

// classRules has a rule for every way the rules refuse a command, approvable
// or guarded, for the Rules test's class marks.
const classRules = `rules:
  - command: git push
    flags:
      allow: [-u, --tags]
      values:
        --repo: {allow: [main]}
    positional:
      allow: [origin]
  - command: git fetch
    flags:
      deny: [--force]
      values:
        --depth: {deny: ["0"]}
        --dir: {}
        --depth-limit: {}
    positional:
      deny: ["+*"]
  - command: git pull
    flags:
      allow: [--rebase]
    positional:
      deny: ["+*"]
  - command: git status
    args: none
  - command: tix
    flag_style: go
    flags:
      allow: [-verbose]
  - command: tixd
    flag_style: go
    flags:
      deny: [-force]
      values:
        -n: {}
  - command: tixp
    flag_style: go
    positional:
      path: check
  - command: curl
    secrets:
      token: {env: API_TOKEN}
    inject:
      command_args: ["-H", "Authorization: Bearer ${token}"]
    flags:
      allow: [-s]
    positional:
      allow: ["https://api.example.com/x"]
`

const (
	approvable = " (approvable)\n"
	guarded    = " (guarded: approvable after a second confirmation)\n"
)

// The class is the Rules test's view of what an approval session would
// make of the refusal: a deny pattern, an argv the rules cannot read and a
// rule with secrets need a second confirmation.
func TestRulesTestMarksTheRefusalsClass(t *testing.T) {
	config := writeRules(t, classRules)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"no rule", []string{"gh", "pr", "list"}, `no rule allows "gh pr"` + approvable},
		{"flag outside the allow list", []string{"git", "push", "--force"}, `denied by rule "git push": flag --force is not allowed` + approvable},
		{"value outside the allow list", []string{"git", "push", "--repo=other"}, `denied by rule "git push": value "other" of flag --repo is not allowed` + approvable},
		{"argument outside the allow list", []string{"git", "push", "upstream"}, `denied by rule "git push": argument "upstream" is not allowed` + approvable},
		{"arguments to args: none", []string{"git", "status", "-s"}, `denied by rule "git status": arguments are not allowed` + approvable},
		{"denied flag", []string{"git", "fetch", "--force"}, `denied by rule "git fetch": flag --force is not allowed` + guarded},
		{"abbreviation of a denied flag", []string{"git", "fetch", "--forc"}, `denied by rule "git fetch": flag --forc is not allowed` + guarded},
		{"denied value", []string{"git", "fetch", "--depth=0"}, `denied by rule "git fetch": value "0" of flag --depth is denied` + guarded},
		{"denied argument", []string{"git", "fetch", "+main"}, `denied by rule "git fetch": argument "+main" is denied` + guarded},
		{"value missing", []string{"git", "fetch", "--dir"}, `denied by rule "git fetch": flag --dir needs a value` + guarded},
		{"abbreviated value flag without =", []string{"git", "fetch", "--di", "x"}, `denied by rule "git fetch": abbreviated flag --di must be given its value as --di=value` + guarded},
		{"ambiguous abbreviation", []string{"git", "fetch", "--dep=1"}, `denied by rule "git fetch": flag --dep is ambiguous` + guarded},
		{"value for a flag that takes none", []string{"git", "push", "--tags=x"}, `denied by rule "git push": flag --tags does not take a value` + guarded},
		{"value for a go flag that takes none", []string{"tix", "-verbose=1"}, `denied by rule "tix": flag -verbose does not take a value` + guarded},
		{"denied go flag", []string{"tixd", "-force"}, `denied by rule "tixd": flag -force is not allowed` + guarded},
		{"go flag missing its value", []string{"tixd", "-n"}, `denied by rule "tixd": flag -n needs a value` + guarded},
		{"go flag with three dashes", []string{"tixd", "---x"}, `denied by rule "tixd": flag ---x has more than two dashes` + guarded},
		{"go value flag in a cluster", []string{"tixd", "-xn"}, `denied by rule "tixd": flag -n in -xn takes a value` + guarded},
		{"go parsers disagree on a value", []string{"tixd", "pos", "-n", "-v"}, `denied by rule "tixd": flag -n is followed by "-v": Go flag parsers disagree on its value here` + guarded},
		{"go path check on a token read differently", []string{"tixp", "a", "-b"}, `denied by rule "tixp": argument "-b" is read differently by Go flag parsers: a path check cannot apply to it` + guarded},
		{"rule with secrets", []string{"curl", "https://other.example/"}, `denied by rule "curl": argument "https://other.example/" is not allowed` + guarded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runTest(t, append([]string{"--config", config}, tt.argv...)...)
			if want := "hostrunner: " + tt.want; code != 126 || stderr != want {
				t.Fatalf("exit code %d, stderr %q; want 126, %q", code, stderr, want)
			}
		})
	}
}

// Unlike a plain refusal, which stops at the first reason, the Rules test
// lists every reason; a deny hit after an allow miss still makes the
// refusal guarded.
func TestRulesTestListsEveryReason(t *testing.T) {
	config := writeRules(t, classRules)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"two allow misses", []string{"git", "push", "--force", "upstream"},
			`denied by rule "git push": flag --force is not allowed; argument "upstream" is not allowed` + approvable},
		{"a deny hit after an allow miss", []string{"git", "pull", "--ff", "+main"},
			`denied by rule "git pull": flag --ff is not allowed; argument "+main" is denied` + guarded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runTest(t, append([]string{"--config", config}, tt.argv...)...)
			if want := "hostrunner: " + tt.want; code != 126 || stderr != want {
				t.Fatalf("exit code %d, stderr %q; want 126, %q", code, stderr, want)
			}
		})
	}
}

// After a flag it refuses, the Rules test still reads the flag's value as
// its value, not as a positional argument with a reason of its own.
func TestRulesTestReadsARefusedFlagsValueAsItsValue(t *testing.T) {
	config := writeRules(t, `rules:
  - command: g
    flags:
      deny: [--force]
      values:
        --depth: {}
        --depth-limit: {}
        --dir: {}
        --force-with-lease: {}
    positional:
      allow: [ok]
  - command: t
    flag_style: go
    flags:
      deny: [-force]
      values:
        -force: {}
    positional:
      allow: [ok]
`)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"ambiguous abbreviation", []string{"g", "--dep", "1", "ok"}, `denied by rule "g": flag --dep is ambiguous` + guarded},
		{"abbreviated value flag without =", []string{"g", "--di", "x", "ok"}, `denied by rule "g": abbreviated flag --di must be given its value as --di=value` + guarded},
		{"abbreviation of a denied flag and a value flag", []string{"g", "--forc", "v", "ok"}, `denied by rule "g": flag --forc is not allowed` + guarded},
		{"denied go value flag", []string{"t", "-force", "x", "ok"}, `denied by rule "t": flag -force is not allowed` + guarded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runTest(t, append([]string{"--config", config}, tt.argv...)...)
			if want := "hostrunner: " + tt.want; code != 126 || stderr != want {
				t.Fatalf("exit code %d, stderr %q; want 126, %q", code, stderr, want)
			}
		})
	}
}

func TestRulesTestDeniesACommandNoRuleMatches(t *testing.T) {
	code, _, stderr := runTest(t, "--config", writeRules(t, testRules), "gh", "pr", "list")
	if code != 126 {
		t.Fatalf("exit code %d, want 126 (stderr %q)", code, stderr)
	}
	if want := "hostrunner: no rule allows \"gh pr\" (approvable)\n"; stderr != want {
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
	if want := "hostrunner: no rule allows \"--config " + config + "\" (approvable)\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	code, _, stderr = runTest(t, "--config", config, "git", "push", "--config")
	if want := "hostrunner: denied by rule \"git push\": flag --config is not allowed (approvable)\n"; code != 126 || stderr != want {
		t.Fatalf("exit code %d, stderr %q; want 126, %q", code, stderr, want)
	}
}

// The Rules test reports the fixed directory, inherit resolved, but tests
// the rules file alone: the directory need not exist.
func TestRulesTestReportsTheFixedDirectory(t *testing.T) {
	config := writeRules(t, `rules:
  - command: tix
    dir: /hostrunner-test/no-such-dir
    args: none
  - command: tix api
    dir: inherit
    args: any
`)
	for argv, want := range map[string]string{
		"tix":       "hostrunner: allowed by rule \"tix\", runs in /hostrunner-test/no-such-dir\n",
		"tix api x": "hostrunner: allowed by rule \"tix api\", runs in /hostrunner-test/no-such-dir\n",
	} {
		code, _, stderr := runTest(t, append([]string{"--config", config}, strings.Fields(argv)...)...)
		if code != 0 || stderr != want {
			t.Errorf("%s: exit code %d, stderr %q; want 0, %q", argv, code, stderr, want)
		}
	}
}

// The Rules test names a rule's secrets and touches none of them.
func TestRulesTestListsTheSecrets(t *testing.T) {
	rules := `rules:
  - command: curl
    secrets:
      token: {command: [no-such-authtool-xyz]}
      key: {file: /no/such/file}
    inject:
      command_args: ["-H", "Authorization: Bearer ${token}", "${key}"]
    args: any
    secrets_with_any_args: true
`
	code, _, stderr := runTest(t, "--config", writeRules(t, rules), "curl", "https://example.com")
	if want := "hostrunner: allowed by rule \"curl\", injects secrets: key, token\n"; code != 0 || stderr != want {
		t.Fatalf("exit code %d, stderr %q; want 0, %q", code, stderr, want)
	}
}
