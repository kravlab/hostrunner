package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// MountPoint is one mount of a live mount table.
type MountPoint struct {
	Path     string // the mount point as the process sees it, in the container
	ReadOnly bool   // whether this mount is read-only: its own options say ro, not those of its filesystem
}

// ErrProcessGone is why a mount table cannot be read: the process whose
// table was asked for does not exist (any more).
var ErrProcessGone = errors.New("the process does not exist")

// MountTable returns the live mount table of the process pid, read from
// /proc/<pid>/mountinfo on the host, in the order of mountinfo. docker and
// podman both run their containers' processes there, so it is the same for
// both. Once ctx is done, it reads nothing and returns ctx's error; the read
// itself does not wait, so ctx does not bound it.
func (c CLI) MountTable(ctx context.Context, pid int) ([]MountPoint, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("mount table of process %d: not a process ID", pid)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, err)
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, ErrProcessGone)
	}
	if err != nil {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, err)
	}
	table, err := parseMountInfo(string(raw))
	if err != nil {
		return nil, fmt.Errorf("mount table of process %d: %w", pid, err)
	}
	return table, nil
}

// parseMountInfo reads the text of a /proc/<pid>/mountinfo (see proc(5)),
// one mount per line, in the order given.
func parseMountInfo(text string) ([]MountPoint, error) {
	var table []MountPoint
	for line := range strings.Lines(text) {
		fields := strings.Fields(line)
		// Six fields precede the optional ones, which a "-" ends.
		if len(fields) < 7 || !slices.Contains(fields[6:], "-") {
			return nil, fmt.Errorf("malformed mountinfo line %q", strings.TrimRight(line, "\r\n"))
		}
		table = append(table, MountPoint{
			Path:     unescapeMountInfo(fields[4]),
			ReadOnly: slices.Contains(strings.Split(fields[5], ","), "ro"),
		})
	}
	return table, nil
}

// unescapeMountInfo decodes the octal escapes (a backslash and three
// digits, as \040 for a space) the kernel writes a field of mountinfo with.
// A backslash that starts no escape stays as it is.
func unescapeMountInfo(field string) string {
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+4 <= len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}
