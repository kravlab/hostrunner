package mountcheck

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/watch"
)

// RequirementFailure is where a mount requirement fails, what
// NotMountedError and ReadOnlyRequirementError both carry.
type RequirementFailure struct {
	Container string // container ID
	Path      string // the required path, relative to the workspace root
	// ContainerPath is where in the container the requirement fails: where
	// Path would have to be a mount point (NotMountedError), or where Path
	// (or a part of it) is writable (ReadOnlyRequirementError).
	ContainerPath string
}

// NotMountedError means a mount of a container reaches a path of the
// workspace that a rule requires to be a mount of its own (the workspace
// itself, a parent of it or a directory below it is mounted), but the path
// is not one at the container path it is reached at.
type NotMountedError struct {
	RequirementFailure
}

func (e *NotMountedError) Error() string {
	return fmt.Sprintf("container %s: %s is not a mount of its own: nothing is mounted at %s",
		e.Container, e.Path, e.ContainerPath)
}

// ReadOnlyRequirementError means a container can write a path of the
// workspace that a rule requires to be read-only. It is not a
// *WritableError, so that a failed requirement is told from a failed
// protected path with errors.As.
type ReadOnlyRequirementError struct {
	RequirementFailure
	Source string // the host source of the writable mount, resolved
}

func (e *ReadOnlyRequirementError) Error() string {
	return fmt.Sprintf("container %s: %s is not read-only: host %s is mounted writable at %s",
		e.Container, e.Path, e.Source, e.ContainerPath)
}

// CheckRequirements returns nil when every container m was read from meets
// every one of reqs, else the first failure found, containers in order then
// reqs in order. workspace is the host path of the workspace root; each
// requirement path is relative to it, and is resolved on the host as a
// protected path is. With no reqs, it returns nil without looking at
// containers. An *UnresolvableError for the workspace or the path of a
// requirement comes before all containers.
//
// A container's mounts are judged as Mounts.Check judges them: an
// *UnreadableError for mounts or a live table that could not be read, an
// *UnresolvableError for a mount source that could not be resolved, and
// only the mounts the live table still holds count.
//
// A MountMounted requirement holds when, for every counted mount of the
// container that reaches the resolved host path of the requirement (its
// resolved source is that path or one of its ancestors: the workspace, a
// parent of it, or a directory between the workspace and the path), the live
// table has a mount point at exactly the container path the host path is
// reached at (the destination joined with the host path relative to the
// source), read-only or not; else it is a *NotMountedError naming that
// container path. A mount that a deeper mount of the same container covers
// at that container path does not reach the path. Without a live table, the
// destinations of the inspected mounts stand for the table. A container with
// no mount that reaches the path holds it nowhere and passes.
//
// A MountReadOnly requirement holds when no writable mount reaches the
// resolved host path of the requirement, as for a protected path in Check, so
// a read-only mount of an ancestor is enough; else it is a
// *ReadOnlyRequirementError.
func (m Mounts) CheckRequirements(workspace string, reqs []rules.MountRequirement) error {
	if len(reqs) == 0 {
		return nil
	}
	// The workspace root is resolved first, and only for its error, so that
	// a root that cannot be resolved is reported before the failure of any
	// requirement's path.
	if _, err := resolve(workspace); err != nil {
		return err
	}
	resolved := make([]resolvedRequirement, len(reqs))
	for i, req := range reqs {
		resolved[i].MountRequirement = req
		var err error
		if resolved[i].hostPath, err = resolve(filepath.Join(workspace, filepath.FromSlash(req.Path))); err != nil {
			return err
		}
	}
	for _, c := range m.containers {
		if c.err != nil {
			return c.err
		}
		for _, req := range resolved {
			var err error
			switch req.Mode {
			case rules.MountMounted:
				err = checkMounted(c.id, c.mounts, c.points, req.hostPath, req.Path)
			case rules.MountReadOnly:
				err = checkReadOnly(c.id, c.mounts, req.hostPath, req.Path)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// resolvedRequirement is a mount requirement as CheckRequirements checks
// it: hostPath is the resolved host path of the required path.
type resolvedRequirement struct {
	rules.MountRequirement
	hostPath string
}

// checkMounted returns a *NotMountedError for the first mount of container
// id that reaches p, the resolved host path of the required path rel, at a
// container path that is not one of points, or nil. A mount reaches p when
// its source is p or one of its ancestors and no deeper mount of the
// container covers the container path it reaches p at, as for a protected
// path in checkPath.
func checkMounted(id string, mounts []mount, points []string, p, rel string) error {
	for i, m := range mounts {
		c, ok := reachedAt(m, p)
		if !ok || shadowed(mounts, i, c) {
			continue
		}
		if !slices.Contains(points, c) {
			return &NotMountedError{RequirementFailure{Container: id, Path: rel, ContainerPath: c}}
		}
	}
	return nil
}

// checkReadOnly returns a *ReadOnlyRequirementError for the first of mounts
// that makes p, the resolved host path of the required path rel, writable in
// container id, or nil. It judges as Check does for a protected path.
func checkReadOnly(id string, mounts []mount, p, rel string) error {
	w, ok := errors.AsType[*WritableError](checkPath(id, mounts, p))
	if !ok {
		return nil
	}
	return &ReadOnlyRequirementError{RequirementFailure: RequirementFailure{Container: id, Path: rel, ContainerPath: w.ContainerPath}, Source: w.Source}
}

// mountPoints returns the paths in the container where something is
// mounted: the points of the live mount table l or, without one (l is nil),
// the cleaned destinations of counted, the inspected mounts, which stand for
// the table.
func mountPoints(counted []watch.Mount, l *watch.LiveMounts) []string {
	var out []string
	if l == nil {
		for _, m := range counted {
			out = append(out, m.PointPath())
		}
		return out
	}
	for _, p := range l.Points {
		out = append(out, p.Path)
	}
	return out
}
