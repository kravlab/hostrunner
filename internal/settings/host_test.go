package settings_test

import (
	"errors"
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

// stopDefaults are the settings of stop and restart without any source
// but XDG_RUNTIME_DIR=/run/user/1000: they hold the keys they take alone.
func stopDefaults() settings.Settings {
	return settings.Settings{RuntimeRoot: "/run/user/1000/hostrunner", StopTimeout: 10 * time.Second}
}

// withXDG is vars and XDG_RUNTIME_DIR=/run/user/1000.
func withXDG(vars map[string]string) map[string]string {
	out := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}
	maps.Copy(out, vars)
	return out
}

// stop and restart take --settings, --runtime-root and --stop-timeout,
// approve the first two and --approve-tail-lines; up and serve take none
// of the last three.
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
	if got, want := names((*settings.Flags).RegisterApprove), []string{"approve-tail-lines", "runtime-root", "settings"}; !slices.Equal(got, want) {
		t.Errorf("RegisterApprove: got %q, want %q", got, want)
	}
	for _, register := range []func(*settings.Flags, *flag.FlagSet){(*settings.Flags).Register, (*settings.Flags).RegisterUp} {
		for _, name := range names(register) {
			if name == "runtime-root" || name == "stop-timeout" || name == "approve-tail-lines" {
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

// The usages of stop and approve name their flags, and
// --approve-tail-lines shows its default in the help.
func TestStopAndApproveUsages(t *testing.T) {
	if got, want := settings.StopUsage(), "[--settings <path>] [--runtime-root <path>] [--stop-timeout <duration>]"; got != want {
		t.Errorf("StopUsage: got %q, want %q", got, want)
	}
	if got, want := settings.ApproveUsage(), "[--settings <path>] [--runtime-root <path>] [--approve-tail-lines <integer>]"; got != want {
		t.Errorf("ApproveUsage: got %q, want %q", got, want)
	}
	var f settings.Flags
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	f.RegisterApprove(fs)
	if got := fs.Lookup("approve-tail-lines").DefValue; got != "3" {
		t.Errorf("--approve-tail-lines: default %q, want 3", got)
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
		want := stopDefaults()
		want.RuntimeRoot, want.StopTimeout = tt.root, tt.stop
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", tt.name, got, err, want)
		}
	}
}

// approve takes the number of tail lines from the flag, the variable, the
// common part of the host settings file, then the default, 3; 0 is a
// number like any other, not the default.
func TestResolveHostTakesTheApproveTailLinesFromTheHighestSource(t *testing.T) {
	file := writeFile(t, "approve_tail_lines: 5\n")
	vars := map[string]string{"HOSTRUNNER_SETTINGS": file, "HOSTRUNNER_APPROVE_TAIL_LINES": "7"}
	tests := []struct {
		name  string
		vars  map[string]string
		flags []string
		want  int
	}{
		{"default", nil, nil, 3},
		{"common part", map[string]string{"HOSTRUNNER_SETTINGS": file}, nil, 5},
		{"zero in the common part", map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, "approve_tail_lines: 0\n")}, nil, 0},
		{"variable over the file", vars, nil, 7},
		{"zero variable", map[string]string{"HOSTRUNNER_SETTINGS": file, "HOSTRUNNER_APPROVE_TAIL_LINES": "0"}, nil, 0},
		{"flag over the variable", vars, []string{"--approve-tail-lines", "9"}, 9},
		{"zero flag", vars, []string{"--approve-tail-lines", "0"}, 0},
		{"signed in the common part", map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, "approve_tail_lines: +3\n")}, nil, 3},
		{"signed variable", map[string]string{"HOSTRUNNER_SETTINGS": file, "HOSTRUNNER_APPROVE_TAIL_LINES": "+3"}, nil, 3},
		{"signed flag", vars, []string{"--approve-tail-lines", "+3"}, 3},
		{"leading zero flag", vars, []string{"--approve-tail-lines", "010"}, 10},
		{"--settings", nil, []string{"--settings", file}, 5},
	}
	for _, tt := range tests {
		got, err := parseWith(t, (*settings.Flags).RegisterApprove, tt.flags...).ResolveHost(env(withXDG(tt.vars)))
		if err != nil || got.ApproveTailLines != tt.want {
			t.Errorf("%s: got %d, %v; want %d", tt.name, got.ApproveTailLines, err, tt.want)
		}
	}
}

