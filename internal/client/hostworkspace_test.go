package client_test

import (
	"fmt"
	"testing"
)

// recordWorkspaceAndPwd is a script that records $WORKSPACE_DIR, set from
// ${host.workspace}, and $PWD, set by the daemon.
const recordWorkspaceAndPwd = `printf "%s\n" "$$WORKSPACE_DIR" "$$PWD" > "$$RECORD"`

// printWorkspace is a script that prints $WORKSPACE_DIR, set from
// ${host.workspace}.
const printWorkspace = `printf "%s" "$$WORKSPACE_DIR"`

// hostWorkspaceRules lets sh run script, a single-quote-free shell script
// that reads $WORKSPACE_DIR, set from ${host.workspace}; the rule has no
// secrets, so no lists.
func hostWorkspaceRules(script string) string {
	return `
rules:
  - command: sh
    inject:
      command_args: ["-c", '` + script + `']
      env: {WORKSPACE_DIR: "${host.workspace}"}
    args: none
`
}

// ${host.workspace} in an env value reaches the program as the
// workspace's host path, which is the working directory at the root.
func TestRunSetsTheHostWorkspaceInTheEnvironment(t *testing.T) {
	h := startDaemon(t, policy(t, hostWorkspaceRules(recordWorkspaceAndPwd)))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, h.hostRoot+"\n"+h.hostRoot+"\n")
}

// From a subdirectory, ${host.workspace} is still the workspace's root,
// while the working directory is the subdirectory.
func TestRunKeepsTheHostWorkspaceAtTheRootFromASubdirectory(t *testing.T) {
	h := startDaemon(t, policy(t, hostWorkspaceRules(recordWorkspaceAndPwd)))
	got, rec := h.recordedIn(t, containerRoot+"/sub", "sh")
	expectRecorded(t, got, rec, h.hostRoot+"\n"+h.hostRoot+"/sub\n")
}

// ${host.workspace} in program_args, command_args and trailing_args each
// reaches the program as the workspace's host path.
func TestRunPutsTheHostWorkspaceInEveryArgumentPosition(t *testing.T) {
	rec := recorder(t)
	config := fmt.Sprintf(`
rules:
  - command: %s sub
    inject:
      program_args: ["p=${host.workspace}"]
      command_args: ["c=${host.workspace}"]
      trailing_args: ["t=${host.workspace}"]
    args: none
`, rec)
	h := startDaemon(t, policy(t, config))
	got, record := h.recorded(t, rec, "sub")
	expectRecorded(t, got, record, "p="+h.hostRoot+"\nsub\nc="+h.hostRoot+"\nt="+h.hostRoot+"\n")
}

// With the workspace given through a symlink, ${host.workspace} is the
// real path the mirrored directory lies in, not the link.
func TestRunSetsTheHostWorkspaceToTheRealPathOfASymlinkedWorkspace(t *testing.T) {
	h := startSymlinkedDaemon(t, policy(t, hostWorkspaceRules(recordWorkspaceAndPwd)))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, h.hostRoot+"\n"+h.hostRoot+"\n")
}

// A rule using only ${host.workspace} is served without allow lists, and
// its output shows the path: it is not a secret, so nothing masks it.
func TestRunServesTheHostWorkspaceWithoutMaskingIt(t *testing.T) {
	h := startDaemon(t, policy(t, hostWorkspaceRules(printWorkspace)))
	got := h.run(t, containerRoot, "", "sh")
	if want := (result{stdout: h.hostRoot}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A dry run of a rule that uses ${host.workspace} reports what a dry run of
// a rule without secrets does: the host path is named nowhere.
func TestDryRunOfTheHostWorkspaceReportsOnlyTheRule(t *testing.T) {
	h := startDaemon(t, policy(t, hostWorkspaceRules(printWorkspace)))
	got := h.run(t, containerRoot, "", "--dry-run", "sh")
	if want := (result{stderr: "hostrun: dry run: allowed by rule \"sh\"\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A secret source is not a template: ${host.workspace} in a command source
// stays literal text, and the program gets it as printed.
func TestRunLeavesTheHostWorkspaceLiteralInASecretSource(t *testing.T) {
	const literal = "${host.workspace}"
	h := startDaemon(t, policy(t, commandRules(`{command: [echo, '`+literal+`']}`)))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, literal+"\n")
}

// A rule with a fixed directory runs its command there, while
// ${host.workspace} is still the workspace's host path.
func TestRunKeepsTheHostWorkspaceForARuleWithAFixedDirectory(t *testing.T) {
	dir := fixedDir(t)
	config := fmt.Sprintf(`
rules:
  - command: sh
    dir: %s
    inject:
      command_args: ["-c", '%s']
      env: {WORKSPACE_DIR: "${host.workspace}"}
    args: none
`, dir, recordWorkspaceAndPwd)
	h := startDaemon(t, policy(t, config))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, h.hostRoot+"\n"+dir+"\n")
}

// ${host.workspace} and a secret in one template both render, and the
// secret, unlike the path, is still masked in what the command prints.
func TestRunRendersTheHostWorkspaceAndASecretInOneTemplate(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, `
rules:
  - command: sh
    secrets:
      token: {env: TOKEN}
    inject:
      command_args: ["-c", 'printf "%s\n" "$$0" > "$$RECORD"; printf "%s" "$$0"', "${host.workspace}:${token}"]
    flags:
      allow: []
    positional:
      allow: ["*"]
`))
	got, rec := h.recorded(t, "sh")
	if want := h.hostRoot + ":" + testToken + "\n"; rec != want {
		t.Fatalf("the program got %q, want %q", rec, want)
	}
	if want := (result{stdout: h.hostRoot + ":***"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
