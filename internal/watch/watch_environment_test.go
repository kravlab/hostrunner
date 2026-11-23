package watch_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/watch"
)

// runtimeDir returns a new directory that holds an executable script named
// name, which runs script.
func runtimeDir(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// DetectIn looks docker and podman up in the PATH of the environment it is
// given: a runtime only the process's PATH has is not found.
func TestDetectInFindsRuntimesInThePathOfTheGivenEnvironment(t *testing.T) {
	givenDir := runtimeDir(t, "docker", "")
	t.Setenv("PATH", runtimeDir(t, "podman", ""))

	got := watch.DetectIn(daemon.NewEnvironment([]string{"PATH=" + givenDir}))

	if want := filepath.Join(givenDir, "docker"); len(got) != 1 || fmt.Sprint(got[0]) != want {
		t.Fatalf("got %v, want only %s", got, want)
	}
}

// A runtime DetectIn found runs its commands with the given environment,
// and nothing of the process's: its script records a variable only the
// given environment has, and one only the process has.
func TestDetectInRunsRuntimesWithTheGivenEnvironment(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	givenDir := runtimeDir(t, "docker", `printf '%s|%s' "$HR_GIVEN" "${HR_PROCESS_ONLY-unset}" > "$HR_RECORD"`)
	t.Setenv("HR_PROCESS_ONLY", "from-the-process")
	env := daemon.NewEnvironment([]string{"PATH=" + givenDir, "HR_GIVEN=from-the-daemon", "HR_RECORD=" + record})
	rt := watch.DetectIn(env)[0]

	if _, err := rt.Containers(context.Background(), "/home/u/app"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if want := "from-the-daemon|unset"; string(got) != want {
		t.Fatalf("the runtime saw %q, want %q", got, want)
	}
}
