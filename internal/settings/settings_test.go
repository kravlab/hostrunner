package settings_test

import (
	"errors"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// defaults are the built-in settings of noWorkspace.
var defaults = settings.Settings{
	RulesFile:      rulesIn(noWorkspace),
	StartupTimeout: 30 * time.Minute,
	Grace:          15 * time.Second,
	Reread:         true,
	PollInterval:   2 * time.Second,
	AskTimeout:     10 * time.Minute,
	RequestTimeout: 10 * time.Second,
	ReadyTimeout:   10 * time.Second,
}

// tuned is defaults with the keys most tests set given.
func tuned(rulesFile string, startupTimeout, grace time.Duration, reread bool) settings.Settings {
	s := defaults
	s.RulesFile, s.StartupTimeout, s.Grace, s.Reread = rulesFile, startupTimeout, grace, reread
	return s
}

// noWorkspace is a workspace where nothing lies: it has no workspace
// settings file and no section of a host settings file names it.
const noWorkspace = "/nonexistent/workspace"

// rulesIn is the default rules file of the workspace at ws.
func rulesIn(ws string) string { return filepath.Join(ws, ".devcontainer", "hostrunner-rules.yaml") }

// env is a getenv over vars; every other variable is empty.
func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// writeFile writes content to a settings file in a temporary directory
// and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// parse registers the flags of the settings serve takes on a new flag
// set and parses args with it.
func parse(t *testing.T, args ...string) *settings.Flags {
	t.Helper()
	return parseWith(t, (*settings.Flags).Register, args...)
}

// parseUp is parse for the settings up takes, ready_timeout too.
func parseUp(t *testing.T, args ...string) *settings.Flags {
	t.Helper()
	return parseWith(t, (*settings.Flags).RegisterUp, args...)
}

// parseWith registers flags with register on a new flag set and parses
// args with it.
func parseWith(t *testing.T, register func(*settings.Flags, *flag.FlagSet), args ...string) *settings.Flags {
	t.Helper()
	var f settings.Flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	register(&f, fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return &f
}

func TestResolveGivesTheDefaultsWhenNoSourceSetsAKey(t *testing.T) {
	got, err := parse(t).Resolve(env(nil), noWorkspace)
	if err != nil || got.Settings != defaults {
		t.Fatalf("got %+v, %v; want %+v", got, err, defaults)
	}
}

// Each key comes from the highest source that sets it: flags, then the
// variables, then the host settings file, then the default.
func TestResolveTakesEachKeyFromTheHighestSourceSettingIt(t *testing.T) {
	file := "startup_timeout: 1h\ngrace: 1m\nreread: false\n"
	vars := map[string]string{"HOSTRUNNER_STARTUP_TIMEOUT": "2h", "HOSTRUNNER_GRACE": "2m", "HOSTRUNNER_REREAD": "true"}
	flags := []string{"--startup-timeout", "3h", "--grace", "3m", "--no-reread"}
	tests := []struct {
		name  string
		file  string
		vars  map[string]string
		flags []string
		want  settings.Settings
	}{
		{"defaults", "", nil, nil, defaults},
		{"file over defaults", file, nil, nil, tuned(defaults.RulesFile, time.Hour, time.Minute, false)},
		{"variables over file", file, vars, nil, tuned(defaults.RulesFile, 2*time.Hour, 2*time.Minute, true)},
		{"flags over variables", file, vars, flags, tuned(defaults.RulesFile, 3*time.Hour, 3*time.Minute, false)},
		{"one key per source", "grace: 1m\n", map[string]string{"HOSTRUNNER_REREAD": "false"}, []string{"--startup-timeout", "1h"}, tuned(defaults.RulesFile, time.Hour, time.Minute, false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, tt.file)}
			maps.Copy(vars, tt.vars)
			got, err := parse(t, tt.flags...).Resolve(env(vars), noWorkspace)
			if err != nil {
				t.Fatal(err)
			}
			if got.Settings != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveReadsEveryVariable(t *testing.T) {
	got, err := parseUp(t).Resolve(env(map[string]string{
		"HOSTRUNNER_RULES_FILE":      "/r/hostrun.yaml",
		"HOSTRUNNER_STARTUP_TIMEOUT": "45m",
		"HOSTRUNNER_GRACE":           "1m30s",
		"HOSTRUNNER_REREAD":          "0",
		"HOSTRUNNER_POLL_INTERVAL":   "5s",
		"HOSTRUNNER_ASK_TIMEOUT":     "20m",
		"HOSTRUNNER_REQUEST_TIMEOUT": "30s",
		"HOSTRUNNER_READY_TIMEOUT":   "1m",
	}), noWorkspace)
	want := settings.Settings{
		RulesFile:      "/r/hostrun.yaml",
		StartupTimeout: 45 * time.Minute,
		Grace:          90 * time.Second,
		Reread:         false,
		PollInterval:   5 * time.Second,
		AskTimeout:     20 * time.Minute,
		RequestTimeout: 30 * time.Second,
		ReadyTimeout:   time.Minute,
	}
	if err != nil || got.Settings != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestResolveTreatsAnEmptyVariableAsUnset(t *testing.T) {
	got, err := parse(t).Resolve(env(map[string]string{"HOSTRUNNER_GRACE": "", "HOSTRUNNER_REREAD": ""}), noWorkspace)
	if err != nil || got.Settings != defaults {
		t.Fatalf("got %+v, %v; want the defaults", got, err)
	}
}

func TestResolveRejectsAnInvalidVariable(t *testing.T) {
	tests := []struct {
		name, variable, value, want string
	}{
		{"unparsable duration", "HOSTRUNNER_GRACE", "abc", `HOSTRUNNER_GRACE: invalid duration "abc"`},
		{"duration without unit", "HOSTRUNNER_STARTUP_TIMEOUT", "30", `HOSTRUNNER_STARTUP_TIMEOUT: invalid duration "30"`},
		{"zero duration", "HOSTRUNNER_GRACE", "0s", `HOSTRUNNER_GRACE: must be positive`},
		{"negative duration", "HOSTRUNNER_STARTUP_TIMEOUT", "-5m", `HOSTRUNNER_STARTUP_TIMEOUT: must be positive`},
		{"unparsable bool", "HOSTRUNNER_REREAD", "maybe", `HOSTRUNNER_REREAD: invalid bool "maybe"`},
		{"zero poll interval", "HOSTRUNNER_POLL_INTERVAL", "0s", `HOSTRUNNER_POLL_INTERVAL: must be positive`},
		{"unparsable ask timeout", "HOSTRUNNER_ASK_TIMEOUT", "soon", `HOSTRUNNER_ASK_TIMEOUT: invalid duration "soon"`},
		{"request timeout without unit", "HOSTRUNNER_REQUEST_TIMEOUT", "10", `HOSTRUNNER_REQUEST_TIMEOUT: invalid duration "10"`},
		{"negative ready timeout", "HOSTRUNNER_READY_TIMEOUT", "-1s", `HOSTRUNNER_READY_TIMEOUT: must be positive`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseUp(t).Resolve(env(map[string]string{tt.variable: tt.value}), noWorkspace)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error starting with %q", err, tt.want)
			}
			if _, ok := errors.AsType[*settings.FlagError](err); ok {
				t.Fatalf("got %v as a flag error", err)
			}
		})
	}
}

// givenRules is the rules file everyKey gives.
const givenRules = "/given/hostrun.yaml"

// daemonFlags gives every setting but the rules file and reread that
// serve takes as a flag; givenTimings is what they give.
var daemonFlags = []string{"--startup-timeout", "1h", "--grace", "1m", "--poll-interval", "5s", "--ask-timeout", "20m", "--request-timeout", "30s"}

// givenTimings is defaults with what daemonFlags gives, the rules file
// given and reread off.
func givenTimings(rulesFile string) settings.Settings {
	s := tuned(rulesFile, time.Hour, time.Minute, false)
	s.PollInterval, s.AskTimeout, s.RequestTimeout = 5*time.Second, 20*time.Minute, 30*time.Second
	return s
}

// everyKey gives every setting serve takes as a flag: all but
// ready_timeout, which is up's own.
var everyKey = append([]string{"--rules-file", givenRules, "--no-reread"}, daemonFlags...)

// Resolve reads every source even when the flags set every key: a
// missing named file, a broken file or an invalid variable is an error.
func TestResolveReadsEverySourceWhenFlagsSetEveryKey(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  string
	}{
		{"missing --settings file", []string{"--settings", missing}, nil, missing},
		{"broken file", []string{"--settings", writeFile(t, "grase: [\n")}, nil, "settings file"},
		{"invalid variable", nil, map[string]string{"HOSTRUNNER_GRACE": "abc"}, "HOSTRUNNER_GRACE"},
	}
	for _, tt := range tests {
		_, err := parse(t, append(everyKey, tt.flags...)...).Resolve(env(tt.vars), noWorkspace)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
}

// ResolveAsGiven reads neither the variables nor any file when the flags
// set every key: a broken settings file is not even opened.
func TestResolveAsGivenReadsNoOtherSourceWhenFlagsSetEveryKey(t *testing.T) {
	getenv := func(name string) string {
		t.Errorf("read %s", name)
		return ""
	}
	got, err := parse(t, append(everyKey, "--settings", writeFile(t, "grase: [\n"))...).ResolveAsGiven(getenv, noWorkspace)
	want := givenTimings(givenRules)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	// --config gives rules_file as --rules-file does.
	got, err = parse(t, append([]string{"--config", givenRules, "--no-reread"}, daemonFlags...)...).ResolveAsGiven(getenv, noWorkspace)
	if err != nil || got != want {
		t.Fatalf("--config: got %+v, %v; want %+v", got, err, want)
	}
}

// With a key no flag sets, ResolveAsGiven resolves like Resolve.
func TestResolveAsGivenReadsEverySourceWhenAFlagIsMissing(t *testing.T) {
	path := writeFile(t, "grace: 2m\n")
	got, err := parse(t, "--startup-timeout", "1h", "--reread").ResolveAsGiven(env(map[string]string{"HOSTRUNNER_SETTINGS": path}), noWorkspace)
	want := tuned(defaults.RulesFile, time.Hour, 2*time.Minute, true)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if _, err := parse(t, "--grace", "1m").ResolveAsGiven(env(map[string]string{"HOSTRUNNER_GRACE": "abc"}), noWorkspace); err == nil {
		t.Fatal("got no error for an invalid variable")
	}
	if _, err := parse(t, append([]string{"--reread"}, daemonFlags...)...).ResolveAsGiven(env(map[string]string{"HOSTRUNNER_GRACE": "abc"}), noWorkspace); err == nil {
		t.Fatal("without --rules-file: got no error for an invalid variable")
	}
	if _, err := parse(t, "--rules-file", givenRules, "--reread", "--startup-timeout", "1h", "--grace", "1m").ResolveAsGiven(env(map[string]string{"HOSTRUNNER_GRACE": "abc"}), noWorkspace); err == nil {
		t.Fatal("without --poll-interval, --ask-timeout and --request-timeout: got no error for an invalid variable")
	}
}

// rules_file comes from every source. A relative path is read against the
// workspace when a file or a variable gives it, wherever that file lies,
// and against the current directory when a flag does.
func TestResolveTakesTheRulesFileFromEverySource(t *testing.T) {
	ws, cwd := t.TempDir(), t.TempDir()
	t.Chdir(cwd)
	tests := []struct {
		name         string
		host, wsFile string
		vars         map[string]string
		flags        []string
		want         string
	}{
		{"default", "", "", nil, nil, rulesIn(ws)},
		{"common part, relative", "rules_file: common.yaml\n", "", nil, nil, filepath.Join(ws, "common.yaml")},
		{"common part, absolute", "rules_file: /etc/common.yaml\n", "", nil, nil, "/etc/common.yaml"},
		{"common part, ~", "rules_file: ~/rules/app.yaml\n", "", map[string]string{"HOME": "/home/u"}, nil, "/home/u/rules/app.yaml"},
		{"variable, ~", "", "", map[string]string{"HOME": "/home/u", "HOSTRUNNER_RULES_FILE": "~/var.yaml"}, nil, "/home/u/var.yaml"},
		{"section, relative", "rules_file: common.yaml\n" + section(ws, "rules_file: rules/section.yaml"), "", nil, nil, filepath.Join(ws, "rules", "section.yaml")},
		{"workspace file, relative", "rules_file: common.yaml\n", "rules_file: ws.yaml\n", nil, nil, filepath.Join(ws, "ws.yaml")},
		{"variable, relative", "", "rules_file: ws.yaml\n", map[string]string{"HOSTRUNNER_RULES_FILE": "var.yaml"}, nil, filepath.Join(ws, "var.yaml")},
		{"variable, absolute", "", "", map[string]string{"HOSTRUNNER_RULES_FILE": "/etc/var.yaml"}, nil, "/etc/var.yaml"},
		{"empty variable", "", "rules_file: ws.yaml\n", map[string]string{"HOSTRUNNER_RULES_FILE": ""}, nil, filepath.Join(ws, "ws.yaml")},
		{"--rules-file, relative", "", "", map[string]string{"HOSTRUNNER_RULES_FILE": "var.yaml"}, []string{"--rules-file", "flag.yaml"}, filepath.Join(cwd, "flag.yaml")},
		{"--rules-file, absolute", "", "", nil, []string{"--rules-file", "/etc/flag.yaml"}, "/etc/flag.yaml"},
		{"--config, relative", "", "", map[string]string{"HOSTRUNNER_RULES_FILE": "var.yaml"}, []string{"--config", "flag.yaml"}, filepath.Join(cwd, "flag.yaml")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, tt.host)}
			if tt.wsFile != "" {
				vars["HOSTRUNNER_WORKSPACE_SETTINGS"] = writeFile(t, tt.wsFile)
			}
			maps.Copy(vars, tt.vars)
			got, err := parse(t, tt.flags...).Resolve(env(vars), ws)
			if err != nil || got.RulesFile != tt.want {
				t.Fatalf("got rules file %q, %v; want %q", got.RulesFile, err, tt.want)
			}
		})
	}
}

