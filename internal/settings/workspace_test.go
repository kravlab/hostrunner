package settings_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// newWorkspace makes a workspace directory, with content as its default
// workspace settings file unless content is empty, and returns its path.
func newWorkspace(t *testing.T, content string) string {
	t.Helper()
	ws := t.TempDir()
	if content == "" {
		return ws
	}
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".devcontainer", "hostrunner.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

// section is a workspaces map of the host settings file with one section,
// for path, holding settings (one `key: value` per line).
func section(path string, settings ...string) string {
	s := fmt.Sprintf("workspaces:\n  %q:\n", path)
	for _, line := range settings {
		s += "    " + line + "\n"
	}
	return s
}

// Each key comes from the highest source that sets it: flags, variables,
// the workspace settings file, the workspace's section of the host
// settings file, its common part, then the default.
func TestResolveTakesEachKeyFromTheHighestOfFiveSources(t *testing.T) {
	ws := t.TempDir()
	common := "startup_timeout: 1h\ngrace: 1m\nreread: false\n"
	sec := section(ws, "startup_timeout: 2h", "grace: 2m", "reread: true")
	wsFile := "startup_timeout: 3h\ngrace: 3m\nreread: false\n"
	vars := map[string]string{"HOSTRUNNER_STARTUP_TIMEOUT": "4h", "HOSTRUNNER_GRACE": "4m", "HOSTRUNNER_REREAD": "true"}
	flags := []string{"--startup-timeout", "5h", "--grace", "5m", "--no-reread"}
	tests := []struct {
		name         string
		host, wsFile string
		vars         map[string]string
		flags        []string
		want         settings.Settings
	}{
		{"common part", common, "", nil, nil, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: time.Hour, Grace: time.Minute, Reread: false}},
		{"section over common part", common + sec, "", nil, nil, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: 2 * time.Hour, Grace: 2 * time.Minute, Reread: true}},
		{"workspace file over section", common + sec, wsFile, nil, nil, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: 3 * time.Hour, Grace: 3 * time.Minute, Reread: false}},
		{"variables over workspace file", common + sec, wsFile, vars, nil, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: 4 * time.Hour, Grace: 4 * time.Minute, Reread: true}},
		{"flags over variables", common + sec, wsFile, vars, flags, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: 5 * time.Hour, Grace: 5 * time.Minute, Reread: false}},
		{"one key per source", "reread: false\n" + section(ws, "grace: 2m"), "startup_timeout: 3h\n", nil, nil, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: 3 * time.Hour, Grace: 2 * time.Minute, Reread: false}},
		{"workspace file over common part", "grace: 1m\n", "grace: 3m\n", nil, nil, settings.Settings{RulesFile: rulesIn(ws), StartupTimeout: defaults.StartupTimeout, Grace: 3 * time.Minute, Reread: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{"HOSTRUNNER_SETTINGS": writeFile(t, tt.host)}
			if tt.wsFile != "" {
				vars["HOSTRUNNER_WORKSPACE_SETTINGS"] = writeFile(t, tt.wsFile)
			}
			maps.Copy(vars, tt.vars)
			got, err := parse(t, tt.flags...).Resolve(env(vars), ws)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A section applies to the workspace whose path is its own after ~
// expansion and symlink resolution, on either side, and to no other.
func TestSectionAppliesToItsWorkspaceAlone(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "app")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	tests := []struct {
		name, key, workspace string
		want                 time.Duration
	}{
		{"same path", ws, ws, time.Minute},
		{"trailing slash", ws + "/", ws, time.Minute},
		{"~ expanded", "~/app", ws, time.Minute},
		{"section key a symlink", link, ws, time.Minute},
		{"workspace a symlink", ws, link, time.Minute},
		{"another workspace", ws, other, defaults.Grace},
		{"a subdirectory", ws, filepath.Join(ws, "sub"), defaults.Grace},
		{"the parent directory", ws, home, defaults.Grace},
		{"missing path", filepath.Join(home, "gone"), ws, defaults.Grace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, section(tt.key, "grace: 1m"))
			got, err := parse(t, "--settings", path).Resolve(env(map[string]string{"HOME": home}), tt.workspace)
			if err != nil || got.Grace != tt.want {
				t.Fatalf("got grace %v, %v; want %v", got.Grace, err, tt.want)
			}
		})
	}
}

