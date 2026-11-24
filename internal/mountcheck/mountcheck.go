// Package mountcheck tells whether a devcontainer can write host paths the
// daemon protects, judging from the container's mounts as its runtime
// reports them and, where the container's live mount table was read, from
// that table, which decides what the mounts are now.
package mountcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
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

// Mounts is what Read reads of the mounts of a list of containers, for
// Check and CheckRequirements to judge: once read, it is judged by both
// without reading the containers or resolving their mount sources again.
type Mounts struct {
	containers []containerMounts
}

// containerMounts is one container as Read reads it: err, why what it can
// write is unknown, or else its mounts with a host source, resolved, and
// its mount points.
type containerMounts struct {
	id     string
	err    error
	mounts []mount
	points []string
}

// Read reads the mounts of containers, in order, for Check and
// CheckRequirements to judge: for each container, the mounts its live
// mount table still holds (see Mounts.Check), with their host sources resolved,
// and its mount points. It does not fail: why a container's mounts are
// unknown, an *UnreadableError for mounts, a live table or a volume that
// could not be read or an *UnresolvableError for a mount source that could
// not be resolved, is kept for the checks to report in their order.
func Read(containers []watch.Container) Mounts {
	out := make([]containerMounts, len(containers))
	for i, c := range containers {
		out[i] = readContainer(c)
	}
	return Mounts{containers: out}
}

// readContainer reads the mounts of c as Read does, its inspection's
// failure before its live table's, and both before its mount sources.
func readContainer(c watch.Container) containerMounts {
	switch {
	case c.MountsErr != nil:
		return containerMounts{id: c.ID, err: &UnreadableError{Container: c.ID, Err: c.MountsErr}}
	case c.Live != nil && c.Live.Err != nil:
		return containerMounts{id: c.ID, err: &UnreadableError{Container: c.ID, Err: c.Live.Err}}
	}
	counted := liveMounts(c.Mounts, c.Live)
	mounts, err := hostMounts(c.ID, counted)
	if err != nil {
		return containerMounts{id: c.ID, err: err}
	}
	return containerMounts{id: c.ID, mounts: mounts, points: mountPoints(counted, c.Live)}
}

