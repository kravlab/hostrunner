package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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

// open is Open for tests that only need the resolved host path.
func open(t *testing.T, m *Mapper, cwd string) (string, error) {
	t.Helper()
	f, path, err := m.Open(cwd)
	if err == nil {
		f.Close()
	}
	return path, err
}

func TestOpenMirrorsCwdInsideWorkspace(t *testing.T) {
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
		got, err := open(t, m, cwd)
		if err != nil {
			t.Errorf("Open(%q): unexpected error %v", cwd, err)
			continue
		}
		if got != want {
			t.Errorf("Open(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestOpenRejectsCwdOutsideWorkspace(t *testing.T) {
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
		if _, err := open(t, m, cwd); !errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("Open(%q): got %v, want ErrOutsideWorkspace", cwd, err)
		}
	}
}

func TestOpenRejectsSymlinkEscapingWorkspace(t *testing.T) {
	root, outside := newHostWorkspace(t)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, m, "/workspaces/app/escape"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("got %v, want ErrOutsideWorkspace", err)
	}
}

func TestOpenRejectsAbsoluteSymlink(t *testing.T) {
	// An absolute symlink written in the container names a container path,
	// which means nothing on the host; os.Root refuses all of them.
	root, _ := newHostWorkspace(t)
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "abs")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, m, "/workspaces/app/abs"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("got %v, want ErrOutsideWorkspace", err)
	}
}

func TestOpenFollowsSymlinkInsideWorkspace(t *testing.T) {
	root, _ := newHostWorkspace(t)
	if err := os.Symlink("sub", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(t, m, "/workspaces/app/link")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "sub"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOpenRejectsMissingDirectory(t *testing.T) {
	root, _ := newHostWorkspace(t)
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, m, "/workspaces/app/missing"); err == nil {
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

func TestOpenedDirectorySurvivesSymlinkSwap(t *testing.T) {
	root, outside := newHostWorkspace(t)
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := m.Open("/workspaces/app/sub")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// After the check, the container swaps the directory for a symlink
	// pointing outside the workspace.
	sub := filepath.Join(root, "sub")
	if err := os.Rename(sub, sub+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}

	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(sub + "-old")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(opened, original) {
		t.Fatal("the opened directory changed after the path was swapped")
	}
}

func TestOpenRejectsFile(t *testing.T) {
	root, _ := newHostWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, m, "/workspaces/app/file"); err == nil {
		t.Fatal("expected an error for a working directory that is a file")
	}
}

func TestOpenDoesNotBlockOnFIFO(t *testing.T) {
	root, _ := newHostWorkspace(t)
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := open(t, m, "/workspaces/app/fifo"); done <- err }()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("got %v, want a not-a-directory error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open blocked on a FIFO")
	}
}

func TestOpenReportsInaccessibleDirectoryAsSuch(t *testing.T) {
	root, _ := newHostWorkspace(t)
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	m, err := NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"/workspaces/app/locked", "/workspaces/app/locked/x"} {
		if _, err := open(t, m, cwd); err == nil || errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("%s: got %v, want an error other than ErrOutsideWorkspace", cwd, err)
		}
	}
}
