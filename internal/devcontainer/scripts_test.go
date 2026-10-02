// Package devcontainer holds tests for the host-side scripts that the
// repository's devcontainer.json files run from initializeCommand
// (docs/specs/devcontainer-host-config.md). The scripts live in
// .devcontainer/, which `go test ./...` does not walk, so their tests are
// here; the package has no non-test code.
package devcontainer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	gitIdentityScript = "../../.devcontainer/git-identity.sh"
	gitExcludesScript = "../../.devcontainer/git-excludes.sh"
	agentsMDScript    = "../../.devcontainer/claude/agents-md.sh"
)

// result is what a script run left behind: its exit status and stderr.
type result struct {
	code   int
	stderr string
}

// runScript runs script with args in an environment that has only PATH and
// a temporary HOME, so the host user's git config never leaks in.
func runScript(t *testing.T, home, script string, args ...string) result {
	t.Helper()
	return runScriptEnv(t, home, []string{"XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}, script, args...)
}

// runScriptEnv is runScript with extra environment entries in place of
// XDG_CONFIG_HOME, so a test can set it, set it empty or leave it out.
func runScriptEnv(t *testing.T, home string, extra []string, script string, args ...string) result {
	t.Helper()
	cmd := exec.Command(script, args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
	}, extra...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result{0, stderr.String()}
	case errors.As(err, &exitErr):
		return result{exitErr.ExitCode(), stderr.String()}
	default:
		t.Fatalf("run %s: %v", script, err)
		return result{}
	}
}

// homeWithGitconfig returns a temporary HOME whose ~/.gitconfig is content,
// or which has no ~/.gitconfig when content is empty.
func homeWithGitconfig(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	if content != "" {
		writeFile(t, filepath.Join(home, ".gitconfig"), content)
	}
	return home
}

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitconfigValue reads key from file the way the container's git does, and
// reports whether it is set.
func gitconfigValue(t *testing.T, file, key string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "config", "--file", file, "--get", key).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", false
	}
	if err != nil {
		t.Fatalf("git config --file %s --get %s: %v", file, key, err)
	}
	return strings.TrimSuffix(string(out), "\n"), true
}

// assertOnlyEntries fails unless dir holds exactly the named entries, which
// catches temporary files a script left behind.
func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries of %s = %v, want %v", dir, got, want)
	}
}

func TestGitIdentityCopiesNameAndEmail(t *testing.T) {
	t.Parallel()
	home := homeWithGitconfig(t, "[user]\n"+
		"\tname = \"Q \\\"x\\\" \\\\b\"\n"+
		"\temail = q@example.com\n"+
		"[credential]\n\thelper = store\n")
	dir := filepath.Join(t.TempDir(), "host-config")

	if r := runScript(t, home, gitIdentityScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	file := filepath.Join(dir, "gitconfig")
	for key, want := range map[string]string{"user.name": `Q "x" \b`, "user.email": "q@example.com"} {
		if got, ok := gitconfigValue(t, file, key); !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", key, got, ok, want)
		}
	}
	if _, ok := gitconfigValue(t, file, "credential.helper"); ok {
		t.Error("credential.helper copied; only the identity may reach the container")
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("mode %o, want 644 (readable through a user namespace)", mode)
	}
	assertOnlyEntries(t, dir, "gitconfig")
}

func TestGitIdentityLeavesOutMissingValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		gitconfig string
		wantName  bool
	}{
		{name: "email missing", gitconfig: "[user]\n\tname = Q\n", wantName: true},
		{name: "no global config", gitconfig: "", wantName: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := homeWithGitconfig(t, tt.gitconfig)
			dir := t.TempDir()

			if r := runScript(t, home, gitIdentityScript, dir); r.code != 0 {
				t.Fatalf("exit %d, stderr %q; a missing identity must not fail devcontainer up", r.code, r.stderr)
			}

			file := filepath.Join(dir, "gitconfig")
			if _, ok := gitconfigValue(t, file, "user.name"); ok != tt.wantName {
				t.Errorf("user.name set = %v, want %v", ok, tt.wantName)
			}
			if _, ok := gitconfigValue(t, file, "user.email"); ok {
				t.Error("user.email set, want it left out")
			}
		})
	}
}

func TestGitIdentityDropsValueRemovedOnHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runScript(t, homeWithGitconfig(t, "[user]\n\tname = Q\n\temail = q@example.com\n"), gitIdentityScript, dir)

	if r := runScript(t, homeWithGitconfig(t, "[user]\n\tname = Q\n"), gitIdentityScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	if _, ok := gitconfigValue(t, filepath.Join(dir, "gitconfig"), "user.email"); ok {
		t.Error("user.email still set after it was removed on the host")
	}
}

func TestGitIdentityFailsOnMalformedConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runScript(t, homeWithGitconfig(t, "[user]\n\tname = Old\n"), gitIdentityScript, dir)

	r := runScript(t, homeWithGitconfig(t, "[user\n\tname = New\n"), gitIdentityScript, dir)

	if r.code == 0 {
		t.Fatal("exit 0; a malformed config must fail instead of silently dropping the identity")
	}
	if !strings.Contains(r.stderr, "bad config") {
		t.Errorf("stderr %q, want git's error", r.stderr)
	}
	if got, _ := gitconfigValue(t, filepath.Join(dir, "gitconfig"), "user.name"); got != "Old" {
		t.Errorf("user.name = %q, want the previous file kept (Old)", got)
	}
	assertOnlyEntries(t, dir, "gitconfig")
}

// excludes is a host excludes file's content: a negation, an anchored
// pattern and no final newline, all of which must survive the copy.
const excludes = "*.log\n!keep.log\n/build"

// writeExcludes writes excludes to path, creating its parent directories.
func writeExcludes(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, excludes)
}

// readGitignore returns the content of <dir>/gitignore, failing the test
// unless its mode is 0644 (readable through a user namespace).
func readGitignore(t *testing.T, dir string) string {
	t.Helper()
	file := filepath.Join(dir, "gitignore")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("gitignore mode %o, want 644 (readable through a user namespace)", mode)
	}
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// seedGitignore runs the excludes script with a host whose excludes are
// excludes, so a test can check what a later run does to the old file.
func seedGitignore(t *testing.T, dir string) {
	t.Helper()
	home := homeWithGitconfig(t, "[core]\n\texcludesFile = ~/excludes\n")
	writeExcludes(t, filepath.Join(home, "excludes"))
	if r := runScript(t, home, gitExcludesScript, dir); r.code != 0 {
		t.Fatalf("seed run: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestGitExcludesCopiesConfiguredFile(t *testing.T) {
	t.Parallel()
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		// gitconfig gets the home directory in place of {home}.
		gitconfig string
		want      string
	}{
		{name: "absolute path", gitconfig: "[core]\n\texcludesFile = {home}/excludes\n", want: excludes},
		{name: "tilde path", gitconfig: "[core]\n\texcludesFile = ~/excludes\n", want: excludes},
		{name: "through an include", gitconfig: "[include]\n\tpath = {home}/included\n", want: excludes},
		// Relative to the workspace, as host git run at the repository's
		// root resolves it: go.mod is a file there.
		{name: "relative path", gitconfig: "[core]\n\texcludesFile = go.mod\n", want: string(goMod)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			writeExcludes(t, filepath.Join(home, "excludes"))
			writeFile(t, filepath.Join(home, "included"), "[core]\n\texcludesFile = "+home+"/excludes\n")
			writeFile(t, filepath.Join(home, ".gitconfig"), strings.ReplaceAll(tt.gitconfig, "{home}", home))
			dir := filepath.Join(t.TempDir(), "host-config")

			if r := runScript(t, home, gitExcludesScript, dir); r.code != 0 {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}

			if got := readGitignore(t, dir); got != tt.want {
				t.Errorf("gitignore = %q, want %q", got, tt.want)
			}
			assertOnlyEntries(t, dir, "gitignore")
		})
	}
}

func TestGitExcludesFallsBackToDefaultPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// env is the XDG_CONFIG_HOME entry, if any; it gets the home
		// directory in place of {home}.
		env []string
		// file is where the excludes are written, relative to the home
		// directory.
		file string
	}{
		{name: "XDG_CONFIG_HOME set", env: []string{"XDG_CONFIG_HOME={home}/xdg"}, file: "xdg/git/ignore"},
		{name: "XDG_CONFIG_HOME empty", env: []string{"XDG_CONFIG_HOME="}, file: ".config/git/ignore"},
		{name: "XDG_CONFIG_HOME unset", env: nil, file: ".config/git/ignore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := homeWithGitconfig(t, "[user]\n\tname = Q\n")
			writeExcludes(t, filepath.Join(home, tt.file))
			var env []string
			for _, e := range tt.env {
				env = append(env, strings.ReplaceAll(e, "{home}", home))
			}
			dir := t.TempDir()

			if r := runScriptEnv(t, home, env, gitExcludesScript, dir); r.code != 0 {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}

			if got := readGitignore(t, dir); got != excludes {
				t.Errorf("gitignore = %q, want %q", got, excludes)
			}
		})
	}
}

func TestGitExcludesWritesEmptyFileWithoutExcludes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		gitconfig string
	}{
		{name: "not set, no default file", gitconfig: ""},
		{name: "set to a missing file", gitconfig: "[core]\n\texcludesFile = ~/missing\n"},
		{name: "set to an empty value", gitconfig: "[core]\n\texcludesFile =\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			if r := runScript(t, homeWithGitconfig(t, tt.gitconfig), gitExcludesScript, dir); r.code != 0 {
				t.Fatalf("exit %d, stderr %q; missing excludes must not fail devcontainer up", r.code, r.stderr)
			}

			if got := readGitignore(t, dir); got != "" {
				t.Errorf("gitignore = %q, want it empty", got)
			}
		})
	}
}

