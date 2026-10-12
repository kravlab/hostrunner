package client_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// writeToken writes testToken, with a line end, to path.
func writeToken(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A file source is read as written when absolute, and from the working
// directory when relative: the mirrored directory, or the rule's dir.
func TestRunTakesASecretFromAHostFile(t *testing.T) {
	outside := filepath.Join(fixedDir(t), "token")
	writeToken(t, outside)
	h := startDaemon(t, policy(t, commandRules(`{file: `+outside+`}`)))
	if got, rec := h.recorded(t, "sh"); got != (result{}) || rec != testToken+"\n" {
		t.Fatalf("absolute: got %+v, the program got %q", got, rec)
	}

	h = startDaemon(t, policy(t, commandRules(`{file: sub/token}`)))
	writeToken(t, filepath.Join(h.hostRoot, "sub", "token"))
	if got, rec := h.recorded(t, "sh"); got != (result{}) || rec != testToken+"\n" {
		t.Fatalf("relative: got %+v, the program got %q", got, rec)
	}

	dir := fixedDir(t)
	writeToken(t, filepath.Join(dir, "token"))
	config := strings.Replace(commandRules(`{file: token}`), "  - command: sh\n", "  - command: sh\n    dir: "+dir+"\n", 1)
	h = startDaemon(t, policy(t, config))
	if got, rec := h.recorded(t, "sh"); got != (result{}) || rec != testToken+"\n" {
		t.Fatalf("relative to dir: got %+v, the program got %q", got, rec)
	}
}

// A file that cannot be read refuses the request without naming where it
// was looked for on the host.
func TestRunRefusesAMissingSecretFile(t *testing.T) {
	h := startDaemon(t, policy(t, commandRules(`{file: missing}`)))
	got, rec := h.recorded(t, "sh")
	if want := (result{code: protocol.ExitHostrunError, stderr: "hostrun: secret \"token\" is not available\n"}); got != want || rec != "" {
		t.Fatalf("got %+v, the program got %q; want %+v", got, rec, want)
	}
}
