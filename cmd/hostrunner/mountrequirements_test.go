package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/allow/allowtest"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

// gitRule is the rule of gitMountConfig for `git`: it allows `git init -q
// <dir>`, which leaves the directory on the host for a test to look for,
// refuses other flags, as `--bare`, approvably, and requires the mounts a
// devcontainer makes to keep the container out of the repository's config
// and hooks.
const gitRule = "  - command: git\n    flags:\n      allow: [-q]\n" +
	"    mounts:\n      .git: mounted\n      .git/config: read-only\n      .git/hooks: read-only\n"

// The reasons a refusal by a mount requirement of the git rule gives,
// right after the container path.
const (
	notMountedReason  = " as a mount point of its own, which rule \"git\" requires: the command is refused until it is mounted"
	notReadOnlyReason = ", which rule \"git\" requires to be read-only: the command is refused until it is mounted read-only"
)

// gitMountConfig is liveMountConfig whose rules also hold gitRule, over a
// workspace with a .git directory holding a config file and a hooks
// directory.
func gitMountConfig(t *testing.T) serveConfig {
	t.Helper()
	cfg := liveMountConfig(t)
	if err := os.WriteFile(cfg.RulesFile, []byte(touchRules+gitRule), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.workspace, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.workspace, ".git", "config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// gitDirMount mounts the .git directory of cfg's workspace writable over
// the workspace mount, a mount point of its own.
func gitDirMount(cfg serveConfig) watch.Mount {
	return watch.Mount{Source: filepath.Join(cfg.workspace, ".git"), Destination: cfg.containerWorkspace + "/.git"}
}

// readOnlyGit mounts name, below the .git directory of cfg's workspace,
// read-only at its place in the container.
func readOnlyGit(cfg serveConfig, name string) watch.Mount {
	return watch.Mount{
		Source:      filepath.Join(cfg.workspace, ".git", name),
		Destination: cfg.containerWorkspace + "/.git/" + name,
		ReadOnly:    true,
	}
}

// gitMounts are the mounts of a container that meets the git rule's
// requirements, besides the workspace and its read-only .devcontainer:
// .git a mount of its own, its config and hooks read-only.
func gitMounts(cfg serveConfig) []watch.Mount {
	return []watch.Mount{
		workspaceMount(cfg), readOnlyDevcontainer(cfg),
		gitDirMount(cfg), readOnlyGit(cfg, "config"), readOnlyGit(cfg, "hooks"),
	}
}

// requireRefusedByRequirement fails the test unless code and stderr are
// the refusal of a command by a mount requirement of the git rule: 126,
// naming the container path that lies suffix below the container
// workspace and reason, not the host workspace, and how to turn the check
// off.
func requireRefusedByRequirement(t *testing.T, cfg serveConfig, code int, stderr, suffix, reason string) {
	t.Helper()
	requireRefusedAt(t, cfg, code, stderr, cfg.containerWorkspace+suffix, reason)
}

// requireRefusedAt fails the test unless code and stderr are the refusal of
// a command by a mount requirement of the git rule: 126, naming the
// container path want and reason, not the host workspace, and how to turn
// the check off.
func requireRefusedAt(t *testing.T, cfg serveConfig, code int, stderr, want, reason string) {
	t.Helper()
	if code != protocol.ExitRejected || !strings.Contains(stderr, want+reason) {
		t.Errorf("got %d, %q; want %d naming %s and %q", code, stderr, protocol.ExitRejected, want, reason)
	}
	if strings.Contains(stderr, cfg.workspace) {
		t.Errorf("the refusal %q names the host workspace %s", stderr, cfg.workspace)
	}
	if !strings.Contains(stderr, "a human on the host can turn the mount check off: mount_check: false in the host settings file, HOSTRUNNER_MOUNT_CHECK=false or --no-mount-check") {
		t.Errorf("the refusal %q does not say how to turn the check off", stderr)
	}
}

// A container whose live mount table has .git as a mount of its own,
// writable or read-only, and its config and hooks read-only, runs `git`.
func TestServeWatchRunsAGitCommandWhenTheGitMountsAreThere(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool // of the .git mount
	}{{".git writable", false}, {".git read-only", true}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gitMountConfig(t)
			mounts := gitMounts(cfg)
			mounts[2].ReadOnly = tc.readOnly
			rt := &watchtest.Runtime{}
			armedContainer(t, cfg, rt, mounts...)
			if code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker"); code != 0 {
				t.Fatalf("got %d, %q; want 0", code, stderr)
			}
			requireMarker(t, cfg, "marker", true)
		})
	}
}

