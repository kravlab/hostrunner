package settings_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/settings"
)

// workspaceWith returns a workspace whose .devcontainer holds the named
// files, each empty.
func workspaceWith(t *testing.T, names ...string) string {
	t.Helper()
	ws := t.TempDir()
	dir := filepath.Join(ws, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// The default rules file is hostrunner-rules.yaml, or hostrun.yaml while
// the first is absent; with both, the first is used and the second is
// named as ignored.
func TestResolveChoosesTheDefaultRulesFile(t *testing.T) {
	tests := []struct {
		name        string
		files       []string
		want        string
		wantIgnored string
	}{
		{"neither", nil, "hostrunner-rules.yaml", ""},
		{"new only", []string{"hostrunner-rules.yaml"}, "hostrunner-rules.yaml", ""},
		{"old only", []string{"hostrun.yaml"}, "hostrun.yaml", ""},
		{"both", []string{"hostrunner-rules.yaml", "hostrun.yaml"}, "hostrunner-rules.yaml", "hostrun.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := workspaceWith(t, tt.files...)
			dir := filepath.Join(ws, ".devcontainer")
			got, err := parse(t).Resolve(env(nil), ws)
			if err != nil {
				t.Fatal(err)
			}
			if got.RulesFile != filepath.Join(dir, tt.want) {
				t.Errorf("RulesFile = %q, want %q", got.RulesFile, filepath.Join(dir, tt.want))
			}
			wantIgnored := ""
			if tt.wantIgnored != "" {
				wantIgnored = filepath.Join(dir, tt.wantIgnored)
			}
			if got.IgnoredRules != wantIgnored {
				t.Errorf("IgnoredRules = %q, want %q", got.IgnoredRules, wantIgnored)
			}
		})
	}
}

// A rules_file from any source is used as given, with no fallback and no
// ignored file, even when it is the default path itself and both files lie
// in .devcontainer.
func TestResolveUsesAnExplicitRulesFileAsGiven(t *testing.T) {
	ws := workspaceWith(t, "hostrunner-rules.yaml", "hostrun.yaml")
	dir := filepath.Join(ws, ".devcontainer")
	newPath, oldPath := filepath.Join(dir, "hostrunner-rules.yaml"), filepath.Join(dir, "hostrun.yaml")
	for _, given := range []string{"/w/rules.yaml", newPath, oldPath} {
		sources := []struct {
			name  string
			flags []string
			vars  map[string]string
			ws    string // workspace settings file content
			host  string // host settings file content
		}{
			{name: "flag", flags: []string{"--rules-file", given}},
			{name: "variable", vars: map[string]string{"HOSTRUNNER_RULES_FILE": given}},
			{name: "workspace settings file", ws: fmt.Sprintf("rules_file: %q\n", given)},
			{name: "host settings file", host: fmt.Sprintf("rules_file: %q\n", given)},
			{name: "host settings file section", host: fmt.Sprintf("workspaces:\n  %q:\n    rules_file: %q\n", ws, given)},
		}
		for _, src := range sources {
			t.Run(src.name+" "+filepath.Base(given), func(t *testing.T) {
				vars := map[string]string{}
				for k, v := range src.vars {
					vars[k] = v
				}
				if src.ws != "" {
					path := filepath.Join(dir, "hostrunner.yaml")
					if err := os.WriteFile(path, []byte(src.ws), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.Remove(path) })
				}
				if src.host != "" {
					vars["HOSTRUNNER_SETTINGS"] = writeFile(t, src.host)
				}
				got, err := parse(t, src.flags...).Resolve(env(vars), ws)
				if err != nil {
					t.Fatal(err)
				}
				if got.RulesFile != given || got.IgnoredRules != "" {
					t.Errorf("got RulesFile %q, IgnoredRules %q; want %q and none", got.RulesFile, got.IgnoredRules, given)
				}
			})
		}
	}
}

// ResolveAsGiven, which reads nothing when the flags set every key, makes
// no choice: the rules file is the flag's, whatever lies beside it.
func TestResolveAsGivenChoosesNoRulesFile(t *testing.T) {
	ws := workspaceWith(t, "hostrunner-rules.yaml", "hostrun.yaml")
	got, err := parse(t, everyKey...).ResolveAsGiven(env(nil), ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.RulesFile != givenRules {
		t.Errorf("RulesFile = %q, want %q", got.RulesFile, givenRules)
	}
}

// The flags' help does not depend on the files in the current directory.
func TestFlagHelpIgnoresTheCurrentDirectory(t *testing.T) {
	help := func() string {
		var (
			f   settings.Flags
			buf strings.Builder
		)
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&buf)
		f.RegisterUp(fs)
		fs.PrintDefaults()
		return buf.String()
	}
	empty := help()
	t.Chdir(workspaceWith(t, "hostrunner-rules.yaml", "hostrun.yaml"))
	if got := help(); got != empty {
		t.Errorf("help changed with the directory:\n%s\nvs\n%s", got, empty)
	}
}
