package watchtest_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

func containers(t *testing.T, r *watchtest.Runtime) []watch.Container {
	t.Helper()
	got, err := r.Containers(context.Background(), "/home/u/app")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSetMountsAreKeptAcrossStartAndStop(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	want := []watch.Mount{{Source: "/home/u/app", Destination: "/workspace"}, {Destination: "/tmp", ReadOnly: true}}
	r.SetMounts("c", want...)
	r.Stop("c")
	r.Start("c")
	got := containers(t, r)
	if len(got) != 1 || !slices.Equal(got[0].Mounts, want) || got[0].MountsErr != nil {
		t.Fatalf("got %+v; want mounts %v, no error", got, want)
	}
}

func TestSetMountsOfUnknownContainerApplyWhenItStarts(t *testing.T) {
	r := &watchtest.Runtime{}
	want := []watch.Mount{{Source: "/home/u/app", Destination: "/workspace"}}
	r.SetMounts("c", want...)
	if got := containers(t, r); len(got) != 0 {
		t.Fatalf("got %+v before the start; want no containers", got)
	}
	r.Start("c")
	if got := containers(t, r); len(got) != 1 || !slices.Equal(got[0].Mounts, want) {
		t.Fatalf("got %+v; want mounts %v", got, want)
	}
}

func TestContainersReturnCopiesOfMounts(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	in := []watch.Mount{{Destination: "/workspace"}}
	r.SetMounts("c", in...)
	in[0].Destination = "/changed by the caller of SetMounts"
	containers(t, r)[0].Mounts[0].Destination = "/changed by the caller of Containers"
	if got := containers(t, r)[0].Mounts; got[0].Destination != "/workspace" {
		t.Fatalf("got %v; want the mounts unchanged", got)
	}
}

func TestFailMountsReportsAnErrorForThatContainerOnly(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("a")
	r.Start("b")
	r.SetMounts("a", watch.Mount{Destination: "/workspace"})
	r.FailMounts("a", true)
	got := containers(t, r)
	if got[0].MountsErr == nil || got[1].MountsErr != nil {
		t.Fatalf("got %v, %v; want an error for a only", got[0].MountsErr, got[1].MountsErr)
	}
	r.FailMounts("a", false)
	if got := containers(t, r); got[0].MountsErr != nil || len(got[0].Mounts) != 1 {
		t.Fatalf("got %+v; want a's mounts back without an error", got[0])
	}
}

// A volume mount carries the volume SetVolume gives for its name, as a
// copy; a volume no SetVolume names, or one FailVolume fails, is reported
// as one that cannot be inspected.
func TestVolumeMountsCarryTheirVolumes(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	r.SetMounts("c",
		watch.Mount{Type: "volume", Name: "v", Destination: "/v"},
		watch.Mount{Type: "volume", Name: "unknown", Destination: "/u"},
		watch.Mount{Source: "/home/u/app", Destination: "/workspace"})
	r.SetVolume("v", watch.Volume{Driver: "local", Options: map[string]string{"device": "/home/u/v"}})
	got := containers(t, r)[0].Mounts
	if v := got[0].Volume; v == nil || v.Driver != "local" || v.Options["device"] != "/home/u/v" || got[0].VolumeErr != nil {
		t.Fatalf("v: got %+v; want its volume", got[0])
	}
	if got[1].Volume != nil || got[1].VolumeErr == nil {
		t.Fatalf("unknown: got %+v; want an error", got[1])
	}
	if got[2].Volume != nil || got[2].VolumeErr != nil {
		t.Fatalf("the bind mount: got %+v; want no volume", got[2])
	}
	got[0].Volume.Options["device"] = "/changed by the caller of Containers"
	if v := containers(t, r)[0].Mounts[0].Volume; v.Options["device"] != "/home/u/v" {
		t.Fatalf("got %+v; want the volume unchanged", v)
	}
	r.FailVolume("v", true)
	if m := containers(t, r)[0].Mounts[0]; m.Volume != nil || m.VolumeErr == nil {
		t.Fatalf("failed v: got %+v; want an error", m)
	}
	r.FailVolume("v", false)
	if m := containers(t, r)[0].Mounts[0]; m.Volume == nil || m.VolumeErr != nil {
		t.Fatalf("v again: got %+v; want its volume", m)
	}
}

// Panic makes the next Containers call panic with the value given, and
// that call only.
func TestPanicMakesTheNextContainersCallPanic(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Panic("runtime panic")
	func() {
		defer func() {
			if got := recover(); got != "runtime panic" {
				t.Fatalf("recovered %v; want the panic value", got)
			}
		}()
		containers(t, r)
		t.Fatal("Containers returned; want a panic")
	}()
	containers(t, r)
}

// Every Start gives the container a new pid, distinct from every pid given
// before, also to the container it restarts.
func TestStartGivesEveryContainerANewPid(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("a")
	r.Start("b")
	first := containers(t, r)
	r.Stop("a")
	r.Start("a")
	again := containers(t, r)
	pids := []int{first[0].Pid, first[1].Pid, again[0].Pid}
	for _, pid := range pids {
		if pid <= 0 {
			t.Fatalf("got pids %v; want every one positive", pids)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(pids)))) != len(pids) {
		t.Fatalf("got pids %v; want them distinct", pids)
	}
}

