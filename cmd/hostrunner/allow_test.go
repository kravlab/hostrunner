package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/launch"
)

// allowRules is a valid rules file; allowRulesDigest is its digest.
const (
	allowRules       = "rules:\n  - command: git status\n    args: any\n"
	allowRulesDigest = "5dae3cd4d8455c5930e19b895e6c5d29acf4ac18b6c64010d05cd47dc2c97ac7"
)

// otherRules is a valid rules file whose content differs from allowRules;
// otherRulesDigest is its digest.
const (
	otherRules       = "rules:\n  - command: git log\n    args: any\n"
	otherRulesDigest = "0efe464eaecc9af757446a56754944130eb10d444c798eccdcc117d9083be592"
)

// allowStore returns the allow store of an isolated test, where `allow`
// and `deny` keep their records.
func allowStore(t *testing.T) allow.Store {
	t.Helper()
	return allow.Store{Dir: filepath.Join(os.Getenv("XDG_DATA_HOME"), "hostrunner", "allow")}
}

// runtimeDir returns the runtime directory name of an isolated test (see
// isolate), with symbolic links followed as the allow store keys it: the
// same path for every `up` of the test that gives the name, as for the
// `up`s of one devcontainer config.
func runtimeDir(t *testing.T, name string) string {
	t.Helper()
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		t.Fatal("runtimeDir needs an isolated test")
	}
	real, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(real, name) // short, for the socket path limit
}

// parseUpOf parses the arguments of an `up` for ws with the extra up
// flags, the runtime directory "a" of the test (see runtimeDir) and the
// environment of the process, failing the test on an error.
func parseUpOf(t *testing.T, ws string, extra ...string) launch.Config {
	t.Helper()
	return parseUpIn(t, runtimeDir(t, "a"), ws, extra...)
}