// A section path may be written unquoted with ~, as the README shows.
func TestSectionPathWithUnquotedTilde(t *testing.T) {
	home := t.TempDir()
	path := writeFile(t, "workspaces:\n  ~:\n    grace: 1m\n")
	got, err := parse(t, "--settings", path).Resolve(env(map[string]string{"HOME": home}), home)
	if err != nil || got.Grace != time.Minute {
		t.Fatalf("~: got grace %v, %v; want 1m", got.Grace, err)
	}
	if err := os.Mkdir(filepath.Join(home, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	path = writeFile(t, "workspaces:\n  ~/app:\n    grace: 2m\n")
	got, err = parse(t, "--settings", path).Resolve(env(map[string]string{"HOME": home}), filepath.Join(home, "app"))
	if err != nil || got.Grace != 2*time.Minute {
		t.Fatalf("~/app: got grace %v, %v; want 2m", got.Grace, err)
	}
}

// A workspace that does not exist matches no section, and that is no
// error.
func TestNoSectionAppliesToAMissingWorkspace(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "gone")
	path := writeFile(t, section(ws, "grace: 1m"))
	got, err := parse(t, "--settings", path).Resolve(env(nil), ws)
	want := defaults
	want.RulesFile = rulesIn(ws)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want the defaults", got, err)
	}
}

func TestSectionsRejectWhatTheyCannotApply(t *testing.T) {
	ws := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // the sections below name another workspace too: they are checked all the same
	tests := []struct {
		name, content, want string
	}{
		{"unknown key", section(other, "grase: 1m"), "workspaces." + other + ": grase: unknown key"},
		{"invalid value", section(other, "grace: soon"), "workspaces." + other + `: grace: invalid duration "soon"`},
		{"key given twice", section(other, "grace: 1s", "grace: 2s"), "workspaces." + other + ": grace: given twice"},
		{"section not a mapping", fmt.Sprintf("workspaces:\n  %q: 1m\n", other), "workspaces." + other + ": not a mapping of settings"},
		{"workspaces not a mapping", "workspaces: [a]\n", "workspaces: not a mapping of workspaces"},
		{"workspaces given twice", "workspaces: {}\nworkspaces: {}\n", "workspaces: given twice"},
		{"section given twice", fmt.Sprintf("workspaces:\n  %q: {}\n  %[1]q: {}\n", other), "workspaces." + other + ": given twice"},
		{"relative path", section("app", "grace: 1m"), "workspaces.app: not an absolute path"},
		{"two paths of the workspace", fmt.Sprintf("workspaces:\n  %q: {}\n  %q: {}\n", ws, link), "workspaces: " + ws + " and " + link + " both name the workspace " + ws},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, tt.content)
			_, err := parse(t, "--settings", path).Resolve(env(nil), ws)
			if err == nil {
				t.Fatal("got no error")
			}
			if !strings.HasPrefix(err.Error(), "settings file "+path+": ") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, want it to name %s and contain %q", err, path, tt.want)
			}
		})
	}
}

// The workspace settings file is the one --workspace-settings names,
// relative to the current directory, else HOSTRUNNER_WORKSPACE_SETTINGS,
// relative to the workspace, else .devcontainer/hostrunner.yaml in the
// workspace.
func TestWorkspaceFileIsNamedByFlagThenVariableThenTheWorkspace(t *testing.T) {
	ws := newWorkspace(t, "grace: 3m\n")
	if err := os.WriteFile(filepath.Join(ws, "var.yaml"), []byte("grace: 2m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "flag.yaml"), []byte("grace: 1m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  time.Duration
	}{
		{"flag, relative to the current directory", []string{"--workspace-settings", "flag.yaml"}, nil, time.Minute},
		{"flag, absolute", []string{"--workspace-settings", filepath.Join(cwd, "flag.yaml")}, nil, time.Minute},
		{"variable, relative to the workspace", nil, map[string]string{"HOSTRUNNER_WORKSPACE_SETTINGS": "var.yaml"}, 2 * time.Minute},
		{"variable, absolute", nil, map[string]string{"HOSTRUNNER_WORKSPACE_SETTINGS": filepath.Join(ws, "var.yaml")}, 2 * time.Minute},
		{"flag beats variable", []string{"--workspace-settings", "flag.yaml"}, map[string]string{"HOSTRUNNER_WORKSPACE_SETTINGS": "var.yaml"}, time.Minute},
		{"default", nil, nil, 3 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(t, tt.flags...).Resolve(env(tt.vars), ws)
			if err != nil || got.Grace != tt.want {
				t.Fatalf("got grace %v, %v; want %v", got.Grace, err, tt.want)
			}
		})
	}
}

