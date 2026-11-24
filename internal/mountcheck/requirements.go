package mountcheck

import (
	"errors"
	"fmt"
	"path"
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

// NotMountedError means a container mounts the workspace but a path of it
// that a rule requires to be a mount of its own is not one.
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

// CheckRequirements returns nil when every container meets every one of
// reqs, else the first failure found, containers in order then reqs in
// order. workspace is the host path of the workspace root; each requirement
// path is relative to it. With no reqs, it returns nil without looking at
// containers. An *UnresolvableError for the workspace or the path of a
// read-only requirement comes before all containers.
//
// A container's mounts are judged as Check judges them: an *UnreadableError
// for mounts or a live table that could not be read, an *UnresolvableError
// for a mount source that could not be resolved, and only the mounts the
// live table still holds count.
//
// A MountMounted requirement holds when, at each place where the container
// mounts the workspace (a counted mount whose resolved source is the
// resolved workspace), the live table has a mount point at exactly the
// requirement path below it, read-only or not; else it is a
// *NotMountedError. Without a live table, the destinations of the inspected
// mounts stand for the table. A container that does not mount the workspace
// holds the path nowhere and passes.
//
// A MountReadOnly requirement holds when no writable mount reaches the
// resolved host path of the requirement, as for a protected path in Check, so
// a read-only mount of an ancestor is enough; else it is a
// *ReadOnlyRequirementError.
func CheckRequirements(containers []watch.Container, workspace string, reqs []rules.MountRequirement) error {
	if len(reqs) == 0 {
		return nil
	}
	root, err := resolve(workspace)
	if err != nil {
		return err
	}
	resolved := make([]resolvedRequirement, len(reqs))
	for i, req := range reqs {
		resolved[i].MountRequirement = req
		if req.Mode != rules.MountReadOnly {
			continue
		}
		if resolved[i].hostPath, err = resolve(filepath.Join(workspace, filepath.FromSlash(req.Path))); err != nil {
			return err
		}
	}
	for _, c := range containers {
		if c.MountsErr != nil {
			return &UnreadableError{Container: c.ID, Err: c.MountsErr}
		}
		if c.Live != nil && c.Live.Err != nil {
			return &UnreadableError{Container: c.ID, Err: c.Live.Err}
		}
		counted := liveMounts(c.Mounts, c.Live)
		mounts, err := hostMounts(c.ID, counted)
		if err != nil {
			return err
		}
		points := mountPoints(counted, c.Live)
		for _, req := range resolved {
			switch req.Mode {
			case rules.MountMounted:
				err = checkMounted(c.ID, mounts, points, root, req.Path)
			case rules.MountReadOnly:
				err = checkReadOnly(c.ID, mounts, req.hostPath, req.Path)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// resolvedRequirement is a mount requirement as CheckRequirements checks
// it: hostPath is the resolved host path of a MountReadOnly requirement,
// "" for a MountMounted one, which is judged on container paths.
type resolvedRequirement struct {
	rules.MountRequirement
	hostPath string
}

// checkMounted returns a *NotMountedError for the first workspace mount of
// container id (a mount of root) at which rel, the required path relative to
// the workspace root, is not one of points, or nil.
func checkMounted(id string, mounts []mount, points []string, root, rel string) error {
	for _, m := range mounts {
		if m.source != root {
			continue
		}
		if cp := path.Join(m.dest, rel); !slices.Contains(points, cp) {
			return &NotMountedError{RequirementFailure{Container: id, Path: rel, ContainerPath: cp}}
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
			out = append(out, m.Point())
		}
		return out
	}
	for _, p := range l.Points {
		out = append(out, p.Path)
	}
	return out
}
