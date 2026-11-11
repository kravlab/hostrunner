package main

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/settings"
)

// allowConfig is a parsed `hostrunner allow`: the workspace to allow and
// the store that holds its request and records the allow.
type allowConfig struct {
	workspace string // absolute path of the workspace on the host
	store     allow.Store
}

// parseAllow parses the arguments of `hostrunner allow`: no flag but -h
// and at most one workspace, the current directory when none is given.
// It takes no settings flag: it allows what a refused `up` or a daemon
// asked for, so nobody repeats the flags devcontainer.json gives `up` (see
// runAllow). The workspace must be an existing directory, and the store is
// the one allow.Open opens, with getenv reading the environment.
func parseAllow(args []string, getenv func(string) string, output io.Writer) (allowConfig, error) {
	var cfg allowConfig
	fs := flag.NewFlagSet("allow", flag.ContinueOnError)
	fs.SetOutput(output)
	ws, err := parseWorkspaceArg(fs, args, allowUsage)
	if err != nil {
		return cfg, err
	}
	info, err := os.Stat(ws)
	if err != nil {
		return cfg, fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return cfg, fmt.Errorf("workspace %s is not a directory", ws)
	}
	cfg.workspace = ws
	cfg.store, err = allow.Open(getenv)
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseWorkspaceArg parses args with fs and returns the workspace they
// name, made absolute: the only argument after the flags, or the current
// directory when there is none. A second argument is an error carrying
// usage.
func parseWorkspaceArg(fs *flag.FlagSet, args []string, usage string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 1 {
		return "", fmt.Errorf("unexpected argument %q; %s", fs.Arg(1), usage)
	}
	ws := "."
	if fs.NArg() == 1 {
		ws = fs.Arg(0)
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	return abs, nil
}

// runAllow allows what a refused `up` or a daemon asked for in cfg's
// workspace and prints to stdout what it allowed: the request in the
// store, settings flags included, once the rules file still has the
// digest the request saw. A rules file changed since then is an error
// naming it and both digests, and records nothing, so a human never
// allows content other than what was shown to `up`. Without a request it
// allows nothing and fails, telling to start the container, since
// `hostrunner up` asks for the allow (ADR 0009). It asks nothing and reads
// no stdin: the human who runs it is the one who allows. Invalid rules
// are an error, as in `hostrunner up`, and record nothing; a missing rules
// file is allowed, as the daemon would serve it, denying every command.
func runAllow(cfg allowConfig, stdout io.Writer) error {
	target, err := allowTarget(cfg)
	if err != nil {
		return err
	}
	e, err := cfg.store.Allow(cfg.workspace, target)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, allowMessage(cfg.workspace, e))
	return err
}

// allowTarget returns what runAllow allows for cfg's workspace: the
// request in the store, checked against the rules file as it is now. No
// request is an error naming `hostrunner up`, which asks for the allow.
func allowTarget(cfg allowConfig) (allow.Target, error) {
	req, ok, err := cfg.store.Pending(cfg.workspace)
	if err != nil {
		return allow.Target{}, err
	}
	if !ok {
		return allow.Target{}, fmt.Errorf("nothing asked to allow %s: start the container first, hostrunner up asks for the allow", hostterm.Display(cfg.workspace))
	}
	policy, err := rules.Load(req.RulesFile)
	if err != nil {
		return allow.Target{}, fmt.Errorf("rules file: %w", err)
	}
	if policy.Digest() != req.Digest {
		return allow.Target{}, fmt.Errorf("the rules file %s changed since hostrunner up asked to allow it (digest %s, now %s): run hostrunner up again",
			hostterm.Display(req.RulesFile), req.Digest, policy.Digest())
	}
	return req.Target, nil
}

// allowMessage is what `allow` prints for the allow e of workspace: the
// workspace, the rules file with its digest and the settings sorted by
// key, one per line, so the human sees what was trusted. A setting that
// differs from its default (settings.DaemonDefaults) carries the default,
// as `mount_check: false (default true)`: the settings flags of a refused
// up come from initializeCommand, not from the human, who sees here which
// of them the allow takes. The defaults are compared as the store holds
// the settings of e, with links followed (allow.CanonicalSettings), so a
// workspace reached through a link has the default rules file. When they
// cannot be put that way, no setting is marked, and a last line says that
// the defaults are not shown and why, so that a setting without a mark is
// not taken for one at its default.
func allowMessage(workspace string, e allow.Entry) string {
	defaults, defaultsErr := allow.CanonicalSettings(settings.DaemonDefaults(workspace))
	var b strings.Builder
	fmt.Fprintf(&b, "hostrunner: allowed %s\n", hostterm.Display(workspace))
	fmt.Fprintf(&b, "  rules file: %s\n", hostterm.Display(e.RulesFile))
	fmt.Fprintf(&b, "  digest: %s\n", e.Digest)
	b.WriteString("  settings:\n")
	for _, name := range slices.Sorted(maps.Keys(e.Settings)) {
		fmt.Fprintf(&b, "    %s: %s", name, hostterm.Display(e.Settings[name]))
		if def, ok := defaults[name]; ok && def != e.Settings[name] {
			fmt.Fprintf(&b, " (default %s)", hostterm.Display(def))
		}
		b.WriteString("\n")
	}
	if defaultsErr != nil {
		fmt.Fprintf(&b, "  defaults not shown: %s\n", hostterm.Display(defaultsErr.Error()))
	}
	return b.String()
}

// denyConfig is a parsed `hostrunner deny`.
type denyConfig struct {
	workspace string // absolute path of the workspace on the host
	store     allow.Store
}

// parseDeny parses the arguments of `hostrunner deny`: no flag but -h and
// at most one workspace, the current directory when none is given. The
// workspace need not exist, since the allow of a removed workspace can
// still be denied. The store is the one allow.Open opens, with getenv
// reading the environment.
func parseDeny(args []string, getenv func(string) string, output io.Writer) (denyConfig, error) {
	var cfg denyConfig
	fs := flag.NewFlagSet("deny", flag.ContinueOnError)
	fs.SetOutput(output)
	ws, err := parseWorkspaceArg(fs, args, denyUsage)
	if err != nil {
		return cfg, err
	}
	store, err := allow.Open(getenv)
	if err != nil {
		return cfg, err
	}
	cfg.workspace, cfg.store = ws, store
	return cfg, nil
}

// runDeny removes the allow of cfg's workspace and says to stdout whether
// there was one; a workspace without an allow is no failure.
func runDeny(cfg denyConfig, stdout io.Writer) error {
	removed, err := cfg.store.Deny(cfg.workspace)
	if err != nil {
		return err
	}
	ws := hostterm.Display(cfg.workspace)
	if removed {
		_, err = fmt.Fprintf(stdout, "hostrunner: denied %s\n", ws)
	} else {
		_, err = fmt.Fprintf(stdout, "hostrunner: %s has no allow\n", ws)
	}
	return err
}
