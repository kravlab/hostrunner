package settings_test

import (
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// Only the flags given set their keys, even with their default value:
// every other key comes from the host settings file, which sets them all.
func TestFlagsSetOnlyTheKeysGiven(t *testing.T) {
	file := writeFile(t, "startup_timeout: 1h\ngrace: 1m\nreread: false\n")
	fromFile := tuned(defaults.RulesFile, time.Hour, time.Minute, false)
	tests := []struct {
		args []string
		want settings.Settings
	}{
		{nil, fromFile},
		{[]string{"--startup-timeout", "2h"}, tuned(defaults.RulesFile, 2*time.Hour, time.Minute, false)},
		{[]string{"--grace", "1m30s"}, tuned(defaults.RulesFile, time.Hour, 90*time.Second, false)},
		{[]string{"--reread"}, tuned(defaults.RulesFile, time.Hour, time.Minute, true)},
		{[]string{"--reread=false"}, fromFile},
		{[]string{"--no-reread=false"}, tuned(defaults.RulesFile, time.Hour, time.Minute, true)},
		// A flag given with its default value is still given.
		{[]string{"--grace", "15s", "--startup-timeout", "30m", "--reread"}, defaults},
	}
	for _, tt := range tests {
		got, err := parse(t, append(tt.args, "--settings", file)...).Resolve(env(nil), noWorkspace)
		if err != nil || got.Settings != tt.want {
			t.Errorf("%q: got %+v, %v; want %+v", tt.args, got, err, tt.want)
		}
	}
}

func TestNoRereadOverTheDefault(t *testing.T) {
	got, err := parse(t, "--no-reread").Resolve(env(nil), noWorkspace)
	if err != nil || got.Reread {
		t.Fatalf("got %+v, %v; want no rereading", got, err)
	}
}

func TestFlagsRejectNonPositiveDurations(t *testing.T) {
	for _, args := range [][]string{{"--grace", "0s"}, {"--startup-timeout", "-1m"}, {"--poll-interval", "0s"}, {"--ask-timeout", "-1s"}, {"--request-timeout", "0s"}} {
		_, err := parse(t, args...).Resolve(env(nil), noWorkspace)
		if want := args[0] + ": must be positive"; err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %q", args, err, want)
		}
		if _, ok := errors.AsType[*settings.FlagError](err); !ok {
			t.Errorf("%q: got %v, want a flag error", args, err)
		}
	}
}

func TestFlagsRejectRereadWithNoReread(t *testing.T) {
	_, err := parse(t, "--reread", "--no-reread").ResolveAsGiven(env(nil), noWorkspace)
	if err == nil || !strings.Contains(err.Error(), "--reread and --no-reread exclude each other") {
		t.Fatalf("got %v, want the conflict named", err)
	}
	if _, ok := errors.AsType[*settings.FlagError](err); !ok {
		t.Fatalf("got %v, want a flag error", err)
	}
}

func TestFlagsRejectRulesFileWithConfig(t *testing.T) {
	_, err := parse(t, "--rules-file", "a.yaml", "--config", "b.yaml").ResolveAsGiven(env(nil), noWorkspace)
	if want := "--rules-file and --config exclude each other"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if _, ok := errors.AsType[*settings.FlagError](err); !ok {
		t.Fatalf("got %v, want a flag error", err)
	}
}

func TestFlagsRejectAnEmptyRulesFile(t *testing.T) {
	for _, name := range []string{"--rules-file", "--config"} {
		_, err := parse(t, name, "").Resolve(env(nil), noWorkspace)
		if want := name + ": empty path"; err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %q", name, err, want)
		}
		if _, ok := errors.AsType[*settings.FlagError](err); !ok {
			t.Errorf("%s: got %v, want a flag error", name, err)
		}
	}
}

// RegisterRulesFile adds only the flags that choose the rules file; the
// settings are still resolved from every source.
func TestRegisterRulesFileAddsTheFlagsThatChooseTheRulesFile(t *testing.T) {
	var f settings.Flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.RegisterRulesFile(fs)
	var names []string
	fs.VisitAll(func(fl *flag.Flag) { names = append(names, fl.Name) })
	if want := []string{"config", "rules-file", "settings", "workspace-settings"}; !slices.Equal(names, want) {
		t.Fatalf("got flags %q, want %q", names, want)
	}
	host := writeFile(t, "grace: 1m\n")
	if err := fs.Parse([]string{"--settings", host, "--rules-file", "/r.yaml"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.Resolve(env(map[string]string{"HOSTRUNNER_REREAD": "false"}), noWorkspace)
	want := tuned("/r.yaml", defaults.StartupTimeout, time.Minute, false)
	if err != nil || got.Settings != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

// Register adds the flags of the keys serve takes; RegisterUp adds
// --ready-timeout too, up's own.
func TestRegisterUpAddsTheReadyTimeout(t *testing.T) {
	names := func(register func(*settings.Flags, *flag.FlagSet)) map[string]bool {
		var f settings.Flags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		register(&f, fs)
		got := map[string]bool{}
		fs.VisitAll(func(fl *flag.Flag) { got[fl.Name] = true })
		return got
	}
	serve := names((*settings.Flags).Register)
	for _, name := range []string{"poll-interval", "ask-timeout", "request-timeout"} {
		if !serve[name] {
			t.Errorf("Register: no --%s", name)
		}
	}
	if serve["ready-timeout"] {
		t.Error("Register: got --ready-timeout, which serve does not take")
	}
	if up := names((*settings.Flags).RegisterUp); !up["ready-timeout"] || len(up) != len(serve)+1 {
		t.Errorf("RegisterUp: got %v, want the flags of Register and --ready-timeout", up)
	}

	var f settings.Flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.RegisterUp(fs)
	if err := fs.Parse([]string{"--ready-timeout", "1m"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.Resolve(env(nil), noWorkspace)
	want := defaults
	want.ReadyTimeout = time.Minute
	if err != nil || got.Settings != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

// A command reads only the keys it takes: serve neither reads nor checks
// up's ready_timeout, in a variable or a file, while up does.
func TestResolveReadsOnlyTheKeysTheCommandTakes(t *testing.T) {
	host := writeFile(t, "ready_timeout: soon\n")
	ws := newWorkspace(t, "ready_timeout: soon\n")
	tests := []struct {
		name string
		vars map[string]string
		ws   string
	}{
		{"variable", map[string]string{"HOSTRUNNER_READY_TIMEOUT": "abc"}, noWorkspace},
		{"host settings file", map[string]string{"HOSTRUNNER_SETTINGS": host}, noWorkspace},
		{"workspace settings file", nil, ws},
	}
	for _, tt := range tests {
		serve := parse(t)
		got, err := serve.Resolve(env(tt.vars), tt.ws)
		if err != nil || got.ReadyTimeout != defaults.ReadyTimeout {
			t.Errorf("serve, %s: got %v, %v; want the default ready timeout", tt.name, got.ReadyTimeout, err)
		}
		var up settings.Flags
		fs := flag.NewFlagSet("up", flag.ContinueOnError)
		up.RegisterUp(fs)
		if _, err := up.Resolve(env(tt.vars), tt.ws); err == nil || !strings.Contains(err.Error(), "ready_timeout") && !strings.Contains(err.Error(), "READY_TIMEOUT") {
			t.Errorf("up, %s: got %v, want an error naming the ready timeout", tt.name, err)
		}
	}
	if _, err := parse(t).Resolve(env(map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, "ready_timeoot: 1s\n")}), noWorkspace); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("serve: got %v for an unknown key, want unknown key", err)
	}
}
