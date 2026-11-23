//go:build unix

package procgroup_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/procgroup"
	"github.com/kravlab/hostrunner/internal/procgroup/procgrouptest"
)

// killOnCleanup kills the process pid when the test ends, if it still runs.
func killOnCleanup(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}

func TestInOwnGroupCancellingKillsEveryProcessOfTheGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 60 & echo $! >"$1"; wait`, "sh", pidFile)
	procgroup.InOwnGroup(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	child := procgrouptest.WaitPID(t, pidFile)
	killOnCleanup(t, child)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got no error from a cancelled command")
		}
	case <-time.After(procgroup.WaitDelay + procgrouptest.Margin):
		t.Fatal("the cancelled command did not return")
	}
	if !procgrouptest.Gone(child) {
		t.Fatalf("the child %d of the cancelled command still runs", child)
	}
}

func TestInOwnGroupBoundsTheWaitForOutputAnExitedCommandLeavesOpen(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `sleep 60 & echo $! >"$1"`, "sh", pidFile)
	var out bytes.Buffer
	cmd.Stdout = &out
	procgroup.InOwnGroup(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	// The child outlives the command, so it is killed when the test ends.
	t.Cleanup(func() {
		if pid, ok := procgrouptest.ReadPID(pidFile); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	select {
	case err := <-done:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("got %v, want %v", err, exec.ErrWaitDelay)
		}
	case <-time.After(procgroup.WaitDelay + procgrouptest.Margin):
		t.Fatal("the command did not return while a child held its output open")
	}
}

// A command that finished (and was reaped) before its context ended is not
// failed by the cancellation: Cancel says the process is done.
func TestInOwnGroupCancelOfACommandThatFinishedSaysItIsDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	procgroup.InOwnGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("got %v, want %v", err, os.ErrProcessDone)
	}
}
