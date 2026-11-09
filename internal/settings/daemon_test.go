package settings_test

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// Daemon names every setting serve takes, by its key, with its value as
// a settings file writes it; the keys of up, stop, restart and approve
// are left out, and a string is kept even when empty.
func TestDaemonNamesEverySettingServeTakes(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    settings.Settings
		want map[string]string
	}{
		{
			name: "every setting",
			s: settings.Settings{
				RulesFile: "/w/rules.yaml", StartupTimeout: time.Hour, Grace: 90 * time.Second,
				Reread: false, PollInterval: 5 * time.Second, AskTimeout: 30 * time.Minute,
				RequestTimeout: 30 * time.Second, ReadyTimeout: time.Minute, RuntimeRoot: "/r",
				StopTimeout: time.Minute, MountCheck: true,
			},
			want: map[string]string{
				"rules_file": "/w/rules.yaml", "startup_timeout": "1h0m0s", "grace": "1m30s",
				"reread": "false", "poll_interval": "5s", "ask_timeout": "30m0s",
				"request_timeout": "30s", "mount_check": "true",
			},
		},
		{
			name: "an empty rules file",
			s:    settings.Settings{StartupTimeout: time.Hour, Grace: time.Second, PollInterval: time.Second, AskTimeout: time.Minute, RequestTimeout: time.Second},
			want: map[string]string{
				"rules_file": "", "startup_timeout": "1h0m0s", "grace": "1s", "reread": "false",
				"poll_interval": "1s", "ask_timeout": "1m0s", "request_timeout": "1s",
				"mount_check": "false",
			},
		},
	} {
		if got := tt.s.Daemon(); !maps.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// DaemonDefaults are the daemon settings no source sets for a workspace,
// as Daemon writes them: the rules file is the one up chooses there by
// default, the older name while the default name is absent.
func TestDaemonDefaultsAreTheSettingsNoSourceSets(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".devcontainer")
	want := map[string]string{
		"rules_file": filepath.Join(dir, "hostrunner-rules.yaml"), "startup_timeout": "30m0s", "grace": "15s",
		"reread": "true", "poll_interval": "2s", "ask_timeout": "10m0s", "request_timeout": "10s",
		"mount_check": "true",
	}
	if got := settings.DaemonDefaults(ws); !maps.Equal(got, want) {
		t.Errorf("no rules file: got %v, want %v", got, want)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostrun.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want["rules_file"] = filepath.Join(dir, "hostrun.yaml")
	if got := settings.DaemonDefaults(ws); !maps.Equal(got, want) {
		t.Errorf("the older rules file only: got %v, want %v", got, want)
	}
}