// A ~ that cannot be expanded, HOME being unset, is an error naming the
// source, not a directory named ~ in the workspace.
func TestResolveRefusesATildeWithoutHome(t *testing.T) {
	ws := t.TempDir()
	host, wsFile := writeFile(t, "rules_file: ~/common.yaml\n"), writeFile(t, "rules_file: ~/ws.yaml\n")
	withSection := writeFile(t, section(ws, "rules_file: ~/section.yaml"))
	const problem = "rules_file: ~ needs HOME, which is not set"
	for _, tt := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"common part", map[string]string{"HOSTRUNNER_SETTINGS": host}, "settings file " + host + ": " + problem},
		{"section", map[string]string{"HOSTRUNNER_SETTINGS": withSection}, "settings file " + withSection + ": workspaces." + ws + ": " + problem},
		{"workspace file", map[string]string{"HOSTRUNNER_WORKSPACE_SETTINGS": wsFile}, "settings file " + wsFile + ": " + problem},
		{"variable", map[string]string{"HOSTRUNNER_RULES_FILE": "~/var.yaml"}, "HOSTRUNNER_RULES_FILE: ~ needs HOME, which is not set"},
	} {
		if _, err := parse(t).Resolve(env(tt.vars), ws); err == nil || err.Error() != tt.want {
			t.Errorf("%s: got %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestUsageNamesEveryFlagServeTakes(t *testing.T) {
	want := "[--settings <path>] [--workspace-settings <path>] [--rules-file <path>] [--startup-timeout <duration>] [--grace <duration>] [--reread | --no-reread] [--poll-interval <duration>] [--ask-timeout <duration>] [--request-timeout <duration>]"
	if got := settings.Usage(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUpUsageAlsoNamesTheReadyTimeout(t *testing.T) {
	want := settings.Usage() + " [--ready-timeout <duration>]"
	if got := settings.UpUsage(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRulesFileUsageNamesTheFlagsThatChooseTheRulesFile(t *testing.T) {
	want := "[--settings <path>] [--workspace-settings <path>] [--rules-file <path>]"
	if got := settings.RulesFileUsage(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
