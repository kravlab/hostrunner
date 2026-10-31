package settings

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

// Flags are the flags of the settings on one command line: --settings,
// one per key (--startup-timeout, --grace) and, for reread, both
// --reread and --no-reread. Register them, parse, then resolve.
type Flags struct {
	fs        *flag.FlagSet
	file      string
	durations map[string]*time.Duration
	reread    bool
	noReread  bool
}

// FlagError is a settings flag given a value no setting can take
// (`--grace: must be positive`) or --reread with --no-reread, as the
// resolving methods of Flags return it, so a caller can tell it from an
// invalid variable or file and add its usage.
type FlagError struct{ err error }

// Error is the problem with the flag, naming it.
func (e *FlagError) Error() string { return e.err.Error() }

// Unwrap returns the problem.
func (e *FlagError) Unwrap() error { return e.err }

// Register adds the settings' flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	f.fs = fs
	f.durations = map[string]*time.Duration{}
	defaults := defaults()
	fs.StringVar(&f.file, "settings", "", "host settings file (default $XDG_CONFIG_HOME/hostrunner/settings.yaml)")
	for _, k := range keys {
		switch p := k.field(&defaults).(type) {
		case *time.Duration:
			f.durations[k.name] = fs.Duration(k.flagName(), *p, k.usage)
		case *bool:
			fs.BoolVar(&f.reread, k.flagName(), false, k.usage)
			fs.BoolVar(&f.noReread, "no-"+k.flagName(), false, "do not "+k.usage)
		}
	}
}

// Resolve returns the settings of the parsed command line, each key
// taken from the highest source that sets it: its flag, its HOSTRUNNER_*
// variable getenv reads, the host settings file (--settings, else
// $HOSTRUNNER_SETTINGS, else $XDG_CONFIG_HOME/hostrunner/settings.yaml or
// $HOME/.config/hostrunner/settings.yaml), then the default. Every source
// is read, even when the flags set every key. A flag that cannot be
// applied is a *FlagError.
func (f *Flags) Resolve(getenv func(string) string) (Settings, error) {
	flags, err := f.layer()
	if err != nil {
		return Settings{}, err
	}
	return resolve(flags, f.file, getenv)
}

// ResolveAsGiven is Resolve, except that when the flags set every key
// neither the variables nor any file is read: a daemon its `up` started
// with every setting as a flag runs with them as given.
func (f *Flags) ResolveAsGiven(getenv func(string) string) (Settings, error) {
	flags, err := f.layer()
	if err != nil {
		return Settings{}, err
	}
	if len(flags) < len(keys) {
		return resolve(flags, f.file, getenv)
	}
	s := defaults()
	apply(&s, flags)
	return s, nil
}

// layer returns the keys the parsed command line sets: only flags given
// count, even with their default value. A duration at or below zero
// (`--grace: must be positive`) and --reread with --no-reread are
// *FlagErrors.
func (f *Flags) layer() (layer, error) {
	l := layer{}
	var err error
	f.fs.Visit(func(fl *flag.Flag) {
		if err != nil {
			return
		}
		name := strings.ReplaceAll(fl.Name, "-", "_")
		switch {
		case f.durations[name] != nil:
			if d := *f.durations[name]; d > 0 {
				l[name] = d
			} else {
				err = &FlagError{fmt.Errorf("--%s: %w", fl.Name, errNotPositive)}
			}
		case name == "reread" || name == "no_reread":
			if _, set := l["reread"]; set {
				err = &FlagError{errors.New("--reread and --no-reread exclude each other")}
				return
			}
			if name == "reread" {
				l["reread"] = f.reread // --reread=X rereads X
			} else {
				l["reread"] = !f.noReread // --no-reread=X rereads !X
			}
		}
	})
	return l, err
}
