// Package workspace maps a working directory inside the container to the
// matching directory on the host, confining it to the workspace.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrOutsideWorkspace means the requested directory does not lie inside the
// workspace, either lexically in the container or after resolving symlinks
// on the host.
var ErrOutsideWorkspace = errors.New("working directory is outside the workspace")

// Mapper translates container paths under containerRoot to host paths under
// hostRoot. Both roots describe the same bind-mounted workspace.
type Mapper struct {
	hostRoot      string // absolute, symlink-free
	containerRoot string // absolute, cleaned
}

// NewMapper builds a Mapper. hostRoot must exist; it is resolved through
// symlinks once so that later containment checks compare real paths.
func NewMapper(hostRoot, containerRoot string) (*Mapper, error) {
	if !filepath.IsAbs(hostRoot) || !filepath.IsAbs(containerRoot) {
		return nil, fmt.Errorf("workspace roots must be absolute: host %q, container %q", hostRoot, containerRoot)
	}
	resolved, err := filepath.EvalSymlinks(hostRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve host workspace: %w", err)
	}
	return &Mapper{hostRoot: resolved, containerRoot: filepath.Clean(containerRoot)}, nil
}

// Open opens the host directory for containerCwd and returns it with its
// real host path. The caller must close it.
//
// The path is checked lexically in the container namespace (so ".." cannot
// climb out) and then opened through an os.Root at the host workspace, which
// refuses any symlink leading outside it. Because the caller gets an open
// directory rather than a path, a symlink the container swaps in after the
// check cannot redirect the command: run it in the directory itself (e.g.
// via /proc/self/fd), not by path.
func (m *Mapper) Open(containerCwd string) (*os.File, string, error) {
	if !filepath.IsAbs(containerCwd) {
		return nil, "", fmt.Errorf("%w: %q is not absolute", ErrOutsideWorkspace, containerCwd)
	}
	rel, ok := within(m.containerRoot, filepath.Clean(containerCwd))
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, containerCwd)
	}
	root, err := os.OpenRoot(m.hostRoot)
	if err != nil {
		return nil, "", fmt.Errorf("open host workspace: %w", err)
	}
	defer root.Close()
	// O_DIRECTORY: opening a FIFO the container planted would block forever.
	dir, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno):
		// ENOENT, ENOTDIR, EACCES, ELOOP, …: the directory is not usable.
		return nil, "", fmt.Errorf("open %s on the host: %w", containerCwd, err)
	case err != nil:
		// os.Root reports escapes (via ".." or symlinks) without an errno.
		return nil, "", fmt.Errorf("%w: %s: %v", ErrOutsideWorkspace, containerCwd, err)
	}
	hostPath, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if err != nil {
		dir.Close()
		return nil, "", fmt.Errorf("open %s on the host: %w", containerCwd, err)
	}
	return dir, hostPath, nil
}

// within reports whether path equals root or lies below it, and returns path
// relative to root. Both arguments must be clean absolute paths.
func within(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