// Check returns nil when none of the containers m was read from can write
// any of paths (host paths), else the first failure found, containers in
// order then paths in order: an *UnreadableError for a container whose
// mounts, or live mount table, could not be read, else a *WritableError.
// An *UnresolvableError for a protected path comes before all of them, one
// for a mount source right after its container's mounts are read.
//
// A container with a live mount table (watch.Container.Live) is not judged
// by its inspection alone, which can be stale: a mount that inspection
// reports counts only when the table has a point at its cleaned
// destination, and it is read-only when the topmost such point, the last
// in the table, is. A mount without a point there vanished from the
// container and is dropped, so it shadows nothing; points that inspection
// does not report hold no host source and are ignored. This holds for
// every kind of mount, volumes included: a volume is still judged by its
// device, and only its presence and read-only flag come from the table. A
// container without a table is judged by its inspection alone.
//
// Protected paths and mount sources are compared after resolving symlinks,
// dangling ones included; a path that does not exist is resolved through
// its nearest existing ancestor, so a writable parent of a missing file
// fails the check. Mounts without an absolute host source (tmpfs, an empty
// or relative source) are ignored. A volume (by its Type) is not judged by
// its source, which lies in the runtime's storage, which the daemon may not
// read, but by its inspection (watch.Mount.Volume): a local volume whose
// options name an absolute device, as one created with type=none, o=bind
// (or rbind) and a host directory binds it, is checked as a mount of that
// device, resolved, with the mount's own read-only flag; one whose device
// is no host path (a tmpfs, an NFS export) or that names none holds only
// what the runtime puts there and is ignored. A volume that could not be
// inspected, or of another driver whose options name a device, which the
// daemon cannot tell the content of, gives an *UnreadableError. A path, a
// bind mount source or a device with a component that cannot be read,
// other than one that does not exist, fails the check with an
// *UnresolvableError, as what an unreadable link leads to is unknown.
// A writable mount reaches a protected path when one lies under
// the other, unless a deeper mount of the same container covers
// the container path it would be reached at.
func (m Mounts) Check(paths []string) error {
	protected := make([]string, len(paths))
	for i, p := range paths {
		r, err := resolve(p)
		if err != nil {
			return err
		}
		protected[i] = r
	}
	for _, c := range m.containers {
		if c.err != nil {
			return c.err
		}
		for _, p := range protected {
			if err := checkPath(c.id, c.mounts, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// liveMounts returns the mounts of ms that the live mount table l holds,
// those with a point at their cleaned destination, each with the read-only
// flag of the topmost such point, the last in the table: it is mounted over
// the others. Without a table (l is nil), it returns ms.
func liveMounts(ms []watch.Mount, l *watch.LiveMounts) []watch.Mount {
	if l == nil {
		return ms
	}
	var out []watch.Mount
	for _, m := range ms {
		for _, p := range slices.Backward(l.Points) {
			if p.Path == m.PointPath() {
				m.ReadOnly = p.ReadOnly
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// hostMounts resolves the mounts that have an absolute host source, a
// volume's being its device (see volumeSource), and drops the others. The
// error is an *UnreadableError, naming container id, for the first volume
// it cannot tell the content of, or an *UnresolvableError, naming it too,
// for the first source that cannot be resolved.
func hostMounts(id string, ms []watch.Mount) ([]mount, error) {
	var out []mount
	for _, m := range ms {
		src := m.Source
		if m.Type == watch.MountTypeVolume {
			var err error
			if src, err = volumeSource(m); err != nil {
				return nil, &UnreadableError{Container: id, Err: err}
			}
		}
		if !filepath.IsAbs(src) {
			continue
		}
		source, err := resolve(src)
		if err != nil {
			if u, ok := errors.AsType[*UnresolvableError](err); ok {
				u.Container = id
			}
			return nil, err
		}
		out = append(out, mount{source: source, dest: m.PointPath(), readOnly: m.ReadOnly})
	}
	return out, nil
}

// volumeSource returns the host source of the volume mount m: the device
// its options name, for a local volume ("" when it names none, which
// hostMounts drops as it does a device that is not an absolute path), or
// why the content of the volume is unknown: it was not inspected, or it is
// of another driver and names a device. Whatever its type and o options
// say, block devices included, every local volume with an absolute device
// is judged as a bind of that path: a safe superset, as no volume that
// could show a protected path through its device goes unchecked.
func volumeSource(m watch.Mount) (string, error) {
	switch {
	case m.VolumeErr != nil:
		return "", m.VolumeErr
	case m.Volume == nil:
		return "", fmt.Errorf("volume %s at %s was not inspected", m.Name, m.Destination)
	}
	device, ok := m.Volume.Options[watch.VolumeOptionDevice]
	if ok && m.Volume.Driver != watch.DriverLocal {
		return "", fmt.Errorf("volume %s at %s, of driver %s, names the device %s, whose content the daemon cannot tell", m.Name, m.Destination, m.Volume.Driver, device)
	}
	return device, nil
}

// checkPath returns a *WritableError for the first of mounts that makes the
// resolved host path p writable in container id, or nil.
func checkPath(id string, mounts []mount, p string) error {
	for i, m := range mounts {
		if m.readOnly {
			continue
		}
		c, ok := reachedAt(m, p)
		if !ok {
			if _, inside := under(p, m.source); !inside {
				continue
			}
			c = m.dest
		}
		if !shadowed(mounts, i, c) {
			return &WritableError{Container: id, Path: p, Source: m.source, ContainerPath: c}
		}
	}
	return nil
}

// reachedAt returns the container path at which m shows the resolved host
// path p, when p is the source of m or lies under it: the destination of m
// joined with p relative to the source. It does not look at the other
// mounts of the container, which may cover that container path (see
// shadowed).
func reachedAt(m mount, p string) (string, bool) {
	rel, ok := under(m.source, p)
	if !ok {
		return "", false
	}
	return path.Join(m.dest, filepath.ToSlash(rel)), true
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
