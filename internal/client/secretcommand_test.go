package client_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// commandRules is recordRules with the token's source replaced by source,
// the YAML of a command source.
func commandRules(source string) string {
	return fmt.Sprintf(`
rules:
  - command: sh
    secrets:
      token: %s
    inject:
      command_args: ["-c", 'printf "%%s\n" "$$0" "$$@" > "$$RECORD"', "${token}"]
    flags:
      allow: []
    positional:
      allow: ["*"]
`, source)
}

// A command source's stdout is the value, with the line end trimmed.
func TestRunTakesASecretFromTheOutputOfAHostCommand(t *testing.T) {
	h := startDaemon(t, policy(t, commandRules(`{command: [echo, `+testToken+`]}`)))
	got, rec := h.recorded(t, "sh")
	if got != (result{}) || rec != testToken+"\n" {
		t.Fatalf("got %+v, the program got %q; want %q", got, rec, testToken+"\n")
	}
}

// A command source runs where the command does, with an empty stdin that
// no source waiting for input can hang on.
func TestRunRunsACommandSourceInTheWorkingDirectoryWithAnEmptyStdin(t *testing.T) {
	source := `{command: [sh, -c, 'cat; pwd -P']}`
	h := startDaemon(t, policy(t, commandRules(source)))
	got, rec := h.recorded(t, "sh")
	if got != (result{}) || rec != h.hostRoot+"\n" {
		t.Fatalf("got %+v, the program got %q; want %q", got, rec, h.hostRoot+"\n")
	}
	dir := fixedDir(t)
	h = startDaemon(t, policy(t, strings.Replace(commandRules(source), "  - command: sh\n", "  - command: sh\n    dir: "+dir+"\n", 1)))
	if got, rec := h.recorded(t, "sh"); got != (result{}) || rec != dir+"\n" {
		t.Fatalf("with dir: got %+v, the program got %q; want %q", got, rec, dir+"\n")
	}
}

// A secret used twice is obtained once per request.
func TestRunRunsACommandSourceOncePerRequest(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv("COUNT", count)
	config := strings.Replace(commandRules(`{command: [sh, -c, 'echo x >> "$COUNT"; echo `+testToken+`']}`),
		`"${token}"]`, `"${token}", "${token}"]`, 1)
	h := startDaemon(t, policy(t, config))
	for range 2 {
		if got, rec := h.recorded(t, "sh"); got != (result{}) || rec != testToken+"\n"+testToken+"\n" {
			t.Fatalf("got %+v, the program got %q", got, rec)
		}
	}
	if data, _ := os.ReadFile(count); string(data) != "x\nx\n" {
		t.Fatalf("the source ran %q, want once per request", data)
	}
}

// A source that hangs is stopped at its timeout, with whatever it started.
func TestRunStopsACommandSourceAtItsTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PIDFILE", pidFile)
	h := startDaemon(t, policy(t, commandRules(`{command: [sh, -c, 'sleep 60 & echo $! > "$PIDFILE"; wait'], timeout: 200ms}`)))
	start := time.Now()
	got, rec := h.recorded(t, "sh")
	if want := (result{code: protocol.ExitHostrunError, stderr: "hostrun: secret \"token\" is not available\n"}); got != want || rec != "" {
		t.Fatalf("got %+v, the program got %q; want %+v", got, rec, want)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the refusal took %s", elapsed)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the source's child survived its timeout")
		}
	}
}

// A failed source's stderr helps fix it, so it goes to the log; a
// successful one's may carry the value, so it goes nowhere. The client
// never sees either.
func TestRunLogsTheStderrOfAFailedCommandSourceOnly(t *testing.T) {
	var log lockedBuffer
	failing := commandRules(`{command: [sh, -c, 'echo please log in >&2; exit 3']}`)
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, failing))
	got, _ := h.recorded(t, "sh")
	if want := (result{code: protocol.ExitHostrunError, stderr: "hostrun: secret \"token\" is not available\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !strings.Contains(log.String(), "please log in") {
		t.Fatalf("the log lacks the source's stderr:\n%s", log.String())
	}

	log = lockedBuffer{}
	ok := commandRules(`{command: [sh, -c, 'echo debug ` + testToken + ` >&2; echo ` + testToken + `']}`)
	h = startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, ok))
	if got, _ := h.recorded(t, "sh"); got != (result{}) {
		t.Fatalf("got %+v", got)
	}
	if strings.Contains(log.String(), testToken) || strings.Contains(log.String(), "debug") {
		t.Fatalf("the log holds a successful source's output:\n%s", log.String())
	}
}

// alive reports whether process pid is running. A zombie is not: it has
// been killed, and in a container whose init does not reap orphans it
// stays one.
func alive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return !strings.HasPrefix(after, "Z")
}
