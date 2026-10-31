// Package settings resolves what tunes a daemon besides its rules: its
// rules file, its timeouts and whether it rereads the rules file.
//
// Each key comes from the highest source that sets it (ADR 0008): the
// command's flags (Flags), the HOSTRUNNER_* variables, the workspace
// settings file, the workspace's section of the host settings file, the
// host settings file's common, top-level part, then the built-in
// default. Every source is read into a layer, a partial set of values,
// and resolve stacks the layers; a new source is one more layer, a new
// key one more entry of keys and one more field of Settings. An unknown
// key or an invalid value in any source is an error naming the source
// and the key: a daemon must not start with settings nobody chose.
package settings

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Settings are the resolved settings of a daemon, and of the up that
// starts it.
type Settings struct {
	// RulesFile (rules_file) is the rules file, absolute when the
	// workspace path is.
	RulesFile string
	// StartupTimeout (startup_timeout) is, with --watch, how long to wait
	// for the container to start.
	StartupTimeout time.Duration
	// Grace (grace) is, with --watch, how long the container may be gone.
	Grace time.Duration
	// Reread (reread) is whether to reread the rules file by itself once
	// it changes.
	Reread bool
	// PollInterval (poll_interval) is how often the runtimes are asked
	// (with --watch) and the rules file is looked at.
	PollInterval time.Duration
	// AskTimeout (ask_timeout) is how long an approval request waits when
	// its client sets no limit.
	AskTimeout time.Duration
	// RequestTimeout (request_timeout) is how long a new connection may
	// take to send its request.
	RequestTimeout time.Duration
	// ReadyTimeout (ready_timeout) is how long up waits for the daemon it
	// starts to listen; up's own, not passed to the daemon.
	ReadyTimeout time.Duration
}

// rulesPath is where a workspace keeps its rules file by default,
// relative to the workspace.
var rulesPath = filepath.Join(".devcontainer", "hostrun.yaml")

// defaults are the settings no source sets, for the workspace at the
// host path workspace.
func defaults(workspace string) Settings {
	return Settings{
		RulesFile:      filepath.Join(workspace, rulesPath),
		StartupTimeout: 30 * time.Minute, // covers image builds, also after a rebuild's arm
		Grace:          15 * time.Second, // how long a stopped container is tolerated
		Reread:         true,
		PollInterval:   2 * time.Second,  // how soon a stopped container or a changed rules file is seen
		AskTimeout:     10 * time.Minute, // time for a human to come to the host and answer
		RequestTimeout: 10 * time.Second, // so idle connections cannot pile up
		ReadyTimeout:   10 * time.Second, // a daemon listens at once, unless something is wrong
	}
}

// layer is what one source sets: values by key name (as in a settings
// file), each a string, a time.Duration or a bool as its key's kind
// says. A key the source does not set is absent.
type layer map[string]any

// kind is the type of a key's value.
type kind int

const (
	duration kind = iota // a time.Duration above zero, written like 1m30s
	boolean              // a bool
	path                 // a file path, not empty; relative to the workspace unless a flag gives it
)

// String names the kind in errors ("invalid duration") and usages.
func (k kind) String() string {
	switch k {
	case boolean:
		return "bool"
	case path:
		return "path"
	}
	return "duration"
}

// scope is the commands that take a key's flag.
type scope int

const (
	scopeDaemon scope = iota // serve, and up, which resolves it and passes it to serve
	scopeUp                  // up alone: it tunes up, not the daemon
)

// key is one setting: its name in a settings file, from which its
// variable and flag are named, the commands that take its flag, and the
// field of Settings it sets.
type key struct {
	name  string
	kind  kind
	scope scope
	usage string                // the flag's help
	field func(s *Settings) any // a *string, *time.Duration or *bool into s
}

// keys are every setting, in the order errors and flags follow.
var keys = []key{
	{
		name: "rules_file", kind: path, scope: scopeDaemon,
		usage: "rules file (default <workspace>/.devcontainer/hostrun.yaml)",
		field: func(s *Settings) any { return &s.RulesFile },
	},
	{
		name: "startup_timeout", kind: duration, scope: scopeDaemon,
		usage: "with --watch: how long to wait for the container to start",
		field: func(s *Settings) any { return &s.StartupTimeout },
	},
	{
		name: "grace", kind: duration, scope: scopeDaemon,
		usage: "with --watch: how long the container may be gone before exiting",
		field: func(s *Settings) any { return &s.Grace },
	},
	{
		name: "reread", kind: boolean, scope: scopeDaemon,
		usage: "reread the rules file by itself once it changes",
		field: func(s *Settings) any { return &s.Reread },
	},
	{
		name: "poll_interval", kind: duration, scope: scopeDaemon,
		usage: "how often to ask the runtimes (with --watch) and look at the rules file",
		field: func(s *Settings) any { return &s.PollInterval },
	},
	{
		name: "ask_timeout", kind: duration, scope: scopeDaemon,
		usage: "how long an approval request waits when the client sets no limit",
		field: func(s *Settings) any { return &s.AskTimeout },
	},
	{
		name: "request_timeout", kind: duration, scope: scopeDaemon,
		usage: "how long a new connection may take to send its request",
		field: func(s *Settings) any { return &s.RequestTimeout },
	},
	{
		name: "ready_timeout", kind: duration, scope: scopeUp,
		usage: "how long to wait for the daemon to listen",
		field: func(s *Settings) any { return &s.ReadyTimeout },
	},
}

