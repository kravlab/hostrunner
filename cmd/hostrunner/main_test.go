package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/instructions"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/settings"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/update"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

func TestBuildVersionReportsTheModuleVersion(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"release tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, true, "v0.1.0"},
		{"pseudo-version", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1-0.20261001120000-abcdef123456+dirty"}}, true, "v0.1.1-0.20261001120000-abcdef123456+dirty"},
		{"no VCS data", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"empty version", &debug.BuildInfo{}, true, "dev"},
		{"no build info", nil, false, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion(tt.info, tt.ok); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A test binary carries no VCS data, so its version is "dev".
func TestRunVersionPrintsTheVersion(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"version"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "dev\n" {
		t.Fatalf("got %q, want %q", got, "dev\n")
	}
}

func TestRunVersionRejectsArguments(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"version", "extra"}, &out, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected argument")
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q despite the error", out.String())
	}
}

func TestParseVersionHelpIsTheFlagHelp(t *testing.T) {
	if err := parseVersion([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}

func TestParseServeAcceptsAllFlags(t *testing.T) {
	got, err := parseServe([]string{"--socket", "/run/h.sock", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app"}, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := serveConfig{
		socket:         "/run/h.sock",
		workspaceFlags: workspaceFlags{workspace: "/home/u/app", containerWorkspace: "/workspaces/app"},
		Settings:       tuned("/home/u/app/.devcontainer/hostrun.yaml", 30*time.Minute, 15*time.Second, true),
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseServeAcceptsWatchSettings(t *testing.T) {
	got, err := parseServe([]string{
		"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c",
		"--watch", "--startup-timeout", "5m", "--grace", "3s",
	}, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !got.watch || got.StartupTimeout != 5*time.Minute || got.Grace != 3*time.Second {
		t.Fatalf("got %+v, want watch with 5m startup timeout and 3s grace", got)
	}
}

func TestParseServeRequiresEveryFlag(t *testing.T) {
	full := map[string]string{"--socket": "/run/h.sock", "--workspace": "/home/u/app", "--container-workspace": "/workspaces/app"}
	for missing := range full {
		var args []string
		for flag, value := range full {
			if flag != missing {
				args = append(args, flag, value)
			}
		}
		if _, err := parseServe(args, noEnv, io.Discard); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

func TestParseServeRejectsPositionalArguments(t *testing.T) {
	args := []string{"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c", "extra"}
	if _, err := parseServe(args, noEnv, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

func TestParseUpBuildsLaunchConfig(t *testing.T) {
	exe := "/home/u/.local/bin/hostrunner"
	got, err := parseUp([]string{"--dir", "/run/user/1000/hostrunner/abc", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app"}, exe, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := launch.Config{
		Dir:                "/run/user/1000/hostrunner/abc",
		Workspace:          "/home/u/app",
		ContainerWorkspace: "/workspaces/app",
		Daemon:             exe,
		Client:             filepath.Join("/home/u/.local/bin", "hostrun"),
		Settings:           tuned("/home/u/app/.devcontainer/hostrun.yaml", 30*time.Minute, 15*time.Second, true),
	}
	if got.launch != want || got.restart || got.answer != stop.Ask {
		t.Fatalf("got %+v, want %+v without --restart", got, want)
	}
}

// up and serve make a relative --workspace absolute against the current
// directory, so both resolve the same rules file, and their settings
// match on every arm.
func TestUpAndServeMakeTheWorkspaceAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(cwd, "app")
	up, err := parseUp([]string{"--dir", "/d", "--workspace", "app", "--container-workspace", "/c"}, "/bin/hostrunner", noEnv, io.Discard)
	if err != nil || up.launch.Workspace != ws || up.launch.RulesFile != filepath.Join(ws, ".devcontainer", "hostrun.yaml") {
		t.Fatalf("up: got %q and %q, %v; want %q", up.launch.Workspace, up.launch.RulesFile, err, ws)
	}
	serve, err := parseServe([]string{"--socket", "/run/h.sock", "--workspace", "app", "--container-workspace", "/c"}, noEnv, io.Discard)
	if err != nil || serve.workspace != ws || serve.RulesFile != filepath.Join(ws, ".devcontainer", "hostrun.yaml") {
		t.Fatalf("serve: got %q and %q, %v; want %q", serve.workspace, serve.RulesFile, err, ws)
	}
}

func TestParseUpRequiresEveryFlag(t *testing.T) {
	full := map[string]string{"--dir": "/d", "--workspace": "/w", "--container-workspace": "/c"}
	for missing := range full {
		var args []string
		for flag, value := range full {
			if flag != missing {
				args = append(args, flag, value)
			}
		}
		if _, err := parseUp(args, "/bin/hostrunner", noEnv, io.Discard); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

// --no-reread keeps a daemon from rereading its rules file by itself: up
// passes it on to the daemon it starts.
func TestParseNoReread(t *testing.T) {
	up, err := parseUp([]string{"--dir", "/d", "--workspace", "/w", "--container-workspace", "/c", "--no-reread"}, "/bin/hostrunner", noEnv, io.Discard)
	if err != nil || up.launch.Reread {
		t.Fatalf("up: got %+v, %v; want no reread", up.launch, err)
	}
	sv, err := parseServe([]string{"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c", "--no-reread"}, noEnv, io.Discard)
	if err != nil || sv.Reread {
		t.Fatalf("serve: got %+v, %v; want no reread", sv, err)
	}
}

// tuned is the built-in settings with the rules file, the startup
// timeout, the grace and reread given.
func tuned(rulesFile string, startupTimeout, grace time.Duration, reread bool) settings.Settings {
	return settings.Settings{
		RulesFile:      rulesFile,
		StartupTimeout: startupTimeout,
		Grace:          grace,
		Reread:         reread,
		PollInterval:   2 * time.Second,
		AskTimeout:     10 * time.Minute,
		RequestTimeout: 10 * time.Second,
		ReadyTimeout:   10 * time.Second,
	}
}

// noEnv is a getenv for an empty environment: no HOSTRUNNER_* variable,
// and no HOME or XDG_CONFIG_HOME, so no default host settings file.
func noEnv(string) string { return "" }

// envOf is a getenv over vars; every other variable is empty.
func envOf(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// settingsFile writes content to a host settings file in a temporary
// directory and returns its path.
func settingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// resolveUp parses `hostrunner up` with flags after the required ones
// and returns the settings it gives the daemon.
func resolveUp(flags []string, getenv func(string) string) (settings.Settings, error) {
	args := append([]string{"--dir", "/d", "--workspace", "/w", "--container-workspace", "/c"}, flags...)
	cfg, err := parseUp(args, "/bin/hostrunner", getenv, io.Discard)
	return cfg.launch.Settings, err
}

// resolveServe parses `hostrunner serve` with flags after the required
// ones and returns the settings it runs with.
func resolveServe(flags []string, getenv func(string) string) (settings.Settings, error) {
	args := append([]string{"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c"}, flags...)
	cfg, err := parseServe(args, getenv, io.Discard)
	return cfg.Settings, err
}

// commands are the parsers that resolve the settings, by command name.
var commands = map[string]func([]string, func(string) string) (settings.Settings, error){"up": resolveUp, "serve": resolveServe}

// up and serve take each setting from the highest source that sets it:
// the flag, the HOSTRUNNER_* variable, the host settings file, then the
// default.
func TestUpAndServeResolveEachSettingFromTheHighestSource(t *testing.T) {
	file := "startup_timeout: 1h\ngrace: 1m\nreread: false\n"
	defaultRules := filepath.Join("/w", ".devcontainer", "hostrun.yaml")
	vars := map[string]string{"HOSTRUNNER_STARTUP_TIMEOUT": "2h", "HOSTRUNNER_GRACE": "2m", "HOSTRUNNER_REREAD": "true"}
	tests := []struct {
		name  string
		file  string
		vars  map[string]string
		flags []string
		want  settings.Settings
	}{
		{"defaults", "", nil, nil, tuned(defaultRules, 30*time.Minute, 15*time.Second, true)},
		{"host settings file", file, nil, nil, tuned(defaultRules, time.Hour, time.Minute, false)},
		{"variables over the file", file, vars, nil, tuned(defaultRules, 2*time.Hour, 2*time.Minute, true)},
		{"flags over the variables", file, vars, []string{"--startup-timeout", "3h", "--grace", "3m", "--no-reread"}, tuned(defaultRules, 3*time.Hour, 3*time.Minute, false)},
		{"--reread over the file", file, nil, []string{"--reread"}, tuned(defaultRules, time.Hour, time.Minute, true)},
	}
	for name, resolve := range commands {
		for _, tt := range tests {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				env := map[string]string{"HOSTRUNNER_SETTINGS": settingsFile(t, tt.file)}
				maps.Copy(env, tt.vars)
				got, err := resolve(tt.flags, envOf(env))
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Fatalf("got %+v, want %+v", got, tt.want)
				}
			})
		}
	}
}

// --settings names the host settings file and beats HOSTRUNNER_SETTINGS;
// without either it is the one under $XDG_CONFIG_HOME, else $HOME/.config.
func TestUpAndServeFindTheHostSettingsFile(t *testing.T) {
	flagFile, varFile := settingsFile(t, "grace: 1m\n"), settingsFile(t, "grace: 2m\n")
	xdg, home := t.TempDir(), t.TempDir()
	for dir, grace := range map[string]string{filepath.Join(xdg, "hostrunner"): "3m", filepath.Join(home, ".config", "hostrunner"): "4m"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("grace: "+grace+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  time.Duration
	}{
		{"--settings", []string{"--settings", flagFile}, nil, time.Minute},
		{"HOSTRUNNER_SETTINGS", nil, map[string]string{"HOSTRUNNER_SETTINGS": varFile}, 2 * time.Minute},
		{"--settings beats HOSTRUNNER_SETTINGS", []string{"--settings", flagFile}, map[string]string{"HOSTRUNNER_SETTINGS": varFile}, time.Minute},
		{"XDG_CONFIG_HOME", nil, map[string]string{"XDG_CONFIG_HOME": xdg, "HOME": home}, 3 * time.Minute},
		{"HOME", nil, map[string]string{"HOME": home}, 4 * time.Minute},
		{"missing default file", nil, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, 15 * time.Second},
	}
	for name, resolve := range commands {
		for _, tt := range tests {
			got, err := resolve(tt.flags, envOf(tt.vars))
			if err != nil || got.Grace != tt.want {
				t.Errorf("%s, %s: got grace %v, %v; want %v", name, tt.name, got.Grace, err, tt.want)
			}
		}
	}
}

// An invalid setting fails up (and so `devcontainer up`) and serve, the
// error naming its source and key.
func TestUpAndServeFailOnAnInvalidSetting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	unknown := settingsFile(t, "grase: 1m\n")
	invalid := settingsFile(t, "grace: soon\n")
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  string
	}{
		{"missing --settings file", []string{"--settings", missing}, nil, missing},
		{"missing HOSTRUNNER_SETTINGS file", nil, map[string]string{"HOSTRUNNER_SETTINGS": missing}, missing},
		{"unknown key", []string{"--settings", unknown}, nil, "settings file " + unknown + ": grase: unknown key"},
		{"invalid value in the file", []string{"--settings", invalid}, nil, "settings file " + invalid + `: grace: invalid duration "soon"`},
		{"invalid variable", nil, map[string]string{"HOSTRUNNER_GRACE": "abc"}, `HOSTRUNNER_GRACE: invalid duration "abc"`},
		{"zero variable", nil, map[string]string{"HOSTRUNNER_STARTUP_TIMEOUT": "0s"}, "HOSTRUNNER_STARTUP_TIMEOUT: must be positive"},
		{"invalid bool variable", nil, map[string]string{"HOSTRUNNER_REREAD": "maybe"}, `HOSTRUNNER_REREAD: invalid bool "maybe"`},
		{"zero flag", []string{"--grace", "0s"}, nil, "--grace: must be positive"},
		{"negative flag", []string{"--startup-timeout", "-1m"}, nil, "--startup-timeout: must be positive"},
		{"--reread with --no-reread", []string{"--reread", "--no-reread"}, nil, "--reread and --no-reread exclude each other"},
	}
	for name, resolve := range commands {
		for _, tt := range tests {
			if _, err := resolve(tt.flags, envOf(tt.vars)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s, %s: got %v, want an error containing %q", name, tt.name, err, tt.want)
			}
		}
	}
}

// A serve given every setting as a flag, as up starts it, reads neither
// the variables nor any settings file: a broken one is ignored.
func TestServeGivenEverySettingReadsNoOtherSource(t *testing.T) {
	getenv := func(name string) string {
		t.Errorf("read %s", name)
		return ""
	}
	flags := []string{"--settings", settingsFile(t, "grase: [\n"), "--workspace-settings", settingsFile(t, "grase: [\n"), "--rules-file", "/r.yaml", "--startup-timeout", "1h", "--grace", "1m", "--reread", "--poll-interval", "1s", "--ask-timeout", "1m", "--request-timeout", "1s"}
	got, err := resolveServe(flags, getenv)
	want := tuned("/r.yaml", time.Hour, time.Minute, true)
	want.PollInterval, want.AskTimeout, want.RequestTimeout = time.Second, time.Minute, time.Second
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

// up and serve read the workspace's section of the host settings file
// and the workspace settings file: .devcontainer/hostrunner.yaml in the
// workspace, or the one --workspace-settings (relative to the current
// directory) or HOSTRUNNER_WORKSPACE_SETTINGS (relative to the workspace)
// names, which must exist. Each is over the one before it and under the
// variables.
func TestUpAndServeReadTheWorkspaceSettings(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{".devcontainer/hostrunner.yaml": "grace: 3m\n", "var.yaml": "grace: 5m\n", "refused.yaml": "stop_timeout: 1m\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "flag.yaml"), []byte("grace: 4m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	host := settingsFile(t, fmt.Sprintf("grace: 1m\nworkspaces:\n  %q:\n    grace: 2m\n", ws))
	sectionOnly := settingsFile(t, fmt.Sprintf("grace: 1m\nworkspaces:\n  %q:\n    grace: 2m\n", filepath.Join(ws, ".devcontainer")))
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  time.Duration
	}{
		{"default workspace file over the section", nil, map[string]string{"HOSTRUNNER_SETTINGS": host}, 3 * time.Minute},
		{"another workspace's section", []string{"--workspace-settings", filepath.Join(ws, "empty.yaml")}, map[string]string{"HOSTRUNNER_SETTINGS": sectionOnly}, time.Minute},
		{"--workspace-settings", []string{"--workspace-settings", "flag.yaml"}, map[string]string{"HOSTRUNNER_SETTINGS": host}, 4 * time.Minute},
		{"HOSTRUNNER_WORKSPACE_SETTINGS", nil, map[string]string{"HOSTRUNNER_SETTINGS": host, "HOSTRUNNER_WORKSPACE_SETTINGS": "var.yaml"}, 5 * time.Minute},
		{"variable over the workspace file", nil, map[string]string{"HOSTRUNNER_SETTINGS": host, "HOSTRUNNER_GRACE": "6m"}, 6 * time.Minute},
	}
	if err := os.WriteFile(filepath.Join(ws, "empty.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, resolve := range commands {
		for _, tt := range tests {
			// A later --workspace replaces the /w resolve gives.
			got, err := resolve(append([]string{"--workspace", ws}, tt.flags...), envOf(tt.vars))
			if err != nil || got.Grace != tt.want {
				t.Errorf("%s, %s: got grace %v, %v; want %v", name, tt.name, got.Grace, err, tt.want)
			}
		}
		without := t.TempDir()
		if got, err := resolve([]string{"--workspace", without, "--settings", host}, noEnv); err != nil || got.Grace != time.Minute {
			t.Errorf("%s, no workspace settings file: got grace %v, %v; want 1m", name, got.Grace, err)
		}
		missing := filepath.Join(without, "missing.yaml")
		for _, args := range [][]string{{"--workspace-settings", missing}, {"--workspace-settings", filepath.Join(ws, "refused.yaml")}} {
			if _, err := resolve(append([]string{"--workspace", ws}, args...), noEnv); err == nil || !strings.Contains(err.Error(), args[1]) {
				t.Errorf("%s, %q: got %v, want an error naming %s", name, args, err, args[1])
			}
		}
		if _, err := resolve([]string{"--workspace", without}, envOf(map[string]string{"HOSTRUNNER_WORKSPACE_SETTINGS": "missing.yaml"})); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("%s, missing HOSTRUNNER_WORKSPACE_SETTINGS file: got %v, want an error naming %s", name, err, missing)
		}
	}
}

// An up given every setting as a flag still reads every other source, so
// a missing named file, a broken file or an invalid variable fails it.
func TestUpGivenEverySettingStillReadsEverySource(t *testing.T) {
	all := []string{"--rules-file", "/r.yaml", "--startup-timeout", "1m", "--grace", "1s", "--reread", "--poll-interval", "1s", "--ask-timeout", "1m", "--request-timeout", "1s", "--ready-timeout", "1s"}
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	broken := settingsFile(t, "grase: 1s\n")
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  string
	}{
		{"missing --settings file", []string{"--settings", missing}, nil, missing},
		{"broken file", []string{"--settings", broken}, nil, "grase: unknown key"},
		{"invalid variable", nil, map[string]string{"HOSTRUNNER_GRACE": "abc"}, "HOSTRUNNER_GRACE"},
	}
	for _, tt := range tests {
		if _, err := resolveUp(append(all, tt.flags...), envOf(tt.vars)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
}

// The usages name the settings' flags.
func TestUsagesNameTheSettingsFlags(t *testing.T) {
	for name, u := range map[string]string{"up": upUsage, "serve": serveUsage} {
		for _, f := range []string{"[--settings <path>]", "[--workspace-settings <path>]", "[--startup-timeout <duration>]", "[--grace <duration>]", "[--reread | --no-reread]", "[--poll-interval <duration>]", "[--ask-timeout <duration>]", "[--request-timeout <duration>]"} {
			if !strings.Contains(u, f) {
				t.Errorf("%s usage %q lacks %s", name, u, f)
			}
		}
	}
	for name, u := range map[string]string{"stop": stopUsage, "restart": restartUsage, "approve": approveUsage} {
		for _, f := range []string{"[--settings <path>]", "[--runtime-root <path>]"} {
			if !strings.Contains(u, f) {
				t.Errorf("%s usage %q lacks %s", name, u, f)
			}
		}
		if hasStop := strings.Contains(u, "[--stop-timeout <duration>]"); hasStop != (name != "approve") {
			t.Errorf("%s usage %q: --stop-timeout named %v", name, u, hasStop)
		}
	}
	for name, u := range map[string]string{"stop": stopUsage, "restart": restartUsage} {
		for _, f := range []string{"[--workspace-settings <path>]", "[--rules-file <path>]"} {
			if strings.Contains(u, f) != (name == "restart") {
				t.Errorf("%s usage %q: %s named %v", name, u, f, name != "restart")
			}
		}
	}
	if !strings.Contains(upUsage, "[--ready-timeout <duration>]") {
		t.Errorf("up usage %q lacks --ready-timeout", upUsage)
	}
	if strings.Contains(serveUsage, "--ready-timeout") {
		t.Errorf("serve usage %q names --ready-timeout, which serve does not take", serveUsage)
	}
}

// up and serve take the poll interval, the ask timeout and the request
// timeout from the settings file, their variables and their flags; up
// also takes its ready timeout, which serve neither reads nor checks.
func TestUpAndServeResolveTheTimings(t *testing.T) {
	file := "poll_interval: 3s\nask_timeout: 3m\nrequest_timeout: 3s\nready_timeout: 3s\n"
	vars := map[string]string{"HOSTRUNNER_POLL_INTERVAL": "4s", "HOSTRUNNER_ASK_TIMEOUT": "4m", "HOSTRUNNER_REQUEST_TIMEOUT": "4s", "HOSTRUNNER_READY_TIMEOUT": "4s"}
	flags := []string{"--poll-interval", "5s", "--ask-timeout", "5m", "--request-timeout", "5s"}
	timings := func(d time.Duration) settings.Settings {
		s := tuned(filepath.Join("/w", ".devcontainer", "hostrun.yaml"), 30*time.Minute, 15*time.Second, true)
		s.PollInterval, s.AskTimeout, s.RequestTimeout, s.ReadyTimeout = d, time.Duration(d.Seconds())*time.Minute, d, d
		return s
	}
	tests := []struct {
		name  string
		vars  map[string]string
		flags []string
		want  settings.Settings
	}{
		{"settings file", nil, nil, timings(3 * time.Second)},
		{"variables over the file", vars, nil, timings(4 * time.Second)},
	}
	for name, resolve := range commands {
		for _, tt := range tests {
			env := map[string]string{"HOSTRUNNER_SETTINGS": settingsFile(t, file)}
			maps.Copy(env, tt.vars)
			want := tt.want
			if name == "serve" {
				want.ReadyTimeout = 10 * time.Second
			}
			if got, err := resolve(tt.flags, envOf(env)); err != nil || got != want {
				t.Errorf("%s, %s: got %+v, %v; want %+v", name, tt.name, got, err, want)
			}
		}
	}
	env := map[string]string{"HOSTRUNNER_SETTINGS": settingsFile(t, file)}
	maps.Copy(env, vars)
	want := timings(5 * time.Second)
	want.ReadyTimeout = 10 * time.Second
	if got, err := resolveServe(flags, envOf(env)); err != nil || got != want {
		t.Errorf("serve, flags over the variables: got %+v, %v; want %+v", got, err, want)
	}
	want.ReadyTimeout = 5 * time.Second
	if got, err := resolveUp(append(flags, "--ready-timeout", "5s"), envOf(env)); err != nil || got != want {
		t.Errorf("up, flags over the variables: got %+v, %v; want %+v", got, err, want)
	}
	if _, err := resolveServe([]string{"--ready-timeout", "5s"}, noEnv); err == nil {
		t.Error("serve: got no error for --ready-timeout")
	}
	if _, err := resolveServe(nil, envOf(map[string]string{"HOSTRUNNER_READY_TIMEOUT": "soon"})); err != nil {
		t.Errorf("serve: got %v for an invalid HOSTRUNNER_READY_TIMEOUT, which it does not read", err)
	}
	if _, err := resolveUp(nil, envOf(map[string]string{"HOSTRUNNER_READY_TIMEOUT": "soon"})); err == nil {
		t.Error("up: got no error for an invalid HOSTRUNNER_READY_TIMEOUT")
	}
}

func TestParseUpRejectsPositionalArguments(t *testing.T) {
	if _, err := parseUp([]string{"--dir", "/d", "--workspace", "/w", "--container-workspace", "/c", "extra"}, "/bin/hostrunner", noEnv, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

// watchConfig is a `serve --watch` over a temporary workspace with short
// timings.
func watchConfig(t *testing.T) serveConfig {
	t.Helper()
	base, err := os.MkdirTemp("", "hrs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	return serveConfig{
		socket:         filepath.Join(base, "h.sock"),
		workspaceFlags: workspaceFlags{workspace: base, containerWorkspace: "/workspaces/app"},
		watch:          true,
		Settings: settings.Settings{
			StartupTimeout: 400 * time.Millisecond,
			Grace:          100 * time.Millisecond,
			Reread:         true,
			PollInterval:   5 * time.Millisecond,
			AskTimeout:     10 * time.Minute,
			RequestTimeout: 10 * time.Second,
		},
	}
}

// startServe runs serve in the background and returns its result channel
// once its socket and its approval socket accept connections. A serve
// still running when the test ends is stopped then.
func startServe(t *testing.T, cfg serveConfig, runtimes ...*watchtest.Runtime) <-chan error {
	t.Helper()
	rts := make([]watch.Runtime, len(runtimes))
	for i, rt := range runtimes {
		rts[i] = rt
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- serve(ctx, cfg, slog.New(slog.DiscardHandler), rts)
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
	})
	waitListening(t, cfg.socket)
	waitListening(t, launch.ApprovalSocket(filepath.Dir(cfg.socket)))
	return done
}

// waitListening waits until the Unix socket at path accepts connections.
func waitListening(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing listens on %s", path)
		}
	}
}

// expectServeExit waits for serve to return nil and checks the socket is gone.
func expectServeExit(t *testing.T, done <-chan error, socket string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve --watch did not exit")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestServeWatchExitsWhenContainerStops(t *testing.T) {
	cfg := watchConfig(t)
	rt := &watchtest.Runtime{}
	done := startServe(t, cfg, rt)
	rt.Start("c")
	time.Sleep(50 * time.Millisecond)
	rt.Stop("c")
	expectServeExit(t, done, cfg.socket)
}

func TestServeWatchExitsWhenContainerNeverStarts(t *testing.T) {
	cfg := watchConfig(t)
	expectServeExit(t, startServe(t, cfg, &watchtest.Runtime{}), cfg.socket)
}

func TestServeWatchWaitsAgainWhenArmed(t *testing.T) {
	cfg := watchConfig(t)
	rt := &watchtest.Runtime{}
	rt.Start("c")
	done := startServe(t, cfg, rt)
	time.Sleep(50 * time.Millisecond)

	rt.Stop("c") // rebuild: old container removed, image building
	if stepAside := armServe(t, cfg, rulesDigest(t, cfg)); stepAside {
		t.Fatal("serve stepped aside although the rules did not change")
	}

	select {
	case err := <-done:
		t.Fatalf("serve exited (%v) within the grace period after being armed", err)
	case <-time.After(3 * cfg.Grace):
	}
	rt.Start("c") // the rebuilt container starts
	time.Sleep(cfg.StartupTimeout)
	select {
	case err := <-done:
		t.Fatalf("serve exited (%v) while the rebuilt container runs", err)
	default:
	}
	rt.Stop("c")
	expectServeExit(t, done, cfg.socket)
}

func TestParseServeAcceptsConfigPath(t *testing.T) {
	for _, name := range []string{"--rules-file", "--config"} {
		got, err := parseServe([]string{"--socket", "/s", "--workspace", "/w", "--container-workspace", "/c", name, "/etc/rules.yaml"}, noEnv, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if got.RulesFile != "/etc/rules.yaml" {
			t.Fatalf("%s: rules file %q, want /etc/rules.yaml", name, got.RulesFile)
		}
	}
}

// up and serve take the rules file from rules_file in any source: a
// relative path from a file or a variable against the workspace, from a
// flag against the current directory.
func TestUpAndServeResolveTheRulesFile(t *testing.T) {
	ws, cwd := t.TempDir(), t.TempDir()
	t.Chdir(cwd)
	host := settingsFile(t, "rules_file: common.yaml\n")
	tests := []struct {
		name  string
		vars  map[string]string
		flags []string
		want  string
	}{
		{"default", nil, nil, filepath.Join(ws, ".devcontainer", "hostrun.yaml")},
		{"host settings file", map[string]string{"HOSTRUNNER_SETTINGS": host}, nil, filepath.Join(ws, "common.yaml")},
		{"variable", map[string]string{"HOSTRUNNER_SETTINGS": host, "HOSTRUNNER_RULES_FILE": "var.yaml"}, nil, filepath.Join(ws, "var.yaml")},
		{"--rules-file", map[string]string{"HOSTRUNNER_RULES_FILE": "var.yaml"}, []string{"--rules-file", "flag.yaml"}, filepath.Join(cwd, "flag.yaml")},
		{"--config", map[string]string{"HOSTRUNNER_RULES_FILE": "var.yaml"}, []string{"--config", "flag.yaml"}, filepath.Join(cwd, "flag.yaml")},
	}
	for name, resolve := range commands {
		for _, tt := range tests {
			got, err := resolve(append([]string{"--workspace", ws}, tt.flags...), envOf(tt.vars))
			if err != nil || got.RulesFile != tt.want {
				t.Errorf("%s, %s: got rules file %q, %v; want %q", name, tt.name, got.RulesFile, err, tt.want)
			}
		}
	}
}

func TestUpAndServeRejectRulesFileWithConfig(t *testing.T) {
	for name, resolve := range commands {
		_, err := resolve([]string{"--rules-file", "/a.yaml", "--config", "/b.yaml"}, noEnv)
		if err == nil || !strings.HasPrefix(err.Error(), "--rules-file and --config exclude each other; usage: hostrunner "+name) {
			t.Errorf("%s: got %v, want the conflict with the usage", name, err)
		}
	}
}

func TestServeRefusesInvalidConfig(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.RulesFile = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := serve(context.Background(), cfg, slog.New(slog.DiscardHandler), nil)
	if err == nil || !strings.Contains(err.Error(), cfg.RulesFile) {
		t.Fatalf("got %v, want an error naming the config", err)
	}
	if _, err := os.Stat(cfg.socket); !os.IsNotExist(err) {
		t.Fatal("serve listened despite an invalid config")
	}
}

func TestServeEnforcesConfig(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.RulesFile = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: true\n    args: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	startServe(t, cfg)
	tr := transport.Unix{Path: cfg.socket}
	run := func(argv ...string) int {
		return client.Run(context.Background(), tr, argv, cfg.containerWorkspace, strings.NewReader(""), io.Discard, io.Discard)
	}
	if code := run("true"); code != 0 {
		t.Errorf("allowed command exited %d", code)
	}
	if code := run("false"); code != protocol.ExitRejected {
		t.Errorf("unlisted command exited %d, want %d", code, protocol.ExitRejected)
	}
}

// rulesDigest is the digest of the rules serve loaded for cfg.
func rulesDigest(t *testing.T, cfg serveConfig) string {
	t.Helper()
	p, err := rules.Load(cfg.RulesFile)
	if err != nil {
		t.Fatal(err)
	}
	return p.Digest()
}

// armServe arms the daemon as `hostrunner up` would and returns whether it
// stepped aside.
func armServe(t *testing.T, cfg serveConfig, digest string) bool {
	t.Helper()
	return armServeWith(t, cfg, digest, nil)
}

// armServeWith is armServe for an arm that carries the given daemon
// settings.
func armServeWith(t *testing.T, cfg serveConfig, digest string, carried map[string]string) bool {
	t.Helper()
	c, err := transport.Unix{Path: cfg.socket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := protocol.WriteJSON(c, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: digest, Settings: carried}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	var reply protocol.Armed
	if err != nil || f.Type != protocol.FrameArmed || protocol.DecodeJSON(f, &reply) != nil {
		t.Fatalf("arm: frame %v, err %v", f.Type, err)
	}
	return reply.Restart
}

func TestArmHandlerStepsAsideOnceTheWatchEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		discard := slog.New(slog.DiscardHandler)
		w := watch.New([]watch.Runtime{&watchtest.Runtime{}}, watch.Config{
			PollInterval: time.Second, QueryTimeout: time.Second, StartupTimeout: time.Hour, Grace: time.Second,
		}, discard)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		synctest.Wait() // the watch runs, waiting for its next poll

		onArm := armHandler("rules.yaml", true, noSettings, w, discard)
		if onArm(protocol.Arm{ConfigDigest: "digest"}, "digest") {
			t.Fatal("stepped aside while the watch runs")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("watch ended with %v, want context.Canceled", err)
		}
		if !onArm(protocol.Arm{ConfigDigest: "digest"}, "digest") {
			t.Fatal("did not step aside after the watch ended")
		}
	})
}

// An arm from an up whose --no-reread differs from the daemon's makes it
// step aside, so the new setting takes effect.
func TestArmHandlerStepsAsideWhenRereadingDiffers(t *testing.T) {
	for _, noReread := range []bool{false, true} {
		onArm := armHandler("rules.yaml", !noReread, noSettings, nil, slog.New(slog.DiscardHandler))
		if !onArm(protocol.Arm{ConfigDigest: "digest", NoReread: !noReread}, "digest") {
			t.Errorf("daemon noReread=%v kept running for an arm with the other setting", noReread)
		}
		if onArm(protocol.Arm{ConfigDigest: "digest", NoReread: noReread}, "digest") {
			t.Errorf("daemon noReread=%v stepped aside for an arm with its own setting", noReread)
		}
	}
}

// serve rereads its rules file by itself unless --no-reread, and its
// hello says so.
func TestServeSaysWhetherItRereads(t *testing.T) {
	for _, noReread := range []bool{false, true} {
		cfg := watchConfig(t)
		cfg.Reread = !noReread
		startServe(t, cfg, &watchtest.Runtime{})
		c, err := transport.Unix{Path: launch.ApprovalSocket(filepath.Dir(cfg.socket))}.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
			t.Fatal(err)
		}
		f, err := protocol.ReadFrame(c)
		var info protocol.DaemonInfo
		if err != nil || f.Type != protocol.FrameDaemonInfo || protocol.DecodeJSON(f, &info) != nil {
			t.Fatalf("hello: frame %v, err %v", f.Type, err)
		}
		if info.Rereads == noReread || info.NoReread != noReread {
			t.Errorf("--no-reread %v: got Rereads=%v NoReread=%v", noReread, info.Rereads, info.NoReread)
		}
	}
}

// noSettings are the daemon settings of an armHandler whose tests carry
// none in their arms.
var noSettings map[string]string

// with is m with key set to value, m unchanged.
func with(m map[string]string, key, value string) map[string]string {
	out := maps.Clone(m)
	out[key] = value
	return out
}

// An arm carrying daemon settings other than the daemon's makes it step
// aside, so a settings edit applies on the next container start; the
// daemon compares only the settings the arm carries, and an arm with
// none, from an up that predates them, compares nothing.
func TestArmHandlerStepsAsideWhenSettingsDiffer(t *testing.T) {
	own := map[string]string{"rules_file": "/w/rules.yaml", "grace": "15s", "reread": "true"}
	onArm := armHandler("rules.yaml", true, own, nil, slog.New(slog.DiscardHandler))
	for _, tt := range []struct {
		name      string
		carried   map[string]string
		stepAside bool
	}{
		{"same settings", own, false},
		{"a subset of the same", map[string]string{"rules_file": "/w/rules.yaml"}, false},
		{"none", nil, false},
		{"another duration", with(own, "grace", "1m0s"), true},
		{"another path", with(own, "rules_file", "/w/other.yaml"), true},
		{"another bool", with(own, "reread", "false"), true},
		{"an unknown key", with(own, "new_key", "x"), true},
		{"an empty unknown key", with(own, "new_key", ""), true},
	} {
		if got := onArm(protocol.Arm{ConfigDigest: "digest", Settings: tt.carried}, "digest"); got != tt.stepAside {
			t.Errorf("%s: stepped aside %v, want %v", tt.name, got, tt.stepAside)
		}
	}
}

func TestArmHandlerStepsAsideWhenRulesChanged(t *testing.T) {
	onArm := armHandler("rules.yaml", true, noSettings, nil, slog.New(slog.DiscardHandler))
	if !onArm(protocol.Arm{ConfigDigest: "other"}, "digest") {
		t.Fatal("kept running with other rules")
	}
	if onArm(protocol.Arm{ConfigDigest: "digest"}, "digest") {
		t.Fatal("stepped aside with the same rules and no watch")
	}
}

func TestServeStepsAsideWhenRulesChanged(t *testing.T) {
	cfg := watchConfig(t)
	rt := &watchtest.Runtime{}
	rt.Start("c")
	done := startServe(t, cfg, rt)
	if stepAside := armServe(t, cfg, "some other digest"); !stepAside {
		t.Fatal("serve kept running with stale rules")
	}
	expectServeExit(t, done, cfg.socket)
}

// serve keeps running for an arm carrying its own settings and steps
// aside for one carrying another duration, path or bool.
func TestServeStepsAsideWhenSettingsDiffer(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*settings.Settings)
	}{
		{"another duration", func(s *settings.Settings) { s.Grace += time.Second }},
		{"another path", func(s *settings.Settings) { s.RulesFile += ".other" }},
		{"another bool", func(s *settings.Settings) { s.Reread = !s.Reread }},
	} {
		cfg := watchConfig(t)
		rt := &watchtest.Runtime{}
		rt.Start("c")
		done := startServe(t, cfg, rt)
		if stepAside := armServeWith(t, cfg, rulesDigest(t, cfg), cfg.Settings.Daemon()); stepAside {
			t.Fatalf("%s: serve stepped aside for its own settings", tt.name)
		}
		other := cfg.Settings
		tt.change(&other)
		if stepAside := armServeWith(t, cfg, rulesDigest(t, cfg), other.Daemon()); !stepAside {
			t.Fatalf("%s: serve kept running with other settings", tt.name)
		}
		expectServeExit(t, done, cfg.socket)
	}
}

// TestParseUpdateFlags checks that --check is the only flag of update.
func TestParseUpdateFlags(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{"no flags", nil, false},
		{"check", []string{"--check"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUpdate(tt.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("check: got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseUpdateRejectsArguments checks that update takes no version
// or other argument.
func TestParseUpdateRejectsArguments(t *testing.T) {
	if _, err := parseUpdate([]string{"v0.5.0"}, io.Discard); err == nil || !strings.Contains(err.Error(), updateUsage) {
		t.Fatalf("got %v, want an error with the usage", err)
	}
}

// TestParseUpdateHelpIsTheFlagHelp checks that -h is handled like the
// other subcommands' -h.
func TestParseUpdateHelpIsTheFlagHelp(t *testing.T) {
	if _, err := parseUpdate([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}

// TestRunUpdateRefusesADevBuild checks that runUpdate passes a dev build's
// refusal on with the "update: " prefix. The version is passed in, so the
// test does not depend on the test binary's build info; that the refusal
// comes before any request is tested in internal/update.
func TestRunUpdateRefusesADevBuild(t *testing.T) {
	var out strings.Builder
	err := runUpdate(context.Background(), false, "dev", &out)
	if !errors.Is(err, update.ErrDevBuild) || !strings.HasPrefix(err.Error(), "update: ") {
		t.Fatalf("got %v, want update.ErrDevBuild prefixed with \"update: \"", err)
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q despite the error", out.String())
	}
}

// TestRunDispatchesUpdate checks that run hands `update` to parseUpdate:
// its errors, not the top-level usage, come back. No case reaches the
// network.
func TestRunDispatchesUpdate(t *testing.T) {
	if err := run([]string{"update", "v0.5.0"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), updateUsage) {
		t.Fatalf("extra argument: got %v, want an error with %q", err, updateUsage)
	}
	if err := run([]string{"update", "-h"}, io.Discard, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: got %v, want flag.ErrHelp", err)
	}
}

// TestUsageListsTheSubcommands checks that the top-level usage names
// every subcommand.
func TestUsageListsTheSubcommands(t *testing.T) {
	if err := run(nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "up|serve|test|approve|stop|restart|update|instructions|version") {
		t.Fatalf("got %v, want the usage listing every subcommand", err)
	}
}

// TestUpdateMessage checks the line update prints for each outcome.
func TestUpdateMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		res  update.Result
		want string
	}{
		{"up to date", update.Result{Current: "v0.5.0", Latest: "v0.5.0", Outcome: update.UpToDate}, "hostrunner: already up to date (v0.5.0)"},
		{"available", update.Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: update.Available}, "hostrunner: v0.5.0 available (current v0.4.0)"},
		{"updated", update.Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: update.Updated}, "hostrunner: updated v0.4.0 -> v0.5.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateMessage(tt.res); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// serve listens for approval sessions beside its socket's directory, the
// runtime directory, which the container mounts, so the container cannot
// reach it; it removes the socket when it stops.
func TestServeListensOnTheApprovalSocket(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.RulesFile = filepath.Join(cfg.workspace, "hostrun.yaml")
	approvalSocket := launch.ApprovalSocket(filepath.Dir(cfg.socket))
	t.Cleanup(func() { os.Remove(approvalSocket) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.DiscardHandler), nil) }()
	// Stopped here as the test ends, should it fail before it stops serve.
	t.Cleanup(cancel)
	waitListening(t, approvalSocket)
	c, err := net.Dial("unix", approvalSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := os.Stat(approvalSocket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("approval socket mode %v, want 0600", info.Mode().Perm())
	}
	if err := protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	var got protocol.DaemonInfo
	if err != nil || f.Type != protocol.FrameDaemonInfo || protocol.DecodeJSON(f, &got) != nil {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	want := protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: cfg.workspace, ContainerWorkspace: cfg.containerWorkspace, RulesFile: cfg.RulesFile, RulesDigest: "missing", Stops: true, Rereads: true}
	if real, err := filepath.EvalSymlinks(cfg.workspace); err == nil {
		want.Workspace = real
	}
	if got != want {
		t.Fatalf("hello: got %+v, want %+v", got, want)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := os.Stat(approvalSocket); !os.IsNotExist(err) {
		t.Fatalf("approval socket left behind: %v", err)
	}
}

// With --dir, the session serves the approval socket beside that runtime
// directory alone.
func TestParseApproveServesTheRuntimeDirectorysApprovalSocket(t *testing.T) {
	got, err := parseApprove([]string{"--dir", "/run/user/1000/hostrunner/abc"}, xdgEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/run/user/1000/hostrunner/abc.approval.sock"; got.Socket != want {
		t.Fatalf("socket %q, want %q", got.Socket, want)
	}
	if got.Root != "" {
		t.Fatalf("root %q: a session given --dir serves that daemon alone", got.Root)
	}
}

// Without --dir, the session lists the daemons under the runtime root,
// the current directory choosing the default.
func TestParseApproveListsTheDaemonsOfTheRuntimeRootWithoutADirectory(t *testing.T) {
	got, err := parseApprove(nil, xdgEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := (approval.Config{Root: "/run/user/1000/hostrunner", Cwd: cwd}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := parseApprove(nil, noEnv, io.Discard); !errors.Is(err, errNoRuntimeRoot) {
		t.Fatalf("got %v, want it to need a runtime root", err)
	}
	if _, err := parseApprove([]string{"--dir", "/d", "extra"}, xdgEnv, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

// An approval socket inside the workspace, which the container mounts,
// would let the container answer its own requests: serve refuses it.
func TestServeRefusesAnApprovalSocketInsideTheWorkspace(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	runtimeDir := filepath.Join(cfg.workspace, "rt")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.socket = filepath.Join(runtimeDir, "h.sock")
	err := serve(context.Background(), cfg, slog.New(slog.DiscardHandler), nil)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("got %v, want the approval socket inside the workspace refused", err)
	}
	if _, err := os.Stat(launch.ApprovalSocket(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("approval socket created: %v", err)
	}
}

// A daemon stepping aside closes its approval socket before its socket, so
// once `up` sees the socket closed, the new daemon can take the approval
// socket.
func TestServeClosesTheApprovalSocketFirstWhenSteppingAside(t *testing.T) {
	cfg := watchConfig(t)
	approvalSocket := launch.ApprovalSocket(filepath.Dir(cfg.socket))
	t.Cleanup(func() { os.Remove(approvalSocket) })
	rt := &watchtest.Runtime{}
	rt.Start("c")
	done := startServe(t, cfg, rt)
	if stepAside := armServe(t, cfg, "some other digest"); !stepAside {
		t.Fatal("serve kept running with stale rules")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		c, err := net.Dial("unix", cfg.socket)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the socket stayed open")
		}
	}
	if c, err := net.Dial("unix", approvalSocket); err == nil {
		c.Close()
		t.Fatal("the approval socket still listens after the socket closed")
	}
	expectServeExit(t, done, cfg.socket)
}

// A session that adds a rule makes serve reread its rules file: the next
// arm, carrying the new file's digest, does not make the daemon step aside.
func TestServeRereadsItsRulesOnceARuleIsAdded(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.RulesFile = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: echo\n    args: any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	approvalSocket := launch.ApprovalSocket(filepath.Dir(cfg.socket))
	t.Cleanup(func() { os.Remove(approvalSocket) })
	startServe(t, cfg)
	c, err := transport.Unix{Path: approvalSocket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(c); err != nil || f.Type != protocol.FrameDaemonInfo {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	if err := protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameServe}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(c); err != nil || f.Type != protocol.FrameServing {
		t.Fatalf("serve: frame %v, err %v", f.Type, err)
	}
	added := []byte("rules:\n  - command: echo\n    args: any\n  - command: date\n    args: none\n")
	if err := os.WriteFile(cfg.RulesFile, added, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteJSON(c, protocol.FrameReread, protocol.Reread{Rule: "date", Digest: rules.ContentDigest(added)}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	var res protocol.RereadResult
	if err != nil || f.Type != protocol.FrameRereadResult || protocol.DecodeJSON(f, &res) != nil {
		t.Fatalf("reread: frame %v, err %v", f.Type, err)
	}
	p, err := rules.Load(cfg.RulesFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "" || res.Digest != p.Digest() {
		t.Fatalf("reread: got %+v, want the new file's digest %s", res, p.Digest())
	}
	if stepAside := armServe(t, cfg, p.Digest()); stepAside {
		t.Fatal("stepped aside for the rules it reread")
	}
}

// TestParseInstructions checks that --append takes an optional path,
// AGENTS.md by default, and that a path needs --append.
func TestParseInstructions(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want instructionsConfig
	}{
		{"no flags", nil, instructionsConfig{}},
		{"append", []string{"--append"}, instructionsConfig{append: true, path: "AGENTS.md"}},
		{"append with path", []string{"--append", "docs/CLAUDE.md"}, instructionsConfig{append: true, path: "docs/CLAUDE.md"}},
		{"append before an end of flags", []string{"--append", "--"}, instructionsConfig{append: true, path: "AGENTS.md"}},
		{"append with a path starting with a dash", []string{"--append", "--", "-x.md"}, instructionsConfig{append: true, path: "-x.md"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInstructions(tt.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseInstructionsRejectsArguments checks that a path without
// --append and a second path are errors carrying the usage.
func TestParseInstructionsRejectsArguments(t *testing.T) {
	for _, args := range [][]string{
		{"AGENTS.md"},
		{"--append", "AGENTS.md", "CLAUDE.md"},
		{"AGENTS.md", "--append"},
		{"--append=false", "AGENTS.md"},
		{"--append", ""},
	} {
		if _, err := parseInstructions(args, io.Discard); err == nil || !strings.Contains(err.Error(), instructionsUsage) {
			t.Fatalf("%q: got %v, want an error with the usage", args, err)
		}
	}
}

// TestParseInstructionsHelpIsTheFlagHelp checks that -h is handled like
// the other subcommands' -h.
func TestParseInstructionsHelpIsTheFlagHelp(t *testing.T) {
	if _, err := parseInstructions([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}

// TestRunInstructionsPrintsTheBlock checks that without --append the
// block is printed as is, so redirecting it equals --append.
func TestRunInstructionsPrintsTheBlock(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"instructions"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != instructions.Block() {
		t.Fatalf("got %q, want the block", out.String())
	}
}

// TestRunInstructionsAppendsToAgentsMD checks that --append without a
// path writes AGENTS.md in the current directory and reports it.
func TestRunInstructionsAppendsToAgentsMD(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("AGENTS.md", []byte("# Agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"instructions", "--append"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := "hostrunner: appended instructions to AGENTS.md\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	got, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := string(instructions.Append([]byte("# Agents\n"))); string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRunInstructionsAsksOnStderr checks that the question goes to
// stderr and the outcome line to stdout.
func TestRunInstructionsAsksOnStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, []byte(instructions.Block()), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	cfg := instructionsConfig{append: true, path: path}
	if err := runInstructions(cfg, strings.NewReader("c\n"), true, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if want := "hostrunner: " + path + " left unchanged\n"; stdout.String() != want {
		t.Fatalf("stdout: got %q, want %q", stdout.String(), want)
	}
	if !strings.HasPrefix(stderr.String(), path+" already has hostrunner instructions") {
		t.Fatalf("stderr: got %q, want the question", stderr.String())
	}
}

// TestRunInstructionsReportsErrors checks that a refusal comes back as the
// error and prints nothing.
func TestRunInstructionsReportsErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	var stdout, stderr strings.Builder
	cfg := instructionsConfig{append: true, path: path}
	err := runInstructions(cfg, strings.NewReader(""), false, &stdout, &stderr)
	if want := path + " does not exist; run in a terminal to create it"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("printed %q and %q despite the error", stdout.String(), stderr.String())
	}
}

// TestInstructionsMessage checks the line --append prints for each
// outcome.
func TestInstructionsMessage(t *testing.T) {
	for _, tt := range []struct {
		outcome instructions.Outcome
		want    string
	}{
		{instructions.Created, "hostrunner: created AGENTS.md"},
		{instructions.Appended, "hostrunner: appended instructions to AGENTS.md"},
		{instructions.Replaced, "hostrunner: replaced instructions in AGENTS.md"},
		{instructions.Unchanged, "hostrunner: AGENTS.md left unchanged"},
	} {
		if got := instructionsMessage(tt.outcome, "AGENTS.md"); got != tt.want {
			t.Fatalf("outcome %v: got %q, want %q", tt.outcome, got, tt.want)
		}
	}
}

// TestIsTerminal checks that a pty is a terminal and that a pipe and a
// regular file, what stdin is when piped or redirected, are not.
func TestIsTerminal(t *testing.T) {
	t.Run("pty", func(t *testing.T) {
		pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
		if err != nil {
			t.Skipf("no pty: %v", err)
		}
		defer pty.Close()
		if !isTerminal(pty) {
			t.Fatal("a pty not reported as a terminal")
		}
	})
	t.Run("pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if isTerminal(r) {
			t.Fatal("a pipe reported as a terminal")
		}
	})
	t.Run("regular file", func(t *testing.T) {
		f, err := os.Create(filepath.Join(t.TempDir(), "in"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if isTerminal(f) {
			t.Fatal("a regular file reported as a terminal")
		}
	})
}

// helloInfo opens the approval socket of the serve on socket and returns
// the daemon's hello.
func helloInfo(t *testing.T, socket string) protocol.DaemonInfo {
	t.Helper()
	c, err := net.Dial("unix", launch.ApprovalSocket(filepath.Dir(socket)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	var info protocol.DaemonInfo
	if err != nil || f.Type != protocol.FrameDaemonInfo || protocol.DecodeJSON(f, &info) != nil {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	return info
}

// serve --watch reports its armed container stopped once the watcher sees
// it stop, while it counts the grace; not while waiting for it or while it
// runs.
func TestServeReportsTheArmedContainerStopped(t *testing.T) {
	cfg := watchConfig(t)
	cfg.Grace = 2 * time.Second // long enough to ask while it counts
	rt := &watchtest.Runtime{}
	startServe(t, cfg, rt)
	if helloInfo(t, cfg.socket).ContainerStopped {
		t.Fatal("the hello reported the container stopped while waiting for it")
	}
	rt.Start("c")
	for until := time.Now().Add(20 * cfg.PollInterval); time.Now().Before(until); time.Sleep(cfg.PollInterval) {
		if helloInfo(t, cfg.socket).ContainerStopped {
			t.Fatal("the hello reported the armed container stopped while it runs")
		}
	}
	rt.Stop("c")
	for deadline := time.Now().Add(5 * time.Second); !helloInfo(t, cfg.socket).ContainerStopped; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hello never reported the armed container stopped")
		}
	}
}

func TestParseUpRestart(t *testing.T) {
	base := []string{"--dir", "/d", "--workspace", "/w", "--container-workspace", "/c"}
	for _, tt := range []struct {
		flags  []string
		answer stop.Answer
	}{
		{[]string{"--restart"}, stop.Ask},
		{[]string{"--restart", "--kill"}, stop.Kill},
		{[]string{"--restart", "--wait"}, stop.Wait},
	} {
		got, err := parseUp(append(base, tt.flags...), "/bin/hostrunner", noEnv, io.Discard)
		if err != nil {
			t.Fatalf("%q: %v", tt.flags, err)
		}
		if !got.restart || got.answer != tt.answer {
			t.Errorf("%q: got restart %v, answer %v", tt.flags, got.restart, got.answer)
		}
	}
	for _, flags := range [][]string{{"--kill"}, {"--wait"}, {"--restart", "--kill", "--wait"}} {
		if _, err := parseUp(append(base, flags...), "/bin/hostrunner", noEnv, io.Discard); err == nil {
			t.Errorf("%q: expected a usage error", flags)
		}
	}
}

// xdgVars are the variables of xdgEnv, a getenv with
// XDG_RUNTIME_DIR=/run/user/1000 alone.
var (
	xdgVars = map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}
	xdgEnv  = envOf(xdgVars)
)

func TestParseStop(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const stopTimeout = 10 * time.Second // the default
	for _, tt := range []struct {
		args []string
		want stop.Config
	}{
		{nil, stop.Config{Root: "/run/user/1000/hostrunner", Cwd: cwd, Commands: stop.Commands{StopTimeout: stopTimeout}}},
		{[]string{"--dir", "/run/user/1000/hostrunner/abc"}, stop.Config{Dir: "/run/user/1000/hostrunner/abc", Cwd: cwd, Commands: stop.Commands{StopTimeout: stopTimeout}}},
		{[]string{"--all"}, stop.Config{Root: "/run/user/1000/hostrunner", All: true, Cwd: cwd, Commands: stop.Commands{StopTimeout: stopTimeout}}},
		{[]string{"--choose", "--kill"}, stop.Config{Root: "/run/user/1000/hostrunner", Choose: true, Cwd: cwd, Commands: stop.Commands{Answer: stop.Kill, StopTimeout: stopTimeout}}},
		{[]string{"--wait"}, stop.Config{Root: "/run/user/1000/hostrunner", Cwd: cwd, Commands: stop.Commands{Answer: stop.Wait, StopTimeout: stopTimeout}}},
	} {
		got, err := parseStop("stop", tt.args, "/bin/hostrunner", xdgEnv, io.Discard)
		if err != nil {
			t.Fatalf("%q: %v", tt.args, err)
		}
		if got != tt.want {
			t.Errorf("%q: got %+v, want %+v", tt.args, got, tt.want)
		}
	}
}

func TestParseStopRejectsExclusiveFlags(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000") // run below reads the process environment
	for _, args := range [][]string{
		{"--dir", "/d", "--all"},
		{"--dir", "/d", "--choose"},
		{"--all", "--choose"},
		{"--kill", "--wait"},
		{"extra"},
	} {
		if _, err := parseStop("stop", args, "/bin/hostrunner", xdgEnv, io.Discard); err == nil {
			t.Errorf("%q: expected a usage error", args)
		}
	}
	if err := run([]string{"stop", "--all", "--choose"}, io.Discard, io.Discard); exitCode(err) != 1 {
		t.Errorf("a usage error exits %d, want 1", exitCode(err))
	}
}

// Without XDG_RUNTIME_DIR, stop needs a runtime root from a setting or a
// runtime directory.
func TestParseStopNeedsARuntimeRootWithoutDir(t *testing.T) {
	if _, err := parseStop("stop", nil, "/bin/hostrunner", noEnv, io.Discard); !errors.Is(err, errNoRuntimeRoot) {
		t.Fatalf("got %v, want it to need a runtime root or --dir", err)
	}
	if _, err := parseStop("stop", []string{"--dir", "/d"}, "/bin/hostrunner", noEnv, io.Discard); err != nil {
		t.Fatalf("--dir: %v", err)
	}
	if got, err := parseStop("stop", []string{"--runtime-root", "/r"}, "/bin/hostrunner", noEnv, io.Discard); err != nil || got.Root != "/r" {
		t.Fatalf("--runtime-root: got %q, %v; want /r", got.Root, err)
	}
}

// stop, restart and approve hand the settings they resolve (the order of
// the sources is internal/settings' to test) to what they run: the
// runtime root to look for daemons under and, for stop and restart, the
// stop timeout. A flag comes first, then the variable, then the common
// part of the host settings file, by default the one under
// XDG_CONFIG_HOME; the workspace sections are not read.
func TestStopRestartAndApproveResolveTheirSettings(t *testing.T) {
	config := t.TempDir()
	if err := os.MkdirAll(filepath.Join(config, "hostrunner"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "runtime_root: /file/root\nstop_timeout: 1m\ngrace: soon\nworkspaces:\n  /w:\n    grase: 1m\n"
	if err := os.WriteFile(filepath.Join(config, "hostrunner", "settings.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile := map[string]string{"XDG_CONFIG_HOME": config}
	tests := []struct {
		name        string
		vars        map[string]string
		stopFlags   []string // for stop and restart
		rootFlags   []string // for approve
		root        string
		stopTimeout time.Duration
	}{
		{"the default host settings file", fromFile, nil, nil, "/file/root", time.Minute},
		{"variables", map[string]string{"XDG_CONFIG_HOME": config, "HOSTRUNNER_RUNTIME_ROOT": "/var/root", "HOSTRUNNER_STOP_TIMEOUT": "2m"}, nil, nil, "/var/root", 2 * time.Minute},
		{"flags", fromFile, []string{"--runtime-root", "/flag/root", "--stop-timeout", "3m"}, []string{"--runtime-root", "/flag/root"}, "/flag/root", 3 * time.Minute},
	}
	for _, tt := range tests {
		for _, name := range []string{"stop", "restart"} {
			got, err := parseStop(name, tt.stopFlags, "/bin/hostrunner", envOf(tt.vars), io.Discard)
			if err != nil || got.Root != tt.root || got.StopTimeout != tt.stopTimeout {
				t.Errorf("%s, %s: got %q and %v, %v; want %q and %v", name, tt.name, got.Root, got.StopTimeout, err, tt.root, tt.stopTimeout)
			}
		}
		got, err := parseApprove(tt.rootFlags, envOf(tt.vars), io.Discard)
		if err != nil || got.Root != tt.root {
			t.Errorf("approve, %s: got %q, %v; want %q", tt.name, got.Root, err, tt.root)
		}
	}
	if _, err := parseApprove([]string{"--stop-timeout", "1m"}, xdgEnv, io.Discard); err == nil {
		t.Error("approve: got no error for --stop-timeout")
	}
}

// An invalid setting of stop, restart or approve fails the command, a
// flag's with the command's usage.
func TestStopRestartAndApproveRefuseInvalidSettings(t *testing.T) {
	withFile := func(content string) map[string]string {
		return map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "HOSTRUNNER_SETTINGS": settingsFile(t, content)}
	}
	relative := withFile("runtime_root: rel\n")
	for _, tt := range []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"relative variable", map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "HOSTRUNNER_RUNTIME_ROOT": "rel"}, nil, `HOSTRUNNER_RUNTIME_ROOT: must be absolute, got "rel"`},
		{"relative in the file", relative, nil, "settings file " + relative["HOSTRUNNER_SETTINGS"] + `: runtime_root: must be absolute, got "rel"`},
		{"invalid variable", map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "HOSTRUNNER_STOP_TIMEOUT": "soon"}, nil, `HOSTRUNNER_STOP_TIMEOUT: invalid duration "soon"`},
		{"invalid in the file", withFile("stop_timeout: soon\n"), nil, `stop_timeout: invalid duration "soon"`},
		{"zero flag", xdgVars, []string{"--stop-timeout", "0s"}, "--stop-timeout: must be positive; usage: hostrunner stop"},
		{"empty flag", xdgVars, []string{"--runtime-root", ""}, "--runtime-root: empty path; usage: hostrunner stop"},
	} {
		if _, err := parseStop("stop", tt.args, "/bin/hostrunner", envOf(tt.vars), io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("stop, %s: got %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
	for _, tt := range []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"relative in the file", relative, nil, `runtime_root: must be absolute, got "rel"`},
		{"empty flag", xdgVars, []string{"--runtime-root", ""}, "--runtime-root: empty path; usage: hostrunner approve"},
	} {
		if _, err := parseApprove(tt.args, envOf(tt.vars), io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("approve, %s: got %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
}

func TestParseRestartStartsTheInstalledHostrunner(t *testing.T) {
	got, err := parseStop("restart", []string{"--all"}, "/home/u/.local/bin/hostrunner", xdgEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.Restart == nil || got.Restart.Daemon != "/home/u/.local/bin/hostrunner" || got.Restart.Client != "/home/u/.local/bin/hostrun" || got.Restart.Settings == nil {
		t.Fatalf("got %+v, want the installed hostrunner and hostrun, and the settings resolved", got.Restart)
	}
	if stopped, _ := parseStop("stop", nil, "/bin/hostrunner", xdgEnv, io.Discard); stopped.Restart != nil {
		t.Fatal("stop restarts")
	}
}

// restart resolves the new daemon's settings for the old daemon's
// workspace as up would, from restart's flags, the environment getenv
// reads and the settings files: their section for that workspace and the
// workspace settings file apply, and ready_timeout too; its own
// settings stay apart.
func TestParseRestartResolvesTheNewDaemonsSettings(t *testing.T) {
	ws := t.TempDir()
	host := settingsFile(t, "stop_timeout: 1m\ngrace: 2m\nworkspaces:\n  "+ws+":\n    poll_interval: 3s\n")
	wsFile := settingsFile(t, "ask_timeout: 4m\n")
	vars := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "HOSTRUNNER_SETTINGS": host, "HOSTRUNNER_READY_TIMEOUT": "5s"}
	got, err := parseStop("restart", []string{"--workspace-settings", wsFile, "--rules-file", "/r/hostrun.yaml"}, "/bin/hostrunner", envOf(vars), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopTimeout != time.Minute {
		t.Errorf("stop timeout %v, want 1m", got.StopTimeout)
	}
	s, err := got.Restart.Settings(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"rules file (flag)", s.RulesFile, "/r/hostrun.yaml"},
		{"ready timeout (variable)", s.ReadyTimeout, 5 * time.Second},
		{"ask timeout (workspace settings file)", s.AskTimeout, 4 * time.Minute},
		{"poll interval (workspace section)", s.PollInterval, 3 * time.Second},
		{"grace (common part)", s.Grace, 2 * time.Minute},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// stop has no flag of a new daemon's settings.
func TestParseStopRefusesTheRulesFileFlag(t *testing.T) {
	if _, err := parseStop("stop", []string{"--rules-file", "/r/hostrun.yaml"}, "/bin/hostrunner", xdgEnv, io.Discard); err == nil {
		t.Fatal("got no error for --rules-file")
	}
}

// A path flag of the new daemon that cannot be applied is a usage error
// of restart.
func TestParseRestartRefusesAnEmptyConfig(t *testing.T) {
	_, err := parseStop("restart", []string{"--config", ""}, "/bin/hostrunner", xdgEnv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--config: empty path; usage: hostrunner restart") {
		t.Fatalf("got %v, want the flag error with the usage", err)
	}
}

// An invalid daemon setting does not fail restart's own parsing: it is
// found when the new daemon's settings are resolved, named with its
// source.
func TestParseRestartFindsAnInvalidDaemonSettingForTheNewDaemon(t *testing.T) {
	bad := settingsFile(t, "grace: soon\n")
	got, err := parseStop("restart", nil, "/bin/hostrunner", envOf(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "HOSTRUNNER_SETTINGS": bad}), io.Discard)
	if err != nil {
		t.Fatalf("parse: %v; want the setting found only for the new daemon", err)
	}
	if _, err := got.Restart.Settings(t.TempDir()); err == nil || !strings.Contains(err.Error(), "settings file "+bad+`: grace: invalid duration "soon"`) {
		t.Fatalf("got %v, want the invalid setting named with its source", err)
	}
}

func TestParseStopHelpIsTheFlagHelp(t *testing.T) {
	for _, name := range []string{"stop", "restart"} {
		if _, err := parseStop(name, []string{"-h"}, "/bin/hostrunner", xdgEnv, io.Discard); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: got %v, want flag.ErrHelp", name, err)
		}
	}
}