// A negative or non-numeric number of tail lines is an error naming its
// source; a flag's is a *FlagError, which the command adds its usage to.
func TestResolveHostRefusesAnInvalidApproveTailLines(t *testing.T) {
	inFile := func(content string) (map[string]string, string) {
		path := writeFile(t, content)
		return map[string]string{"HOSTRUNNER_SETTINGS": path}, "settings file " + path + ": approve_tail_lines: "
	}
	negative, negativeIn := inFile("approve_tail_lines: -2\n")
	word, wordIn := inFile("approve_tail_lines: abc\n")
	fraction, fractionIn := inFile("approve_tail_lines: 2.5\n")
	quoted, quotedIn := inFile("approve_tail_lines: \"3\"\n")
	empty, emptyIn := inFile("approve_tail_lines:\n")
	list, listIn := inFile("approve_tail_lines: [3]\n")
	tests := []struct {
		name  string
		vars  map[string]string
		flags []string
		want  string
	}{
		{"negative variable", map[string]string{"HOSTRUNNER_APPROVE_TAIL_LINES": "-1"}, nil, "HOSTRUNNER_APPROVE_TAIL_LINES: must not be negative, got -1"},
		{"non-numeric variable", map[string]string{"HOSTRUNNER_APPROVE_TAIL_LINES": "abc"}, nil, `HOSTRUNNER_APPROVE_TAIL_LINES: invalid integer "abc"`},
		{"negative in the file", negative, nil, negativeIn + "must not be negative, got -2"},
		{"non-numeric in the file", word, nil, wordIn + `invalid integer "abc"`},
		{"fraction in the file", fraction, nil, fractionIn + "invalid integer 2.5"},
		{"string in the file", quoted, nil, quotedIn + `invalid integer "3"`},
		{"no value in the file", empty, nil, emptyIn + "invalid integer: no value (line 1)"},
		{"list in the file", list, nil, listIn + "invalid integer: not a single value (line 1)"},
		{"negative flag", nil, []string{"--approve-tail-lines", "-1"}, "--approve-tail-lines: must not be negative"},
	}
	for _, tt := range tests {
		_, err := parseWith(t, (*settings.Flags).RegisterApprove, tt.flags...).ResolveHost(env(withXDG(tt.vars)))
		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: got %v, want %q", tt.name, err, tt.want)
		}
		if _, isFlag := errors.AsType[*settings.FlagError](err); isFlag != (tt.flags != nil) {
			t.Errorf("%s: got a *FlagError %v, want one only for a flag", tt.name, isFlag)
		}
	}
}

// stop and restart take no tail lines: they neither read nor check them.
func TestResolveHostForStopIgnoresTheApproveTailLines(t *testing.T) {
	file := writeFile(t, "approve_tail_lines: abc\n")
	got, err := parseWith(t, (*settings.Flags).RegisterStop).ResolveHost(env(withXDG(map[string]string{
		"HOSTRUNNER_SETTINGS":           file,
		"HOSTRUNNER_APPROVE_TAIL_LINES": "-1",
	})))
	if want := stopDefaults(); err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
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
		{name: "~ in the variable without HOME", vars: map[string]string{"HOSTRUNNER_RUNTIME_ROOT": "~/var"}, wantErr: "HOSTRUNNER_RUNTIME_ROOT: ~ needs HOME, which is not set"},
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
	if want := stopDefaults(); err != nil || got != want {
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
	file := writeFile(t, "runtime_root: rt\nstop_timeout: soon\napprove_tail_lines: -1\n")
	vars := map[string]string{"HOSTRUNNER_SETTINGS": file, "HOSTRUNNER_RUNTIME_ROOT": "rt", "HOSTRUNNER_STOP_TIMEOUT": "soon", "HOSTRUNNER_APPROVE_TAIL_LINES": "abc"}
	for name, f := range map[string]*settings.Flags{"serve": parse(t), "up": parseUp(t)} {
		if got, err := f.Resolve(env(vars), noWorkspace); err != nil || got.RuntimeRoot != "" || got.StopTimeout != 0 || got.ApproveTailLines != 0 {
			t.Errorf("%s: got %+v, %v; want no runtime root, stop timeout or tail lines", name, got, err)
		}
	}
}
