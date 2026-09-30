package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// TestCommandRunsInOpenedDirectoryAfterSwap covers the window between the
// cwd check and the start of the command: a symlink swapped into the path
// in that window must not redirect the command.
func TestCommandRunsInOpenedDirectoryAfterSwap(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside := filepath.Join(base, "app"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mapper, err := workspace.NewMapper(root, "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	dir, hostPath, err := mapper.Open("/workspaces/app/sub")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	sub := filepath.Join(root, "sub")
	if err := os.Rename(sub, sub+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}

	client, server := net.Pipe()
	defer client.Close()
	frames := make(chan protocol.Frame, 1)
	go func() {
		if f, err := protocol.ReadFrame(client); err == nil {
			frames <- f
		}
		close(frames)
	}()
	cmd := newCommand(context.Background(), []string{"pwd", "-P"}, dir, hostPath, &frameWriter{conn: server})
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	f, ok := <-frames
	if !ok {
		t.Fatal("no output from the command")
	}
	if got := strings.TrimSpace(string(f.Payload)); got != sub+"-old" {
		t.Fatalf("command ran in %q, want the opened directory %q", got, sub+"-old")
	}
}
