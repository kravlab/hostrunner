// Package workspace maps a working directory inside the container to the
// matching directory on the host, confining it to the workspace, and keeps
// a rule's fixed directory out of it.
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrOutsideWorkspace means a requested directory or file does not lie
// inside the workspace, either lexically in the container or after
// resolving symlinks on the host.
var ErrOutsideWorkspace = errors.New("outside the workspace")

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
	hostPath, err := realPath(dir, containerCwd)
	if err != nil {
		return nil, "", err
	}
	return dir, hostPath, nil
}

// realPath returns the real host path of the directory dir, opened for
// name, as the kernel resolved it on opening. On failure it closes dir.
func realPath(dir *os.File, name string) (string, error) {
	hostPath, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if err != nil {
		dir.Close()
		return "", fmt.Errorf("open %s on the host: %w", name, err)
	}
	return hostPath, nil
}

// ErrInsideWorkspace means a fixed directory's real path lies inside the
// workspace, which the container writes.
var ErrInsideWorkspace = errors.New("inside the workspace")

// OpenFixed opens the fixed directory path, an absolute host path a rule
// names, and returns it with its real host path. The caller must close it
// and run the command in the open directory, as for Open.
//
// The directory is refused with ErrInsideWorkspace when its real path, as
// opened (symlinks followed), is the workspace or lies below it: the
// container could plant there the configuration a fixed directory keeps a
// command away from. A missing path keeps fs.ErrNotExist and anything but
// a directory fails with ENOTDIR.
func (m *Mapper) OpenFixed(path string) (*os.File, string, error) {
	// O_DIRECTORY: opening a FIFO in its place would block forever.
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	hostPath, err := realPath(dir, path)
	if err != nil {
		return nil, "", err
	}
	if _, inside := within(m.hostRoot, hostPath); inside {
		dir.Close()
		return nil, "", fmt.Errorf("%s leads to %s: %w", path, hostPath, ErrInsideWorkspace)
	}
	return dir, hostPath, nil
}

// Reasons OpenFile refuses a name, besides ErrOutsideWorkspace and
// fs.ErrNotExist.
var (
	ErrEmptyPath      = errors.New("path is empty")
	ErrAbsolutePath   = errors.New("path is absolute")
	ErrNotRegularFile = errors.New("not a regular file")
)

// OpenFile opens the workspace file that name, a path relative to the host
// directory dirPath (the real path Open returned), refers to, and returns
// it open for reading. The caller must close it.
//
// The name is resolved as the kernel would resolve it from dirPath (".."
// after a symlink climbs from the symlink's target), but through an
// os.Root at the host workspace, so neither ".." nor a symlink can leave
// the workspace: the confinement is the workspace, not dirPath. Only a
// regular file is opened. The open is non-blocking and takes no
// controlling terminal, so a FIFO or terminal swapped in after the type
// check can neither stall the daemon nor be acted on beyond the open; the
// type is checked again on the open file.
func (m *Mapper) OpenFile(dirPath, name string) (*os.File, error) {
	switch {
	case name == "":
		return nil, ErrEmptyPath
	case filepath.IsAbs(name):
		return nil, ErrAbsolutePath
	}
	dir, ok := within(m.hostRoot, dirPath)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOutsideWorkspace, dirPath)
	}
	// Not filepath.Join: cleaning "link/.." lexically would check another
	// file than the one the program opens.
	rel := dir + string(filepath.Separator) + name
	root, err := os.OpenRoot(m.hostRoot)
	if err != nil {
		return nil, fmt.Errorf("open host workspace: %w", err)
	}
	defer root.Close()
	fi, err := root.Stat(rel)
	if err != nil {
		return nil, fileError(name, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fileError(name, err)
	}
	if fi, err = f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, ErrNotRegularFile
	}
	return f, nil
}

// fileError classifies why the os.Root could not reach name: a missing file
// keeps fs.ErrNotExist, other errnos are reported as they are, and an error
// without an errno is os.Root reporting an escape.
func fileError(name string, err error) error {
	var errno syscall.Errno
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	case errors.As(err, &errno):
		return fmt.Errorf("open %s on the host: %w", name, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrOutsideWorkspace, name, err)
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
