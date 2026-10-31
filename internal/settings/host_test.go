package settings_test

import (
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// hostDefaults are the settings of stop and restart without any source
// but XDG_RUNTIME_DIR=/run/user/1000: they hold the keys they take alone.
func hostDefaults() settings.Settings {
	return settings.Settings{RuntimeRoot: "/run/user/1000/hostrunner", StopTimeout: 10 * time.Second}
}

// withXDG is vars and XDG_RUNTIME_DIR=/run/user/1000.
func withXDG(vars map[string]string) map[string]string {
	out := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}
	maps.Copy(out, vars)
	return out
}

// stop and restart take --settings, --runtime-root and --stop-timeout,
// approve the first two; up and serve take neither of the last two.
func TestRegisterStopAndRegisterApproveAddTheirFlagsAlone(t *testing.T) {
	names := func(register func(*settings.Flags, *flag.FlagSet)) []string {
		var f settings.Flags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		register(&f, fs)
		var got []string
		fs.VisitAll(func(fl *flag.Flag) { got = append(got, fl.Name) })
		return got
	}
	if got, want := names((*settings.Flags).RegisterStop), []string{"runtime-root", "settings", "stop-timeout"}; !slices.Equal(got, want) {
		t.Errorf("RegisterStop: got %q, want %q", got, want)
	}
	if got, want := names((*settings.Flags).RegisterApprove), []string{"runtime-root", "settings"}; !slices.Equal(got, want) {
		t.Errorf("RegisterApprove: got %q, want %q", got, want)
	}
	for _, register := range []func(*settings.Flags, *flag.FlagSet){(*settings.Flags).Register, (*settings.Flags).RegisterUp} {
		for _, name := range names(register) {
			if name == "runtime-root" || name == "stop-timeout" {
				t.Errorf("up or serve takes --%s", name)
			}
		}
	}
}

// --stop-timeout shows its default in the help, as the daemon's
// durations do.
func TestStopTimeoutFlagShowsItsDefault(t *testing.T) {
	var f settings.Flags
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	f.RegisterStop(fs)
	if got := fs.Lookup("stop-timeout").DefValue; got != "10s" {
		t.Fatalf("default %q, want 10s", got)
	}
}

// The usages of stop and approve name their flags.
func TestStopAndApproveUsages(t *testing.T) {
	if got, want := settings.StopUsage(), "[--settings <path>] [--runtime-root <path>] [--stop-timeout <duration>]"; got != want {
		t.Errorf("StopUsage: got %q, want %q", got, want)
	}
	if got, want := settings.ApproveUsage(), "[--settings <path>] [--runtime-root <path>]"; got != want {
		t.Errorf("ApproveUsage: got %q, want %q", got, want)
	}
}

// ResolveHost takes each key from the flag, the variable, the common part
// of the host settings file, then the default: the runtime root under
// XDG_RUNTIME_DIR, 10s to stop.
func TestResolveHostTakesEachKeyFromTheHighestSource(t *testing.T) {
	file := writeFile(t, "runtime_root: /file/root\nstop_timeout: 1m\n")
	vars := map[string]string{"HOSTRUNNER_SETTINGS": file, "HOSTRUNNER_RUNTIME_ROOT": "/var/root", "HOSTRUNNER_STOP_TIMEOUT": "2m"}
	tests := []struct {
		name  string
		vars  map[string]string
		flags []string
		root  string
		stop  time.Duration
	}{
		{"defaults", nil, nil, "/run/user/1000/hostrunner", 10 * time.Second},
		{"common part", map[string]string{"HOSTRUNNER_SETTINGS": file}, nil, "/file/root", time.Minute},
		{"variables over the file", vars, nil, "/var/root", 2 * time.Minute},
		{"flags over the variables", vars, []string{"--runtime-root", "/flag/root", "--stop-timeout", "3m"}, "/flag/root", 3 * time.Minute},
		{"--settings", nil, []string{"--settings", file}, "/file/root", time.Minute},
	}
	for _, tt := range tests {
		got, err := parseWith(t, (*settings.Flags).RegisterStop, tt.flags...).ResolveHost(env(withXDG(tt.vars)))
		want := hostDefaults()
		want.RuntimeRoot, want.StopTimeout = tt.root, tt.stop
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", tt.name, got, err, want)
		}
	}
}

// Without XDG_RUNTIME_DIR and a runtime root from any source, the runtime
// root is empty: the command says what to give.
func TestResolveHostWithoutXDGRuntimeDirHasNoRuntimeRoot(t *testing.T) {
	got, err := parseWith(t, (*settings.Flags).RegisterApprove).ResolveHost(env(nil))
	if err != nil || got.RuntimeRoot != "" {
		t.Fatalf("got %q, %v; want no runtime root", got.RuntimeRoot, err)
	}
}