// parseUpIn is parseUpOf with the runtime directory rd.
func parseUpIn(t *testing.T, rd, ws string, extra ...string) launch.Config {
	t.Helper()
	args := append([]string{"--dir", rd, "--workspace", ws, "--container-workspace", "/w"}, extra...)
	cfg, err := parseUp(args, "/bin/hostrunner", os.Getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.launch
}

// upSettings returns the daemon settings an `up` resolves for ws given the
// extra up flags: what an allow has to hold for that `up` to pass.
func upSettings(t *testing.T, ws string, extra ...string) map[string]string {
	t.Helper()
	return parseUpOf(t, ws, extra...).Settings.Daemon()
}

// runAllowCmd runs `hostrunner allow` and `hostrunner deny` (by name) with
// args and returns what they printed.
func runAllowCmd(t *testing.T, name string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut strings.Builder
	err = run(append([]string{name}, args...), &out, &errOut)
	return out.String(), errOut.String(), err
}

// upAllowCheck prepares an `up` for ws with the extra up flags as up
// does, its allow check included, and returns the error, nil when the
// `up` is allowed.
func upAllowCheck(t *testing.T, ws string, extra ...string) error {
	t.Helper()
	return upAllowCheckIn(t, runtimeDir(t, "a"), ws, extra...)
}

// upAllowCheckIn is upAllowCheck with the runtime directory rd.
func upAllowCheckIn(t *testing.T, rd, ws string, extra ...string) error {
	t.Helper()
	_, err := launch.PrepareAllowed(parseUpIn(t, rd, ws, extra...))
	return err
}

// refuseUp prepares an `up` for ws with the extra up flags, as
// upAllowCheck, and fails the test unless it is refused as not allowed:
// the refused `up` records the request that `allow` allows.
func refuseUp(t *testing.T, ws string, extra ...string) {
	t.Helper()
	refuseUpIn(t, runtimeDir(t, "a"), ws, extra...)
}

// refuseUpIn is refuseUp with the runtime directory rd.
func refuseUpIn(t *testing.T, rd, ws string, extra ...string) {
	t.Helper()
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheckIn(t, rd, ws, extra...); !errors.As(err, &notAllowed) {
		t.Fatalf("up %v for %s in %s: got %v, want a *launch.NotAllowedError", extra, ws, rd, err)
	}
}

// storeAllows reports whether the store holds ws with digest and settings
// for the runtime directory "a" of the test (see runtimeDir).
func storeAllows(t *testing.T, ws, digest string, settings map[string]string) bool {
	t.Helper()
	ok, err := allowStore(t).Allowed(allow.Place{Workspace: ws, RuntimeDir: runtimeDir(t, "a")}, allow.Scope{Digest: digest, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// pendingOf returns the pending request of the runtime directory rd of
// ws in store, as PendingAll returns it; ok is false when there is none.
func pendingOf(store allow.Store, ws, rd string) (e allow.Entry, ok bool, err error) {
	requests, err := store.PendingAll(ws)
	if err != nil {
		return allow.Entry{}, false, err
	}
	for _, e := range requests {
		if same, err := allow.SameWorkspace(e.RuntimeDir, rd); err != nil || same {
			return e, err == nil, err
		}
	}
	return allow.Entry{}, false, nil
}

// allow in the current directory, after a refused up, records the digest
// of its rules file with the settings up resolves, and prints what it
// allowed.
func TestAllowRecordsTheCurrentDirectoryAndPrintsWhatItAllowed(t *testing.T) {
	isolate(t)
	dir, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	t.Chdir(dir)
	ws, err := os.Getwd() // the directory as the process names it
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(ws, ".devcontainer")
	refuseUp(t, ws)
	stdout, stderr, err := runAllowCmd(t, "allow")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("stderr %q, want none", stderr)
	}
	if !storeAllows(t, ws, allowRulesDigest, upSettings(t, ws)) {
		t.Error("the rules and the default settings are not allowed")
	}
	want := "hostrunner: allowed " + ws + "\n" +
		"  runtime directory: " + runtimeDir(t, "a") + "\n" +
		"  rules file: " + filepath.Join(dir, "hostrunner-rules.yaml") + "\n" +
		"  digest: " + allowRulesDigest + "\n" +
		"  settings:\n" +
		"    ask_timeout: 10m0s\n" +
		"    grace: 15s\n" +
		"    mount_check: true\n" +
		"    poll_interval: 2s\n" +
		"    request_timeout: 10s\n" +
		"    reread: true\n" +
		"    rules_file: " + filepath.Join(dir, "hostrunner-rules.yaml") + "\n" +
		"    startup_timeout: 30m0s\n"
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

// A workspace given as an argument is the one allowed, wherever the
// command runs and however the path is written.
func TestAllowAllowsTheWorkspaceGiven(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	t.Chdir(t.TempDir())
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	if !storeAllows(t, ws, allowRulesDigest, upSettings(t, ws)) {
		t.Error("the workspace given is not allowed")
	}
}

// A relative workspace is made absolute against the current directory.
func TestAllowMakesARelativeWorkspaceAbsolute(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	t.Chdir(filepath.Dir(ws))
	stdout, _, err := runAllowCmd(t, "allow", filepath.Base(ws))
	if err != nil {
		t.Fatal(err)
	}
	if want := "hostrunner: allowed " + ws + "\n"; !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout %q does not start with %q", stdout, want)
	}
}

// allow without flags allows what the refused up asked for, settings
// flags included, so the same up then passes.
func TestAllowAllowsWhatTheRefusedUpAskedFor(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheck(t, ws, "--grace", "1m"); !errors.As(err, &notAllowed) {
		t.Fatalf("up --grace 1m before the allow: got %v, want a *launch.NotAllowedError", err)
	}
	t.Chdir(ws)
	stdout, stderr, err := runAllowCmd(t, "allow")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("stderr %q, want none", stderr)
	}
	for _, want := range []string{
		"  rules file: " + filepath.Join(dir, "hostrunner-rules.yaml") + "\n" +
			"  digest: " + allowRulesDigest + "\n",
		"    grace: 1m0s (default 15s)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if err := upAllowCheck(t, ws, "--grace", "1m"); err != nil {
		t.Errorf("up --grace 1m after the allow: %v", err)
	}
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Errorf("up without --grace after the allow: got %v, want a *launch.NotAllowedError", err)
	}
}

// The store holds the rules file of an allow with symbolic links followed,
// so allow compares it with the default the same way: a workspace
// reached through a link has the default rules file, which it does not
// mark as one that differs.
func TestAllowDoesNotMarkTheDefaultRulesFileOfAWorkspaceReachedThroughALink(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	realRules, err := filepath.EvalSymlinks(filepath.Join(dir, "hostrunner-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refuseUp(t, link)
	stdout, _, err := runAllowCmd(t, "allow", link)
	if err != nil {
		t.Fatal(err)
	}
	if want := "    rules_file: " + realRules + "\n"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "(default") {
		t.Errorf("stdout marks a setting as differing from its default:\n%s", stdout)
	}
}

// Defaults that cannot be made canonical (here a relative workspace in a
// current directory that was removed, which cannot be made absolute) mark
// no setting, and a last line says why, rather than printing a mark with
// an empty default.
func TestAllowMarksNoDefaultsThatCannotBeResolved(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	e := allow.Entry{Workspace: "ws", Target: allow.NewTarget("/r/rules.yaml", "d1",
		map[string]string{"grace": "1m0s", "rules_file": "/r/rules.yaml"})}
	got := allowMessage("ws", e)
	if strings.Contains(got, "(default") {
		t.Errorf("a setting is marked though no default is known:\n%s", got)
	}
	if want := "    grace: 1m0s\n    rules_file: /r/rules.yaml\n  defaults not shown: "; !strings.Contains(got, want) {
		t.Errorf("message lacks %q:\n%s", want, got)
	}
}

// allow marks each setting it allows that differs from its default, with
// the default, so the human sees which flags of the refused up, given by
// initializeCommand rather than typed, the allow takes; a setting at its
// default is printed bare.
func TestAllowMarksTheSettingsThatDifferFromTheirDefaults(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	other := filepath.Join(dir, "other-rules.yaml")
	if err := os.WriteFile(other, []byte(allowRules), 0o644); err != nil {
		t.Fatal(err)
	}
	var notAllowed *launch.NotAllowedError
	flags := []string{"--no-mount-check", "--no-reread", "--grace", "1m", "--rules-file", other}
	if err := upAllowCheck(t, ws, flags...); !errors.As(err, &notAllowed) {
		t.Fatalf("up %v before the allow: got %v, want a *launch.NotAllowedError", flags, err)
	}
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err != nil {
		t.Fatal(err)
	}
	want := "  settings:\n" +
		"    ask_timeout: 10m0s\n" +
		"    grace: 1m0s (default 15s)\n" +
		"    mount_check: false (default true)\n" +
		"    poll_interval: 2s\n" +
		"    request_timeout: 10s\n" +
		"    reread: false (default true)\n" +
		"    rules_file: " + other + " (default " + filepath.Join(dir, "hostrunner-rules.yaml") + ")\n" +
		"    startup_timeout: 30m0s\n"
	if !strings.HasSuffix(stdout, want) {
		t.Errorf("stdout:\n%s\nwant it to end with:\n%s", stdout, want)
	}
}

// An allow covers the mount check it was recorded with: one of the
// default settings, the check on, does not let an up with the check off
// pass, and the other way round.
func TestAllowCoversTheMountCheck(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheck(t, ws); err != nil {
		t.Fatalf("up after the allow: %v", err)
	}
	if err := upAllowCheck(t, ws, "--no-mount-check"); !errors.As(err, &notAllowed) {
		t.Fatalf("up --no-mount-check after an allow with the check on: got %v, want a *launch.NotAllowedError", err)
	}
	t.Chdir(ws)
	if _, _, err := runAllowCmd(t, "allow"); err != nil {
		t.Fatal(err)
	}
	if err := upAllowCheck(t, ws, "--no-mount-check"); err != nil {
		t.Errorf("up --no-mount-check after its allow: %v", err)
	}
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Errorf("up with the check on after an allow with it off: got %v, want a *launch.NotAllowedError", err)
	}
}

// A rules file that changed after the refused up is not what it asked
// for: allow fails naming the file and both digests, and allows nothing.
func TestAllowFailsWhenTheRulesChangedSinceTheRefusedUp(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	rulesFile := filepath.Join(dir, "hostrunner-rules.yaml")
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Fatalf("up before the allow: got %v, want a *launch.NotAllowedError", err)
	}
	if err := os.WriteFile(rulesFile, []byte(otherRules), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err == nil {
		t.Fatal("allow succeeded, want an error that the rules file changed")
	}
	for _, want := range []string{rulesFile, allowRulesDigest, otherRulesDigest, "hostrunner up"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if stdout != "" {
		t.Errorf("stdout %q, want none", stdout)
	}
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Errorf("up after the failed allow: got %v, want a *launch.NotAllowedError", err)
	}
}

// Invalid rules after the refused up fail as in up, and allow nothing.
func TestAllowRefusesRulesMadeInvalidSinceTheRefusedUp(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Fatalf("up before the allow: got %v, want a *launch.NotAllowedError", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostrunner-rules.yaml"), []byte(invalidRules), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runAllowCmd(t, "allow", ws)
	if err == nil || !strings.HasPrefix(err.Error(), "rules file of the runtime directory "+runtimeDir(t, "a")+": ") {
		t.Fatalf("got %v, want a rules file error naming the runtime directory", err)
	}
	if storeAllows(t, ws, allowRulesDigest, upSettings(t, ws)) {
		t.Error("the rules up asked for were allowed")
	}
}

// allow allows the settings the refused up read from the workspace
// settings file.
func TestAllowHonoursTheWorkspaceSettingsFile(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	if err := os.WriteFile(filepath.Join(dir, "hostrunner.yaml"), []byte("grace: 2m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refuseUp(t, ws)
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "    grace: 2m0s (default 15s)\n") {
		t.Errorf("stdout lacks the grace of the settings file:\n%s", stdout)
	}
	if !storeAllows(t, ws, allowRulesDigest, upSettings(t, ws)) {
		t.Error("the settings of the workspace are not allowed")
	}
}

// allow takes no settings flag: it allows what up asked for; a flag is an
// error and records nothing.
func TestAllowRejectsSettingsFlags(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	_, stderr, err := runAllowCmd(t, "allow", "--grace", "1m", ws)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -grace") {
		t.Fatalf("got %v, want an undefined flag error", err)
	}
	if !strings.Contains(stderr, "Usage of allow") {
		t.Errorf("stderr %q lacks the usage of allow", stderr)
	}
	if _, err := os.Stat(allowStore(t).Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the allow store was created: %v", err)
	}
}

// After an allow an up for the workspace passes its check; before, and
// with other settings, it is refused as not allowed.
func TestUpPassesTheAllowCheckExactlyWhenAllowed(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Fatalf("before the allow: got %v, want a *launch.NotAllowedError", err)
	}
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	if err := upAllowCheck(t, ws); err != nil {
		t.Errorf("after the allow: %v", err)
	}
	if err := upAllowCheck(t, ws, "--grace", "1m"); !errors.As(err, &notAllowed) {
		t.Errorf("up --grace 1m after an allow without it: got %v, want a *launch.NotAllowedError", err)
	}
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	if err := upAllowCheck(t, ws, "--grace", "1m"); err != nil {
		t.Errorf("up --grace 1m after an allow with it: %v", err)
	}
}

// A missing rules file is allowed, as up would serve it, denying everything.
func TestAllowAllowsAMissingRulesFile(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, "")
	refuseUp(t, ws)
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  digest: missing\n") {
		t.Errorf("stdout lacks the digest missing:\n%s", stdout)
	}
	if err := upAllowCheck(t, ws); err != nil {
		t.Errorf("up after the allow: %v", err)
	}
}

