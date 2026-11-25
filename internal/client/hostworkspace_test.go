package client_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordWorkspaceAndPwd is a script that records $WORKSPACE_DIR, set from
// ${host.workspace}, and $PWD, set by the daemon.
const recordWorkspaceAndPwd = `printf "%s\n" "$$WORKSPACE_DIR" "$$PWD" > "$$RECORD"`

// printWorkspace is a script that prints $WORKSPACE_DIR, set from
// ${host.workspace}.
const printWorkspace = `printf "%s" "$$WORKSPACE_DIR"`

// shRule returns the rules YAML of a single rule for sh. Each element of
// inject is one entry of the rule's inject block, and each element of fields
// is one further line of the rule (a nested line starts with its own
// indentation); the helper indents both to their place in the rule.
func shRule(inject, fields []string) string {
	var b strings.Builder
	b.WriteString("\nrules:\n  - command: sh\n    inject:\n")
	for _, line := range inject {
		b.WriteString("      " + line + "\n")
	}
	for _, line := range fields {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// hostWorkspaceRules lets sh run script, a single-quote-free shell script
// that reads $WORKSPACE_DIR, set from ${host.workspace}; the rule has no
// secrets, so no lists.
func hostWorkspaceRules(script string) string {
	return shRule([]string{
		`command_args: ["-c", '` + script + `']`,
		`env: {WORKSPACE_DIR: "${host.workspace}"}`,
	}, []string{"args: none"})
}

// hostWorkspaceFileRules lets sh record the contents of an injected file
// that holds ${host.workspace}.
var hostWorkspaceFileRules = shRule([]string{
	`command_args: ["-c", 'cat "$$0" > "$$RECORD"', "${file.cfg}"]`,
	`files: {cfg: "root = ${host.workspace}\n"}`,
}, []string{"args: none"})

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

// A secret source is not a template: an env source's name is looked up as
// written, so the variable literally named ${host.workspace} is the one
// read. Were the name rendered, it would be the workspace's host path, no
// variable of that name is set, and the request would be refused.
func TestRunLeavesTheHostWorkspaceLiteralInAnEnvSecretSource(t *testing.T) {
	const literal = "${host.workspace}"
	t.Setenv(literal, testToken)
	h := startDaemon(t, policy(t, commandRules(`{env: '`+literal+`'}`)))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, testToken+"\n")
}

// A secret source is not a template: ${host.workspace} in a file source's
// path stays literal text, so the file in the directory literally named
// ${host.workspace} is the one read, not a file under the workspace's host
// path.
func TestRunLeavesTheHostWorkspaceLiteralInAFileSecretSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "${host.workspace}")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := startDaemon(t, policy(t, commandRules(`{file: '`+path+`'}`)))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, testToken+"\n")
}

// A rule with a fixed directory runs its command there, while
// ${host.workspace} is still the workspace's host path.
func TestRunKeepsTheHostWorkspaceForARuleWithAFixedDirectory(t *testing.T) {
	dir := fixedDir(t)
	config := shRule([]string{
		`command_args: ["-c", '` + recordWorkspaceAndPwd + `']`,
		`env: {WORKSPACE_DIR: "${host.workspace}"}`,
	}, []string{"dir: " + dir, "args: none"})
	h := startDaemon(t, policy(t, config))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, h.hostRoot+"\n"+dir+"\n")
}

// ${host.workspace} and a secret in one template both render, and the
// secret, unlike the path, is still masked in what the command prints.
func TestRunRendersTheHostWorkspaceAndASecretInOneTemplate(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, shRule([]string{
		`command_args: ["-c", 'printf "%s\n" "$$0" > "$$RECORD"; printf "%s" "$$0"', "${host.workspace}:${token}"]`,
	}, []string{
		"secrets:",
		"  token: {env: TOKEN}",
		"flags:",
		"  allow: []",
		"positional:",
		`  allow: ["*"]`,
	})))
	got, rec := h.recorded(t, "sh")
	if want := h.hostRoot + ":" + testToken + "\n"; rec != want {
		t.Fatalf("the program got %q, want %q", rec, want)
	}
	if want := (result{stdout: h.hostRoot + ":***"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// ${host.workspace} in an injected file's contents reaches the program as
// the workspace's host path.
func TestRunRendersTheHostWorkspaceInAnInjectedFile(t *testing.T) {
	h := startDaemon(t, policy(t, hostWorkspaceFileRules))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, "root = "+h.hostRoot+"\n")
}

// With the workspace given through a symlink, ${host.workspace} in an
// injected file's contents is the real path, not the link.
func TestRunRendersTheRealPathOfASymlinkedWorkspaceInAnInjectedFile(t *testing.T) {
	h := startSymlinkedDaemon(t, policy(t, hostWorkspaceFileRules))
	got, rec := h.recorded(t, "sh")
	expectRecorded(t, got, rec, "root = "+h.hostRoot+"\n")
}

// ${host.workspace} and a secret in one file's contents both render, and
// the secret, unlike the path, is still masked in what the command prints.
func TestRunRendersTheHostWorkspaceAndASecretInOneInjectedFile(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, shRule([]string{
		`command_args: ["-c", 'cat "$$0" > "$$RECORD"; cat "$$0"', "${file.cfg}"]`,
		`files: {cfg: "${host.workspace}:${token}\n"}`,
	}, []string{
		"secrets:",
		"  token: {env: TOKEN}",
		"flags:",
		"  allow: []",
		"positional:",
		`  allow: ["*"]`,
	})))
	got, rec := h.recorded(t, "sh")
	if want := h.hostRoot + ":" + testToken + "\n"; rec != want {
		t.Fatalf("the program got %q, want %q", rec, want)
	}
	if want := (result{stdout: h.hostRoot + ":***\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