// A container that mounts all of .git read-only, with no mount of its
// config or hooks, runs `git`: the read-only .git mount keeps both out of
// reach and is a mount point of its own.
func TestServeWatchRunsAGitCommandWhenAllOfGitIsReadOnly(t *testing.T) {
	cfg := gitMountConfig(t)
	git := gitDirMount(cfg)
	git.ReadOnly = true
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg), git)
	if code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want 0", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
}

// A container without the git mounts runs the command of another rule,
// one that requires no mounts, while `git` is refused there.
func TestServeWatchRunsACommandOfAnotherRuleWithoutTheGitMounts(t *testing.T) {
	cfg := gitMountConfig(t)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg))
	if code, stderr := runServedOutput(cfg, "touch", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want 0", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
	code, stderr := runServedOutput(cfg, "git", "init", "-q", "repo")
	requireRefusedByRequirement(t, cfg, code, stderr, "/.git", notMountedReason)
}

// gitAmendRule is a longer rule than gitRule, with no mounts of its own,
// that allows `git commit amend` whatever follows.
const gitAmendRule = "  - command: git commit amend\n    args: any\n"

// gitAmendConfig is gitMountConfig whose rules also hold gitAmendRule.
func gitAmendConfig(t *testing.T) serveConfig {
	t.Helper()
	cfg := gitMountConfig(t)
	if err := os.WriteFile(cfg.RulesFile, []byte(touchRules+gitRule+gitAmendRule), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// `git commit amend`, which its own rule allows, is refused with 126 when
// the requirements of the shorter `git` rule fail, the refusal naming that
// rule and not the one that selects the command: a longer rule inherits
// the requirements of the shorter ones. A dry run is refused the same.
func TestServeWatchRefusesALongerRulesCommandWhenAShorterRulesMountRequirementFails(t *testing.T) {
	for _, argv := range [][]string{
		{"git", "commit", "amend"},
		{"--dry-run", "git", "commit", "amend"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			cfg := gitAmendConfig(t)
			rt := &watchtest.Runtime{}
			armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyGit(cfg, "config"), readOnlyGit(cfg, "hooks"))
			code, stderr := runServedOutput(cfg, argv...)
			requireRefusedByRequirement(t, cfg, code, stderr, "/.git", notMountedReason)
		})
	}
}

// A dry run of `git commit amend` passes in a container that meets the
// requirements of the `git` rule, which the longer rule inherits.
func TestServeWatchDryRunOfALongerRulesCommandPassesWhenTheShorterRulesMountsAreThere(t *testing.T) {
	cfg := gitAmendConfig(t)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, gitMounts(cfg)...)
	if code, stderr := runServedOutput(cfg, "--dry-run", "git", "commit", "amend"); code != 0 {
		t.Fatalf("got %d, %q; want 0", code, stderr)
	}
}

// A dry run of `git` reports the refusal a run would get, for either
// requirement that fails.
func TestServeWatchDryRunReportsTheRefusalOfAMountRequirement(t *testing.T) {
	tests := []struct {
		name   string
		mounts func(cfg serveConfig) []watch.Mount
		suffix string // of the container path the refusal names
		reason string
	}{
		{
			name: ".git not a mount of its own",
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyGit(cfg, "config"), readOnlyGit(cfg, "hooks")}
			},
			suffix: "/.git",
			reason: notMountedReason,
		},
		{
			name:   ".git/hooks writable",
			mounts: func(cfg serveConfig) []watch.Mount { return gitMounts(cfg)[:4] },
			suffix: "/.git/hooks",
			reason: notReadOnlyReason,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := gitMountConfig(t)
			rt := &watchtest.Runtime{}
			armedContainer(t, cfg, rt, tt.mounts(cfg)...)
			code, stderr := runServedOutput(cfg, "--dry-run", "git", "init", "-q", "marker")
			requireRefusedByRequirement(t, cfg, code, stderr, tt.suffix, tt.reason)
			requireMarker(t, cfg, "marker", false)
		})
	}
}

// gitRequirementNotice is what the approval session prints when a mount
// requirement of the git rule fails.
const gitRequirementNotice = "the mount check failed, so the daemon refuses the commands of rule \"git\": "

