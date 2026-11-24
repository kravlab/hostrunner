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
	err := mountcheck.Read([]watch.Container{c}).CheckRequirements(
		tr.ws, requirements(rules.MountMounted, ".devcontainer"))
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
			err := mountcheck.Read([]watch.Container{c}).CheckRequirements(
				tr.ws, requirements(rules.MountMounted, ".devcontainer"))
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
	if err := mountcheck.Read([]watch.Container{both}).CheckRequirements(tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}

	second := withLive(container("c2", mounts...), live(
		point("/workspaces/ws"), point("/workspaces/ws/.devcontainer"),
		point("/other/ws"),
	))
	wantNotMounted(t, mountcheck.Read([]watch.Container{second}).CheckRequirements(tr.ws, reqs),
		mountcheck.NotMountedError{mountcheck.RequirementFailure{
			Container: "c2", Path: ".devcontainer", ContainerPath: "/other/ws/.devcontainer",
		}})
}

// Without a live table, the inspected mounts stand for it: a path is a mount
// of its own when inspection reports a mount at it.
func TestCheckRequirementsMountedWithoutALiveTableTrustsInspection(t *testing.T) {
	tr := newTree(t)
	reqs := requirements(rules.MountMounted, ".devcontainer")

	own := container("c1", rw(tr.ws, "/workspaces/ws"), rw(tr.dc, "/workspaces/ws/.devcontainer"))
	if err := mountcheck.Read([]watch.Container{own}).CheckRequirements(tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}

	without := container("c2", rw(tr.ws, "/workspaces/ws"))
	wantNotMounted(t, mountcheck.Read([]watch.Container{without}).CheckRequirements(tr.ws, reqs),
		mountcheck.NotMountedError{mountcheck.RequirementFailure{
			Container: "c2", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer",
		}})
}

func TestCheckRequirementsReadOnlyFailsWhenOnlyTheWritableWorkspaceCoversThePath(t *testing.T) {
	tr := newTree(t)
	c := container("c1", rw(tr.ws, "/workspaces/ws"))
	err := mountcheck.Read([]watch.Container{c}).CheckRequirements(
		tr.ws, requirements(rules.MountReadOnly, ".devcontainer"))
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
			err := mountcheck.Read([]watch.Container{tt.c}).CheckRequirements(
				tr.ws, requirements(rules.MountReadOnly, tt.req))
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
			err := mountcheck.Read([]watch.Container{withLive(container("c1", mounts...), tt.live)}).CheckRequirements(
				tr.ws, reqs)
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
				err := mountcheck.Read([]watch.Container{fine, tt.c}).CheckRequirements(
					tr.ws, requirements(mode, ".devcontainer"))
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

	if err := mountcheck.Read([]watch.Container{good, other}).CheckRequirements(tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
	wantReadOnlyRequirement(t, mountcheck.Read([]watch.Container{good, bad, worse, other}).CheckRequirements(
		tr.ws, reqs),
		mountcheck.ReadOnlyRequirementError{
			RequirementFailure: mountcheck.RequirementFailure{Container: "bad", Path: ".devcontainer", ContainerPath: "/workspaces/ws/.devcontainer"}, Source: tr.ws,
		})

	both := []rules.MountRequirement{
		{Path: ".git", Mode: rules.MountMounted},
		{Path: ".devcontainer", Mode: rules.MountReadOnly},
	}
	wantNotMounted(t, mountcheck.Read([]watch.Container{bad}).CheckRequirements(tr.ws, both),
		mountcheck.NotMountedError{mountcheck.RequirementFailure{
			Container: "bad", Path: ".git", ContainerPath: "/workspaces/ws/.git",
		}})
	slices.Reverse(both)
	wantReadOnlyRequirement(t, mountcheck.Read([]watch.Container{bad}).CheckRequirements(tr.ws, both),
		mountcheck.ReadOnlyRequirementError{
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
			err := mountcheck.Read([]watch.Container{fine, bad}).CheckRequirements(
				tr.ws, requirements(mode, ".devcontainer"))
			wantUnresolvable(t, err, "c2", loop, syscall.ELOOP)
		}
	})
	t.Run("workspace", func(t *testing.T) {
		link := filepath.Join(tr.base, "ws-loop")
		symlink(t, "ws-loop", link)
		err := mountcheck.Read([]watch.Container{fine}).CheckRequirements(
			link, requirements(rules.MountMounted, ".devcontainer"))
		wantUnresolvable(t, err, "", link, syscall.ELOOP)
	})
	t.Run("read-only path", func(t *testing.T) {
		err := mountcheck.Read([]watch.Container{fine}).CheckRequirements(
			tr.ws, requirements(rules.MountReadOnly, ".devcontainer/loop-a"))
		wantUnresolvable(t, err, "", loop, syscall.ELOOP)
	})
}

func TestCheckRequirementsPassesWithNothingRequired(t *testing.T) {
	tr := newTree(t)
	unreadable := watch.Container{ID: "c1", Running: true, MountsErr: errors.New("cannot read")}
	if err := mountcheck.Read([]watch.Container{unreadable}).CheckRequirements(tr.ws, nil); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
}

func TestCheckRequirementsMountedPassesForAContainerWithoutTheWorkspace(t *testing.T) {
	tr := newTree(t)
	c := withLive(
		container("c1", rw(filepath.Join(tr.base, "elsewhere"), "/workspaces/ws")),
		live(point("/workspaces/ws")),
	)
	err := mountcheck.Read([]watch.Container{c}).CheckRequirements(
		tr.ws, requirements(rules.MountMounted, ".devcontainer"))
	if err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
}

