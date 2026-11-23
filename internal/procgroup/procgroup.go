//go:build unix

// Package procgroup runs commands in a process group of their own, so that
// cancelling one ends whatever it spawned too and a process that holds its
// output open cannot keep the caller waiting.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay bounds how long a command's output is still read after it exits
// or is cancelled, so a process holding the output open cannot keep the
// caller waiting.
const WaitDelay = 2 * time.Second

// InOwnGroup starts cmd in a process group of its own, so that cancelling
// it kills whatever it spawned too (e.g. git's ssh), and waits at most
// WaitDelay for its output after it exits or is cancelled. A cancellation
// that finds the group gone, as the command already exited and was reaped,
// is reported as os.ErrProcessDone, so that os/exec does not fail a command
// that finished as its context ended. It must be called before cmd starts.
func InOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = WaitDelay
}