// Without a pending request nobody asked for an allow, so allow allows
// nothing, whatever the rules file holds: it fails naming the workspace
// and telling to start the container with hostrunner up, prints nothing
// and records nothing.
func TestAllowWithoutAPendingRequestFails(t *testing.T) {
	for name, rules := range map[string]string{"valid rules": allowRules, "invalid rules": invalidRules} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			ws, _ := devcontainerWith(t, rules, "hostrunner-rules.yaml")
			stdout, stderr, err := runAllowCmd(t, "allow", ws)
			if err == nil {
				t.Fatal("allow succeeded, want an error that nothing asked for an allow")
			}
			for _, want := range []string{ws, "hostrunner up"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if stdout != "" || stderr != "" {
				t.Errorf("printed %q on stdout and %q on stderr, want nothing", stdout, stderr)
			}
			if _, err := os.Stat(allowStore(t).Dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the allow store was created: %v", err)
			}
		})
	}
}

// allow takes the request it allows: a second allow finds none and fails,
// naming hostrunner up, while the first allow stays.
func TestASecondAllowFindsNoRequest(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runAllowCmd(t, "allow", ws); err == nil || !strings.Contains(err.Error(), "hostrunner up") {
		t.Errorf("second allow: got %v, want an error naming hostrunner up", err)
	}
	if err := upAllowCheck(t, ws); err != nil {
		t.Errorf("up after the second allow: %v", err)
	}
}

