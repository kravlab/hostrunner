// Package devcontainer holds tests for the host-side scripts that the
// repository's devcontainer.json files run from initializeCommand
// (docs/specs/devcontainer-host-config.md,
// docs/specs/devcontainer-claude-skills-hooks.md). The scripts live in
// .devcontainer/, which `go test ./...` does not walk, so their tests are
// here; the package has no non-test code.
package devcontainer

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	gitIdentityScript = "../../.devcontainer/git-identity.sh"
	gitExcludesScript = "../../.devcontainer/git-excludes.sh"
	agentsMDScript    = "../../.devcontainer/claude/agents-md.sh"
	skillsScript      = "../../.devcontainer/claude/skills.sh"
	hooksScript       = "../../.devcontainer/claude/hooks.sh"
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

// writeSkill writes a skill, a directory with a SKILL.md of the given
// content, at dir, creating its parent directories.
func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "SKILL.md"), content)
}

// symlink makes link a symbolic link to target, failing the test on error.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// assertRegularFile fails unless path is a regular file, not a symlink to
// one, with the given content.
func assertRegularFile(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Error(err)
		return
	}
	if !info.Mode().IsRegular() {
		t.Errorf("%s has mode %v, want a regular file: a link would dangle in the container", path, info.Mode())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Errorf("%s = %q (%v), want %q", path, got, err, want)
	}
}

func TestSkillsCopiesWhatSymlinksPointTo(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skills := filepath.Join(home, ".claude", "skills")
	elsewhere := t.TempDir()
	writeSkill(t, filepath.Join(skills, "plain"), "plain\n")
	// Skills installed outside ~/.claude and linked into it, relatively
	// and absolutely, as skill installers do.
	writeSkill(t, filepath.Join(home, ".agents", "skills", "relative"), "relative\n")
	symlink(t, "../../.agents/skills/relative", filepath.Join(skills, "relative"))
	writeSkill(t, filepath.Join(elsewhere, "absolute"), "absolute\n")
	symlink(t, filepath.Join(elsewhere, "absolute"), filepath.Join(skills, "absolute"))
	// A link inside a skill.
	writeFile(t, filepath.Join(elsewhere, "shared.md"), "shared\n")
	symlink(t, filepath.Join(elsewhere, "shared.md"), filepath.Join(skills, "plain", "shared.md"))
	dir := filepath.Join(t.TempDir(), "host-config")

	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	copied := filepath.Join(dir, "claude", "skills")
	for file, want := range map[string]string{
		"plain/SKILL.md":    "plain\n",
		"plain/shared.md":   "shared\n",
		"relative/SKILL.md": "relative\n",
		"absolute/SKILL.md": "absolute\n",
	} {
		assertRegularFile(t, filepath.Join(copied, file), want)
	}
	for _, skill := range []string{"relative", "absolute"} {
		if info, err := os.Lstat(filepath.Join(copied, skill)); err != nil || !info.IsDir() {
			t.Errorf("%s: %v (%v), want a directory: a link would dangle in the container", skill, info, err)
		}
	}
	assertOnlyEntries(t, copied, "absolute", "plain", "relative")
	assertOnlyEntries(t, filepath.Join(dir, "claude"), "skills")
}

func TestSkillsLeavesOutSyncedAccountSkills(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skills := filepath.Join(home, ".claude", "skills")
	writeSkill(t, filepath.Join(skills, "mine"), "mine\n")
	writeSkill(t, filepath.Join(skills, "synced", "bucket", "account-skill"), "account\n")
	dir := t.TempDir()

	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	// The container syncs the account's skills into its own volume.
	assertOnlyEntries(t, filepath.Join(dir, "claude", "skills"), "mine")
}

func TestSkillsWritesEmptyDirectoryWithoutSkills(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// skillsDirExists is whether ~/.claude/skills exists, empty.
		skillsDirExists bool
	}{
		{name: "no skills directory", skillsDirExists: false},
		{name: "empty skills directory", skillsDirExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tt.skillsDirExists {
				if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()

			if r := runScript(t, home, skillsScript, dir); r.code != 0 {
				t.Fatalf("exit %d, stderr %q; a host without skills must not fail devcontainer up", r.code, r.stderr)
			}

			assertOnlyEntries(t, filepath.Join(dir, "claude", "skills"))
			assertOnlyEntries(t, filepath.Join(dir, "claude"), "skills")
		})
	}
}

func TestSkillsDropsSkillRemovedOnHost(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skills := filepath.Join(home, ".claude", "skills")
	writeSkill(t, filepath.Join(skills, "kept"), "old\n")
	writeSkill(t, filepath.Join(skills, "removed"), "removed\n")
	dir := t.TempDir()
	runScript(t, home, skillsScript, dir)
	if err := os.RemoveAll(filepath.Join(skills, "removed")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(skills, "kept"), "edited\n")

	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	copied := filepath.Join(dir, "claude", "skills")
	assertOnlyEntries(t, copied, "kept")
	assertRegularFile(t, filepath.Join(copied, "kept", "SKILL.md"), "edited\n")
	assertOnlyEntries(t, filepath.Join(dir, "claude"), "skills")
}

