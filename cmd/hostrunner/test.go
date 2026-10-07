package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

const testUsage = "usage: hostrunner test [--config <path>] [--] <command> [args...]"

// testConfig holds the settings of `hostrunner test`.
type testConfig struct {
	config string   // rules file
	argv   []string // the command to test
}

// parseTest parses the arguments of `hostrunner test`: its flags end at
// the first non-flag argument or `--`, and the rest is the command.
func parseTest(args []string, output io.Writer) (testConfig, error) {
	var cfg testConfig
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.config, "config", rulesPath, "rules file")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.argv = fs.Args(); len(cfg.argv) == 0 {
		return cfg, errors.New(testUsage)
	}
	return cfg, nil
}

// rulesTest runs the Rules test: it tests cfg.argv against the rules file
// alone, with no daemon or container, and reports the allowing rule and
// its fixed directory on stderr. A denied command is an exitError with
// protocol.ExitRejected, as hostrun exits when the daemon denies it.
// Unlike the daemon, it takes a missing file for an error; an invalid one
// fails as in `hostrunner up`.
func rulesTest(cfg testConfig, stderr io.Writer) error {
	policy, err := rules.Read(cfg.config)
	if err != nil {
		return fmt.Errorf("rules file: %w", err)
	}
	allowed, err := policy.Check(cfg.argv)
	if err != nil {
		return &exitError{code: protocol.ExitRejected, err: err}
	}
	// The report a dry run gets, without checking the fixed directory:
	// that needs the workspace.
	_, err = fmt.Fprintf(stderr, "hostrunner: %s\n", protocol.Allowed{Rule: allowed.Rule, Dir: allowed.Dir})
	return err
}
