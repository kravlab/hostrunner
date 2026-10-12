package rules

import (
	"strings"
	"testing"
	"time"
)

// secretRule is a rule for curl with a secret token from $TOKEN injected
// as a header, and strict lists; rest is appended to it as written.
func secretRule(rest string) string {
	return `rules:
  - command: curl
    secrets:
      token: {env: TOKEN}
    inject:
      command_args: ["-H", "Authorization: Bearer ${token}"]
` + rest
}

// strictLists are lists a rule with secrets may use: allow lists for
// flags, a flag's value and positional arguments.
const strictLists = `    flags:
      allow: [-s]
      values:
        -X: { allow: [GET, PUT] }
    positional:
      allow_regex: ['^https://api\.example\.com/v1/items/42$']
`

// expectParseError checks that config is refused with an error that
// names the rule and contains want.
func expectParseError(t *testing.T, config, want string) {
	t.Helper()
	_, err := Parse([]byte(config))
	if err == nil {
		t.Fatalf("accepted:\n%s", config)
	}
	if !strings.Contains(err.Error(), `"curl"`) || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name the rule and %q", err, want)
	}
}

func TestParseAcceptsASecretRuleWithAllowLists(t *testing.T) {
	mustParse(t, secretRule(strictLists))
	mustParse(t, secretRule("    args: none\n"))
	mustParse(t, secretRule("    args: any\n    secrets_with_any_args: true\n"))
}

// A rule with secrets takes allow lists only: a deny list cannot name
// every way a program gives its secret away, and a missing list is no
// restriction at all.
func TestParseRefusesASecretRuleWithoutStrictLists(t *testing.T) {
	for name, tc := range map[string]struct{ lists, want string }{
		"args any":   {"    args: any\n", "secrets_with_any_args"},
		"flags deny": {"    flags:\n      deny: [-v]\n    positional:\n      allow: ['*']\n", "flags.allow"},
		"no flags":   {"    positional:\n      allow: ['*']\n", "flags.allow"},
		"flag value without list": {
			"    flags:\n      allow: []\n      values:\n        -X:\n    positional:\n      allow: ['*']\n", "value of -X",
		},
		"flag value deny regex": {
			"    flags:\n      allow: []\n      values:\n        -X: { deny_regex: [DELETE] }\n    positional:\n      allow: ['*']\n", "value of -X",
		},
		"positional deny":      {"    flags:\n      allow: []\n    positional:\n      deny: ['*evil*']\n", "positional"},
		"positional path only": {"    flags:\n      allow: []\n    positional:\n      path: open\n", "positional"},
		"no positional":        {"    flags:\n      allow: []\n", "positional"},
	} {
		t.Run(name, func(t *testing.T) { expectParseError(t, secretRule(tc.lists), tc.want) })
	}
}

// secrets_with_any_args always means what it says: it allows args: any
// next to secrets, and nothing else.
func TestParseRefusesSecretsWithAnyArgsWhereItAllowsNothing(t *testing.T) {
	expectParseError(t, secretRule(strictLists+"    secrets_with_any_args: true\n"), "secrets_with_any_args")
	expectParseError(t, secretRule("    args: none\n    secrets_with_any_args: true\n"), "secrets_with_any_args")
	expectParseError(t, "rules:\n  - command: curl\n    args: any\n    secrets_with_any_args: true\n", "secrets_with_any_args")
}

// An injection without secrets has nothing to give away, so it needs no
// strict lists.
func TestParseAcceptsAnInjectionWithoutSecretsAndLooseLists(t *testing.T) {
	mustParse(t, "rules:\n  - command: curl\n    inject:\n      command_args: [--silent]\n    args: any\n")
}

// Template mistakes are caught before any request reaches a server.
func TestParseRefusesABadTemplate(t *testing.T) {
	for name, tc := range map[string]struct{ arg, want string }{
		"lone dollar":    {"cost: 5$", "lone $"},
		"unterminated":   {"Bearer ${token", "unterminated"},
		"bad name":       {"Bearer ${Token}", "not a secret name"},
		"unknown secret": {"Bearer ${other}", `unknown secret "other"`},
	} {
		t.Run(name, func(t *testing.T) {
			config := `rules:
  - command: curl
    secrets:
      token: {env: TOKEN}
    inject:
      command_args: ["${token}", "` + tc.arg + `"]
` + strictLists
			expectParseError(t, config, tc.want)
		})
	}
}

// An unused secret is most likely a typo, and its source would be obtained
// for nothing.
func TestParseRefusesAnUnusedSecret(t *testing.T) {
	config := `rules:
  - command: curl
    secrets:
      token: {env: TOKEN}
      spare: {env: SPARE}
    inject:
      command_args: ["${token}"]
` + strictLists
	expectParseError(t, config, `secret "spare" is not used`)
}

func TestParseRefusesABadSecret(t *testing.T) {
	expectParseError(t, "rules:\n  - command: curl\n    secrets:\n      Token: {env: TOKEN}\n    args: none\n", `secret "Token"`)
	expectParseError(t, "rules:\n  - command: curl\n    secrets:\n      token: {}\n    args: none\n", `secret "token": set its source`)
}

