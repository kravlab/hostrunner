package rules

import (
	"slices"
	"strings"
	"testing"
)

// A rule may name its flag style; under go a flag name may have one dash.
func TestParseAcceptsFlagStyle(t *testing.T) {
	cases := map[string]string{
		"getopt":                "rules:\n  - command: git push\n    flag_style: getopt\n    flags:\n      allow: [-u]\n",
		"go with positional":    "rules:\n  - command: tix pr\n    flag_style: go\n    positional:\n      allow: ['[0-9]*']\n",
		"go single-dash long":   "rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      deny: [-asap]\n",
		"go single-dash value":  "rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      values:\n        -repo: {}\n",
		"go two-dash short":     "rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      allow: [--l]\n",
		"go same spelling once": "rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      allow: [-a]\n      values:\n        --a: {}\n",
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(config)); err != nil {
				t.Fatalf("rejected: %v\n%s", err, config)
			}
		})
	}
}

// An invalid flag style, or a flag name it does not take, fails to load.
func TestParseRejectsInvalidFlagStyle(t *testing.T) {
	cases := map[string]struct{ config, want string }{
		"unknown style": {
			"rules:\n  - command: tix pr\n    flag_style: urfave\n    flags:\n      allow: [-a]\n",
			`rule 1: "tix pr": flag_style must be getopt or go, not "urfave"`,
		},
		"with args any": {
			"rules:\n  - command: tix pr\n    flag_style: go\n    args: any\n",
			`rule 1: "tix pr": args cannot be combined with flags, positional or flag_style`,
		},
		"with args none": {
			"rules:\n  - command: tix pr\n    flag_style: getopt\n    args: none\n",
			`rule 1: "tix pr": args cannot be combined with flags, positional or flag_style`,
		},
		"style alone": {
			"rules:\n  - command: tix pr\n    flag_style: go\n",
			`rule 1: "tix pr": say what is allowed (args: any, args: none, flags or positional)`,
		},
		"getopt single-dash long": {
			"rules:\n  - command: tix pr\n    flag_style: getopt\n    flags:\n      deny: [-asap]\n",
			`rule 1: "tix pr" flags: "-asap" is not a flag name (use -x or --name)`,
		},
		"go both spellings in a list": {
			"rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      deny: [-asap, --asap]\n",
			`rule 1: "tix pr" flags: --asap and -asap name the same flag`,
		},
		"go both spellings under values": {
			"rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      values:\n        -repo: {}\n        --repo: {}\n",
			`rule 1: "tix pr" flags: --repo and -repo name the same flag`,
		},
		"go three dashes": {
			"rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      deny: [---asap]\n",
			`rule 1: "tix pr" flags: "---asap" is not a flag name (use -name or --name)`,
		},
		"go name with =": {
			"rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      allow: [-a=b]\n",
			`rule 1: "tix pr" flags: "-a=b" is not a flag name (use -name or --name)`,
		},
		"go dashes only": {
			"rules:\n  - command: tix pr\n    flag_style: go\n    flags:\n      allow: [--]\n",
			`rule 1: "tix pr" flags: "--" is not a flag name (use -name or --name)`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.config))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// goConfig is a rules file for a urfave/cli program such as tix.
const goConfig = `
rules:
  - command: tix deny
    flag_style: go
    flags:
      deny: [-asap, --force, -f]
      values:
        --repo: { allow: [my/repo] }
        -o: {}
  - command: tix allow
    flag_style: go
    flags:
      allow: [-limit, --comments, -ab]
      values:
        -repo: { allow: [my/repo] }
        --dry-run: { allow: ["false"] }
  - command: tix values
    flag_style: go
    flags:
      values:
        --repo: {}
  - command: tix pos
    flag_style: go
    flags:
      allow: [--comments]
      values:
        --repo: { allow: [my/repo] }
    positional:
      deny: [main, "-*x*"]
  - command: tix any
    flag_style: go
    positional:
      deny: [main]
`

// expectGoCheck checks each argv against goConfig.
func expectGoCheck(t *testing.T, cases []struct{ argv, deny string }) {
	t.Helper()
	p := mustParse(t, goConfig)
	for _, tc := range cases {
		t.Run(tc.argv, func(t *testing.T) {
			expectCheck(t, p, strings.Fields(tc.argv), tc.deny)
		})
	}
}

// Under go, one or two dashes name the same flag, matched exactly.
func TestGoFlagStyleReadsOneOrTwoDashesAsOneFlag(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix deny -asap", `denied by rule "tix deny": flag -asap is not allowed`},
		{"tix deny --asap", `denied by rule "tix deny": flag --asap is not allowed`},
		{"tix deny --f", `denied by rule "tix deny": flag --f is not allowed`},
		{"tix deny -force", `denied by rule "tix deny": flag -force is not allowed`},
		{"tix deny --forc", ""}, // no abbreviations: the program rejects --forc itself
		{"tix allow -limit", ""},
		{"tix allow --limit", ""},
		{"tix allow -comments --comments", ""},
		{"tix allow --lim", `denied by rule "tix allow": flag --lim is not allowed`},
		{"tix allow -l", `denied by rule "tix allow": flag -l is not allowed`},
	})
}

