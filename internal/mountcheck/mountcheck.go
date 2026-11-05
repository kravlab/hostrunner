// Package mountcheck tells whether a devcontainer can write host paths the
// daemon protects, judging from the container's mounts as its runtime
// reports them.
package mountcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kravlab/hostrunner/internal/watch"
)

// WritableError means a container can write a protected host path, or a part
// of it, through a writable mount.
type WritableError struct {
	Container     string // container ID
	Path          string // the protected host path, resolved
	Source        string // the host source of the writable mount, resolved
	ContainerPath string // where in the container Path (or a part of it) is writable
}

func (e *WritableError) Error() string {
	return fmt.Sprintf("container %s can write %s: host %s is mounted writable at %s",
		e.Container, e.Path, e.Source, e.ContainerPath)
}

// UnreadableError means a container's mounts could not be read, so whether it
// can write a protected path is unknown.
type UnreadableError struct {
	Container string // container ID
	Err       error  // why the mounts could not be read
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("container %s: cannot tell whether it can write protected paths: %v", e.Container, e.Err)
}

// Unwrap returns why the mounts could not be read.
func (e *UnreadableError) Unwrap() error { return e.Err }

// UnresolvableError means a protected path or a mount source could not be
// resolved, as for a symlink loop or a component that cannot be read, so
// what it names on the host is unknown.
type UnresolvableError struct {
	Container string // container ID for a mount source; empty for a protected path
	Path      string // the path, absolute and clean
	Err       error  // why it could not be resolved
}

func (e *UnresolvableError) Error() string {
	if e.Container != "" {
		return fmt.Sprintf("container %s: cannot resolve %s: %v", e.Container, e.Path, e.Err)
	}
	return fmt.Sprintf("cannot resolve %s: %v", e.Path, e.Err)
}

// Unwrap returns why the path could not be resolved.
func (e *UnresolvableError) Unwrap() error { return e.Err }

// mount is a watch.Mount with a host source, resolved, and its destination
// cleaned.
type mount struct {
	source, dest string
	readOnly     bool
}

// Check returns nil when none of containers can write any of paths (host
// paths), else the first failure found, containers in order then paths in
// order: an *UnreadableError for a container whose mounts could not be read,
// else a *WritableError. An *UnresolvableError for a protected path comes before
// all of them, one for a mount source right after its container's mounts
// are read.
//
// Protected paths and mount sources are compared after resolving symlinks,
// dangling ones included; a path that does not exist is resolved through
// its nearest existing ancestor, so a writable parent of a missing file
// fails the check. Mounts without an absolute host source (tmpfs, an empty
// or relative source) are ignored, and so are volumes, by their Type: their
// source lies in the runtime's storage, which the daemon may not read, and
// holds only what the runtime puts there. A path or a bind mount source with
// a component that cannot be read, other than one that does not exist, fails
// the check with an *UnresolvableError, as what an unreadable link leads to is
// unknown. A writable mount reaches a protected path when one
// lies under the other, unless a deeper mount of the same container covers
// the container path it would be reached at.
func Check(containers []watch.Container, paths []string) error {
	protected := make([]string, len(paths))
	for i, p := range paths {
		r, err := resolve(p)
		if err != nil {
			return err
		}
		protected[i] = r
	}
	for _, c := range containers {
		if c.MountsErr != nil {
			return &UnreadableError{Container: c.ID, Err: c.MountsErr}
		}
		mounts, err := hostMounts(c.ID, c.Mounts)
		if err != nil {
			return err
		}
		for _, p := range protected {
			if err := checkPath(c.ID, mounts, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// hostMounts resolves the mounts that have an absolute host source and
// drops the others, and volumes. The error is an *UnresolvableError, naming
// container id, for the first source that cannot be resolved.
func hostMounts(id string, ms []watch.Mount) ([]mount, error) {
	var out []mount
	for _, m := range ms {
		if m.Type == "volume" || !filepath.IsAbs(m.Source) {
			continue
		}
		source, err := resolve(m.Source)
		if err != nil {
			if u, ok := errors.AsType[*UnresolvableError](err); ok {
				u.Container = id
			}
			return nil, err
		}
		out = append(out, mount{source: source, dest: path.Clean(m.Destination), readOnly: m.ReadOnly})
	}
	return out, nil
}

// checkPath returns a *WritableError for the first of mounts that makes the
// resolved host path p writable in container id, or nil.
func checkPath(id string, mounts []mount, p string) error {
	for i, m := range mounts {
		if m.readOnly {
			continue
		}
		var c string
		if rel, ok := under(m.source, p); ok {
			c = path.Join(m.dest, filepath.ToSlash(rel))
		} else if _, ok := under(p, m.source); ok {
			c = m.dest
		} else {
			continue
		}
		if !shadowed(mounts, i, c) {
			return &WritableError{Container: id, Path: p, Source: m.source, ContainerPath: c}
		}
	}
	return nil
}

// shadowed reports whether a mount other than mounts[i] covers container
// path c from deeper than mounts[i]'s destination.
func shadowed(mounts []mount, i int, c string) bool {
	for j, o := range mounts {
		if j == i || len(o.dest) <= len(mounts[i].dest) {
			continue
		}
		if o.dest == c || strings.HasPrefix(c, o.dest+"/") {
			return true
		}
	}
	return false
}

// under reports whether p is base or lies under it, component-wise, and
// returns p relative to base. Both are clean absolute host paths.
func under(base, p string) (string, bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// maxLinks is how many symlinks one resolution follows at most, as the
// Linux kernel does.
const maxLinks = 40

// resolve makes p absolute and clean and resolves its symlinks, following
// dangling ones to their targets; a relative target is taken against the
// link's directory. Where a component does not exist, the missing rest is
// appended to what is resolved so far. A resolution that takes more than
// maxLinks links, as a loop does, or meets a component that cannot be
// inspected or a link that cannot be read, for any reason but the component
// not existing, gives an *UnresolvableError: an unreadable link could lead
// anywhere, a writable place included.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p), nil
	}
	root := string(filepath.Separator)
	cur, rest, links := root, components(abs), 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		if name == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return filepath.Join(append([]string{next}, rest...)...), nil
		case err != nil:
			return "", &UnresolvableError{Path: abs, Err: err}
		case fi.Mode()&fs.ModeSymlink == 0:
			cur = next
			continue
		}
		if links++; links > maxLinks {
			return "", &UnresolvableError{Path: abs, Err: syscall.ELOOP}
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", &UnresolvableError{Path: abs, Err: err}
		}
		if filepath.IsAbs(target) {
			cur = root
		}
		rest = append(components(target), rest...)
	}
	return cur, nil
}

// components splits p into its names, dropping empty ones and ".". It
// keeps "..", which only a walk that knows where links lead can apply.
func components(p string) []string {
	var out []string
	for _, name := range strings.Split(p, string(filepath.Separator)) {
		if name != "" && name != "." {
			out = append(out, name)
		}
	}
	return out
}
