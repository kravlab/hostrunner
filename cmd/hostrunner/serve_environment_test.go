package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/transport"
)

// printenvConfig is the config of a serve without watch whose rules allow
// printenv.
func printenvConfig(t *testing.T) serveConfig {
	t.Helper()
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.RulesFile = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: printenv\n    args: any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// serve gives the commands of its daemon the environment it is given, not
// the process's: the process's variable is not set for them.
func TestServeRunsCommandsWithTheGivenEnvironment(t *testing.T) {
	t.Setenv("HR_PROCESS_ONLY", "from-the-process")
	cfg := printenvConfig(t)
	cfg.environment = daemon.NewEnvironment([]string{"PATH=" + os.Getenv("PATH"), "HR_GIVEN=from-the-daemon"})
	startServe(t, cfg)
	tr := transport.Unix{Path: cfg.socket}
	printenv := func(name string) (string, int) {
		var stdout bytes.Buffer
		code := client.Run(context.Background(), tr, []string{"printenv", name}, cfg.containerWorkspace, strings.NewReader(""), &stdout, io.Discard)
		return stdout.String(), code
	}

	if got, code := printenv("HR_GIVEN"); got != "from-the-daemon\n" || code != 0 {
		t.Errorf("the given variable: got %q, exit %d; want its value, exit 0", got, code)
	}
	if got, code := printenv("HR_PROCESS_ONLY"); got != "" || code == 0 {
		t.Errorf("the process's variable: got %q, exit %d; want nothing and a failure", got, code)
	}
}
