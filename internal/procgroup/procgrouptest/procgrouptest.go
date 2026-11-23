//go:build unix

// Package procgrouptest provides helpers for tests of code that runs
// commands with package procgroup: a command that leaves a child behind,
// reading the process IDs such commands record, and telling when a process
// has ended.
package procgrouptest

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Margin is the slack over procgroup.WaitDelay that a test allows a command
// the package bounds.
const Margin = 1500 * time.Millisecond

// wait is how long WaitPID and Gone wait.
const wait = 5 * time.Second

// ReadPID reads the process ID that a command recorded in the file path. It
// does not wait: ok is false while the file is missing or holds no process ID.
func ReadPID(path string) (pid int, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid, err == nil
}

// WaitPID waits up to five seconds until the file path holds a process ID
// and returns it; it fails the test otherwise.
func WaitPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if pid, ok := ReadPID(path); ok {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no process ID in %s", path)
	return 0
}

// Gone reports whether the process pid ends within five seconds. A zombie
// has ended: kill with signal 0 still succeeds on one, and where nothing
// reaps orphans (a container whose PID 1 does not) a killed process stays
// one.
func Gone(pid int) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) || isZombie(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// isZombie reports whether /proc shows the process pid as a zombie, which is
// the field after the last ')' of its stat file (the command name, in
// parentheses, may itself hold one).
func isZombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	stat := string(raw)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(stat[end+1:])
	return len(fields) > 0 && fields[0] == "Z"
}

// LeakyCommand writes an executable shell script into a directory of the
// test and returns its path and the path of a file that will hold the process
// ID of the child it starts. The script starts a background child that
// inherits its output and so holds it open, records the child's process ID,
// prints each line of stdout, then runs the shell commands tail (for example
// "wait" or "exit 0"). The child is killed when the test ends.
func LeakyCommand(t *testing.T, stdout []string, tail string) (binary, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "leakyrt")
	pidFile = filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\nsleep 60 &\necho $! >" + shellQuote(pidFile) + "\n"
	for _, line := range stdout {
		script += "printf '%s\\n' " + shellQuote(line) + "\n"
	}
	script += tail + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pid, ok := ReadPID(pidFile); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return binary, pidFile
}

// shellQuote quotes s as one word of a POSIX shell script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