func TestMissingDefaultWorkspaceFileSetsNothing(t *testing.T) {
	ws := newWorkspace(t, "")
	got, err := parse(t).Resolve(env(nil), ws)
	want := defaults
	want.RulesFile = rulesIn(ws)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want the defaults", got, err)
	}
}

func TestResolveFailsOnAMissingNamedWorkspaceFile(t *testing.T) {
	ws := newWorkspace(t, "")
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for name, tt := range map[string]struct {
		flags []string
		vars  map[string]string
		want  string
	}{
		"--workspace-settings":          {[]string{"--workspace-settings", missing}, nil, missing},
		"HOSTRUNNER_WORKSPACE_SETTINGS": {nil, map[string]string{"HOSTRUNNER_WORKSPACE_SETTINGS": "missing.yaml"}, filepath.Join(ws, "missing.yaml")},
	} {
		if _, err := parse(t, tt.flags...).Resolve(env(tt.vars), ws); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want an error naming %s", name, err, tt.want)
		}
	}
}

// The workspace settings file holds the keys at its top level only: it has
// no workspace sections.
func TestWorkspaceFileRejectsWhatItCannotApply(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"unknown key", "grase: 15s\n", "grase: unknown key"},
		{"invalid value", "grace: soon\n", `grace: invalid duration "soon"`},
		{"workspace sections", section("/home/u/app", "grace: 1m"), "workspaces: unknown key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := newWorkspace(t, tt.content)
			path := filepath.Join(ws, ".devcontainer", "hostrunner.yaml")
			_, err := parse(t).Resolve(env(nil), ws)
			if want := "settings file " + path + ": " + tt.want; err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}

// runtime_root and stop_timeout are read before a workspace is known, so
// neither the workspace settings file nor a workspace section may set
// them.
func TestCommonOnlyKeysAreRefusedOutsideTheCommonPart(t *testing.T) {
	for _, name := range []string{"runtime_root", "stop_timeout"} {
		t.Run(name, func(t *testing.T) {
			ws := newWorkspace(t, name+": x\n")
			path := filepath.Join(ws, ".devcontainer", "hostrunner.yaml")
			_, err := parse(t).Resolve(env(nil), ws)
			if want := "settings file " + path + ": " + name + ": only allowed in the common part of the host settings file"; err == nil || err.Error() != want {
				t.Errorf("workspace file: got %v, want %q", err, want)
			}
			other := t.TempDir()
			host := writeFile(t, section(other, name+": x"))
			_, err = parse(t, "--settings", host).Resolve(env(nil), newWorkspace(t, ""))
			if want := "settings file " + host + ": workspaces." + other + ": " + name + ": only allowed in the common part of the host settings file"; err == nil || err.Error() != want {
				t.Errorf("section: got %v, want %q", err, want)
			}
		})
	}
}

// ResolveAsGiven given every key as a flag does not read the workspace
// settings file either: a broken one is not even opened.
func TestResolveAsGivenReadsNoWorkspaceFileWhenFlagsSetEveryKey(t *testing.T) {
	ws := newWorkspace(t, "grase: [\n")
	got, err := parse(t, everyKey...).ResolveAsGiven(env(nil), ws)
	want := settings.Settings{RulesFile: givenRules, StartupTimeout: time.Hour, Grace: time.Minute, Reread: false}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if _, err := parse(t, everyKey...).Resolve(env(nil), ws); err == nil {
		t.Fatal("Resolve: got no error for a broken workspace settings file")
	}
}
