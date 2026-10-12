package client_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// testToken is the value tests give a secret: fixed, and long enough to
// be masked.
const testToken = "tok-0123456789"

// recorded runs argv through h and returns what the started program wrote
// to the file named by $RECORD, the command's arguments one per line. A
// test's program writes there rather than to stdout, which the daemon
// would mask.
func (h harness) recorded(t *testing.T, argv ...string) (result, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "record")
	t.Setenv("RECORD", path)
	got := h.run(t, containerRoot, "", argv...)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return got, string(data)
}

// recordRules allows sh with any positional arguments and no flags, adding
// a script that records $0 and its arguments, and the secret token read
// from $TOKEN, as "Bearer <token>".
const recordRules = `
rules:
  - command: sh
    secrets:
      token: {env: TOKEN}
    inject:
      command_args: ["-c", 'printf "%s\n" "$$0" "$$@" > "$$RECORD"', "Bearer ${token}"]
    flags:
      allow: []
    positional:
      allow: ["*"]
`

// The program gets the secret where the rule puts it, right after the
// rule's command and before the container's arguments.
func TestRunInjectsAnEnvSecretAfterTheRuleCommand(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, recordRules))
	got, rec := h.recorded(t, "sh", "first", "second")
	if got != (result{}) {
		t.Fatalf("got %+v, want a clean exit", got)
	}
	if want := "Bearer " + testToken + "\nfirst\nsecond\n"; rec != want {
		t.Fatalf("the program got %q, want %q", rec, want)
	}
}

// The value is read for every request, so a rotated token is never stale,
// and the line end a file or a command would leave is trimmed.
func TestRunReadsTheSecretFreshAndTrimsTheLineEnd(t *testing.T) {
	h := startDaemon(t, policy(t, recordRules))
	for _, value := range []string{testToken + "\r\n", "tok-rotated-9876\n"} {
		t.Setenv("TOKEN", value)
		_, rec := h.recorded(t, "sh")
		if want := "Bearer " + strings.TrimRight(value, "\r\n") + "\n"; rec != want {
			t.Fatalf("with $TOKEN %q the program got %q, want %q", value, rec, want)
		}
	}
}

// A secret without a usable value stops the request before the command
// starts, naming the secret only, with the code for a host that could not
// prepare the command rather than a refusal of the container's arguments.
func TestRunRefusesASecretWithoutAUsableValue(t *testing.T) {
	h := startDaemon(t, policy(t, recordRules))
	for _, tc := range []struct {
		name, value, want string
		set               bool
	}{
		{name: "unset", want: `secret "token" is not available`},
		{name: "empty", set: true, value: "\n", want: `secret "token" is not available`},
		{name: "short", set: true, value: "short12", want: `secret "token" is too short to mask`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TOKEN", tc.value)
			} else {
				t.Setenv("TOKEN", "")
				os.Unsetenv("TOKEN")
			}
			got, rec := h.recorded(t, "sh")
			if want := (result{code: protocol.ExitHostrunError, stderr: "hostrun: " + tc.want + "\n"}); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			if rec != "" {
				t.Fatalf("the command ran: %q", rec)
			}
		})
	}
}

// The daemon log names the secrets a command got, next to the container's
// argv, and never holds a value, not even when the source fails.
func TestRunLogsTheSecretsByNameOnly(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, recordRules))
	t.Setenv("TOKEN", testToken)
	if got, _ := h.recorded(t, "sh", "first"); got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	t.Setenv("TOKEN", "short12")
	h.recorded(t, "sh", "first")
	lines := strings.Split(log.String(), "\n")
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, `msg="command finished"`) && strings.Contains(l, "secrets=[token]") && strings.Contains(l, `argv="[sh first]"`)
	}) {
		t.Errorf("no command finished line with secrets=[token] and the container's argv in:\n%s", log.String())
	}
	for _, value := range []string{testToken, "short12"} {
		if strings.Contains(log.String(), value) {
			t.Errorf("the log holds the value %q:\n%s", value, log.String())
		}
	}
}

// An injection without secrets needs no strict lists, and the arguments it
// adds before the container's do not shift what a path check replaces.
func TestRunInjectsFixedArgumentsBeforeAPathOpenArgument(t *testing.T) {
	config := "rules:\n  - command: cat\n    inject:\n      command_args: [-n]\n    positional:\n      path: open\n"
	h := startDaemon(t, policy(t, config))
	if err := os.WriteFile(filepath.Join(h.hostRoot, "app.txt"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t, containerRoot, "", "cat", "app.txt"); got != (result{stdout: "     1\tinside\n"}) {
		t.Fatalf("got %+v, want app.txt numbered", got)
	}
}