// An --ask of an argv that starts with git's command, refused by the git
// rule but approvable, is refused with 126 before it is queued when a
// requirement of the rule fails: the approval session is shown no
// request, only the notice of the failed requirement.
func TestServeWatchRefusesAnAskBeforeQueuingItWhenAMountRequirementFails(t *testing.T) {
	cfg := gitMountConfig(t)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyGit(cfg, "config"), readOnlyGit(cfg, "hooks"))
	s := startHumanSession(t, cfg)
	got := awaitServed(t, askServed(cfg, "git", "init", "--bare", "marker"))
	requireRefusedByRequirement(t, cfg, got.code, got.stderr, "/.git", notMountedReason)
	s.waitFor(t, gitRequirementNotice)
	if out := s.out.String(); strings.Contains(out, "── request") {
		t.Fatalf("the session was shown a request:\n%s", out)
	}
	requireMarker(t, cfg, "marker", false)
}

// An --ask of git whose .git/config mount vanishes while the human
// decides is refused with 126 once the human approves it: the check made
// after the answer, right before the approved run starts, finds the
// config writable, and git does not run.
func TestServeWatchRefusesAnApprovedRunWhenAMountRequirementFailsWhileTheHumanDecides(t *testing.T) {
	cfg := gitMountConfig(t)
	mounts := gitMounts(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, mounts...)
	s := startHumanSession(t, cfg)
	ch := askServed(cfg, "git", "init", "--bare", "marker")
	s.waitFor(t, answerPrompt)
	rt.SetMountTable("c", mountPoints(mounts[0], mounts[1], mounts[2], mounts[4])...)
	s.answer(t, "o")
	got := awaitServed(t, ch)
	requireRefusedByRequirement(t, cfg, got.code, got.stderr, "/.git/config", notReadOnlyReason)
	requireMarker(t, cfg, "marker", false)
}