// Under go a value flag takes its value inline or from the next token, even
// one that starts with "-", and never attached to a one-letter name.
func TestGoFlagStyleReadsValues(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix deny --repo my/repo", ""},
		{"tix deny -repo my/repo", ""},
		{"tix deny -repo=my/repo", ""},
		{"tix deny --repo=other", `denied by rule "tix deny": value "other" of flag --repo is not allowed`},
		{"tix deny -repo other", `denied by rule "tix deny": value "other" of flag -repo is not allowed`},
		{"tix deny --rep=other", `denied by rule "tix deny": flag --rep does not take a value`}, // not an abbreviation of --repo
		{"tix deny -o -asap", ""}, // -asap is the value of -o
		{"tix deny -ofoo", `denied by rule "tix deny": flag -o in -ofoo takes a value`}, // never -o with a value; as a cluster it hides -o
		{"tix deny --repo", `denied by rule "tix deny": flag --repo needs a value`},
		{"tix allow -repo my/repo", ""},
		{"tix allow --repo=my/repo", ""},
		{"tix allow --repo x", `denied by rule "tix allow": value "x" of flag --repo is not allowed`},
	})
}

// Under go an inline value on a flag that the rule does not declare under
// values is denied: a boolean flag would take it (--dry-run=false).
func TestGoFlagStyleDeniesInlineValueOfOtherFlags(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix allow --comments=false", `denied by rule "tix allow": flag --comments does not take a value`},
		{"tix deny --verbose=false", `denied by rule "tix deny": flag --verbose does not take a value`},
		{"tix values -v=false", `denied by rule "tix values": flag -v does not take a value`},
		{"tix allow --dry-run=false", ""},
		{"tix allow --dry-run=true", `denied by rule "tix allow": value "true" of flag --dry-run is not allowed`},
		{"tix any --verbose=false", ""}, // no flags section: flags are unrestricted
	})
}

// A single-dash token the rule does not list whole may be a cluster of short
// flags (UseShortOptionHandling): each letter is checked too.
func TestGoFlagStyleChecksClusters(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix deny -xf", `denied by rule "tix deny": flag -f in -xf is not allowed`},
		{"tix deny -xo", `denied by rule "tix deny": flag -o in -xo takes a value`},
		{"tix deny -xy", ""},
		{"tix allow -ab", ""}, // listed whole: every parser tries the whole name first
		{"tix allow -ba", `denied by rule "tix allow": flag -ba is not allowed`},
		{"tix values -or", ""}, // no -o under values here
		{"tix deny --xf", ""},  // two dashes: never a cluster
	})
}