func TestGitExcludesDropsPatternRemovedOnHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedGitignore(t, dir)
	home := homeWithGitconfig(t, "[core]\n\texcludesFile = ~/excludes\n")
	writeFile(t, filepath.Join(home, "excludes"), "*.log\n")

	if r := runScript(t, home, gitExcludesScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	if got := readGitignore(t, dir); got != "*.log\n" {
		t.Errorf("gitignore = %q, want the host's current excludes", got)
	}
}

func TestGitExcludesFailsAndKeepsOldFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		gitconfig string
		// stderr is the failing tool's message.
		stderr string
	}{
		{name: "excludes path is a directory", gitconfig: "[core]\n\texcludesFile = ~\n", stderr: "cp:"},
		{name: "malformed config", gitconfig: "[core\n\texcludesFile = ~/excludes\n", stderr: "bad config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			seedGitignore(t, dir)

			r := runScript(t, homeWithGitconfig(t, tt.gitconfig), gitExcludesScript, dir)

			if r.code == 0 {
				t.Fatal("exit 0; an unreadable excludes file or config must fail devcontainer up")
			}
			if !strings.Contains(r.stderr, tt.stderr) {
				t.Errorf("stderr %q, want %q", r.stderr, tt.stderr)
			}
			if got := readGitignore(t, dir); got != excludes {
				t.Errorf("gitignore = %q, want the previous file kept", got)
			}
			assertOnlyEntries(t, dir, "gitignore")
		})
	}
}

func TestAgentsMDWithoutVariableLinksEmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if r := runScript(t, t.TempDir(), agentsMDScript, dir, ""); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	link := filepath.Join(dir, "CLAUDE.md")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(dir, "empty.md") {
		t.Errorf("CLAUDE.md -> %q (%v), want %s/empty.md", target, err, dir)
	}
	content, err := os.ReadFile(link)
	if err != nil || len(content) != 0 {
		t.Errorf("CLAUDE.md content %q (%v), want an existing empty file", content, err)
	}
}

func TestAgentsMDLinksOriginal(t *testing.T) {
	t.Parallel()
	files := t.TempDir()
	original := filepath.Join(files, "AGENTS.md")
	writeFile(t, original, "rules\n")
	symlink := filepath.Join(files, "CLAUDE.md")
	if err := os.Symlink(original, symlink); err != nil {
		t.Fatal(err)
	}

	// Sequential: both subtests rewrite the shared original.
	for _, agentsMD := range []string{original, symlink} {
		t.Run(filepath.Base(agentsMD), func(t *testing.T) {
			dir := t.TempDir()

			if r := runScript(t, t.TempDir(), agentsMDScript, dir, agentsMD); r.code != 0 {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}

			link := filepath.Join(dir, "CLAUDE.md")
			if target, err := os.Readlink(link); err != nil || target != agentsMD {
				t.Errorf("CLAUDE.md -> %q (%v), want %s", target, err, agentsMD)
			}
			// Edits must reach the container live: the link is to the
			// original, not to a copy.
			writeFile(t, original, "edited\n")
			if content, err := os.ReadFile(link); err != nil || string(content) != "edited\n" {
				t.Errorf("CLAUDE.md content %q (%v), want the original's", content, err)
			}
		})
	}
}

func TestAgentsMDRelinksWhenVariableChanges(t *testing.T) {
	t.Parallel()
	original := filepath.Join(t.TempDir(), "AGENTS.md")
	writeFile(t, original, "rules\n")
	dir := t.TempDir()
	runScript(t, t.TempDir(), agentsMDScript, dir, original)

	if r := runScript(t, t.TempDir(), agentsMDScript, dir, ""); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	if target, _ := os.Readlink(filepath.Join(dir, "CLAUDE.md")); target != filepath.Join(dir, "empty.md") {
		t.Errorf("CLAUDE.md -> %q after unsetting, want empty.md", target)
	}
}

func TestAgentsMDRejectsInvalidPath(t *testing.T) {
	t.Parallel()
	files := t.TempDir()
	writeFile(t, filepath.Join(files, "AGENTS.md"), "rules\n")
	dangling := filepath.Join(files, "dangling.md")
	if err := os.Symlink(filepath.Join(files, "gone.md"), dangling); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		agentsMD string
	}{
		{name: "relative", agentsMD: "AGENTS.md"},
		{name: "missing", agentsMD: filepath.Join(files, "missing.md")},
		{name: "directory", agentsMD: files},
		{name: "dangling symlink", agentsMD: dangling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := runScript(t, t.TempDir(), agentsMDScript, t.TempDir(), tt.agentsMD)

			if r.code != 1 {
				t.Errorf("exit %d, want 1: a set variable that names no file must fail devcontainer up", r.code)
			}
			want := "agents-md: HOSTRUNNER_AGENTS_MD (" + tt.agentsMD + ") is not an absolute path to a file"
			if !strings.Contains(r.stderr, want) {
				t.Errorf("stderr %q, want %q", r.stderr, want)
			}
		})
	}
}
