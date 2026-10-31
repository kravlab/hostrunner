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

// Settings are the resolved settings of a daemon, of the up that starts
// it, and of stop, restart and approve; a command's settings hold the
// keys it takes.
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
	// RuntimeRoot (runtime_root) is where stop, restart and approve look
	// for daemons' runtime directories; empty when no source gives one
	// and XDG_RUNTIME_DIR is unset.
	RuntimeRoot string
	// StopTimeout (stop_timeout) is how long stop and restart wait for a
	// daemon to close its sockets once it has stopped.
	StopTimeout time.Duration
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

// DefaultStopTimeout is the stop_timeout no source sets: as long as a
// daemon takes to close its sockets, unless something is wrong.
const DefaultStopTimeout = 10 * time.Second

// flagDefaults are the defaults the flags' help shows: those of every
// key, the runtime root aside, whose usage names its default.
func flagDefaults() Settings {
	s := defaults("")
	s.StopTimeout = DefaultStopTimeout
	return s
}

// Daemon returns the settings serve takes, keyed by their names, each
// value written as a settings file writes it (`1m30s`, `true`, a path):
// what an arm of up carries to the daemon, and what the daemon compares
// it with.
func (s Settings) Daemon() map[string]string {
	daemonKeys := keysOf(scopeDaemon)
	out := make(map[string]string, len(daemonKeys))
	for _, k := range daemonKeys {
		switch p := k.field(&s).(type) {
		case *string:
			out[k.name] = *p
		case *time.Duration:
			out[k.name] = p.String()
		case *bool:
			out[k.name] = strconv.FormatBool(*p)
		default:
			panic(fmt.Sprintf("settings: key %s has a field of type %T", k.name, p))
		}
	}
	return out
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
	path                 // a file path, not empty; a relative one is read against the workspace
	absPath              // a directory path, not empty; a relative one is an error
)

// String names the kind in errors ("invalid duration") and usages.
func (k kind) String() string {
	switch k {
	case boolean:
		return "bool"
	case path, absPath:
		return "path"
	}
	return "duration"
}

// scope is the commands that take a key's flag.
type scope int

const (
	scopeDaemon      scope = iota // serve, and up, which resolves it and passes it to serve
	scopeUp                       // up alone: it tunes up, not the daemon
	scopeRuntimeRoot              // stop, restart and approve
	scopeStop                     // stop and restart
)

// commonOnly says whether only the common part of the host settings file
// may set the keys of s: their commands (stop, restart, approve) know no
// workspace when they read them (ADR 0008), so the workspace settings file
// and a workspace section refuse them.
func (s scope) commonOnly() bool { return s == scopeRuntimeRoot || s == scopeStop }

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
	{
		name: "runtime_root", kind: absPath, scope: scopeRuntimeRoot,
		usage: "where to look for daemons (default $XDG_RUNTIME_DIR/hostrunner)",
		field: func(s *Settings) any { return &s.RuntimeRoot },
	},
	{
		name: "stop_timeout", kind: duration, scope: scopeStop,
		usage: "how long to wait for a stopped daemon to close its sockets",
		field: func(s *Settings) any { return &s.StopTimeout },
	},
}

// scopes are the scopes of the keys one command takes.
type scopes []scope

// takes says whether the command takes k.
func (in scopes) takes(k key) bool { return slices.Contains(in, k.scope) }

// knowWorkspace says whether the command knows a workspace: whether it
// takes a key a workspace may set. Only such a command reads the
// workspace settings file and the workspace sections, and takes
// --workspace-settings.
func (in scopes) knowWorkspace() bool {
	return slices.ContainsFunc(in, func(s scope) bool { return !s.commonOnly() })
}

// hostOnly returns the scopes of in whose keys only the common part of
// the host settings file may set, those ResolveHost resolves: restart's
// own.
func (in scopes) hostOnly() scopes {
	return slices.DeleteFunc(slices.Clone(in), func(s scope) bool { return !s.commonOnly() })
}

// ofWorkspace returns the scopes of in whose keys a workspace may set,
// those Resolve resolves: for restart, the new daemon's.
func (in scopes) ofWorkspace() scopes {
	return slices.DeleteFunc(slices.Clone(in), scope.commonOnly)
}

// restartScopes are the scopes of the keys restart takes: those of stop,
// and those of up for the new daemon, which it starts as up does.
var restartScopes = []scope{scopeRuntimeRoot, scopeStop, scopeDaemon, scopeUp}

// restartKeys are the keys restart has flags for: the rules file of the
// new daemon and the keys of stop.
func restartKeys() []key {
	return append([]key{mustLookup("rules_file")}, keysOf(scopeRuntimeRoot, scopeStop)...)
}

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

// mustLookup returns the key named name, one of keys.
func mustLookup(name string) key {
	k, ok := lookup(name)
	if !ok {
		panic("settings: no key " + name)
	}
	return k
}

// Usage is the usage of the settings' flags Register adds, as serve's
// usage line shows them: [--settings <path>] [--workspace-settings
// <path>] [--rules-file <path>] [--grace <duration>] [--reread |
// --no-reread] and so on, one per key; --config, the synonym of
// --rules-file, is left out.
func Usage() string { return usageOf(keysOf(scopeDaemon)) }

