package rules

import (
	"strings"
	"testing"
)

// A relative or empty dir would depend on where the daemon runs, or fall
// back to the mirrored directory unnoticed.
func TestParseRejectsADirThatIsNotAnAbsolutePath(t *testing.T) {
	for name, dir := range map[string]string{
		"empty":     `""`,
		"null":      "",
		"relative":  "srv/tix",
		"dot-slash": "./srv",
	} {
		t.Run(name, func(t *testing.T) {
			config := "rules:\n  - command: tix\n    dir: " + dir + "\n    args: any\n"
			_, err := Parse([]byte(config))
			if err == nil {
				t.Fatalf("accepted:\n%s", config)
			}
			if !strings.Contains(err.Error(), `"tix"`) {
				t.Fatalf("error %q does not name the rule", err)
			}
		})
	}
}

// The program would resolve a path: check argument from the fixed
// directory, not from the mirrored directory it was checked in.
func TestParseRejectsDirWithPathCheck(t *testing.T) {
	for name, lists := range map[string]string{
		"positional":  "    positional:\n      path: check\n",
		"flag value":  "    flags:\n      values:\n        --file: { path: check }\n",
		"go spelling": "    flag_style: go\n    flags:\n      values:\n        -file: { allow: [\"*.md\"], path: check }\n",
	} {
		t.Run(name, func(t *testing.T) {
			config := "rules:\n  - command: tix\n    dir: /\n" + lists
			_, err := Parse([]byte(config))
			if err == nil {
				t.Fatalf("accepted:\n%s", config)
			}
			if !strings.Contains(err.Error(), `"tix"`) || !strings.Contains(err.Error(), "path: check") {
				t.Fatalf("error %q does not name the rule and path: check", err)
			}
		})
	}
}

// path: open hands the program the opened file, so it is safe with dir.
func TestParseAcceptsDirWithPathOpen(t *testing.T) {
	mustParse(t, "rules:\n  - command: tix\n    dir: /\n    positional:\n      path: open\n    flags:\n      values:\n        --file: { path: open }\n")
}

// Check hands the caller the cleaned fixed directory of the rule that
// decides, and nothing for a rule without dir.
func TestCheckReportsTheFixedDirectoryOfTheAllowingRule(t *testing.T) {
	p := mustParse(t, `
rules:
  - command: tix
    dir: /srv/../srv//tix/
    args: any
  - command: tix api
    args: any
  - command: git
    args: any
`)
	for argv, want := range map[string]string{
		"tix pr list": "/srv/tix", // cleaned
		"tix api x":   "",         // a longer rule without dir does not inherit
		"git status":  "",
	} {
		allowed, err := p.Check(strings.Fields(argv))
		if err != nil {
			t.Fatalf("%s: %v", argv, err)
		}
		if allowed.Dir != want {
			t.Errorf("%s: Dir %q, want %q", argv, allowed.Dir, want)
		}
	}
}

// inherit follows the nearest shorter prefix rule, through chains.
func TestDirInheritTakesTheNearestShorterRulesDirectory(t *testing.T) {
	// Longer rules come first: inheritance does not depend on file order.
	p := mustParse(t, `
rules:
  - command: tix api get
    dir: inherit
    args: any
  - command: tix pr view
    dir: inherit
    args: any
  - command: tix api
    dir: inherit
    args: any
  - command: tix pr
    dir: /srv/pr
    args: any
  - command: tix
    dir: /srv/tix
    args: any
`)
	for argv, want := range map[string]string{
		"tix api x":     "/srv/tix", // from tix
		"tix api get x": "/srv/tix", // through tix api, a chain of inherits
		"tix pr view 5": "/srv/pr",  // the nearest shorter rule, not tix
	} {
		allowed, err := p.Check(strings.Fields(argv))
		if err != nil {
			t.Fatalf("%s: %v", argv, err)
		}
		if allowed.Dir != want {
			t.Errorf("%s: Dir %q, want %q", argv, allowed.Dir, want)
		}
	}
}

// inherit never silently means the mirrored directory: without a shorter
// rule to take a fixed directory from, the file is invalid.
func TestParseRejectsDirInheritWithNothingToInherit(t *testing.T) {
	for name, config := range map[string]string{
		"no shorter rule": "rules:\n  - command: tix api\n    dir: inherit\n    args: any\n",
		"another program": "rules:\n  - command: git\n    dir: /srv\n    args: any\n  - command: tix api\n    dir: inherit\n    args: any\n",
		// The nearest shorter rule decides, though tix has a dir.
		"nearest without dir":  "rules:\n  - command: tix\n    dir: /srv\n    args: any\n  - command: tix api\n    args: any\n  - command: tix api get\n    dir: inherit\n    args: any\n",
		"inherited path check": "rules:\n  - command: tix\n    dir: /srv\n    args: any\n  - command: tix api\n    dir: inherit\n    positional:\n      path: check\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(config))
			if err == nil {
				t.Fatalf("accepted:\n%s", config)
			}
			if !strings.Contains(err.Error(), `"tix api`) {
				t.Fatalf("error %q does not name the rule", err)
			}
		})
	}
}