// A token starting with three dashes is denied under go; under getopt it is
// a long flag as before.
func TestGoFlagStyleDeniesThreeDashes(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix any ---asap", `denied by rule "tix any": flag ---asap has more than two dashes`},
		{"tix pos x ---asap", `denied by rule "tix pos": flag ---asap has more than two dashes`},
	})
}

// "--" before any positional argument ends the flags, as every parser agrees.
func TestGoFlagStyleEndsFlagsAtDoubleDash(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix deny -- -asap", ""},
		{"tix pos -- -ax", `denied by rule "tix pos": argument "-ax" is denied`},
		{"tix pos -- main", `denied by rule "tix pos": argument "main" is denied`},
	})
}

// Rules without flag_style read argv as before: -asap is a cluster.
func TestGetoptStaysTheDefault(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: tix\n    flags:\n      deny: [-a]\n")
	expectCheck(t, p, []string{"tix", "-asap"}, `denied by rule "tix": flag -a is not allowed`)
	p = mustParse(t, "rules:\n  - command: tix\n    flag_style: getopt\n    flags:\n      deny: [--asap]\n")
	expectCheck(t, p, []string{"tix", "-asap"}, "")
	expectCheck(t, p, []string{"tix", "--as"}, `denied by rule "tix": flag --as is not allowed`)
}

// After the first positional argument the parsers disagree: stdlib, v1 and
// v2 read every token as positional, v3 still reads flags. A token there has
// to pass both readings.
func TestGoFlagStyleChecksBothReadingsAfterAPositionalArgument(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix pos 5 --comments", ""},
		{"tix pos 5 --force", `denied by rule "tix pos": flag --force is not allowed`},
		{"tix pos 5 -ax", `denied by rule "tix pos": argument "-ax" is denied`},
		{"tix pos 5 --repo my/repo", ""},
		{"tix pos 5 --repo other", `denied by rule "tix pos": value "other" of flag --repo is not allowed`},
		{"tix pos 5 --repo=main", `denied by rule "tix pos": value "main" of flag --repo is not allowed`},
		{"tix pos 5 main", `denied by rule "tix pos": argument "main" is denied`},
		{"tix pos 5 --repo -x", `denied by rule "tix pos": flag --repo is followed by "-x": Go flag parsers disagree on its value here`},
		{"tix pos 5 --repo", `denied by rule "tix pos": flag --repo needs a value`},
		{"tix pos 5 -- --force", ""}, // v3 ends the flags; the others read positionals already
		{"tix pos 5 -- -ax", `denied by rule "tix pos": argument "-ax" is denied`},
		{"tix pos 5 ---asap", `denied by rule "tix pos": flag ---asap has more than two dashes`},
		{"tix any 5 --anything", ""},
		{"tix any 5 --anything main", `denied by rule "tix any": argument "main" is denied`},
	})
}

// v3 stops reading flags at a single-dash token not followed by a letter,
// the others read it as a flag; a lone "-" is positional, after which the
// parsers disagree too.
func TestGoFlagStyleChecksBothReadingsFromADashToken(t *testing.T) {
	expectGoCheck(t, []struct{ argv, deny string }{
		{"tix pos -1", `denied by rule "tix pos": flag -1 is not allowed`},
		{"tix any -1 main", `denied by rule "tix any": argument "main" is denied`},
		{"tix deny -1 --force", `denied by rule "tix deny": flag --force is not allowed`},
		{"tix pos - --force", `denied by rule "tix pos": flag --force is not allowed`},
		{"tix pos - --comments", ""},
		{"tix any - main", `denied by rule "tix any": argument "main" is denied`},
	})
}

