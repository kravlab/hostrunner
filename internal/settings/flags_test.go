package settings_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// Only the flags given set their keys, even with their default value:
// every other key comes from the host settings file, which sets them all.
func TestFlagsSetOnlyTheKeysGiven(t *testing.T) {
	file := writeFile(t, "startup_timeout: 1h\ngrace: 1m\nreread: false\n")
	fromFile := settings.Settings{StartupTimeout: time.Hour, Grace: time.Minute, Reread: false}
	tests := []struct {
		args []string
		want settings.Settings
	}{
		{nil, fromFile},
		{[]string{"--startup-timeout", "2h"}, settings.Settings{StartupTimeout: 2 * time.Hour, Grace: time.Minute, Reread: false}},
		{[]string{"--grace", "1m30s"}, settings.Settings{StartupTimeout: time.Hour, Grace: 90 * time.Second, Reread: false}},
		{[]string{"--reread"}, settings.Settings{StartupTimeout: time.Hour, Grace: time.Minute, Reread: true}},
		{[]string{"--reread=false"}, fromFile},
		{[]string{"--no-reread=false"}, settings.Settings{StartupTimeout: time.Hour, Grace: time.Minute, Reread: true}},
		// A flag given with its default value is still given.
		{[]string{"--grace", "15s", "--startup-timeout", "30m", "--reread"}, defaults},
	}
	for _, tt := range tests {
		got, err := parse(t, append(tt.args, "--settings", file)...).Resolve(env(nil), noWorkspace)
		if err != nil || got != tt.want {
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
	for _, args := range [][]string{{"--grace", "0s"}, {"--startup-timeout", "-1m"}} {
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
