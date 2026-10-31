// Package settings resolves what tunes a daemon besides its rules: its
// timeouts and whether it rereads its rules file.
//
// Each key comes from the highest source that sets it (ADR 0008): the
// command's flags (Flags), the HOSTRUNNER_* variables, the workspace
// settings file, the workspace's section of the host settings file, the
// host settings file's common, top-level part, then the built-in
// default. Every source is read into a layer, a partial set of values,
// and resolve stacks the layers; a new source is one more layer, a new
// key one more entry of keys and one more field of Settings. An unknown key or an invalid value
// in any source is an error naming the source and the key: a daemon must
// not start with settings nobody chose.
package settings

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Settings are the resolved settings of a daemon.
type Settings struct {
	StartupTimeout time.Duration // startup_timeout: with --watch, how long to wait for the container
	Grace          time.Duration // grace: with --watch, how long the container may be gone
	Reread         bool          // reread: reread the rules file by itself once it changes
}

// defaults are the settings no source sets.
func defaults() Settings {
	return Settings{
		StartupTimeout: 30 * time.Minute, // covers image builds, also after a rebuild's arm
		Grace:          15 * time.Second, // how long a stopped container is tolerated
		Reread:         true,
	}
}

// layer is what one source sets: values by key name (as in a settings
// file), each a time.Duration or a bool as its key's kind says. A key
// the source does not set is absent.
type layer map[string]any

// kind is the type of a key's value.
type kind int

const (
	duration kind = iota // a time.Duration above zero, written like 1m30s
	boolean              // a bool
)

// String names the kind in errors ("invalid duration") and usages.
func (k kind) String() string {
	if k == boolean {
		return "bool"
	}
	return "duration"
}

// key is one setting: its name in a settings file, from which its
// variable and flag are named, and the field of Settings it sets.
type key struct {
	name  string
	kind  kind
	usage string                // the flag's help
	field func(s *Settings) any // a *time.Duration or *bool into s
}

// keys are every setting, in the order errors and flags follow.
var keys = []key{
	{"startup_timeout", duration, "with --watch: how long to wait for the container to start", func(s *Settings) any { return &s.StartupTimeout }},
	{"grace", duration, "with --watch: how long the container may be gone before exiting", func(s *Settings) any { return &s.Grace }},
	{"reread", boolean, "reread the rules file by itself once it changes", func(s *Settings) any { return &s.Reread }},
}

// commonOnly are the keys only the common part of the host settings file
// may set: `stop`, `restart` and `approve` read them before they know a
// workspace (ADR 0008), so the workspace settings file and a workspace
// section refuse them. Neither is a key yet: the common part rejects them
// as unknown.
var commonOnly = []string{"runtime_root", "stop_timeout"}

// variable is the environment variable that sets k: HOSTRUNNER_GRACE.
func (k key) variable() string { return "HOSTRUNNER_" + strings.ToUpper(k.name) }

// flagName is the flag that sets k, without dashes: startup-timeout.
func (k key) flagName() string { return strings.ReplaceAll(k.name, "_", "-") }

// lookup returns the key named name.
func lookup(name string) (key, bool) {
	for _, k := range keys {
		if k.name == name {
			return k, true
		}
	}
	return key{}, false
}

// Usage is the usage of the settings' flags, as a command's usage line
// shows them: [--settings <path>] [--workspace-settings <path>] [--grace
// <duration>] [--reread | --no-reread] and so on, one per key.
func Usage() string {
	parts := []string{"[--settings <path>]", "[--workspace-settings <path>]"}
	for _, k := range keys {
		if k.kind == boolean {
			parts = append(parts, fmt.Sprintf("[--%s | --no-%[1]s]", k.flagName()))
		} else {
			parts = append(parts, fmt.Sprintf("[--%s <%s>]", k.flagName(), k.kind))
		}
	}
	return strings.Join(parts, " ")
}

// errNotPositive is the problem with a duration at or below zero, which
// no setting can mean.
var errNotPositive = errors.New("must be positive")

// parse reads raw, the text a variable or a file gives, as a value of
// kind k; the error describes the problem without naming the source.
func (k kind) parse(raw string) (any, error) {
	if k == boolean {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid bool %q", raw)
		}
		return b, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid duration %q", raw)
	}
	if d <= 0 {
		return nil, fmt.Errorf("%w, got %s", errNotPositive, raw)
	}
	return d, nil
}

// apply sets the fields of s that l sets.
func apply(s *Settings, l layer) {
	for _, k := range keys {
		v, ok := l[k.name]
		if !ok {
			continue
		}
		switch p := k.field(s).(type) {
		case *time.Duration:
			*p = v.(time.Duration)
		case *bool:
			*p = v.(bool)
		}
	}
}

// resolve returns the settings of the workspace at the host path
// workspace, each key taken from the highest source that sets it, in the
// order of the package doc; getenv reads the variables, workspaceNamed
// is the --workspace-settings flag (see workspaceFile) and hostNamed the
// --settings flag (see hostFile), each empty when not given.
func resolve(flags layer, hostNamed, workspaceNamed, workspace string, getenv func(string) string) (Settings, error) {
	common, section, err := readHostFile(hostNamed, workspace, getenv)
	if err != nil {
		return Settings{}, err
	}
	wsFile, err := readWorkspaceFile(workspaceNamed, workspace, getenv)
	if err != nil {
		return Settings{}, err
	}
	vars, err := env(getenv)
	if err != nil {
		return Settings{}, err
	}
	s := defaults()
	for _, l := range []layer{common, section, wsFile, vars, flags} {
		apply(&s, l)
	}
	return s, nil
}

// env reads the HOSTRUNNER_* variables of every key through getenv; an
// empty variable is unset. An invalid value is an error naming the
// variable: `HOSTRUNNER_GRACE: invalid duration "abc"`.
func env(getenv func(string) string) (layer, error) {
	l := layer{}
	for _, k := range keys {
		raw := getenv(k.variable())
		if raw == "" {
			continue
		}
		v, err := k.kind.parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k.variable(), err)
		}
		l[k.name] = v
	}
	return l, nil
}
