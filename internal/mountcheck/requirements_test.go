package mountcheck_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/watch"
)

// requirements is one rules.MountRequirement of mode for each of paths.
func requirements(mode rules.MountMode, paths ...string) []rules.MountRequirement {
	var out []rules.MountRequirement
	for _, p := range paths {
		out = append(out, rules.MountRequirement{Path: p, Mode: mode})
	}
	return out
}

// wantNotMounted fails the test unless err is a *NotMountedError equal to
// want whose message names the container, the path and the container path.
func wantNotMounted(t *testing.T, err error, want mountcheck.NotMountedError) {
	t.Helper()
	got, ok := errors.AsType[*mountcheck.NotMountedError](err)
	if !ok {
		t.Fatalf("CheckRequirements = %v, want a *NotMountedError", err)
	}
	if *got != want {
		t.Fatalf("CheckRequirements = %+v, want %+v", *got, want)
	}
	for _, s := range []string{want.Container, want.Path, want.ContainerPath} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("message %q does not name %q", err.Error(), s)
		}
	}
}

// wantReadOnlyRequirement fails the test unless err is a
// *ReadOnlyRequirementError equal to want whose message names the container,
// the path and the container path, and which is not a *WritableError.
func wantReadOnlyRequirement(t *testing.T, err error, want mountcheck.ReadOnlyRequirementError) {
	t.Helper()
	got, ok := errors.AsType[*mountcheck.ReadOnlyRequirementError](err)
	if !ok {
		t.Fatalf("CheckRequirements = %v, want a *ReadOnlyRequirementError", err)
	}
	if *got != want {
		t.Fatalf("CheckRequirements = %+v, want %+v", *got, want)
	}
	for _, s := range []string{want.Container, want.Path, want.ContainerPath} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("message %q does not name %q", err.Error(), s)
		}
	}
	if _, ok := errors.AsType[*mountcheck.WritableError](err); ok {
		t.Errorf("CheckRequirements = %v, a requirement failure must not be a *WritableError", err)
	}
}

func TestCheckRequirementsMountedFailsWhenThePointIsMissingFromTheLiveTable(t *testing.T) {
	tr := newTree(t)
	c := withLive(
		container("c1", rw(tr.ws, "/workspaces/ws"), rw(tr.dc, "/workspaces/ws/.devcontainer")),
		live(point("/workspaces/ws")),
	)
	err := mountcheck.CheckRequirements([]watch.Container{c}, tr.ws, requirements(rules.MountMounted, ".devcontainer"))
	wantNotMounted(t, err, mountcheck.NotMountedError{mountcheck.RequirementFailure{
		Container: "c1", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer",
	}})
}

func TestCheckRequirementsMountedPassesWithAMountPointOfItsOwn(t *testing.T) {
	tr := newTree(t)
	tests := []struct {
		name  string
		mount watch.Mount
		point watch.MountPoint
	}{
		{"writable", rw(tr.dc, "/workspaces/ws/.devcontainer"), point("/workspaces/ws/.devcontainer")},
		{"read-only", ro(tr.dc, "/workspaces/ws/.devcontainer"), roPoint("/workspaces/ws/.devcontainer")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := withLive(
				container("c1", rw(tr.ws, "/workspaces/ws"), tt.mount),
				live(point("/workspaces/ws"), tt.point),
			)
			err := mountcheck.CheckRequirements([]watch.Container{c}, tr.ws, requirements(rules.MountMounted, ".devcontainer"))
			if err != nil {
				t.Fatalf("CheckRequirements = %v, want nil", err)
			}
		})
	}
}

// A workspace mounted twice has to hold the required path as a mount of its
// own at both places.
func TestCheckRequirementsMountedChecksEveryWorkspaceMount(t *testing.T) {
	tr := newTree(t)
	reqs := requirements(rules.MountMounted, ".devcontainer")
	mounts := []watch.Mount{
		rw(tr.ws, "/workspaces/ws"), rw(tr.dc, "/workspaces/ws/.devcontainer"),
		rw(tr.ws, "/other/ws"), rw(tr.dc, "/other/ws/.devcontainer"),
	}
	both := withLive(container("c1", mounts...), live(
		point("/workspaces/ws"), point("/workspaces/ws/.devcontainer"),
		point("/other/ws"), point("/other/ws/.devcontainer"),
	))
	if err := mountcheck.CheckRequirements([]watch.Container{both}, tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}

	second := withLive(container("c2", mounts...), live(
		point("/workspaces/ws"), point("/workspaces/ws/.devcontainer"),
		point("/other/ws"),
	))
	wantNotMounted(t, mountcheck.CheckRequirements([]watch.Container{second}, tr.ws, reqs), mountcheck.NotMountedError{mountcheck.RequirementFailure{
		Container: "c2", Path: ".devcontainer", ContainerPath: "/other/ws/.devcontainer",
	}})
}