// A runtime root from a file or a variable must be absolute after ~
// expansion, and is cleaned; a flag's is read against the current
// directory. An invalid key or value is an error naming its source.
func TestResolveHostRuntimeRootPaths(t *testing.T) {
	relative := writeFile(t, "runtime_root: rt\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		vars    map[string]string
		flags   []string
		want    string // the runtime root, when no error is wanted
		wantErr string
	}{
		{name: "~ in the file", vars: map[string]string{"HOME": "/home/u", "HOSTRUNNER_SETTINGS": writeFile(t, "runtime_root: ~/rt\n")}, want: "/home/u/rt"},
		{name: "~ in the variable", vars: map[string]string{"HOME": "/home/u", "HOSTRUNNER_RUNTIME_ROOT": "~/var"}, want: "/home/u/var"},
		{name: "unclean variable", vars: map[string]string{"HOSTRUNNER_RUNTIME_ROOT": "/a//b/../c/"}, want: "/a/c"},
		{name: "relative flag", flags: []string{"--runtime-root", "rt"}, want: filepath.Join(cwd, "rt")},
		{name: "relative in the file", vars: map[string]string{"HOSTRUNNER_SETTINGS": relative}, wantErr: "settings file " + relative + `: runtime_root: must be absolute, got "rt"`},
		{name: "relative variable", vars: map[string]string{"HOSTRUNNER_RUNTIME_ROOT": "var"}, wantErr: `HOSTRUNNER_RUNTIME_ROOT: must be absolute, got "var"`},
		{name: "empty flag", flags: []string{"--runtime-root", ""}, wantErr: "--runtime-root: empty path"},
		{name: "zero stop timeout", vars: map[string]string{"HOSTRUNNER_STOP_TIMEOUT": "0s"}, wantErr: "HOSTRUNNER_STOP_TIMEOUT: must be positive"},
		{name: "invalid stop timeout in the file", vars: map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, "stop_timeout: soon\n")}, wantErr: `stop_timeout: invalid duration "soon"`},
		{name: "unknown key in the common part", vars: map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, "stop_timeot: 1s\n")}, wantErr: "stop_timeot: unknown key"},
	}
	for _, tt := range tests {
		got, err := parseWith(t, (*settings.Flags).RegisterStop, tt.flags...).ResolveHost(env(withXDG(tt.vars)))
		switch {
		case tt.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: got %v, want an error containing %q", tt.name, err, tt.wantErr)
			}
		case err != nil || got.RuntimeRoot != tt.want:
			t.Errorf("%s: got %q, %v; want %q", tt.name, got.RuntimeRoot, err, tt.want)
		}
	}
}

// stop, restart and approve read neither the workspace sections, which
// hold no key they take, nor the daemon's keys in the common part, nor
// any workspace settings file.
func TestResolveHostReadsTheCommonPartAlone(t *testing.T) {
	file := writeFile(t, "grace: soon\nrules_file: ''\n"+section(t.TempDir(), "grase: 1m"))
	got, err := parseWith(t, (*settings.Flags).RegisterStop).ResolveHost(env(withXDG(map[string]string{
		"HOSTRUNNER_SETTINGS":           file,
		"HOSTRUNNER_GRACE":              "abc",
		"HOSTRUNNER_WORKSPACE_SETTINGS": "/nonexistent/hostrunner.yaml",
	})))
	if want := hostDefaults(); err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

// approve takes no stop timeout: it neither reads nor checks one.
func TestResolveHostForApproveIgnoresTheStopTimeout(t *testing.T) {
	got, err := parseWith(t, (*settings.Flags).RegisterApprove).ResolveHost(env(withXDG(map[string]string{"HOSTRUNNER_STOP_TIMEOUT": "soon"})))
	if err != nil || got.RuntimeRoot != "/run/user/1000/hostrunner" {
		t.Fatalf("got %+v, %v; want the default runtime root", got, err)
	}
}

// up and serve neither read nor check the keys of stop, restart and
// approve in the common part or their variables.
func TestDaemonCommandsIgnoreTheHostKeys(t *testing.T) {
	file := writeFile(t, "runtime_root: rt\nstop_timeout: soon\n")
	vars := map[string]string{"HOSTRUNNER_SETTINGS": file, "HOSTRUNNER_RUNTIME_ROOT": "rt", "HOSTRUNNER_STOP_TIMEOUT": "soon"}
	for name, f := range map[string]*settings.Flags{"serve": parse(t), "up": parseUp(t)} {
		if got, err := f.Resolve(env(vars), noWorkspace); err != nil || got.RuntimeRoot != "" || got.StopTimeout != 0 {
			t.Errorf("%s: got %+v, %v; want no runtime root or stop timeout", name, got, err)
		}
	}
}
