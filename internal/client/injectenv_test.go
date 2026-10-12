package client_test

import (
	"log/slog"
	"strings"
	"testing"
)

// envRules lets printenv print $API_TOKEN and $PWD, set from the secret in
// $TOKEN and by the daemon.
const envRules = `
rules:
  - command: sh
    secrets:
      token: {env: TOKEN}
    inject:
      command_args: ["-c", 'printf "%s\n" "$$API_TOKEN" "$$PWD" > "$$RECORD"']
      env: {API_TOKEN: "Bearer ${token}"}
    flags:
      allow: []
    positional:
      allow: ["*"]
`

// inject.env overrides the daemon's variable of the same name, and PWD is
// still the working directory.
func TestRunSetsTheInjectedEnvironmentOverTheDaemons(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	t.Setenv("API_TOKEN", "from-the-daemon")
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, envRules))
	got, rec := h.recorded(t, "sh")
	if want := "Bearer " + testToken + "\n" + h.hostRoot + "\n"; got != (result{}) || rec != want {
		t.Fatalf("got %+v, the program got %q; want %q", got, rec, want)
	}
	if strings.Contains(log.String(), testToken) {
		t.Fatalf("the log holds the value:\n%s", log.String())
	}
}
