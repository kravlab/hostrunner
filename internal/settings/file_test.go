package settings_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// writeHostFile writes `grace: <grace>` to dir/hostrunner/settings.yaml,
// where a default host settings file lies.
func writeHostFile(t *testing.T, dir, grace string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "hostrunner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostrunner", "settings.yaml"), []byte("grace: "+grace+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostFileIsNamedByFlagThenVariableThenXDGThenHome(t *testing.T) {
	flagFile, varFile := writeFile(t, "grace: 1m\n"), writeFile(t, "grace: 2m\n")
	xdg, home := t.TempDir(), t.TempDir()
	writeHostFile(t, xdg, "3m")
	writeHostFile(t, filepath.Join(home, ".config"), "4m")
	tests := []struct {
		name  string
		flags []string
		vars  map[string]string
		want  time.Duration
	}{
		{"flag", []string{"--settings", flagFile}, nil, time.Minute},
		{"variable", nil, map[string]string{"HOSTRUNNER_SETTINGS": varFile}, 2 * time.Minute},
		{"flag beats variable", []string{"--settings", flagFile}, map[string]string{"HOSTRUNNER_SETTINGS": varFile}, time.Minute},
		{"XDG_CONFIG_HOME", nil, map[string]string{"XDG_CONFIG_HOME": xdg, "HOME": home}, 3 * time.Minute},
		{"HOME without XDG_CONFIG_HOME", nil, map[string]string{"HOME": home}, 4 * time.Minute},
		{"missing default file", nil, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, defaults.Grace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(t, tt.flags...).Resolve(env(tt.vars), noWorkspace)
			if err != nil || got.Grace != tt.want {
				t.Fatalf("got grace %v, %v; want %v", got.Grace, err, tt.want)
			}
		})
	}
}

// With neither XDG_CONFIG_HOME nor HOME there is no default host
// settings file: none is read, not even one relative to the current
// directory, and that is no error.
func TestNoDefaultHostFileWithoutXDGOrHome(t *testing.T) {
	dir := t.TempDir()
	writeHostFile(t, dir, "3m")
	writeHostFile(t, filepath.Join(dir, ".config"), "4m")
	t.Chdir(dir)
	got, err := parse(t).Resolve(env(nil), noWorkspace)
	if err != nil || got != defaults {
		t.Fatalf("got %+v, %v; want the defaults", got, err)
	}
}

func TestResolveFailsOnAMissingNamedHostFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for name, tt := range map[string]struct {
		flags []string
		vars  map[string]string
	}{
		"--settings":          {[]string{"--settings", missing}, nil},
		"HOSTRUNNER_SETTINGS": {nil, map[string]string{"HOSTRUNNER_SETTINGS": missing}},
	} {
		if _, err := parse(t, tt.flags...).Resolve(env(tt.vars), noWorkspace); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("%s: got %v, want an error naming %s", name, err, missing)
		}
	}
}

func TestHostFileSetsEveryKey(t *testing.T) {
	path := writeFile(t, "# the host's settings\nstartup_timeout: 1h\ngrace: 1m30s\nreread: false\n")
	got, err := parse(t, "--settings", path).Resolve(env(nil), noWorkspace)
	want := settings.Settings{RulesFile: defaults.RulesFile, StartupTimeout: time.Hour, Grace: 90 * time.Second, Reread: false}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestHostFileWithoutSettingsSetsNothing(t *testing.T) {
	for _, content := range []string{"", "# nothing set\n"} {
		got, err := parse(t, "--settings", writeFile(t, content)).Resolve(env(nil), noWorkspace)
		if err != nil || got != defaults {
			t.Errorf("%q: got %+v, %v; want the defaults", content, got, err)
		}
	}
}

func TestHostFileRejectsWhatItCannotApply(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"unknown key", "grase: 15s\n", `grase: unknown key`},
		{"unparsable duration", "grace: abc\n", `grace: invalid duration "abc"`},
		{"duration without unit", "grace: 90\n", `grace: invalid duration 90`},
		{"zero duration", "startup_timeout: 0s\n", `startup_timeout: must be positive`},
		{"negative duration", "grace: -1s\n", `grace: must be positive`},
		{"duration as a list", "grace: [1s]\n", `grace: invalid duration`},
		{"unparsable bool", "reread: maybe\n", `reread: invalid bool "maybe"`},
		{"bool as a string", "reread: \"true\"\n", `reread: invalid bool "true"`},
		{"empty value", "grace:\n", `grace: invalid duration`},
		{"empty path", "rules_file: \"\"\n", `rules_file: empty path`},
		{"path without value", "rules_file:\n", `rules_file: invalid path: no value`},
		{"path as a number", "rules_file: 12\n", `rules_file: invalid path 12`},
		{"path as a list", "rules_file: [a]\n", `rules_file: invalid path: not a single value`},
		{"key given twice", "grace: 1s\ngrace: 2s\n", `grace: given twice`},
		{"not a mapping", "- grace\n", `not a mapping of settings`},
		{"broken YAML", "grace: [\n", `settings file`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, tt.content)
			_, err := parse(t, "--settings", path).Resolve(env(nil), noWorkspace)
			if err == nil {
				t.Fatal("got no error")
			}
			if !strings.HasPrefix(err.Error(), "settings file "+path+": ") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, want it to name %s and contain %q", err, path, tt.want)
			}
		})
	}
}