// Without a live table, the inspected mounts stand for it: a path is a mount
// of its own when inspection reports a mount at it.
func TestCheckRequirementsMountedWithoutALiveTableTrustsInspection(t *testing.T) {
	tr := newTree(t)
	reqs := requirements(rules.MountMounted, ".devcontainer")

	own := container("c1", rw(tr.ws, "/workspaces/ws"), rw(tr.dc, "/workspaces/ws/.devcontainer"))
	if err := mountcheck.CheckRequirements([]watch.Container{own}, tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}

	without := container("c2", rw(tr.ws, "/workspaces/ws"))
	wantNotMounted(t, mountcheck.CheckRequirements([]watch.Container{without}, tr.ws, reqs), mountcheck.NotMountedError{mountcheck.RequirementFailure{
		Container: "c2", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer",
	}})
}

func TestCheckRequirementsReadOnlyFailsWhenOnlyTheWritableWorkspaceCoversThePath(t *testing.T) {
	tr := newTree(t)
	c := container("c1", rw(tr.ws, "/workspaces/ws"))
	err := mountcheck.CheckRequirements([]watch.Container{c}, tr.ws, requirements(rules.MountReadOnly, ".devcontainer"))
	wantReadOnlyRequirement(t, err, mountcheck.ReadOnlyRequirementError{
		RequirementFailure: mountcheck.RequirementFailure{Container: "c1", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer"}, Source: tr.ws,
	})
}

func TestCheckRequirementsReadOnlyPasses(t *testing.T) {
	tr := newTree(t)
	git := filepath.Join(tr.ws, ".git")
	tests := []struct {
		name string
		c    watch.Container
		req  string
	}{
		{"its own read-only mount", withLive(
			container("c1", rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")),
			live(point("/workspaces/ws"), roPoint("/workspaces/ws/.devcontainer")),
		), ".devcontainer"},
		{"its own read-only mount, without a live table",
			container("c1", rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")),
			".devcontainer"},
		{"a read-only mount of an ancestor", withLive(
			container("c1", rw(tr.ws, "/workspaces/ws"), ro(git, "/workspaces/ws/.git")),
			live(point("/workspaces/ws"), roPoint("/workspaces/ws/.git")),
		), ".git/config"},
		{"a container that does not mount the workspace",
			container("c1", rw(filepath.Join(tr.base, "elsewhere"), "/workspaces/ws")),
			".devcontainer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.CheckRequirements([]watch.Container{tt.c}, tr.ws, requirements(rules.MountReadOnly, tt.req))
			if err != nil {
				t.Fatalf("CheckRequirements = %v, want nil", err)
			}
		})
	}
}

func TestCheckRequirementsReadOnlyFailsWhenTheLiveTableSaysWritable(t *testing.T) {
	tr := newTree(t)
	mounts := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")}
	reqs := requirements(rules.MountReadOnly, ".devcontainer")
	tests := []struct {
		name   string
		live   *watch.LiveMounts
		source string
	}{
		{"the read-only mount vanished", live(point("/workspaces/ws")), tr.ws},
		{"the read-only mount turned writable", live(point("/workspaces/ws"), point("/workspaces/ws/.devcontainer")), tr.dc},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.CheckRequirements([]watch.Container{withLive(container("c1", mounts...), tt.live)}, tr.ws, reqs)
			wantReadOnlyRequirement(t, err, mountcheck.ReadOnlyRequirementError{
				RequirementFailure: mountcheck.RequirementFailure{Container: "c1", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer"}, Source: tt.source,
			})
		})
	}
}

func TestCheckRequirementsReportsUnreadableMounts(t *testing.T) {
	tr := newTree(t)
	cause := errors.New("cannot read")
	mounts := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")}
	fine := container("c1", mounts...)
	tests := []struct {
		name string
		c    watch.Container
	}{
		{"inspection", watch.Container{ID: "c2", Running: true, MountsErr: cause}},
		{"live table", withLive(container("c2", mounts...), &watch.LiveMounts{
			Points: []watch.MountPoint{point("/workspaces/ws"), roPoint("/workspaces/ws/.devcontainer")}, Err: cause,
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, mode := range []rules.MountMode{rules.MountMounted, rules.MountReadOnly} {
				err := mountcheck.CheckRequirements([]watch.Container{fine, tt.c}, tr.ws, requirements(mode, ".devcontainer"))
				got, ok := errors.AsType[*mountcheck.UnreadableError](err)
				if !ok {
					t.Fatalf("CheckRequirements(%v) = %v, want an *UnreadableError", mode, err)
				}
				if got.Container != "c2" || got.Err != cause {
					t.Errorf("CheckRequirements(%v) = %+v, want container c2 and the cause", mode, *got)
				}
				if !errors.Is(err, cause) {
					t.Errorf("errors.Is(%v, cause) = false, want true", err)
				}
			}
		})
	}
}

// With several containers, the one that fails is reported, and the first
// failure in the order of the containers then of the requirements.
func TestCheckRequirementsReportsTheFirstFailure(t *testing.T) {
	tr := newTree(t)
	reqs := requirements(rules.MountReadOnly, ".devcontainer")
	good := container("good", rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer"))
	bad := container("bad", rw(tr.ws, "/workspaces/ws"))
	worse := container("worse", rw(tr.ws, "/workspaces/ws"), rw(tr.dc, "/workspaces/ws/.devcontainer"))
	other := container("other", rw(filepath.Join(tr.base, "elsewhere"), "/workspaces/ws"))

	if err := mountcheck.CheckRequirements([]watch.Container{good, other}, tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
	wantReadOnlyRequirement(t, mountcheck.CheckRequirements([]watch.Container{good, bad, worse, other}, tr.ws, reqs),
		mountcheck.ReadOnlyRequirementError{
			RequirementFailure: mountcheck.RequirementFailure{Container: "bad", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer"}, Source: tr.ws,
		})

	both := []rules.MountRequirement{
		{Path: ".git", Mode: rules.MountMounted},
		{Path: ".devcontainer", Mode: rules.MountReadOnly},
	}
	wantNotMounted(t, mountcheck.CheckRequirements([]watch.Container{bad}, tr.ws, both), mountcheck.NotMountedError{mountcheck.RequirementFailure{
		Container: "bad", Path: ".git", ContainerPath: "/workspaces/ws/.git",
	}})
	slices.Reverse(both)
	wantReadOnlyRequirement(t, mountcheck.CheckRequirements([]watch.Container{bad}, tr.ws, both), mountcheck.ReadOnlyRequirementError{
		RequirementFailure: mountcheck.RequirementFailure{Container: "bad", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer"}, Source: tr.ws,
	})
}

func TestCheckRequirementsFailsOnAnUnresolvablePath(t *testing.T) {
	tr := newTree(t)
	loop := filepath.Join(tr.dc, "loop-a")
	symlink(t, "loop-b", loop)
	symlink(t, "loop-a", filepath.Join(tr.dc, "loop-b"))
	fine := container("c1", rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer"))

	t.Run("mount source", func(t *testing.T) {
		bad := container("c2", rw(tr.ws, "/workspaces/ws"), ro(loop, "/loop"))
		for _, mode := range []rules.MountMode{rules.MountMounted, rules.MountReadOnly} {
			err := mountcheck.CheckRequirements([]watch.Container{fine, bad}, tr.ws, requirements(mode, ".devcontainer"))
			wantUnresolvable(t, err, "c2", loop, syscall.ELOOP)
		}
	})
	t.Run("workspace", func(t *testing.T) {
		link := filepath.Join(tr.base, "ws-loop")
		symlink(t, "ws-loop", link)
		err := mountcheck.CheckRequirements([]watch.Container{fine}, link, requirements(rules.MountMounted, ".devcontainer"))
		wantUnresolvable(t, err, "", link, syscall.ELOOP)
	})
	t.Run("read-only path", func(t *testing.T) {
		err := mountcheck.CheckRequirements([]watch.Container{fine}, tr.ws, requirements(rules.MountReadOnly, ".devcontainer/loop-a"))
		wantUnresolvable(t, err, "", loop, syscall.ELOOP)
	})
}

func TestCheckRequirementsPassesWithNothingRequired(t *testing.T) {
	tr := newTree(t)
	unreadable := watch.Container{ID: "c1", Running: true, MountsErr: errors.New("cannot read")}
	if err := mountcheck.CheckRequirements([]watch.Container{unreadable}, tr.ws, nil); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
}

func TestCheckRequirementsMountedPassesForAContainerWithoutTheWorkspace(t *testing.T) {
	tr := newTree(t)
	c := withLive(
		container("c1", rw(filepath.Join(tr.base, "elsewhere"), "/workspaces/ws")),
		live(point("/workspaces/ws")),
	)
	err := mountcheck.CheckRequirements([]watch.Container{c}, tr.ws, requirements(rules.MountMounted, ".devcontainer"))
	if err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
}