func TestSkillsFailsOnDanglingLinkAndKeepsOldCopy(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skills := filepath.Join(home, ".claude", "skills")
	writeSkill(t, filepath.Join(skills, "kept"), "kept\n")
	dir := t.TempDir()
	runScript(t, home, skillsScript, dir)
	writeSkill(t, filepath.Join(skills, "added"), "added\n")
	symlink(t, filepath.Join(home, "gone"), filepath.Join(skills, "broken"))

	r := runScript(t, home, skillsScript, dir)

	if r.code == 0 {
		t.Fatal("exit 0; a skill that cannot be copied must fail devcontainer up instead of silently disappearing")
	}
	if !strings.Contains(r.stderr, "cp:") || !strings.Contains(r.stderr, "broken") {
		t.Errorf("stderr %q, want cp's error naming the broken skill", r.stderr)
	}
	copied := filepath.Join(dir, "claude", "skills")
	assertOnlyEntries(t, copied, "kept")
	assertRegularFile(t, filepath.Join(copied, "kept", "SKILL.md"), "kept\n")
	assertOnlyEntries(t, filepath.Join(dir, "claude"), "skills")
}

func TestSkillsCopyIsReadableByOthers(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skill := filepath.Join(home, ".claude", "skills", "private")
	writeSkill(t, skill, "private\n")
	if err := os.Chmod(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(skill, "SKILL.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	// Rootless Podman's user namespace reads the mount as another user.
	copied := filepath.Join(dir, "claude", "skills")
	for path, want := range map[string]os.FileMode{
		copied:                           0o005,
		filepath.Join(copied, "private"): 0o005,
		filepath.Join(copied, "private", "SKILL.md"): 0o004,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&want != want {
			t.Errorf("%s has mode %o, want others to have %o", path, mode, want)
		}
	}
}

// readHooks returns dir/claude/hooks.json, the hooks script's output,
// decoded.
func readHooks(t *testing.T, dir string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "claude", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(content, &settings); err != nil {
		t.Fatalf("hooks.json %q: %v", content, err)
	}
	return settings
}

// homeWithSettings returns a temporary HOME whose ~/.claude/settings.json
// is content.
func homeWithSettings(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), content)
	return home
}

// hostHooks is the hooks key of a host's settings.json.
const hostHooks = `{"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "sed 1d \"$HOME/x\" | jq -Rs .", "timeout": 5}]}]}`

func TestHooksCopiesOnlyHooks(t *testing.T) {
	t.Parallel()
	home := homeWithSettings(t, `{"env": {"TOKEN": "secret"}, "permissions": {"allow": ["Bash(ls)"]}, "hooks": `+hostHooks+`}`)
	dir := filepath.Join(t.TempDir(), "host-config")

	if r := runScript(t, home, hooksScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	var wantHooks any
	if err := json.Unmarshal([]byte(hostHooks), &wantHooks); err != nil {
		t.Fatal(err)
	}
	// Only the hooks reach the container, not the host's environment or
	// permissions.
	if got, want := readHooks(t, dir), map[string]any{"hooks": wantHooks}; !reflect.DeepEqual(got, want) {
		t.Errorf("hooks.json = %v, want %v", got, want)
	}
	assertOnlyEntries(t, filepath.Join(dir, "claude"), "hooks.json")
}

func TestHooksWritesEmptySettingsWithoutHooks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// settings is the content of ~/.claude/settings.json;
		// settingsExists is whether the file exists.
		settings       string
		settingsExists bool
	}{
		{name: "no settings file", settingsExists: false},
		{name: "no hooks key", settings: `{"env": {"TOKEN": "secret"}}`, settingsExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tt.settingsExists {
				home = homeWithSettings(t, tt.settings)
			}
			dir := t.TempDir()

			if r := runScript(t, home, hooksScript, dir); r.code != 0 {
				t.Fatalf("exit %d, stderr %q; a host without hooks must not fail devcontainer up", r.code, r.stderr)
			}

			if got := readHooks(t, dir); len(got) != 0 {
				t.Errorf("hooks.json = %v, want {}", got)
			}
			assertOnlyEntries(t, filepath.Join(dir, "claude"), "hooks.json")
		})
	}
}

func TestHooksDropsHooksRemovedOnHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runScript(t, homeWithSettings(t, `{"hooks": `+hostHooks+`}`), hooksScript, dir)

	if r := runScript(t, homeWithSettings(t, `{}`), hooksScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	if got := readHooks(t, dir); len(got) != 0 {
		t.Errorf("hooks.json = %v after the hooks were removed on the host, want {}", got)
	}
}

