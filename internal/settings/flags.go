package settings

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Flags are the flags of the settings on one command line: --settings,
// --workspace-settings, one per key the command takes (--rules-file,
// --startup-timeout, --grace and so on), --config as the synonym of
// --rules-file and, for reread, both --reread and --no-reread. Register
// them, parse, then resolve.
type Flags struct {
	fs            *flag.FlagSet
	file          string
	workspaceFile string
	rulesFile     string
	config        string
	durations     map[string]*time.Duration
	reread        bool
	noReread      bool
	scopes        scopes // the keys the command takes, which alone are read
}

// FlagError is a settings flag given a value no setting can take
// (`--grace: must be positive`), or --reread with --no-reread, or
// --rules-file with --config, as the resolving methods of Flags return
// it, so a caller can tell it from an invalid variable or file and add
// its usage.
type FlagError struct{ err error }

// Error is the problem with the flag, naming it.
func (e *FlagError) Error() string { return e.err.Error() }

// Unwrap returns the problem.
func (e *FlagError) Unwrap() error { return e.err }

// Register adds to fs the flags of the settings serve takes: every key
// but up's own (ready_timeout).
func (f *Flags) Register(fs *flag.FlagSet) { f.register(fs, scopeDaemon) }

// RegisterUp adds to fs the flags of the settings up takes: those
// Register adds, and --ready-timeout.
func (f *Flags) RegisterUp(fs *flag.FlagSet) { f.register(fs, scopeDaemon, scopeUp) }

// register adds to fs the flags of the keys of in, which alone are read.
func (f *Flags) register(fs *flag.FlagSet, in ...scope) {
	f.RegisterRulesFile(fs)
	f.scopes = in
	f.durations = map[string]*time.Duration{}
	defaults := defaults("")
	for _, k := range keysOf(in...) {
		switch p := k.field(&defaults).(type) {
		case *time.Duration:
			f.durations[k.name] = fs.Duration(k.flagName(), *p, k.usage)
		case *bool:
			fs.BoolVar(&f.reread, k.flagName(), false, k.usage)
			fs.BoolVar(&f.noReread, "no-"+k.flagName(), false, "do not "+k.usage)
		}
	}
}

// RegisterRulesFile adds to fs only the flags that choose the rules
// file: --settings, --workspace-settings, --rules-file and --config, for
// a command that uses no other setting; the other keys serve takes still
// come from the variables and the files, as for serve.
func (f *Flags) RegisterRulesFile(fs *flag.FlagSet) {
	f.fs = fs
	f.scopes = scopes{scopeDaemon}
	k, _ := lookup("rules_file")
	fs.StringVar(&f.file, "settings", "", "host settings file (default $XDG_CONFIG_HOME/hostrunner/settings.yaml)")
	fs.StringVar(&f.workspaceFile, "workspace-settings", "", "workspace settings file (default <workspace>/.devcontainer/hostrunner.yaml)")
	fs.StringVar(&f.rulesFile, k.flagName(), "", k.usage)
	fs.StringVar(&f.config, "config", "", "synonym of --"+k.flagName())
}

// Resolve returns the settings of the parsed command line for the
// workspace at the host path workspace, each key taken from the highest
// source that sets it: its flag, its HOSTRUNNER_* variable getenv reads,
// the workspace settings file (--workspace-settings, else
// $HOSTRUNNER_WORKSPACE_SETTINGS, else .devcontainer/hostrunner.yaml in
// the workspace), the workspace's section of the host settings file, its
// common part (--settings, else $HOSTRUNNER_SETTINGS, else
// $XDG_CONFIG_HOME/hostrunner/settings.yaml or
// $HOME/.config/hostrunner/settings.yaml), then the default. Every source
// is read, even when the flags set every key. A flag that cannot be
// applied is a *FlagError.
func (f *Flags) Resolve(getenv func(string) string, workspace string) (Settings, error) {
	flags, err := f.layer()
	if err != nil {
		return Settings{}, err
	}
	return resolve(f.scopes, flags, f.file, f.workspaceFile, workspace, getenv)
}

// ResolveAsGiven is Resolve, except that when the flags set every key
// serve takes neither the variables nor any file is read: a daemon its
// `up` started with every setting as a flag runs with them as given.
func (f *Flags) ResolveAsGiven(getenv func(string) string, workspace string) (Settings, error) {
	flags, err := f.layer()
	if err != nil {
		return Settings{}, err
	}
	for _, k := range keysOf(scopeDaemon) {
		if _, given := flags[k.name]; !given {
			return resolve(f.scopes, flags, f.file, f.workspaceFile, workspace, getenv)
		}
	}
	s := defaults(workspace)
	apply(&s, flags)
	return s, nil
}

// layer returns the keys the parsed command line sets: only flags given
// count, even with their default value. A path is made absolute against
// the current directory. A duration at or below zero (`--grace: must be
// positive`), an empty path, --reread with --no-reread and --rules-file
// with --config are *FlagErrors.
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
		case name == "rules_file" || name == "config":
			if _, set := l["rules_file"]; set {
				err = &FlagError{errors.New("--rules-file and --config exclude each other")}
				return
			}
			if fl.Value.String() == "" {
				err = &FlagError{fmt.Errorf("--%s: %w", fl.Name, errEmptyPath)}
				return
			}
			p, absErr := filepath.Abs(fl.Value.String())
			if absErr != nil {
				err = &FlagError{fmt.Errorf("--%s: %w", fl.Name, absErr)}
				return
			}
			l["rules_file"] = p
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