// v3 trims whitespace around a token; the others take it as given.
func TestGoFlagStyleChecksTokensWithWhitespaceBothWays(t *testing.T) {
	p := mustParse(t, goConfig)
	cases := []struct {
		argv []string
		deny string
	}{
		{[]string{"tix", "pos", " --force"}, `denied by rule "tix pos": flag " --force" is not allowed`},
		{[]string{"tix", "pos", "--comments "}, ""},
		{[]string{"tix", "any", " main"}, `denied by rule "tix any": argument "main" is denied`},
		{[]string{"tix", "pos", "5", "main "}, `denied by rule "tix pos": argument "main" is denied`},
		{[]string{"tix", "pos", "--repo", " my/repo"}, `denied by rule "tix pos": value " my/repo" of flag --repo is not allowed`},
		{[]string{"tix", "pos", " --repo=my/repo "}, `denied by rule "tix pos": value "my/repo " of flag " --repo" is not allowed`},
		{[]string{"tix", "pos", " --"}, ""},
		{[]string{"tix", "pos", " --", "--force"}, ""},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, "|"), func(t *testing.T) {
			expectCheck(t, p, tc.argv, tc.deny)
		})
	}
}

// A path check applies where every parser reads the token the same way, and
// denies a token they read differently.
func TestGoFlagStylePathChecks(t *testing.T) {
	p := mustParse(t, `
rules:
  - command: tix path
    flag_style: go
    flags:
      values:
        --file: { path: check }
        -o: {}
    positional:
      path: open
`)
	allowed := []struct {
		argv  []string
		paths []PathArg
	}{
		{[]string{"tix", "path", "a.txt"}, []PathArg{{Index: 2, Mode: PathModeOpen}}},
		{[]string{"tix", "path", "a.txt", "b.txt"}, []PathArg{{Index: 2, Mode: PathModeOpen}, {Index: 3, Mode: PathModeOpen}}},
		{[]string{"tix", "path", "--file", "a.txt", "b.txt"}, []PathArg{{Index: 3, Mode: PathModeCheck}, {Index: 4, Mode: PathModeOpen}}},
		{[]string{"tix", "path", "-file=a.txt"}, []PathArg{{Index: 2, Prefix: "-file=", Mode: PathModeCheck}}},
		{[]string{"tix", "path", "-o", "x", "--", "--file"}, []PathArg{{Index: 5, Mode: PathModeOpen}}},
	}
	for _, tc := range allowed {
		t.Run(strings.Join(tc.argv, "|"), func(t *testing.T) {
			allowed, err := p.Check(tc.argv)
			if err != nil {
				t.Fatalf("denied: %v", err)
			}
			if !slices.Equal(allowed.Paths, tc.paths) {
				t.Fatalf("got paths %+v, want %+v", allowed.Paths, tc.paths)
			}
		})
	}
	denied := []struct {
		argv []string
		deny string
	}{
		{[]string{"tix", "path", "a.txt", "--file", "b.txt"}, `denied by rule "tix path": argument "--file" is read differently by Go flag parsers: a path check cannot apply to it`},
		{[]string{"tix", "path", "a.txt", "-o", "b.txt"}, `denied by rule "tix path": argument "-o" is read differently by Go flag parsers: a path check cannot apply to it`},
		{[]string{"tix", "path", "a.txt", "--", "b.txt"}, `denied by rule "tix path": argument "--" is read differently by Go flag parsers: a path check cannot apply to it`},
		{[]string{"tix", "path", " a.txt"}, `denied by rule "tix path": argument " a.txt" is read differently by Go flag parsers: a path check cannot apply to it`},
		{[]string{"tix", "path", "--file", "a.txt "}, `denied by rule "tix path": value "a.txt " of flag --file is read differently by Go flag parsers: a path check cannot apply to it`},
		{[]string{"tix", "path", "-1", "--file=a.txt"}, `denied by rule "tix path": argument "-1" is read differently by Go flag parsers: a path check cannot apply to it`},
	}
	for _, tc := range denied {
		t.Run(strings.Join(tc.argv, "|"), func(t *testing.T) {
			expectCheck(t, p, tc.argv, tc.deny)
		})
	}
}

