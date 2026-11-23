//go:build unix

package procgrouptest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/procgroup/procgrouptest"
)

func TestReadPID(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name string
		path string
		pid  int
		ok   bool
	}{
		{"missing file", filepath.Join(dir, "missing"), 0, false},
		{"empty file", write("empty", ""), 0, false},
		{"not a number", write("garbage", "soon\n"), 0, false},
		{"a process ID", write("pid", "4242\n"), 4242, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, ok := procgrouptest.ReadPID(tt.path)
			if pid != tt.pid || ok != tt.ok {
				t.Fatalf("got %d, %v; want %d, %v", pid, ok, tt.pid, tt.ok)
			}
		})
	}
}

func TestWaitPIDWaitsForTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.pid")
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(path, []byte("4242\n"), 0o600)
	}()
	if got := procgrouptest.WaitPID(t, path); got != 4242 {
		t.Fatalf("got %d, want 4242", got)
	}
}

// startZombie starts a command whose child ends at once and stays a zombie,
// as the command does not reap it, while the command runs for three more
// seconds. It returns the child's process ID.
func startZombie(t *testing.T) int {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", `sleep 0 & echo $! >"$1"; exec sleep 3`, "sh", pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return procgrouptest.WaitPID(t, pidFile)
}

func TestGoneCountsAZombieAsEnded(t *testing.T) {
	zombie := startZombie(t)
	start := time.Now()
	if !procgrouptest.Gone(zombie) {
		t.Fatalf("the zombie %d did not count as ended", zombie)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the zombie counted as ended after %v, only when its parent ended", elapsed)
	}
}

func TestGoneCountsAProcessThatIsGoneAsEnded(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !procgrouptest.Gone(cmd.Process.Pid) {
		t.Fatalf("the reaped process %d did not count as ended", cmd.Process.Pid)
	}
}

func TestLeakyCommandPrintsStdoutLeavesALiveChildAndKillsItOnCleanup(t *testing.T) {
	lines := []string{"plain", "it's $HOME `x` \"q\""}
	var child int
	t.Run("run", func(t *testing.T) {
		binary, pidFile := procgrouptest.LeakyCommand(t, lines, "exit 0")
		// The child holds the command's output open, so write to a file
		// instead of a pipe that would not close.
		out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		cmd := exec.Command(binary)
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		if want := "plain\nit's $HOME `x` \"q\"\n"; string(got) != want {
			t.Fatalf("got stdout %q, want %q", got, want)
		}
		child = procgrouptest.WaitPID(t, pidFile)
		if err := syscall.Kill(child, 0); err != nil {
			t.Fatalf("the child %d is not alive: %v", child, err)
		}
	})
	if !procgrouptest.Gone(child) {
		t.Fatalf("the child %d still runs after the test's cleanup", child)
	}
}
