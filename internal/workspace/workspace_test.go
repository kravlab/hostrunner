package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const containerRoot = "/workspaces/app"

// newHostWorkspace creates a host workspace with a "sub" directory and an
// "outside" directory next to it, and returns their resolved paths.
func newHostWorkspace(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "app")
	outside = filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

func TestHostPathMirrorsCwdInsideWorkspace(t *testing.T) {
	root, _ := newHostWorkspace(t)
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"/workspaces/app":            root,
		"/workspaces/app/sub":        filepath.Join(root, "sub"),
		"/workspaces/app/sub/../sub": filepath.Join(root, "sub"),
	}
	for cwd, want := range cases {
		got, err := m.HostPath(cwd)
		if err != nil {
			t.Errorf("HostPath(%q): unexpected error %v", cwd, err)
			continue
		}
		if got != want {
			t.Errorf("HostPath(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestHostPathRejectsCwdOutsideWorkspace(t *testing.T) {
	root, _ := newHostWorkspace(t)
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{
		"/",
		"/workspaces",
		"/workspaces/app2",
		"/workspaces/app/..",
		"/workspaces/app/sub/../../outside",
		"workspaces/app",
		"",
	} {
		if _, err := m.HostPath(cwd); !errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("HostPath(%q): got %v, want ErrOutsideWorkspace", cwd, err)
		}
	}
}

func TestHostPathRejectsSymlinkEscapingWorkspace(t *testing.T) {
	root, outside := newHostWorkspace(t)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.HostPath("/workspaces/app/escape"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("got %v, want ErrOutsideWorkspace", err)
	}
}

func TestHostPathFollowsSymlinkInsideWorkspace(t *testing.T) {
	root, _ := newHostWorkspace(t)
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.HostPath("/workspaces/app/link")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "sub"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHostPathRejectsMissingDirectory(t *testing.T) {
	root, _ := newHostWorkspace(t)
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.HostPath("/workspaces/app/missing"); err == nil {
		t.Fatal("expected an error for a directory that does not exist on the host")
	}
}

func TestNewMapperRejectsRelativeRoots(t *testing.T) {
	root, _ := newHostWorkspace(t)
	if _, err := NewMapper("relative/app", containerRoot); err == nil {
		t.Error("expected an error for a relative host root")
	}
	if _, err := NewMapper(root, "workspaces/app"); err == nil {
		t.Error("expected an error for a relative container root")
	}
}

func TestNewMapperRejectsMissingHostRoot(t *testing.T) {
	if _, err := NewMapper(filepath.Join(t.TempDir(), "missing"), containerRoot); err == nil {
		t.Fatal("expected an error for a host root that does not exist")
	}
}