// A repeat of git that a standing approval lets through, although the git
// rule refuses it, is checked against the rule's requirements: once the
// .git/config mount vanishes, the repeat, with or without --ask, is
// refused with 126 and does not run, and it runs again once the mount is
// back, the approval standing meanwhile.
func TestServeWatchChecksTheMountRequirementsOfStandingApprovalRepeats(t *testing.T) {
	cfg := gitMountConfig(t)
	mounts := gitMounts(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, mounts...)
	argv := []string{"git", "init", "--bare", "marker"}
	if code, stderr := runServedOutput(cfg, argv...); code != protocol.ExitRejected || !strings.Contains(stderr, "--bare") {
		t.Fatalf("got %d, %q before the approval; want %d, refused by the rule", code, stderr, protocol.ExitRejected)
	}
	s := startHumanSession(t, cfg)
	ch := askServed(cfg, argv...)
	s.waitFor(t, answerPrompt)
	s.answer(t, "d")
	if got := awaitServed(t, ch); got.code != 0 {
		t.Fatalf("got %d, %q; want the approved command to run", got.code, got.stderr)
	}
	s.waitFor(t, "approved as standing (until the daemon stops) · run ")
	s.waitFor(t, " ──\n")
	requireMarker(t, cfg, "marker", true)
	if err := os.RemoveAll(filepath.Join(cfg.workspace, "marker")); err != nil {
		t.Fatal(err)
	}

	rt.SetMountTable("c", mountPoints(mounts[0], mounts[1], mounts[2], mounts[4])...)
	for _, repeat := range [][]string{argv, append([]string{"--ask"}, argv...)} {
		code, stderr := runServedOutput(cfg, repeat...)
		requireRefusedByRequirement(t, cfg, code, stderr, "/.git/config", notReadOnlyReason)
	}
	requireMarker(t, cfg, "marker", false)

	rt.SetMountTable("c", mountPoints(mounts...)...)
	if code, stderr := runServedOutput(cfg, argv...); code != 0 {
		t.Fatalf("got %d, %q; want the repeat to run once the mount is back", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
}

// Every armed container is checked against the git rule's requirements:
// one that mounts the workspace without the git mounts refuses `git`
// whatever the others mount, while one that does not mount the workspace
// at all is never the reason.
func TestServeWatchRefusesAGitCommandWhileAnyArmedContainerLacksAGitMount(t *testing.T) {
	cfg := gitMountConfig(t)
	var log logBuffer
	rt := &watchtest.Runtime{}
	startLoggingServe(t, cfg, slog.New(slog.NewTextHandler(&log, nil)), rt)
	rt.SetMounts("app", gitMounts(cfg)...)
	rt.Start("app")
	if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
		t.Fatalf("got %d, %q; want 0 for the first container", code, stderr)
	}
	rt.SetMounts("db", watch.Mount{Source: t.TempDir(), Destination: "/var/lib/data"})
	rt.Start("db")
	waitForLogCount(t, &log, "a container joined the armed containers", 1)
	if code, stderr := runServedOutput(cfg, "git", "init", "-q", "before"); code != 0 {
		t.Fatalf("got %d, %q; want 0 while no container lacks a git mount", code, stderr)
	}
	rt.SetMounts("web", workspaceMount(cfg), readOnlyDevcontainer(cfg))
	rt.Start("web")
	waitForLogCount(t, &log, "a container joined the armed containers", 2)
	code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker")
	requireRefusedByRequirement(t, cfg, code, stderr, "/.git", notMountedReason)
	requireMarker(t, cfg, "marker", false)
}

// With the mount check off (mount_check false), the git rule's
// requirements are not checked either: a dry run and a run of `git` go
// ahead in a container without the git mounts.
func TestServeWatchWithTheMountCheckOffRunsAGitCommandWithoutTheGitMounts(t *testing.T) {
	cfg := gitMountConfig(t)
	cfg.MountCheck = false
	rt := &watchtest.Runtime{}
	startServe(t, cfg, rt)
	rt.SetMounts("c", workspaceMount(cfg), readOnlyDevcontainer(cfg))
	rt.Start("c")
	for _, argv := range [][]string{{"--dry-run", "git", "init", "-q", "marker"}, {"git", "init", "-q", "marker"}} {
		if code, stderr := runServedOutput(cfg, argv...); code != 0 {
			t.Fatalf("%q: got %d, %q; want 0", argv, code, stderr)
		}
	}
	requireMarker(t, cfg, "marker", true)
}

// Adding `mounts:` to a rule changes the rules file's digest, so serve
// rereads it only once it is allowed, as any rules change: until then
// `git` runs in a container without the git mounts under the rules
// served, and once the edit is allowed it is refused there.
func TestServeServesAChangeOfMountsOnlyOnceAllowed(t *testing.T) {
	cfg := gitMountConfig(t)
	withoutMounts := touchRules + "  - command: git\n    flags:\n      allow: [-q]\n"
	if err := os.WriteFile(cfg.RulesFile, []byte(withoutMounts), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := allow.Open(envOf(map[string]string{"XDG_DATA_HOME": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	cfg.allows = store
	var log logBuffer
	rt := &watchtest.Runtime{}
	startLoggingServe(t, cfg, slog.New(slog.NewTextHandler(&log, nil)), rt)
	rt.SetMounts("c", workspaceMount(cfg), readOnlyDevcontainer(cfg))
	rt.Start("c")
	if code, stderr := runOnceChecked(t, cfg, "git", "init", "-q", "before"); code != 0 {
		t.Fatalf("got %d, %q; want 0 under the rules without mounts", code, stderr)
	}
	withMounts := touchRules + gitRule
	if err := os.WriteFile(cfg.RulesFile, []byte(withMounts), 0o600); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, "rules changed but not allowed")
	if digest := rules.ContentDigest([]byte(withMounts)); !strings.Contains(log.String(), "digest="+digest) {
		t.Errorf("the log does not name the digest %s of the edit:\n%s", digest, log.String())
	}
	if code, stderr := runServedOutput(cfg, "git", "init", "-q", "unallowed"); code != 0 {
		t.Fatalf("got %d, %q; want 0 while the edit is not allowed", code, stderr)
	}
	requireMarker(t, cfg, "unallowed", true)
	allowtest.Allow(t, store, servePlace(cfg), allow.NewTarget(cfg.RulesFile, rules.ContentDigest([]byte(withMounts)), cfg.Settings.AllowedSettings()))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(cfg.PollInterval) {
		code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker")
		if code == protocol.ExitRejected {
			requireRefusedByRequirement(t, cfg, code, stderr, "/.git", notMountedReason)
			break
		}
		if err := os.RemoveAll(filepath.Join(cfg.workspace, "marker")); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d, %q; want the allowed edit served, refusing git", code, stderr)
		}
	}
	requireMarker(t, cfg, "marker", false)
}

// The approval session is told once when a requirement of the git rule
// fails, in a notice naming the rule and the failed requirement: not again
// for the next request the same failure refuses.
func TestServeWatchTellsTheApprovalSessionOnceOfAFailedMountRequirement(t *testing.T) {
	cfg := gitMountConfig(t)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyGit(cfg, "config"), readOnlyGit(cfg, "hooks"))
	s := startHumanSession(t, cfg)
	for range 2 {
		if code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker"); code != protocol.ExitRejected {
			t.Fatalf("got %d, %q; want %d", code, stderr, protocol.ExitRejected)
		}
	}
	// A failure of another kind ends the run: notices come in order, so
	// once it is shown, so is every notice before it.
	rt.FailMountTable("c", true)
	if code, stderr := runServedOutput(cfg, "true"); code != protocol.ExitRejected {
		t.Fatalf("got %d, %q; want %d while the table cannot be read", code, stderr, protocol.ExitRejected)
	}
	s.waitFor(t, mountCheckNotice+"container c: cannot tell whether")
	out := s.out.String()
	want := gitRequirementNotice + "container c: .git is not a mount of its own: nothing is mounted at " + cfg.containerWorkspace + "/.git"
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("the session was told %d times of the failed requirement %q, want once:\n%s", n, want, out)
	}
	if n := strings.Count(out, "the mount check failed"); n != 2 {
		t.Errorf("the session was told of %d failed checks, want 2:\n%s", n, out)
	}
}