// The order of failures across containers: an unresolvable workspace root
// or read-only requirement path before any container, then each container
// in order, its unreadable mounts before its unresolvable mount sources.
func TestCheckRequirementsReportsFailuresInOrder(t *testing.T) {
	tr := newTree(t)
	loop := filepath.Join(tr.dc, "loop-a")
	symlink(t, "loop-b", loop)
	symlink(t, "loop-a", filepath.Join(tr.dc, "loop-b"))
	cause := errors.New("cannot read")
	unreadable := watch.Container{ID: "c1", Running: true, MountsErr: cause}
	unresolvable := container("c2", rw(tr.ws, "/workspaces/ws"), ro(loop, "/loop"))
	containers := []watch.Container{unreadable, unresolvable}

	t.Run("workspace", func(t *testing.T) {
		link := filepath.Join(tr.base, "ws-loop")
		symlink(t, "ws-loop", link)
		err := mountcheck.Read(containers).CheckRequirements(link, requirements(rules.MountMounted, ".devcontainer"))
		wantUnresolvable(t, err, "", link, syscall.ELOOP)
	})
	t.Run("read-only path", func(t *testing.T) {
		err := mountcheck.Read(containers).CheckRequirements(
			tr.ws, requirements(rules.MountReadOnly, ".devcontainer/loop-a"))
		wantUnresolvable(t, err, "", loop, syscall.ELOOP)
	})
	t.Run("containers in order", func(t *testing.T) {
		for _, mode := range []rules.MountMode{rules.MountMounted, rules.MountReadOnly} {
			err := mountcheck.Read(containers).CheckRequirements(tr.ws, requirements(mode, ".devcontainer"))
			if got, ok := errors.AsType[*mountcheck.UnreadableError](err); !ok || got.Container != "c1" {
				t.Fatalf("CheckRequirements(%v) = %v, want an *UnreadableError of c1", mode, err)
			}
			err = mountcheck.Read([]watch.Container{unresolvable, unreadable}).CheckRequirements(
				tr.ws, requirements(mode, ".devcontainer"))
			wantUnresolvable(t, err, "c2", loop, syscall.ELOOP)
		}
	})
}

// A parent of the workspace mounted without a mount of the required path
// onto itself reaches the path at a container path under the parent mount,
// where nothing is mounted.
func TestCheckRequirementsMountedFailsThroughAMountOfAParentOfTheWorkspace(t *testing.T) {
	tr := newTree(t)
	c := withLive(container("c1", rw(tr.base, "/p")), live(point("/p")))
	err := mountcheck.Read([]watch.Container{c}).CheckRequirements(tr.ws, requirements(rules.MountMounted, ".git"))
	wantNotMounted(t, err, mountcheck.NotMountedError{mountcheck.RequirementFailure{
		Container: "c1", Path: ".git", ContainerPath: "/p/ws/.git",
	}})
}

// The mount point that holds the required path under a mount of a parent of
// the workspace makes it a mount of its own there; so does a mount of the
// path itself, which reaches the path at its own destination.
func TestCheckRequirementsMountedPassesThroughAMountOfAParentOfTheWorkspace(t *testing.T) {
	tr := newTree(t)
	git := filepath.Join(tr.ws, ".git")
	tests := []struct {
		name string
		c    watch.Container
	}{
		{"a mount point at the path under the parent", withLive(
			container("c1", rw(tr.base, "/p"), rw(git, "/p/ws/.git")),
			live(point("/p"), point("/p/ws/.git")),
		)},
		{"a mount of the path itself", withLive(
			container("c1", rw(git, "/workspaces/ws/.git")),
			live(point("/workspaces/ws/.git")),
		)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.Read([]watch.Container{tt.c}).CheckRequirements(
				tr.ws, requirements(rules.MountMounted, ".git"))
			if err != nil {
				t.Fatalf("CheckRequirements = %v, want nil", err)
			}
		})
	}
}

// A parent mount does not reach the path at a container path that a deeper
// mount of the same container covers: it is not judged through the parent.
func TestCheckRequirementsMountedDoesNotJudgeThroughAParentMountACoveringMountShadows(t *testing.T) {
	tr := newTree(t)
	c := withLive(
		container("c1", rw(tr.base, "/p"), rw(filepath.Join(tr.base, "elsewhere"), "/p/ws")),
		live(point("/p"), point("/p/ws")),
	)
	err := mountcheck.Read([]watch.Container{c}).CheckRequirements(tr.ws, requirements(rules.MountMounted, ".git"))
	if err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
}

// A mount of a directory between the workspace root and the required path
// reaches the path at its destination joined with the rest of the path.
func TestCheckRequirementsMountedJudgesThroughAMountOfADirectoryBetweenTheRootAndThePath(t *testing.T) {
	tr := newTree(t)
	git := filepath.Join(tr.ws, ".git")
	reqs := requirements(rules.MountMounted, ".git/hooks")

	missing := withLive(container("c1", rw(git, "/g")), live(point("/g")))
	wantNotMounted(t, mountcheck.Read([]watch.Container{missing}).CheckRequirements(tr.ws, reqs),
		mountcheck.NotMountedError{mountcheck.RequirementFailure{
			Container: "c1", Path: ".git/hooks", ContainerPath: "/g/hooks",
		}})

	held := withLive(
		container("c1", rw(git, "/g"), rw(filepath.Join(tr.base, "hooks"), "/g/hooks")),
		live(point("/g"), point("/g/hooks")),
	)
	if err := mountcheck.Read([]watch.Container{held}).CheckRequirements(tr.ws, reqs); err != nil {
		t.Fatalf("CheckRequirements = %v, want nil", err)
	}
}
