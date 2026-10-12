package client_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// recorder writes a program that records its arguments, one per line, to
// $RECORD, and returns its absolute path, as a rule's program must be.
func recorder(t *testing.T) string {
	t.Helper()
	path := filepath.Join(fixedDir(t), "rec")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RECORD\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// positionsRules gives the recorder's sub command an injection at every
// argv position, with $TOKEN at the end.
func positionsRules(rec string) string {
	return fmt.Sprintf(`
rules:
  - command: %s sub
    secrets:
      token: {env: TOKEN}
    inject:
      program_args: [--global]
      command_args: [--local]
      trailing_args: ["${token}"]
    flags:
      allow: []
    positional:
      allow: ["*"]
      path: open
`, rec)
}

// The injection lands at its three argv positions around the container's
// arguments, and a path: open argument is still replaced where it moved to.
func TestRunPlacesTheInjectionAroundTheContainersArguments(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	rec := recorder(t)
	h := startDaemon(t, policy(t, positionsRules(rec)))
	if err := os.WriteFile(filepath.Join(h.hostRoot, "app.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, record := h.recorded(t, rec, "sub", "app.txt")
	if want := "--global\nsub\n--local\n/proc/self/fd/3\n" + testToken + "\n"; got != (result{}) || record != want {
		t.Fatalf("got %+v, the program got %q; want %q", got, record, want)
	}
}

// After a -- from the container, trailing_args are still positional, as
// they always are.
func TestRunKeepsTrailingArgsPositionalAfterADoubleDash(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	rec := recorder(t)
	config := strings.Replace(positionsRules(rec), "      path: open\n", "", 1)
	h := startDaemon(t, policy(t, config))
	got, record := h.recorded(t, rec, "sub", "--", "-x")
	if want := "--global\nsub\n--local\n--\n-x\n" + testToken + "\n"; got != (result{}) || record != want {
		t.Fatalf("got %+v, the program got %q; want %q", got, record, want)
	}
}

// A value that would make a trailing argument read as a flag stops the
// request, without showing the value.
func TestRunRefusesATrailingArgumentRenderedAsAFlag(t *testing.T) {
	t.Setenv("TOKEN", "-"+testToken)
	rec := recorder(t)
	h := startDaemon(t, policy(t, strings.Replace(positionsRules(rec), "      path: open\n", "", 1)))
	got, record := h.recorded(t, rec, "sub")
	if got.code != protocol.ExitHostrunError || record != "" || strings.Contains(got.stderr, testToken) ||
		!strings.Contains(got.stderr, "starts with -") {
		t.Fatalf("got %+v, the program got %q; want a refusal that hides the value", got, record)
	}
}
