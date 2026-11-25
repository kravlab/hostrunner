package settings_test

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// ArmSettings names every setting serve takes, by its key, with its value as
// a settings file writes it; the keys of up, stop, restart and approve
// are left out, and a string is kept even when empty.
func TestArmSettingsNameEverySettingServeTakes(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    settings.Settings
		want map[string]string
	}{
		{
			name: "every setting",
			s: settings.Settings{
				RulesFile: "/w/rules.yaml", StartupTimeout: time.Hour, Grace: 90 * time.Second,
				Reread: false, PollInterval: 5 * time.Second, StoppedInspectionMaxAge: 7 * time.Second,
				AskTimeout: 30 * time.Minute, RequestTimeout: 30 * time.Second, ReadyTimeout: time.Minute,
				RuntimeRoot: "/r", StopTimeout: time.Minute, MountCheck: true, Process: settings.ProcessShared,
			},
			want: map[string]string{
				"rules_file": "/w/rules.yaml", "startup_timeout": "1h0m0s", "grace": "1m30s",
				"reread": "false", "poll_interval": "5s", "stopped_inspection_max_age": "7s",
				"ask_timeout": "30m0s", "request_timeout": "30s", "mount_check": "true", "process": "shared",
			},
		},
		{
			name: "an empty rules file",
			s:    settings.Settings{StartupTimeout: time.Hour, Grace: time.Second, PollInterval: time.Second, AskTimeout: time.Minute, RequestTimeout: time.Second, Process: settings.ProcessOwn},
			want: map[string]string{
				"rules_file": "", "startup_timeout": "1h0m0s", "grace": "1s", "reread": "false",
				"poll_interval": "1s", "stopped_inspection_max_age": "0s", "ask_timeout": "1m0s", "request_timeout": "1s",
				"mount_check": "false", "process": "own",
			},
		},
	} {
		if got := tt.s.ArmSettings(); !maps.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// DaemonDefaults are the defaults of the daemon settings the allow covers
// (no process), as AllowedSettings writes them: the rules file is the one
// up chooses there by default, the older name while the default name is
// absent.
func TestDaemonDefaultsAreTheSettingsNoSourceSets(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".devcontainer")
	want := map[string]string{
		"rules_file": filepath.Join(dir, "hostrunner-rules.yaml"), "startup_timeout": "30m0s", "grace": "15s",
		"reread": "true", "poll_interval": "2s", "stopped_inspection_max_age": "0s", "ask_timeout": "10m0s",
		"request_timeout": "10s", "mount_check": "true",
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