// A second allow after the rules changed replaces the first.
func TestAllowAfterTheRulesChangedReplacesTheEarlierAllow(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostrunner-rules.yaml"), []byte(otherRules), 0o644); err != nil {
		t.Fatal(err)
	}
	refuseUp(t, ws)
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, allowRulesDigest) {
		t.Errorf("stdout names the old digest:\n%s", stdout)
	}
	settings := upSettings(t, ws)
	if storeAllows(t, ws, allowRulesDigest, settings) {
		t.Error("the old rules are still allowed")
	}
	if err := upAllowCheck(t, ws); err != nil {
		t.Errorf("up with the new rules: %v", err)
	}
}

// A directory that does not exist is an error naming it, and records
// nothing; so is a file.
func TestAllowFailsOnAWorkspaceThatIsNotADirectory(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{missing, file} {
		_, _, err := runAllowCmd(t, "allow", p)
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("allow %s: got %v, want an error naming it", p, err)
		}
	}
	if _, err := os.Stat(allowStore(t).Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the allow store was created: %v", err)
	}
}

// More than one workspace is a usage error.
func TestAllowAndDenyRejectTwoWorkspaces(t *testing.T) {
	isolate(t)
	for _, name := range []string{"allow", "deny"} {
		_, _, err := runAllowCmd(t, name, "/a", "/b")
		if err == nil || !strings.Contains(err.Error(), `unexpected argument "/b"`) || !strings.Contains(err.Error(), "usage: hostrunner "+name) {
			t.Errorf("%s: got %v, want an unexpected argument error with the usage", name, err)
		}
	}
}

