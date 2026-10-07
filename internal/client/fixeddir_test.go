package client_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// fixedDir returns a new, symlink-free directory outside every harness's
// workspace, for a rule to name as its fixed directory.
func fixedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fixedDirRules allows pwd and printenv, both running in dir.
func fixedDirRules(dir string) string {
	return fmt.Sprintf(`
rules:
  - command: pwd
    dir: %[1]s
    args: any
  - command: printenv
    dir: %[1]s
    args: any
`, dir)
}

// laterPolicy is a policy set after the daemon has started, for rules that
// name the daemon's own workspace.
type laterPolicy struct{ daemon.Policy }

// A fixed directory that is missing, not a directory, or leads into the
// workspace (where the container could plant configuration) is refused,
// without revealing where the workspace is on the host.
func TestRunRefusesABadFixedDirectory(t *testing.T) {
	outside := fixedDir(t)
	lp := &laterPolicy{}
	h := startDaemon(t, lp)
	if err := os.WriteFile(filepath.Join(outside, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.hostRoot, "sub"), filepath.Join(outside, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir, want string
	}{
		{"missing", outside + "/missing", "fixed directory " + outside + "/missing does not exist"},
		{"a file", outside + "/file", "fixed directory " + outside + "/file is not a directory"},
		// Written in the rules file, so naming it reveals nothing.
		{"inside the workspace", h.hostRoot + "/sub", "fixed directory " + h.hostRoot + "/sub is inside the workspace"},
		{"a symlink into the workspace", outside + "/link", "fixed directory " + outside + "/link is inside the workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lp.Policy = policy(t, fixedDirRules(tc.dir))
			got := h.run(t, containerRoot, "", "pwd")
			if got.code != protocol.ExitRejected || got.stdout != "" || !strings.HasPrefix(got.stderr, "hostrun: ") {
				t.Fatalf("got %+v, want code %d and a hostrun error", got, protocol.ExitRejected)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Fatalf("got %q, want it to say %q", got.stderr, tc.want)
			}
			if tc.dir != h.hostRoot+"/sub" && strings.Contains(got.stderr, h.hostRoot) {
				t.Fatalf("message reveals the workspace's host path: %q", got.stderr)
			}
		})
	}
}

// The checks a rule with dir shares with every rule come first and are
// reported as they are today.
func TestRunChecksRulesAndTheMirroredDirectoryBeforeTheFixedDirectory(t *testing.T) {
	missing := fixedDir(t) + "/missing"
	h := startDaemon(t, policy(t, fmt.Sprintf("rules:\n  - command: pwd\n    dir: %s\n    args: none\n", missing)))
	for _, tc := range []struct {
		name, cwd string
		argv      []string
		want      string
	}{
		{"denied by the rule", containerRoot, []string{"pwd", "-P"}, "hostrun: denied by rule \"pwd\": arguments are not allowed\n"},
		{"cwd outside the workspace", "/etc", []string{"pwd"}, "hostrun: working directory /etc is outside the workspace\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.run(t, tc.cwd, "", tc.argv...)
			if want := (result{code: protocol.ExitRejected, stderr: tc.want}); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

// dir is not inherited: a longer rule without it runs its command in the
// mirrored directory.
func TestRunRunsALongerRuleWithoutDirInTheMirroredDirectory(t *testing.T) {
	config := fmt.Sprintf("rules:\n  - command: sh\n    dir: %s\n    args: any\n  - command: sh -c\n    args: any\n", fixedDir(t))
	h := startDaemon(t, policy(t, config))
	want := filepath.Join(h.hostRoot, "sub")
	if got := h.run(t, containerRoot+"/sub", "", "sh", "-c", "pwd -P"); got != (result{stdout: want + "\n"}) {
		t.Fatalf("got %+v, want %s", got, want)
	}
}

// A path: open argument is resolved from the mirrored directory; the
// program, in the fixed directory, gets the opened file.
func TestRunResolvesPathOpenFromTheMirroredDirectoryUnderDir(t *testing.T) {
	dir := fixedDir(t)
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := startDaemon(t, policy(t, fmt.Sprintf("rules:\n  - command: cat\n    dir: %s\n    positional:\n      path: open\n", dir)))
	if err := os.WriteFile(filepath.Join(h.hostRoot, "sub", "notes.md"), []byte("workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t, containerRoot+"/sub", "", "cat", "notes.md"); got != (result{stdout: "workspace"}) {
		t.Fatalf("got %+v, want the workspace file", got)
	}
	got := h.run(t, containerRoot, "", "cat", "notes.md")
	want := result{code: protocol.ExitRejected, stderr: "hostrun: argument \"notes.md\" is not a workspace file: it does not exist\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// inherit takes the shorter rule's fixed directory, so the longer rule's
// command runs there too.
func TestRunRunsARuleWithDirInheritInTheInheritedDirectory(t *testing.T) {
	dir := fixedDir(t)
	config := fmt.Sprintf("rules:\n  - command: sh\n    dir: %s\n    args: none\n  - command: sh -c\n    dir: inherit\n    args: any\n", dir)
	h := startDaemon(t, policy(t, config))
	if got := h.run(t, containerRoot+"/sub", "", "sh", "-c", "pwd -P"); got != (result{stdout: dir + "\n"}) {
		t.Fatalf("got %+v, want %s", got, dir)
	}
}

// A dry run names where the command would run, inherit resolved, and runs
// nothing.
func TestDryRunReportsTheFixedDirectoryWithoutRunning(t *testing.T) {
	dir := fixedDir(t)
	config := fmt.Sprintf("rules:\n  - command: sh\n    dir: %s\n    args: any\n  - command: sh -c\n    dir: inherit\n    args: any\n", dir)
	h := startDaemon(t, policy(t, config))
	got := h.run(t, containerRoot, "", "--dry-run", "sh", "-c", "touch marker")
	if want := (result{stderr: "hostrun: dry run: allowed by rule \"sh -c\", runs in " + dir + "\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err == nil {
		t.Fatal("the dry run ran the command")
	}
}

// A dry run predicts the run, so a bad fixed directory refuses both alike.
func TestDryRunIsRefusedForABadFixedDirectoryAsTheRunIs(t *testing.T) {
	h := startDaemon(t, policy(t, fixedDirRules(fixedDir(t)+"/missing")))
	real := h.run(t, containerRoot, "", "pwd")
	if real.code != protocol.ExitRejected {
		t.Fatalf("the run was not refused: %+v", real)
	}
	if got := h.run(t, containerRoot, "", "--dry-run", "pwd"); got != real {
		t.Fatalf("dry run %+v, want the run's %+v", got, real)
	}
}

// The fixed directory replaces the mirrored one for both the physical
// directory and PWD, which a daemon started elsewhere must not leak.
func TestRunRunsARuleWithDirInTheFixedDirectory(t *testing.T) {
	t.Setenv("PWD", "/daemon/started/elsewhere") // must not leak into the command
	dir := fixedDir(t)
	h := startDaemon(t, policy(t, fixedDirRules(dir)))
	if got := h.run(t, containerRoot+"/sub", "", "pwd", "-P"); got != (result{stdout: dir + "\n"}) {
		t.Fatalf("physical: got %+v, want %s", got, dir)
	}
	// printenv reads the environment as given; a shell would repair a bad
	// PWD by itself and hide the difference.
	if got := h.run(t, containerRoot+"/sub", "", "printenv", "PWD"); got != (result{stdout: dir + "\n"}) {
		t.Fatalf("logical: got %+v, want %s", got, dir)
	}
}