func TestHooksFailsOnMalformedSettingsAndKeepsOldFile(t *testing.T) {
	t.Parallel()
	withSettings := func(content string) func(*testing.T) string {
		return func(t *testing.T) string { return homeWithSettings(t, content) }
	}
	tests := []struct {
		name string
		// home returns a HOME whose ~/.claude/settings.json is broken.
		home func(t *testing.T) string
	}{
		{name: "not JSON", home: withSettings(`{"hooks": `)},
		{name: "not an object", home: withSettings(`["hooks"]`)},
		{name: "null", home: withSettings(`null`)},
		{name: "empty file", home: withSettings(``)},
		{name: "several values", home: withSettings(`{"hooks": {}} {"hooks": {}}`)},
		{name: "dangling symlink", home: func(t *testing.T) string {
			home := t.TempDir()
			if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join(home, "gone.json"), filepath.Join(home, ".claude", "settings.json"))
			return home
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			runScript(t, homeWithSettings(t, `{"hooks": `+hostHooks+`}`), hooksScript, dir)

			r := runScript(t, tt.home(t), hooksScript, dir)

			if r.code == 0 {
				t.Fatal("exit 0; malformed settings must fail devcontainer up instead of silently dropping the hooks")
			}
			if !strings.Contains(r.stderr, "jq:") {
				t.Errorf("stderr %q, want jq's error", r.stderr)
			}
			if _, ok := readHooks(t, dir)["hooks"]; !ok {
				t.Error("hooks.json lost its hooks, want the previous file kept")
			}
			assertOnlyEntries(t, filepath.Join(dir, "claude"), "hooks.json")
		})
	}
}

func TestHooksFileIsReadableByOthers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if r := runScript(t, homeWithSettings(t, `{"hooks": `+hostHooks+`}`), hooksScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	info, err := os.Stat(filepath.Join(dir, "claude", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("mode %o, want 644 (readable through a user namespace)", mode)
	}
}

func TestSkillsLeavesOutHiddenEntries(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skills := filepath.Join(home, ".claude", "skills")
	writeSkill(t, filepath.Join(skills, "mine"), "mine\n")
	writeSkill(t, filepath.Join(skills, ".hidden"), "hidden\n")
	writeFile(t, filepath.Join(skills, ".DS_Store"), "")
	dir := t.TempDir()

	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}

	assertOnlyEntries(t, filepath.Join(dir, "claude", "skills"), "mine")
}

func TestSkillsReplacesCopyOfReadOnlySkill(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skill := filepath.Join(home, ".claude", "skills", "readonly")
	writeSkill(t, skill, "readonly\n")
	// A skill linked from a read-only tree; the cleanup lets the test
	// remove its own temporary directory.
	if err := os.Chmod(skill, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(skill, 0o755) })
	dir := t.TempDir()
	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("first run: exit %d, stderr %q", r.code, r.stderr)
	}

	// The second run has an old copy to remove.
	if r := runScript(t, home, skillsScript, dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %q; the copy of a read-only skill must be removable", r.code, r.stderr)
	}

	copied := filepath.Join(dir, "claude", "skills")
	assertRegularFile(t, filepath.Join(copied, "readonly", "SKILL.md"), "readonly\n")
	assertOnlyEntries(t, filepath.Join(dir, "claude"), "skills")
}

// pathWithoutJq returns a PATH entry for runScriptEnv: a directory with the
// tools hooks.sh runs except jq, as on a host that lacks it.
func pathWithoutJq(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"mkdir", "mktemp", "chmod", "mv", "rm"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		symlink(t, path, filepath.Join(bin, tool))
	}
	return "PATH=" + bin
}

func TestHooksNeedsNoJqWithoutSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	r := runScriptEnv(t, t.TempDir(), []string{pathWithoutJq(t)}, hooksScript, dir)

	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q; a host with neither settings nor jq must not fail devcontainer up", r.code, r.stderr)
	}
	if got := readHooks(t, dir); len(got) != 0 {
		t.Errorf("hooks.json = %v, want {}", got)
	}
}

func TestHooksFailsWithoutJqAndKeepsOldFile(t *testing.T) {
	t.Parallel()
	home := homeWithSettings(t, `{"hooks": `+hostHooks+`}`)
	dir := t.TempDir()
	runScript(t, home, hooksScript, dir)

	r := runScriptEnv(t, home, []string{pathWithoutJq(t)}, hooksScript, dir)

	if r.code == 0 {
		t.Fatal("exit 0; settings that cannot be read without jq must fail devcontainer up instead of silently dropping the hooks")
	}
	if !strings.Contains(r.stderr, "jq") {
		t.Errorf("stderr %q, want the shell's error naming jq", r.stderr)
	}
	if _, ok := readHooks(t, dir)["hooks"]; !ok {
		t.Error("hooks.json lost its hooks, want the previous file kept")
	}
	assertOnlyEntries(t, filepath.Join(dir, "claude"), "hooks.json")
}