// A token an allow list names whole is that flag in every parser, so it is
// not read as a cluster even when one of its letters is a value flag.
func TestGoFlagStyleDoesNotReadAnAllowedTokenAsACluster(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: tix\n    flag_style: go\n    flags:\n      allow: [-ab]\n      values:\n        -a: { allow: [x] }\n")
	expectCheck(t, p, []string{"tix", "-ab"}, "")
	expectCheck(t, p, []string{"tix", "--ab"}, "")
	expectCheck(t, p, []string{"tix", "-ba"}, `denied by rule "tix": flag -ba is not allowed`)
}

// issue30Config holds the tix rules of issue #30, with flag_style: go added.
const issue30Config = `
rules:
  - command: tix issues edit
    flag_style: go
    flags:
      allow: []
      values:
        --title: {}
        --description-file: { allow: ["-"] }
  - command: tix issues list
    flag_style: go
    flags:
      allow: []
  - command: tix issues create
    flag_style: go
    flags:
      allow: []
      values:
        --labels: {}
`

// expectIssue30Checks checks each argv, given as tokens so whitespace stays
// inside one, against issue30Config.
func expectIssue30Checks(t *testing.T, cases []struct {
	argv []string
	deny string
}) {
	t.Helper()
	p := mustParse(t, issue30Config)
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, "|"), func(t *testing.T) {
			expectCheck(t, p, tc.argv, tc.deny)
		})
	}
}

// Issue #30: urfave/cli v3 trims whitespace around a token, so it reads
// " --description-file" as the flag; the value filter must still hold, also
// after a positional argument.
func TestGoFlagStyleDeniesWhitespaceLedFlagsOfIssue30(t *testing.T) {
	expectIssue30Checks(t, []struct {
		argv []string
		deny string
	}{
		{[]string{"tix", "issues", "edit", "999999999", " --description-file", "/etc/hostname"},
			`denied by rule "tix issues edit": value "/etc/hostname" of flag " --description-file" is not allowed`},
		{[]string{"tix", "issues", "edit", "999999999", "\t--description-file", "/etc/hostname"},
			`denied by rule "tix issues edit": value "/etc/hostname" of flag "\t--description-file" is not allowed`},
		{[]string{"tix", "issues", "edit", " --description-file", "/etc/hostname"},
			`denied by rule "tix issues edit": value "/etc/hostname" of flag " --description-file" is not allowed`},
		{[]string{"tix", "issues", "edit", "999999999", "--description-file", "/etc/hostname"},
			`denied by rule "tix issues edit": value "/etc/hostname" of flag --description-file is not allowed`},
		{[]string{"tix", "issues", "edit", "--description-file", "-", "999999999"}, ""},
		{[]string{"tix", "issues", "edit", "999999999", "--description-file", "-"},
			`denied by rule "tix issues edit": flag --description-file is followed by "-": Go flag parsers disagree on its value here`},
		{[]string{"tix", "issues", "list", " --login", "somename"},
			`denied by rule "tix issues list": flag " --login" is not allowed`},
		{[]string{"tix", "issues", "list", "--login", "somename"},
			`denied by rule "tix issues list": flag --login is not allowed`},
	})
}

// Issue #30: urfave/cli reads --l as -l and accepts no abbreviation, so --l
// must not be taken for --labels.
func TestGoFlagStyleDeniesOneLetterLongFlagsOfIssue30(t *testing.T) {
	expectIssue30Checks(t, []struct {
		argv []string
		deny string
	}{
		{[]string{"tix", "issues", "create", "--l=name"}, `denied by rule "tix issues create": flag --l is not allowed`},
		{[]string{"tix", "issues", "create", "--l", "name"}, `denied by rule "tix issues create": flag --l is not allowed`},
		{[]string{"tix", "issues", "create", "-l", "name"}, `denied by rule "tix issues create": flag -l is not allowed`},
		{[]string{"tix", "issues", "create", "--labels=bug"}, ""},
		{[]string{"tix", "issues", "create", "--labels", "bug"}, ""},
	})
}
