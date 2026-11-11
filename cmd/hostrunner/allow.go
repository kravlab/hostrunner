package main

import (
	"errors"
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

// runAllow allows what the refused `up`s or the daemons of cfg's
// workspace asked for and prints to stdout what it allowed: every pending
// request in the store, one per runtime directory, settings flags
// included, once the rules file of each still has the digest the request
// saw. It allows all of them or none: a rules file changed since a request
// is an error naming each such request, its runtime directory, its rules
// file and both digests, and records nothing, so a human never allows
// content other than what was shown to `up`. Without a pending request it
// allows nothing and fails, telling to start the container, since
// `hostrunner up` asks for the allow (ADR 0009). It asks nothing and reads
// no stdin: the human who runs it is the one who allows. Invalid rules are
// an error, as in `hostrunner up`, and record nothing; a missing rules
// file is allowed, as the daemon would serve it, denying every command.
// When the allows are recorded but a request answered cannot be removed,
// it prints what it allowed and then returns the error.
func runAllow(cfg allowConfig, stdout io.Writer) error {
	requests, err := allowRequests(cfg)
	if err != nil {
		return err
	}
	allowed, allowErr := cfg.store.AllowAll(cfg.workspace, requests)
	var b strings.Builder
	for _, e := range allowed {
		b.WriteString(allowMessage(cfg.workspace, e))
	}
	_, err = io.WriteString(stdout, b.String())
	return errors.Join(allowErr, err)
}

// allowRequests returns what runAllow allows for cfg's workspace: the
// pending requests in the store, each checked against its rules file as it
// is now (see staleRequest). No pending request is an error naming
// `hostrunner up`, which asks for the allow; so is any request whose rules
// file changed since, every such one named.
func allowRequests(cfg allowConfig) ([]allow.Entry, error) {
	requests, err := cfg.store.PendingAll(cfg.workspace)
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("nothing asked to allow %s: start the container first, hostrunner up asks for the allow", hostterm.Display(cfg.workspace))
	}
	var changed []string
	for _, req := range requests {
		stale, err := staleRequest(req)
		if err != nil {
			return nil, err
		}
		if stale != "" {
			changed = append(changed, stale)
		}
	}
	if len(changed) > 0 {
		return nil, fmt.Errorf("%s: nothing allowed, run hostrunner up again", strings.Join(changed, "; "))
	}
	return requests, nil
}

// staleRequest checks the pending request req against its rules file as
// it is now, and returns what changed since `hostrunner up` asked for it,
// naming its runtime directory, its rules file and both digests, or ""
// when the file still has the digest of req. Rules that do not load are
// an error naming the runtime directory of req.
func staleRequest(req allow.Entry) (string, error) {
	policy, err := rules.Load(req.RulesFile)
	if err != nil {
		return "", fmt.Errorf("rules file of the runtime directory %s: %w", hostterm.Display(req.RuntimeDir), err)
	}
	if policy.Digest() == req.Digest {
		return "", nil
	}
	return fmt.Sprintf("the rules file %s changed since hostrunner up asked to allow it for the runtime directory %s (digest %s, now %s)",
		hostterm.Display(req.RulesFile), hostterm.Display(req.RuntimeDir), req.Digest, policy.Digest()), nil
}

// allowMessage is what `allow` prints for the allow e of workspace: the
// workspace, the runtime directory, the rules file with its digest and the
// settings sorted by key, one per line, so the human sees what was
// trusted. A setting that differs from its default
// (settings.DaemonDefaults) carries the default, as
// `mount_check: false (default true)`: the settings flags of a refused up
// come from initializeCommand, not from the human, who sees here which of
// them the allow takes. The defaults are compared as the store holds the
// settings of e, with links followed (allow.CanonicalSettings), so a
// workspace reached through a link has the default rules file. When they
// cannot be put that way, no setting is marked, and a last line says that
// the defaults are not shown and why, so that a setting without a mark is
// not taken for one at its default.
func allowMessage(workspace string, e allow.Entry) string {
	defaults, defaultsErr := allow.CanonicalSettings(settings.DaemonDefaults(workspace))
	var b strings.Builder
	fmt.Fprintf(&b, "hostrunner: allowed %s\n", hostterm.Display(workspace))
	fmt.Fprintf(&b, "  runtime directory: %s\n", hostterm.Display(e.RuntimeDir))
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

// runDeny removes the allows of every runtime directory of cfg's
// workspace and says to stdout which it removed, one line each, or that
// there was none; a workspace without an allow is no failure. An allow
// file that cannot be read or is not valid is removed too, with a warning
// on stderr naming it and why, and is no failure either (see
// allow.Store.Deny). When a file cannot be removed it still prints those
// removed before it, then returns the error.
func runDeny(cfg denyConfig, stdout, stderr io.Writer) error {
	removed, denyErr := cfg.store.Deny(cfg.workspace)
	ws := hostterm.Display(cfg.workspace)
	var b, warn strings.Builder
	for _, e := range removed {
		fmt.Fprintf(&b, "hostrunner: denied %s for %s\n", ws, hostterm.Display(e.RuntimeDir))
	}
	if damaged, ok := errors.AsType[*allow.DamagedFilesError](denyErr); ok {
		for _, f := range damaged.Files {
			fmt.Fprintf(&warn, "hostrunner: warning: removed the allow file %s, which cannot be read: %s\n", hostterm.Display(f.Path), hostterm.Display(f.Err.Error()))
		}
	}
	if len(removed) == 0 && denyErr == nil {
		fmt.Fprintf(&b, "hostrunner: %s has no allow\n", ws)
	}
	if allow.OnlyWarns(denyErr) {
		denyErr = nil
	}
	_, err := io.WriteString(stdout, b.String())
	_, warnErr := io.WriteString(stderr, warn.String())
	return errors.Join(denyErr, err, warnErr)
}