// scopes are the scopes of the keys one command takes.
type scopes []scope

// takes says whether the command takes k.
func (in scopes) takes(k key) bool { return slices.Contains(in, k.scope) }

// keysOf returns the keys whose flags the commands of in take.
func keysOf(in ...scope) []key {
	var out []key
	for _, k := range keys {
		if scopes(in).takes(k) {
			out = append(out, k)
		}
	}
	return out
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

// Usage is the usage of the settings' flags Register adds, as serve's
// usage line shows them: [--settings <path>] [--workspace-settings
// <path>] [--rules-file <path>] [--grace <duration>] [--reread |
// --no-reread] and so on, one per key; --config, the synonym of
// --rules-file, is left out.
func Usage() string { return usage(scopeDaemon) }

// UpUsage is Usage for the flags RegisterUp adds, as up's usage line
// shows them: Usage and [--ready-timeout <duration>].
func UpUsage() string { return usage(scopeDaemon, scopeUp) }

// usage is the usage of the flags of the keys of scopes.
func usage(scopes ...scope) string {
	parts := []string{fileFlagsUsage}
	for _, k := range keysOf(scopes...) {
		parts = append(parts, k.usageOf())
	}
	return strings.Join(parts, " ")
}

// RulesFileUsage is the usage of the flags RegisterRulesFile adds, those
// that choose the rules file.
func RulesFileUsage() string {
	k, _ := lookup("rules_file")
	return fileFlagsUsage + " " + k.usageOf()
}

// fileFlagsUsage is the usage of the flags that name the settings files.
const fileFlagsUsage = "[--settings <path>] [--workspace-settings <path>]"

// usageOf is k's flag as a usage line shows it: [--grace <duration>].
func (k key) usageOf() string {
	if k.kind == boolean {
		return fmt.Sprintf("[--%s | --no-%[1]s]", k.flagName())
	}
	return fmt.Sprintf("[--%s <%s>]", k.flagName(), k.kind)
}

// errNotPositive is the problem with a duration at or below zero, which
// no setting can mean.
var errNotPositive = errors.New("must be positive")

// errEmptyPath is the problem with an empty path, which names no file.
var errEmptyPath = errors.New("empty path")

// parse reads raw, the text a variable or a file gives, as a value of
// kind k; the error describes the problem without naming the source.
func (k kind) parse(raw string) (any, error) {
	switch k {
	case path:
		if raw == "" {
			return nil, errEmptyPath
		}
		return raw, nil
	case boolean:
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
		case *string:
			*p = v.(string)
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
// --settings flag (see hostFile), each empty when not given. A path a
// file or a variable gives is read against the workspace, after ~
// expansion; flags gives its paths absolute already.
func resolve(in scopes, flags layer, hostNamed, workspaceNamed, workspace string, getenv func(string) string) (Settings, error) {
	common, section, err := readHostFile(in, hostNamed, workspace, getenv)
	if err != nil {
		return Settings{}, err
	}
	wsFile, err := readWorkspaceFile(in, workspaceNamed, workspace, getenv)
	if err != nil {
		return Settings{}, err
	}
	vars, err := env(in, getenv)
	if err != nil {
		return Settings{}, err
	}
	s := defaults(workspace)
	for _, l := range []layer{common, section, wsFile, vars} {
		apply(&s, inWorkspace(l, workspace, getenv))
	}
	apply(&s, flags)
	return s, nil
}

// inWorkspace is l with each path made a host path: ~ expanded (with
// $HOME getenv reads), then a relative one joined to the workspace.
func inWorkspace(l layer, workspace string, getenv func(string) string) layer {
	out := layer{}
	for name, v := range l {
		if k, _ := lookup(name); k.kind == path {
			p := expandHome(v.(string), getenv)
			if !filepath.IsAbs(p) {
				p = filepath.Join(workspace, p)
			}
			v = p
		}
		out[name] = v
	}
	return out
}

// env reads the HOSTRUNNER_* variables of the keys of in through getenv;
// an empty variable is unset. An invalid value is an error naming the
// variable: `HOSTRUNNER_GRACE: invalid duration "abc"`.
func env(in scopes, getenv func(string) string) (layer, error) {
	l := layer{}
	for _, k := range keysOf(in...) {
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
