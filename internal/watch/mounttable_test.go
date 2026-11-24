package watch_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/watch"
)

// The read-only flag comes from the per-mount options, never from the
// super options: a read-write mount of a read-only filesystem is writable
// as far as the mount goes, and one marked ro of a read-write one is not.
func TestParseMountInfoTakesReadOnlyFromThePerMountOptions(t *testing.T) {
	text := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 ro,errors=remount-ro
23 22 0:20 / /ro ro,nosuid,nodev - tmpfs tmpfs rw,size=1k
24 22 8:1 /data /data ro,relatime master:2 shared:3 - ext4 /dev/sda1 rw
`
	got, err := watch.ParseMountInfo(text)
	want := []watch.MountPoint{
		{Path: "/"},
		{Path: "/ro", ReadOnly: true},
		{Path: "/data", ReadOnly: true},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v; want %v, nil", got, err, want)
	}
}

// The kernel writes a space, a tab, a newline and a backslash of a mount
// point as an octal escape, so they do not split the line.
func TestParseMountInfoDecodesOctalEscapesInTheMountPoint(t *testing.T) {
	text := `30 22 8:1 / /my\040app rw - ext4 /dev/sda1 rw
31 22 8:1 / /tab\011and\012newline rw - ext4 /dev/sda1 rw
32 22 8:1 / /back\134slash rw - ext4 /dev/sda1 rw
`
	got, err := watch.ParseMountInfo(text)
	want := []watch.MountPoint{{Path: "/my app"}, {Path: "/tab\tand\nnewline"}, {Path: `/back\slash`}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %#v, %v; want %#v, nil", got, err, want)
	}
}

// A line that lacks the mount point or the per-mount options, or has no
// "-" that ends the optional fields, is not a mount table the caller can
// trust: the whole text is an error.
func TestParseMountInfoRejectsAMalformedLine(t *testing.T) {
	for _, tt := range []struct{ name, text string }{
		{"too few fields", "22 1 8:1 / /\n"},
		{"no separator", "22 1 8:1 / / rw shared:1 ext4 /dev/sda1 rw\n"},
		{"a bad line after a good one", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\nnonsense\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := watch.ParseMountInfo(tt.text); err == nil {
				t.Fatalf("got %v, nil; want an error", got)
			}
		})
	}
}

// The table of a live process, the test's own, holds the root mount and
// one entry for each line of its mountinfo.
func TestCLIMountTableReadsTheLiveTableOfAProcess(t *testing.T) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skipf("no /proc/self/mountinfo: %v", err)
	}
	got, err := (watch.CLI{}).MountTable(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Count(string(raw), "\n"); len(got) != want {
		t.Fatalf("got %d mounts; want %d, one for each line of mountinfo", len(got), want)
	}
	if !slices.ContainsFunc(got, func(m watch.MountPoint) bool { return m.Path == "/" }) {
		t.Fatalf("got %v; want the root mount among them", got)
	}
}

// A process that has exited has no mount table: the caller can tell that
// from a failure to read it.
func TestCLIMountTableOfAProcessThatIsGoneIsErrProcessGone(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := (watch.CLI{}).MountTable(context.Background(), cmd.Process.Pid)
	if !errors.Is(err, watch.ErrProcessGone) {
		t.Fatalf("got %v, %v; want an error wrapping ErrProcessGone", got, err)
	}
}

// No process has an ID of 0 or less: asking for one is a mistake of the
// caller, not a process that is gone.
func TestCLIMountTableRejectsAProcessIDThatIsNoProcess(t *testing.T) {
	for _, pid := range []int{0, -1} {
		got, err := (watch.CLI{}).MountTable(context.Background(), pid)
		if err == nil || errors.Is(err, watch.ErrProcessGone) {
			t.Fatalf("pid %d: got %v, %v; want an error that is not ErrProcessGone", pid, got, err)
		}
	}
}

// A live mount table lists a mount at its cleaned destination: a trailing
// slash or a dot segment the runtime reports does not make it another
// point.
func TestMountPointIsTheCleanedDestination(t *testing.T) {
	for _, tt := range []struct{ dest, want string }{
		{"/workspace", "/workspace"},
		{"/workspace/", "/workspace"},
		{"/workspace/./.devcontainer/../.devcontainer", "/workspace/.devcontainer"},
	} {
		if got := (watch.Mount{Destination: tt.dest}).PointPath(); got != tt.want {
			t.Errorf("Mount{Destination: %q}.PointPath() = %q; want %q", tt.dest, got, tt.want)
		}
	}
}

// A query whose context is done reads no table: the caller learns why,
// which does not say the process is gone, as a restart would.
func TestCLIMountTableWithACancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := (watch.CLI{}).MountTable(ctx, os.Getpid())
	if !errors.Is(err, context.Canceled) || errors.Is(err, watch.ErrProcessGone) {
		t.Fatalf("got %v, %v; want an error wrapping context.Canceled, not ErrProcessGone", got, err)
	}
}
