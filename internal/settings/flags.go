package settings

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Flags are the flags of the settings on one command line. Every command
// has --settings; a command that knows a workspace also has
// --workspace-settings. Each key the command takes has its flag
// (--rules-file, --grace, --stop-timeout and so on); --rules-file also
// has the synonym --config, and reread has both --reread and
// --no-reread. Register them, parse, then resolve.
type Flags struct {
	fs            *flag.FlagSet
	file          string
	workspaceFile string
	paths         map[string]*string
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
// a daemon applies.
func (f *Flags) Register(fs *flag.FlagSet) { f.register(fs, keysOf(scopeDaemon), scopeDaemon) }

// RegisterUp adds to fs the flags of the settings up takes: those
// Register adds, and --ready-timeout.
func (f *Flags) RegisterUp(fs *flag.FlagSet) {
	f.register(fs, keysOf(scopeDaemon, scopeUp), scopeDaemon, scopeUp)
}

// RegisterRulesFile adds to fs only the flags that choose the rules
// file: --settings, --workspace-settings, --rules-file and --config, for
// a command that uses no other setting; the other keys serve takes still
// come from the variables and the files, as for serve.
func (f *Flags) RegisterRulesFile(fs *flag.FlagSet) {
	k, _ := lookup("rules_file")
	f.register(fs, []key{k}, scopeDaemon)
}

// RegisterStop adds to fs the flags of the settings stop and restart
// take: --settings, --runtime-root and --stop-timeout. Resolve them with
// ResolveHost.
func (f *Flags) RegisterStop(fs *flag.FlagSet) {
	f.register(fs, keysOf(scopeRuntimeRoot, scopeStop), scopeRuntimeRoot, scopeStop)
}

// RegisterApprove adds to fs the flags of the settings approve takes:
// --settings and --runtime-root. Resolve them with ResolveHost.
func (f *Flags) RegisterApprove(fs *flag.FlagSet) {
	f.register(fs, keysOf(scopeRuntimeRoot), scopeRuntimeRoot)
}

// synonyms are the flags that set the key of another flag, by their names:
// --config sets the rules file as --rules-file does.
var synonyms = map[string]string{"config": "rules-file"}

// register adds to fs --settings, --workspace-settings for a command
// that knows a workspace, and the flags of flagged; only the keys of in
// are read.
func (f *Flags) register(fs *flag.FlagSet, flagged []key, in ...scope) {
	f.fs, f.scopes = fs, in
	f.paths, f.durations = map[string]*string{}, map[string]*time.Duration{}
	fs.StringVar(&f.file, "settings", "", "host settings file (default $XDG_CONFIG_HOME/hostrunner/settings.yaml)")
	if f.scopes.knowWorkspace() {
		fs.StringVar(&f.workspaceFile, "workspace-settings", "", "workspace settings file (default <workspace>/.devcontainer/hostrunner.yaml)")
	}
	defaults := flagDefaults()
	for _, k := range flagged {
		switch p := k.field(&defaults).(type) {
		case *string:
			f.paths[k.name] = fs.String(k.flagName(), "", k.usage)
			for synonym, of := range synonyms {
				if of == k.flagName() {
					fs.String(synonym, "", "synonym of --"+of)
				}
			}
		case *time.Duration:
			f.durations[k.name] = fs.Duration(k.flagName(), *p, k.usage)
		case *bool:
			fs.BoolVar(&f.reread, k.flagName(), false, k.usage)
			fs.BoolVar(&f.noReread, "no-"+k.flagName(), false, "do not "+k.usage)
		}
	}
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

// ResolveHost returns the settings of a parsed command line of a command
// that knows no workspace (stop, restart, approve), each key it takes
// from its flag, its HOSTRUNNER_* variable getenv reads, the common part
// of the host settings file (as for Resolve), then the default: the
// runtime root under $XDG_RUNTIME_DIR, none when it is unset, and 10s to
// stop. A runtime root from the file or a variable must be absolute
// after ~ expansion. A flag that cannot be applied is a *FlagError.
func (f *Flags) ResolveHost(getenv func(string) string) (Settings, error) {
	flags, err := f.layer()
	if err != nil {
		return Settings{}, err
	}
	return resolveHost(f.scopes, flags, f.file, getenv)
}

// layer returns the keys the parsed command line sets: only flags given
// count, even with their default value. A path is made absolute against
// the current directory. A duration at or below zero (`--grace: must be
// positive`), an empty path, --reread with --no-reread and a flag with
// its synonym (--rules-file with --config) are *FlagErrors.
func (f *Flags) layer() (layer, error) {
	l := layer{}
	var err error
	f.fs.Visit(func(fl *flag.Flag) {
		if err != nil {
			return
		}
		flagName := fl.Name
		if of, ok := synonyms[fl.Name]; ok {
			flagName = of
		}
		name := strings.ReplaceAll(flagName, "-", "_")
		switch {
		case f.durations[name] != nil:
			if d := *f.durations[name]; d > 0 {
				l[name] = d
			} else {
				err = &FlagError{fmt.Errorf("--%s: %w", fl.Name, errNotPositive)}
			}
		case f.paths[name] != nil:
			if _, set := l[name]; set { // given as itself and as its synonym
				err = &FlagError{fmt.Errorf("--%s and --%s exclude each other", flagName, synonymOf(flagName))}
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
			l[name] = p
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

// synonymOf returns the synonym of the flag named flagName.
func synonymOf(flagName string) string {
	for synonym, of := range synonyms {
		if of == flagName {
			return synonym
		}
	}
	return ""
}
