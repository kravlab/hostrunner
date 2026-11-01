package settings_test

import (
	"flag"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// restart takes the flags of stop and the paths of the new daemon's
// settings files: --workspace-settings and --rules-file, with --config.
func TestRegisterRestartAddsTheFlagsOfStopAndTheDaemonsPaths(t *testing.T) {
	var f settings.Flags
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	f.RegisterRestart(fs)
	var got []string
	fs.VisitAll(func(fl *flag.Flag) { got = append(got, fl.Name) })
	if want := []string{"config", "rules-file", "runtime-root", "settings", "stop-timeout", "workspace-settings"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := settings.RestartUsage(), "[--settings <path>] [--workspace-settings <path>] [--rules-file <path>] [--runtime-root <path>] [--stop-timeout <duration>]"; got != want {
		t.Fatalf("RestartUsage: got %q, want %q", got, want)
	}
}

// restart resolves its own settings with ResolveHost and the new
// daemon's, for the old daemon's workspace, with Resolve: each reads
// the keys and the flags of its own alone.
func TestRestartResolvesItsSettingsAndTheNewDaemonsApart(t *testing.T) {
	ws := t.TempDir()
	host := writeFile(t, "stop_timeout: 1m\ngrace: 2m\n"+section(ws, "poll_interval: 3s"))
	wsFile := writeFile(t, "ask_timeout: 4m\n")
	f := parseWith(t, (*settings.Flags).RegisterRestart,
		"--settings", host, "--workspace-settings", wsFile, "--rules-file", "/r/hostrun.yaml", "--runtime-root", "/root")
	vars := env(withXDG(map[string]string{"HOSTRUNNER_READY_TIMEOUT": "5s"}))

	own, err := f.ResolveHost(vars)
	if want := (settings.Settings{RuntimeRoot: "/root", StopTimeout: time.Minute}); err != nil || own != want {
		t.Fatalf("ResolveHost: got %+v, %v; want %+v", own, err, want)
	}
	got, err := f.Resolve(vars, ws)
	want := defaults
	want.RulesFile, want.Grace, want.PollInterval, want.AskTimeout, want.ReadyTimeout = "/r/hostrun.yaml", 2*time.Minute, 3*time.Second, 4*time.Minute, 5*time.Second
	if err != nil || got.Settings != want {
		t.Fatalf("Resolve: got %+v, %v; want %+v", got, err, want)
	}
}

// A relative --rules-file or --config of restart is read against the
// current directory, as for up.
func TestRestartRulesFileFlagIsReadAgainstTheCurrentDirectory(t *testing.T) {
	got, err := parseWith(t, (*settings.Flags).RegisterRestart, "--config", "rules.yaml").Resolve(env(nil), noWorkspace)
	if abs, _ := filepath.Abs("rules.yaml"); err != nil || got.RulesFile != abs {
		t.Fatalf("got %q, %v; want %q", got.RulesFile, err, abs)
	}
}

// A relative --workspace-settings of restart is read against the current
// directory, not the old daemon's workspace, as for up.
func TestRestartWorkspaceSettingsFlagIsReadAgainstTheCurrentDirectory(t *testing.T) {
	file := writeFile(t, "grace: 7m\n")
	t.Chdir(filepath.Dir(file))
	got, err := parseWith(t, (*settings.Flags).RegisterRestart, "--workspace-settings", filepath.Base(file)).Resolve(env(nil), t.TempDir())
	if err != nil || got.Grace != 7*time.Minute {
		t.Fatalf("got grace %v, %v; want 7m0s from the file beside", got.Grace, err)
	}
}