// A container whose .git is no mount point of its own, its config and
// hooks mounted read-only all the same, is refused `git` with 126, naming
// the container path, and git does not run.
func TestServeWatchRefusesAGitCommandWhenGitIsNotAMountOfItsOwn(t *testing.T) {
	cfg := gitMountConfig(t)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyGit(cfg, "config"), readOnlyGit(cfg, "hooks"))
	code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker")
	requireRefusedByRequirement(t, cfg, code, stderr, "/.git", notMountedReason)
	requireMarker(t, cfg, "marker", false)
}

// A container that can write .git/config, mounted read-write over its own
// .git mount, is refused `git` with 126, naming the container path, and
// git does not run.
func TestServeWatchRefusesAGitCommandWhenTheGitConfigIsWritable(t *testing.T) {
	cfg := gitMountConfig(t)
	mounts := gitMounts(cfg)
	mounts[3] = writable(mounts[3])
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, mounts...)
	code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker")
	requireRefusedByRequirement(t, cfg, code, stderr, "/.git/config", notReadOnlyReason)
	requireMarker(t, cfg, "marker", false)
}

// The read-only mount of .git/config vanishing from the live mount table
// between two requests, as when a host command replaces the file, refuses
// the second with 126: the config is writable through the .git mount,
// although the inspected mounts still list it read-only.
func TestServeWatchRefusesAGitCommandOnceTheGitConfigMountVanished(t *testing.T) {
	cfg := gitMountConfig(t)
	mounts := gitMounts(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, mounts...)
	if code, stderr := runServedOutput(cfg, "git", "init", "-q", "before"); code != 0 {
		t.Fatalf("got %d, %q; want 0 before the mount vanished", code, stderr)
	}
	rt.SetMountTable("c", mountPoints(mounts[0], mounts[1], mounts[2], mounts[4])...)
	code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker")
	requireRefusedByRequirement(t, cfg, code, stderr, "/.git/config", notReadOnlyReason)
	requireMarker(t, cfg, "marker", false)
}

// parentMounts are the mounts of a container that mounts the parent of
// cfg's workspace writable at /p instead of the workspace itself, with the
// workspace's .devcontainer read-only over it, and the container path of
// the workspace's .git below it.
func parentMounts(cfg serveConfig) (mounts []watch.Mount, gitPath string) {
	ws := "/p/" + filepath.Base(cfg.workspace)
	return []watch.Mount{
		{Source: filepath.Dir(cfg.workspace), Destination: "/p"},
		{Source: filepath.Join(cfg.workspace, ".devcontainer"), Destination: ws + "/.devcontainer", ReadOnly: true},
	}, ws + "/.git"
}

// A container that mounts the parent of the workspace writable, with no
// mount of .git onto itself, is refused `git` with 126, naming the
// container path under the parent mount, and git does not run: the
// container could move .git away.
func TestServeWatchRefusesAGitCommandWhenTheWorkspaceParentIsMountedWithoutGit(t *testing.T) {
	cfg := gitMountConfig(t)
	mounts, gitPath := parentMounts(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, mounts...)
	code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker")
	requireRefusedAt(t, cfg, code, stderr, gitPath, notMountedReason)
	requireMarker(t, cfg, "marker", false)
}

// A container that mounts the parent of the workspace writable runs `git`
// when .git is a mount point of its own under the parent mount.
func TestServeWatchRunsAGitCommandWhenGitIsMountedUnderTheWorkspaceParent(t *testing.T) {
	cfg := gitMountConfig(t)
	mounts, gitPath := parentMounts(cfg)
	git := watch.Mount{Source: filepath.Join(cfg.workspace, ".git"), Destination: gitPath, ReadOnly: true}
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, append(mounts, git)...)
	if code, stderr := runServedOutput(cfg, "git", "init", "-q", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want 0", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
}