// UpUsage is Usage for the flags RegisterUp adds, as up's usage line
// shows them: Usage and [--ready-timeout <duration>].
func UpUsage() string { return usageOf(keysOf(scopeDaemon, scopeUp)) }

// StopUsage is the usage of the flags RegisterStop adds: [--settings
// <path>] [--runtime-root <path>] [--stop-timeout <duration>].
func StopUsage() string { return usageOf(keysOf(scopeRuntimeRoot, scopeStop)) }

// ApproveUsage is the usage of the flags RegisterApprove adds:
// [--settings <path>] [--runtime-root <path>].
func ApproveUsage() string { return usageOf(keysOf(scopeRuntimeRoot)) }

// RestartUsage is the usage of the flags RegisterRestart adds: those of
// StopUsage, [--workspace-settings <path>] and [--rules-file <path>].
func RestartUsage() string { return usageOf(restartKeys()) }

// usageOf is the usage of the flags of a command with a flag for each
// key of flagged, and --workspace-settings when one of them is a key a
// workspace may set (see scopes.knowWorkspace).
func usageOf(flagged []key) string {
	parts := []string{settingsFlagUsage}
	if slices.ContainsFunc(flagged, func(k key) bool { return !k.scope.commonOnly() }) {
		parts = append(parts, workspaceFlagUsage)
	}
	for _, k := range flagged {
		parts = append(parts, k.usageOf())
	}
	return strings.Join(parts, " ")
}

// RulesFileUsage is the usage of the flags RegisterRulesFile adds, those
// that choose the rules file.
func RulesFileUsage() string { return usageOf([]key{mustLookup("rules_file")}) }

// settingsFlagUsage and workspaceFlagUsage are the usages of the flags
// that name the host settings file and the workspace settings file.
const (
	settingsFlagUsage  = "[--settings <path>]"
	workspaceFlagUsage = "[--workspace-settings <path>]"
)

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
	case path, absPath:
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

// only is l with the keys in takes alone.
func (l layer) only(in scopes) layer {
	out := layer{}
	for name, v := range l {
		if k, _ := lookup(name); in.takes(k) {
			out[name] = v
		}
	}
	return out
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
		if l, err = hostPaths(l, workspace, getenv, nil); err != nil {
			return Settings{}, err
		}
		apply(&s, l)
	}
	apply(&s, flags)
	return s, nil
}

// hostPaths is l with each path ~ expanded (with $HOME getenv reads) and
// cleaned, as filepath.Abs cleans a flag's: a relative path joined to the
// workspace, a relative absPath an error naming source(k), the source of
// key k. A command that knows a workspace takes no absPath key, so it
// passes no source.
func hostPaths(l layer, workspace string, getenv func(string) string, source func(key) string) (layer, error) {
	out := layer{}
	for name, v := range l {
		k, _ := lookup(name)
		if k.kind == path || k.kind == absPath {
			p := expandHome(v.(string), getenv)
			switch {
			case filepath.IsAbs(p):
				p = filepath.Clean(p)
			case k.kind == path:
				p = filepath.Join(workspace, p)
			default:
				return nil, fmt.Errorf("%s: must be absolute, got %q", source(k), v)
			}
			v = p
		}
		out[name] = v
	}
	return out, nil
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

// hostDefaults are the settings of stop, restart and approve that no
// source sets: the runtime root under $XDG_RUNTIME_DIR, which getenv
// reads (none when it is unset), and DefaultStopTimeout.
func hostDefaults(getenv func(string) string) Settings {
	s := Settings{StopTimeout: DefaultStopTimeout}
	if xdg := getenv("XDG_RUNTIME_DIR"); xdg != "" {
		s.RuntimeRoot = filepath.Join(xdg, "hostrunner")
	}
	return s
}

// resolveHost returns the settings of a command that knows no workspace
// and takes the keys of in, each from the highest source that sets it:
// flags, the variables getenv reads, the common part of the host
// settings file hostNamed or the defaults name (see hostFile), then
// hostDefaults. No workspace section and no workspace settings file is
// read. A runtime root from the file or a variable must be absolute after
// ~ expansion. It mirrors resolve, less the workspace's sources.
func resolveHost(in scopes, flags layer, hostNamed string, getenv func(string) string) (Settings, error) {
	var common layer
	file, required := hostFile(hostNamed, getenv)
	if file != "" {
		l, _, err := readNamed(in, file, required, true)
		if err != nil {
			return Settings{}, err
		}
		common, err = hostPaths(l, "", getenv, func(k key) string { return "settings file " + file + ": " + k.name })
		if err != nil {
			return Settings{}, err
		}
	}
	vars, err := env(in, getenv)
	if err != nil {
		return Settings{}, err
	}
	if vars, err = hostPaths(vars, "", getenv, key.variable); err != nil {
		return Settings{}, err
	}
	s := hostDefaults(getenv)
	for _, l := range []layer{common, vars, flags} {
		apply(&s, l)
	}
	return s, nil
}