// deny takes no flag but -h.
func TestDenyRejectsFlags(t *testing.T) {
	isolate(t)
	if _, _, err := runAllowCmd(t, "deny", "--grace", "1m"); err == nil {
		t.Error("deny --grace 1m: got no error")
	}
	if _, _, err := runAllowCmd(t, "deny", "-h"); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("deny -h: got %v, want flag.ErrHelp", err)
	}
}

// deny removes the allow, so up is refused again; denying again says there
// is none and is no failure.
func TestDenyRemovesTheAllow(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runAllowCmd(t, "deny", ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hostrunner: denied " + ws + " for " + runtimeDir(t, "a") + "\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheck(t, ws); !errors.As(err, &notAllowed) {
		t.Errorf("up after deny: got %v, want a *launch.NotAllowedError", err)
	}
	stdout, _, err = runAllowCmd(t, "deny", ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hostrunner: " + ws + " has no allow\n"; stdout != want {
		t.Errorf("second deny: stdout %q, want %q", stdout, want)
	}
}

// deny defaults to the current directory and needs no existing workspace:
// the allow of a removed workspace can still be denied.
func TestDenyAcceptsTheCurrentDirectoryAndAMissingWorkspace(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	refuseUp(t, ws)
	t.Chdir(ws)
	if _, _, err := runAllowCmd(t, "allow"); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if stdout, _, err := runAllowCmd(t, "deny"); err != nil || stdout != "hostrunner: denied "+wd+" for "+runtimeDir(t, "a")+"\n" {
		t.Errorf("deny: got %q and %v", stdout, err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if stdout, _, err := runAllowCmd(t, "deny", gone); err != nil || stdout != "hostrunner: "+gone+" has no allow\n" {
		t.Errorf("deny of a missing workspace: got %q and %v", stdout, err)
	}
}

// Without XDG_DATA_HOME and HOME there is no store, and both commands say so.
func TestAllowAndDenyFailWithoutAStoreLocation(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_DATA_HOME", "")
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	for _, args := range [][]string{{"allow", ws}, {"deny", ws}} {
		if _, _, err := runAllowCmd(t, args[0], args[1]); err == nil || !strings.Contains(err.Error(), "XDG_DATA_HOME") {
			t.Errorf("%s: got %v, want an error naming XDG_DATA_HOME", args[0], err)
		}
	}
}

// linkedWorkspace returns a workspace with a valid rules file, by its real
// path and by a symbolic link to it.
func linkedWorkspace(t *testing.T) (real, link string) {
	t.Helper()
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	return real, link
}

// allow run in a directory reached through a link allows the workspace an
// up given its real path checks: the rules file is the same file.
func TestAllowThroughALinkAllowsTheUpOfTheRealPath(t *testing.T) {
	isolate(t)
	real, link := linkedWorkspace(t)
	refuseUp(t, real)
	t.Chdir(link)
	t.Setenv("PWD", link) // os.Getwd names the directory through the link
	if wd, err := os.Getwd(); err != nil || wd != link {
		t.Fatalf("Getwd = %q, %v; want %q", wd, err, link)
	}
	if _, _, err := runAllowCmd(t, "allow"); err != nil {
		t.Fatal(err)
	}
	if err := upAllowCheck(t, real); err != nil {
		t.Errorf("up --workspace %s after the allow through the link: %v", real, err)
	}
}

// allow of the real path allows the workspace an up given a link to it
// checks.
func TestAllowOfTheRealPathAllowsTheUpThroughALink(t *testing.T) {
	isolate(t)
	real, link := linkedWorkspace(t)
	refuseUp(t, link)
	if _, _, err := runAllowCmd(t, "allow", real); err != nil {
		t.Fatal(err)
	}
	if err := upAllowCheck(t, link); err != nil {
		t.Errorf("up --workspace %s after the allow of %s: %v", link, real, err)
	}
}

// allow allows the pending request of every runtime directory of the
// workspace at once, each with the settings its refused up resolved, and
// prints one block per allow, sorted by runtime directory, each naming its
// runtime directory; then the up of each passes and allowing one did not
// undo the other.
func TestAllowAllowsTheRequestOfEveryRuntimeDir(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	a, b := runtimeDir(t, "a"), runtimeDir(t, "b")
	refuseUpIn(t, b, ws, "--grace", "1m")
	refuseUpIn(t, a, ws)
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.SplitAfter(stdout, "    startup_timeout: 30m0s\n")
	if len(blocks) != 3 || blocks[2] != "" {
		t.Fatalf("stdout holds %d blocks, want 2:\n%s", len(blocks)-1, stdout)
	}
	for i, want := range []struct{ dir, grace string }{{a, "    grace: 15s\n"}, {b, "    grace: 1m0s (default 15s)\n"}} {
		head := "hostrunner: allowed " + ws + "\n  runtime directory: " + want.dir + "\n"
		if !strings.HasPrefix(blocks[i], head) || !strings.Contains(blocks[i], want.grace) {
			t.Errorf("block %d lacks %q and %q:\n%s", i, head, want.grace, blocks[i])
		}
	}
	if err := upAllowCheckIn(t, a, ws); err != nil {
		t.Errorf("up in A after the allow: %v", err)
	}
	if err := upAllowCheckIn(t, b, ws, "--grace", "1m"); err != nil {
		t.Errorf("up --grace 1m in B after the allow: %v", err)
	}
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheckIn(t, a, ws, "--grace", "1m"); !errors.As(err, &notAllowed) {
		t.Errorf("up --grace 1m in A: got %v, want a *launch.NotAllowedError", err)
	}
}

// When the rules file changed since the request of one runtime directory,
// allow allows none of the requests, names that one with its runtime
// directory and both digests, and tells to run hostrunner up again.
func TestAllowWithOneRequestOutOfDateAllowsNothing(t *testing.T) {
	isolate(t)
	ws, dir := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	a, b := runtimeDir(t, "a"), runtimeDir(t, "b")
	refuseUpIn(t, a, ws)
	if err := os.WriteFile(filepath.Join(dir, "hostrunner-rules.yaml"), []byte(otherRules), 0o644); err != nil {
		t.Fatal(err)
	}
	refuseUpIn(t, b, ws)
	stdout, _, err := runAllowCmd(t, "allow", ws)
	if err == nil {
		t.Fatal("allow succeeded, want an error that a request is out of date")
	}
	for _, want := range []string{a, allowRulesDigest, otherRulesDigest, "run hostrunner up again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), b) {
		t.Errorf("error %q names B, whose request is up to date", err)
	}
	if stdout != "" {
		t.Errorf("stdout %q, want none", stdout)
	}
	var notAllowed *launch.NotAllowedError
	for _, rd := range []string{a, b} {
		if err := upAllowCheckIn(t, rd, ws); !errors.As(err, &notAllowed) {
			t.Errorf("up in %s after the failed allow: got %v, want a *launch.NotAllowedError", rd, err)
		}
	}
}

// deny removes the allows of every runtime directory of the workspace and
// prints each, sorted by runtime directory; then the up of each is refused.
func TestDenyRemovesTheAllowOfEveryRuntimeDir(t *testing.T) {
	isolate(t)
	ws, _ := devcontainerWith(t, allowRules, "hostrunner-rules.yaml")
	a, b := runtimeDir(t, "a"), runtimeDir(t, "b")
	refuseUpIn(t, a, ws)
	refuseUpIn(t, b, ws, "--grace", "1m")
	if _, _, err := runAllowCmd(t, "allow", ws); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runAllowCmd(t, "deny", ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hostrunner: denied " + ws + " for " + a + "\nhostrunner: denied " + ws + " for " + b + "\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	var notAllowed *launch.NotAllowedError
	if err := upAllowCheckIn(t, a, ws); !errors.As(err, &notAllowed) {
		t.Errorf("up in A after deny: got %v, want a *launch.NotAllowedError", err)
	}
	if err := upAllowCheckIn(t, b, ws, "--grace", "1m"); !errors.As(err, &notAllowed) {
		t.Errorf("up in B after deny: got %v, want a *launch.NotAllowedError", err)
	}
}
