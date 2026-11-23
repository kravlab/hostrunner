// Package settingstest holds the settings fixtures that the tests of
// several packages share, so that the built-in settings are written once
// outside the code under test: a change of a default shows in one place.
package settingstest

import (
	"time"

	"github.com/kravlab/hostrunner/internal/settings"
)

// GivenProcess is the process that the tests' every-key flags give: the one
// that is not the default, so that a test sees the flag was read.
const GivenProcess = settings.ProcessOwn

// Defaults returns the built-in settings that no source sets, with the rules
// file rulesFile. It must stay identical to the defaults of package
// settings: tests compare what is resolved against it.
func Defaults(rulesFile string) settings.Settings {
	return settings.Settings{
		RulesFile:      rulesFile,
		StartupTimeout: 30 * time.Minute,
		Grace:          15 * time.Second,
		Reread:         true,
		PollInterval:   2 * time.Second,
		AskTimeout:     10 * time.Minute,
		RequestTimeout: 10 * time.Second,
		ReadyTimeout:   10 * time.Second,
		Process:        settings.ProcessShared,
		MountCheck:     true,
	}
}
