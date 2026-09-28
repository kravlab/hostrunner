// Package workspace maps a working directory inside the container to the
// matching directory on the host, confining it to the workspace.
package workspace

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// HostPath returns the real host directory for containerCwd.
//
// The check runs twice: first lexically in the container namespace (so ".."
// cannot climb out), then on the resolved host path (so a symlink the
// container planted inside the workspace cannot point the command at an
// arbitrary host directory). The directory must exist on the host.
func (m *Mapper) HostPath(containerCwd string) (string, error) {
	if !filepath.IsAbs(containerCwd) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrOutsideWorkspace, containerCwd)
	}
	rel, ok := within(m.containerRoot, filepath.Clean(containerCwd))
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, containerCwd)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(m.hostRoot, rel))
	if err != nil {
		return "", fmt.Errorf("resolve %s on the host: %w", containerCwd, err)
	}
	if _, ok := within(m.hostRoot, resolved); !ok {
		return "", fmt.Errorf("%w: %s resolves outside it on the host", ErrOutsideWorkspace, containerCwd)
	}
	return resolved, nil
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
