package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/settings"
)

// testConfig holds the settings of `hostrunner test`.
type testConfig struct {
	resolved settings.Resolved // the rules file, and the older one left unread
	argv     []string          // the command to test
}

// parseTest parses the arguments of `hostrunner test`: its flags end at
// the first non-flag argument or `--`, and the rest is the command. The
// rules file is resolved as `up` resolves it (see internal/settings),
// with getenv reading the environment and the current directory as the
// workspace, so the test reads the rules file the daemon would.
func parseTest(args []string, getenv func(string) string, output io.Writer) (testConfig, error) {
	var (
		cfg testConfig
		sf  settings.Flags
	)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(output)
	sf.RegisterRulesFile(fs)
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.argv = fs.Args(); len(cfg.argv) == 0 {
		return cfg, errors.New(testUsage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return cfg, err
	}
	s, err := sf.Resolve(getenv, cwd)
	if _, ok := errors.AsType[*settings.FlagError](err); ok {
		return cfg, fmt.Errorf("%w; %s", err, testUsage)
	}
	if err != nil {
		return cfg, err
	}
	cfg.resolved = s
	return cfg, nil
}

// rulesTest runs the Rules test: it tests cfg.argv against the rules file
// alone, with no daemon or container, and reports the allowing rule and
// its fixed directory on stderr, after a warning of an older default
// rules file left unread (see hostterm.WarnIgnoredRules). A denied
// command is an exitError with protocol.ExitRejected, as hostrun exits
// when the daemon denies it; its message lists every reason, not only the
// first a refusal reports, and the class an approval session would put it
// in (see denialClass). Unlike the daemon, it takes a missing file for an
// error; an invalid one fails as in `hostrunner up`.
func rulesTest(cfg testConfig, stderr io.Writer) error {
	hostterm.WarnIgnoredRules(stderr, cfg.resolved.IgnoredRules, cfg.resolved.RulesFile)
	policy, err := rules.Read(cfg.resolved.RulesFile)
	if err != nil {
		return fmt.Errorf("rules file: %w", err)
	}
	allowed, err := policy.Check(cfg.argv)
	if d, ok := rules.ApprovableDenial(err); ok {
		err = fmt.Errorf("%s (%s)", d.Explain(), denialClass(d))
	}
	if err != nil {
		return &exitError{code: protocol.ExitRejected, err: err}
	}
	// The report a dry run gets, without checking the fixed directory or
	// the secrets' sources: those need the workspace and the host.
	_, err = fmt.Fprintf(stderr, "hostrunner: %s\n", protocol.Allowed{Rule: allowed.Rule, Dir: allowed.Dir, Secrets: allowed.SecretNames()})
	return err
}

// denialClass names the class of an approvable refusal, as judged from the
// rules file alone: guarded when approving it needs a second confirmation.
// The path checks, which need the workspace, can make a refusal guarded
// too; the Rules test does not perform them.
func denialClass(d *rules.Denial) string {
	if d.Guarded() {
		return "guarded: approvable after a second confirmation"
	}
	return "approvable"
}
