package client_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// A dry run names the secrets the command would get, and obtains none:
// a command source does not run.
func TestDryRunReportsTheSecretsWithoutObtainingThem(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("MARKER", marker)
	t.Setenv("OTHER", testToken)
	config := `
rules:
  - command: sh
    secrets:
      token: {command: [sh, -c, 'touch "$MARKER"; echo ` + testToken + `']}
      other: {env: OTHER}
    inject:
      command_args: ["-c", "true", "${token}", "${other}"]
    flags:
      allow: []
    positional:
      allow: []
`
	h := startDaemon(t, policy(t, config))
	got := h.run(t, containerRoot, "", "--dry-run", "sh")
	if want := (result{stderr: "hostrun: dry run: allowed by rule \"sh\", injects secrets: other, token\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the dry run ran the command source")
	}
}

// A dry run predicts the run: a source that cannot work refuses both,
// with the same code and message.
func TestDryRunIsRefusedForABrokenSecretSourceAsTheRunIs(t *testing.T) {
	os.Unsetenv("NO_SUCH_TOKEN")
	for name, source := range map[string]string{
		"command": `{command: [no-such-authtool-xyz]}`,
		"env":     `{env: NO_SUCH_TOKEN}`,
		"file":    `{file: no/such/token}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := startDaemon(t, policy(t, commandRules(source)))
			real, _ := h.recorded(t, "sh")
			if real.code != protocol.ExitHostrunError || !strings.Contains(real.stderr, `secret "token" is not available`) {
				t.Fatalf("the run was not refused: %+v", real)
			}
			if got := h.run(t, containerRoot, "", "--dry-run", "sh"); got != real {
				t.Fatalf("dry run %+v, want the run's %+v", got, real)
			}
		})
	}
}