// $$ is a literal $, and a reference may sit inside a larger string.
func TestAssemblePlacesCommandArgsAfterTheRuleCommand(t *testing.T) {
	p := mustParse(t, `rules:
  - command: git push
    secrets:
      token: {env: TOKEN}
    inject:
      command_args: ["--push-option=auth $${token}=${token}"]
    flags:
      allow: []
    positional:
      allow: [origin]
`)
	argv := []string{"git", "push", "origin"}
	allowed, err := p.Check(argv)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := allowed.Assemble(argv, map[string]string{"token": "tok-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(assembled, "|")
	if want := "git|push|--push-option=auth ${token}=tok-0123456789|origin"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestParseRefusesABadCommandSource(t *testing.T) {
	for name, tc := range map[string]struct{ source, want string }{
		"empty command":     {"{command: []}", "command must name a program"},
		"two sources":       {"{command: [authtool], env: TOKEN}", "use only one of env, command"},
		"file and env":      {"{env: TOKEN, file: /etc/token}", "use only one of env, file"},
		"timeout with file": {"{file: /etc/token, timeout: 5s}", "timeout applies to a command source only"},
		"zero timeout":      {"{command: [authtool], timeout: 0s}", "timeout must be a positive duration"},
		"negative timeout":  {"{command: [authtool], timeout: -1s}", "timeout must be a positive duration"},
		"malformed timeout": {"{command: [authtool], timeout: soon}", "timeout must be a positive duration"},
		"timeout with env":  {"{env: TOKEN, timeout: 5s}", "timeout applies to a command source only"},
	} {
		t.Run(name, func(t *testing.T) {
			config := "rules:\n  - command: curl\n    secrets:\n      token: " + tc.source + "\n    inject:\n      command_args: [\"${token}\"]\n" + strictLists
			expectParseError(t, config, tc.want)
		})
	}
}

// A command source runs for 30 s unless its secret says otherwise.
func TestCheckReportsACommandSourceWithItsTimeout(t *testing.T) {
	for source, want := range map[string]time.Duration{
		"{command: [authtool, print-token]}":              30 * time.Second,
		"{command: [authtool, print-token], timeout: 2m}": 2 * time.Minute,
	} {
		p := mustParse(t, "rules:\n  - command: curl\n    secrets:\n      token: "+source+"\n    inject:\n      command_args: [\"${token}\"]\n"+strictLists)
		allowed, err := p.Check([]string{"curl", "https://api.example.com/v1/items/42"})
		if err != nil {
			t.Fatal(err)
		}
		got := allowed.Secrets[0]
		if got.Kind != SourceCommand || strings.Join(got.Command, " ") != "authtool print-token" || got.Timeout != want {
			t.Fatalf("%s: got %+v, want authtool print-token with timeout %s", source, got, want)
		}
	}
}

// A trailing argument is positional: a flag there would turn into one
// after a -- from the container. Flags go to the other two positions.
func TestParseRefusesATrailingArgumentThatIsAFlag(t *testing.T) {
	config := "rules:\n  - command: curl\n    inject:\n      trailing_args: [--verbose]\n    args: none\n"
	expectParseError(t, config, "inject.trailing_args")
}

// The template rules hold at every position.
func TestParseChecksTheTemplatesOfEveryPosition(t *testing.T) {
	for _, key := range []string{"program_args", "trailing_args"} {
		config := "rules:\n  - command: curl\n    secrets:\n      token: {env: TOKEN}\n    inject:\n      command_args: [\"${token}\"]\n      " + key + ": [\"${other}\"]\n" + strictLists
		expectParseError(t, config, "inject."+key)
	}
}

// A one-word command puts program_args before command_args.
func TestAssemblePlacesProgramArgsBeforeCommandArgsForAOneWordCommand(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: curl\n    inject:\n      program_args: [-s]\n      command_args: [-S]\n      trailing_args: [x]\n    args: any\n")
	argv := []string{"curl", "url"}
	allowed, err := p.Check(argv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := allowed.Assemble(argv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "curl -s -S url x"; strings.Join(got, " ") != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The daemon sets PWD to the working directory, and a name the shell could
// not read is most likely a typo.
func TestParseRefusesABadInjectedVariable(t *testing.T) {
	for name, tc := range map[string]struct{ env, want string }{
		"pwd":            {"{PWD: /tmp}", "PWD is set by hostrunner"},
		"dash":           {"{API-TOKEN: x}", `"API-TOKEN" is not a variable name`},
		"leading digit":  {"{1TOKEN: x}", `"1TOKEN" is not a variable name`},
		"unknown secret": {`{API_TOKEN: "${other}"}`, `unknown secret "other"`},
	} {
		t.Run(name, func(t *testing.T) {
			expectParseError(t, "rules:\n  - command: curl\n    inject:\n      env: "+tc.env+"\n    args: any\n", tc.want)
		})
	}
}

func TestEnvironmentRendersTheInjectedVariablesByName(t *testing.T) {
	p := mustParse(t, "rules:\n  - command: curl\n    secrets:\n      token: {env: TOKEN}\n    inject:\n      env: {NO_COLOR: \"1\", API_TOKEN: \"Bearer ${token}\"}\n"+strictLists)
	allowed, err := p.Check([]string{"curl"})
	if err != nil {
		t.Fatal(err)
	}
	got := allowed.Environment(map[string]string{"token": "tok-0123456789"})
	if want := "API_TOKEN=Bearer tok-0123456789|NO_COLOR=1"; strings.Join(got, "|") != want {
		t.Fatalf("got %q, want %s", got, want)
	}
}
