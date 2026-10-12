package client_test

import (
	"strings"
	"testing"
)

// maskRules runs a script the container picks from fixed ones, with two
// secrets the script can print: $TOKEN and $OTHER, both from the daemon's
// environment, also handed to the script as $0 and $1.
const maskRules = `
rules:
  - command: sh
    secrets:
      token: {env: TOKEN}
      other: {env: OTHER}
    inject:
      command_args: ["-c", 'eval "$$SCRIPT"', "${token}", "${other}"]
    flags:
      allow: []
    positional:
      allow: []
`

// The container gets *** wherever the command prints a secret's exact
// value, on stdout and on stderr, even when one write ends in the middle
// of a value and the next completes it.
func TestRunMasksSecretsInTheCommandsOutput(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	t.Setenv("OTHER", "other-secret-42")
	h := startDaemon(t, policy(t, maskRules))
	for name, tc := range map[string]struct {
		script string
		want   result
	}{
		"stdout": {`echo "auth: $0 ok"`, result{stdout: "auth: *** ok\n"}},
		"stderr": {`echo "auth: $0" >&2`, result{stderr: "auth: ***\n"}},
		"split":  {`printf 'x tok-01'; sleep 0.2; printf '23456789 y\n'`, result{stdout: "x *** y\n"}},
		"both":   {`echo "$1 $0 $1$0"`, result{stdout: "*** *** ******\n"}},
		"prefix at the end": {
			`printf 'tok-01'`, result{stdout: "tok-01"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SCRIPT", tc.script)
			if got := h.run(t, containerRoot, "", "sh"); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A value that begins another is not masked on its own while the rest of
// the longer one may still come: the longer value's tail must not leak.
func TestRunMasksALongerSecretSplitAfterAShorterOne(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	t.Setenv("OTHER", testToken+"-long")
	h := startDaemon(t, policy(t, maskRules))
	t.Setenv("SCRIPT", `printf '%s' "$0"; sleep 0.2; printf -- '-long\n'`)
	if got := h.run(t, containerRoot, "", "sh"); got != (result{stdout: "***\n"}) {
		t.Fatalf("got %+v, want the longer value masked whole", got)
	}
}

// Output that holds no value passes byte for byte, even when it keeps
// starting one (held back, then written) across many writes.
func TestRunPassesOutputWithoutSecretValuesUnchanged(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	t.Setenv("OTHER", "other-secret-42")
	h := startDaemon(t, policy(t, maskRules))
	t.Setenv("SCRIPT", `i=0; while [ $i -lt 30000 ]; do printf 'tok-01*'; i=$((i+1)); done; printf 'tok-0123'`)
	got := h.run(t, containerRoot, "", "sh")
	if want := strings.Repeat("tok-01*", 30000) + "tok-0123"; got.code != 0 || got.stdout != want || got.stderr != "" {
		t.Fatalf("got code %d, %d stdout bytes, stderr %q; want 0, the %d bytes unchanged", got.code, len(got.stdout), got.stderr, len(want))
	}
}