// A stopped container has no process: its pid is 0.
func TestStopClearsThePid(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	r.Stop("c")
	if got := containers(t, r); got[0].Running || got[0].Pid != 0 {
		t.Fatalf("got %+v; want a stopped container without a pid", got[0])
	}
}

func mountTable(t *testing.T, r *watchtest.Runtime, pid int) []watch.MountPoint {
	t.Helper()
	got, err := r.MountTable(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// MountTable of the pid of a running container returns the table
// SetMountTable set for it, and a later SetMountTable changes the next
// answer.
func TestMountTableOfTheCurrentPidReturnsTheSetTable(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	pid := containers(t, r)[0].Pid
	want := []watch.MountPoint{{Path: "/"}, {Path: "/workspace", ReadOnly: true}}
	r.SetMountTable("c", want...)
	if got := mountTable(t, r, pid); !slices.Equal(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	next := []watch.MountPoint{{Path: "/"}}
	r.SetMountTable("c", next...)
	if got := mountTable(t, r, pid); !slices.Equal(got, next) {
		t.Fatalf("got %v after the change; want %v", got, next)
	}
}

// A process that does not run any more, or never did, is gone: the old pid
// of a restarted container, the pid of a stopped or removed container, and
// pid 0.
func TestMountTableOfAProcessThatDoesNotRunIsGone(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("restarted")
	r.Start("stopped")
	r.Start("removed")
	first := containers(t, r)
	r.Stop("restarted")
	r.Start("restarted")
	r.Stop("stopped")
	r.Remove("removed")
	for name, pid := range map[string]int{
		"old pid after a restart": first[0].Pid,
		"stopped":                 first[1].Pid,
		"removed":                 first[2].Pid,
		"pid 0":                   0,
	} {
		if _, err := r.MountTable(context.Background(), pid); !errors.Is(err, watch.ErrProcessGone) {
			t.Errorf("%s: got error %v; want ErrProcessGone", name, err)
		}
	}
}

// Without a SetMountTable, the live table has one point per mount SetMounts
// set, as a container just made has; SetMountTable then replaces it.
func TestMountTableDefaultsToTheMounts(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	r.SetMounts("c",
		watch.Mount{Source: "/home/u/app", Destination: "/workspace/"},
		watch.Mount{Destination: "/tmp", ReadOnly: true})
	pid := containers(t, r)[0].Pid
	want := []watch.MountPoint{{Path: "/workspace"}, {Path: "/tmp", ReadOnly: true}}
	if got := mountTable(t, r, pid); !slices.Equal(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	r.SetMountTable("c")
	if got := mountTable(t, r, pid); len(got) != 0 {
		t.Fatalf("got %v after SetMountTable with no points; want an empty table", got)
	}
}

// FailMountTable makes the table of that container's process unreadable,
// with an error that does not say the process is gone, and readable again.
func TestFailMountTableReportsAnErrorForThatContainerOnly(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("a")
	r.Start("b")
	r.SetMountTable("a", watch.MountPoint{Path: "/workspace"})
	got := containers(t, r)
	r.FailMountTable("a", true)
	_, err := r.MountTable(context.Background(), got[0].Pid)
	if err == nil || errors.Is(err, watch.ErrProcessGone) {
		t.Fatalf("got error %v; want one that is not ErrProcessGone", err)
	}
	if _, err := r.MountTable(context.Background(), got[1].Pid); err != nil {
		t.Fatalf("b: got error %v; want none", err)
	}
	r.FailMountTable("a", false)
	if table := mountTable(t, r, got[0].Pid); len(table) != 1 {
		t.Fatalf("got %v; want a's table back", table)
	}
}

func TestMountTableReturnsCopies(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	pid := containers(t, r)[0].Pid
	in := []watch.MountPoint{{Path: "/workspace"}}
	r.SetMountTable("c", in...)
	in[0].Path = "/changed by the caller of SetMountTable"
	mountTable(t, r, pid)[0].Path = "/changed by the caller of MountTable"
	if got := mountTable(t, r, pid); got[0].Path != "/workspace" {
		t.Fatalf("got %v; want the table unchanged", got)
	}
}

func TestDefaultMountTableReturnsCopies(t *testing.T) {
	r := &watchtest.Runtime{}
	r.Start("c")
	r.SetMounts("c", watch.Mount{Destination: "/workspace"})
	pid := containers(t, r)[0].Pid
	mountTable(t, r, pid)[0].Path = "/changed by the caller of MountTable"
	if got := mountTable(t, r, pid); got[0].Path != "/workspace" {
		t.Fatalf("got %v; want the table unchanged", got)
	}
}

// SetMountTable of an id that does not exist yet applies once it starts, and
// is kept across Stop and Start.
func TestSetMountTableOfUnknownContainerAppliesWhenItStarts(t *testing.T) {
	r := &watchtest.Runtime{}
	want := []watch.MountPoint{{Path: "/workspace", ReadOnly: true}}
	r.SetMountTable("c", want...)
	r.Start("c")
	r.Stop("c")
	r.Start("c")
	if got := mountTable(t, r, containers(t, r)[0].Pid); !slices.Equal(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
}
