package watchtest_test

import (
	"context"
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
