package stop_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/settings"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
)

// busyRules allow the sleep a test keeps a daemon busy with.
const busyRules = validRules + "  - command: sleep\n    args: any\n"

// upConfig is the `up` of a test for the runtime directory dir, serving
// workspace with rulesFile; its new daemon is the test binary.
func upConfig(t *testing.T, dir, workspace, rulesFile string) launch.Config {
	t.Helper()
	r := restartConfig(t)
	return launch.Config{
		Dir: dir, Workspace: workspace, ContainerWorkspace: "/workspaces/new",
		Daemon: r.Daemon, Client: r.Client,
		Settings: settings.Settings{RulesFile: rulesFile, Reread: true, ReadyTimeout: 5 * time.Second},
	}
}

// runUpRestart runs `up --restart` for up, its commands treated as
// commands says, and returns what it printed and returned.
func runUpRestart(t *testing.T, up launch.Config, commands stop.Commands) (string, error) {
	t.Helper()
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- stop.UpRestart(context.Background(), up, commands, strings.NewReader(""), &out) }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(15 * time.Second):
		t.Fatal("up --restart did not end")
		return "", nil
	}
}

// keepBusy runs sleep 60 on d until the test ends or the daemon kills it,
// and waits until d lists it running; the returned channel receives its
// exit code.
func (d *currentDaemon) keepBusy(t *testing.T) <-chan int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	code := make(chan int, 1)
	tr := transport.Unix{Path: filepath.Join(d.dir, launch.SocketName)}
	go func() {
		code <- client.Run(ctx, tr, []string{"sleep", "60"}, "/workspaces/app", strings.NewReader(""), io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := transport.Unix{Path: launch.ApprovalSocket(d.dir)}.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var list protocol.RunningList
		_ = protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion})
		_, _ = protocol.ReadFrame(c)
		_ = protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameRunning})
		if f, err := protocol.ReadFrame(c); err == nil {
			_ = protocol.DecodeJSON(f, &list)
		}
		c.Close()
		if len(list.Commands) > 0 {
			return code
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpRestartWithoutADaemonStartsOne(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if _, err := runUpRestart(t, upConfig(t, dir, ws, filepath.Join(ws, "missing.yaml")), stop.Commands{}); err != nil {
		t.Fatalf("up --restart: %v", err)
	}
	if args := newDaemonArgs(t, dir); args == nil {
		t.Fatal("no daemon started")
	}
}

// The new daemon serves what `up` names, its container about to start.
func TestUpRestartReplacesAnIdleDaemon(t *testing.T) {
	d := startCurrentDaemon(t, validRules, true)
	ws := t.TempDir()
	rulesFile := filepath.Join(ws, "hostrun.yaml")
	if err := os.WriteFile(rulesFile, []byte(validRules), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runUpRestart(t, upConfig(t, d.dir, ws, rulesFile), stop.Commands{})
	if err != nil {
		t.Fatalf("up --restart: %v; printed %q", err, out)
	}
	if out != "hostrunner: restarted "+d.dir+"\n" {
		t.Fatalf("printed %q", out)
	}
	d.waitStopped(t)
	args := strings.Join(newDaemonArgs(t, d.dir), " ")
	if !strings.Contains(args, "--workspace "+ws+" --container-workspace /workspaces/new --rules-file "+rulesFile) {
		t.Fatalf("the new daemon runs %q", args)
	}
}

func TestUpRestartOfABusyDaemonWithoutKillFailsAndLeavesIt(t *testing.T) {
	d := startCurrentDaemon(t, busyRules, false)
	code := d.keepBusy(t)
	_, err := runUpRestart(t, upConfig(t, d.dir, d.workspace, d.rulesFile), stop.Commands{})
	if err == nil || !strings.Contains(err.Error(), "--kill or --wait") {
		t.Fatalf("got %v, want an error naming --kill and --wait", err)
	}
	d.stillServes(t)
	select {
	case c := <-code:
		t.Fatalf("the command ended with %d", c)
	default:
	}
	if args := newDaemonArgs(t, d.dir); args != nil {
		t.Fatalf("a new daemon started: %q", args)
	}
}

func TestUpRestartWithKillReplacesABusyDaemon(t *testing.T) {
	d := startCurrentDaemon(t, busyRules, false)
	code := d.keepBusy(t)
	if out, err := runUpRestart(t, upConfig(t, d.dir, d.workspace, d.rulesFile), stop.Commands{Answer: stop.Kill}); err != nil {
		t.Fatalf("up --restart --kill: %v; printed %q", err, out)
	}
	d.waitStopped(t)
	select {
	case <-code:
	case <-time.After(5 * time.Second):
		t.Fatal("the running command was not killed")
	}
	if args := newDaemonArgs(t, d.dir); args == nil {
		t.Fatal("no new daemon started")
	}
}

func TestUpRestartWithInvalidRulesLeavesTheDaemon(t *testing.T) {
	d := startCurrentDaemon(t, validRules, false)
	if err := os.WriteFile(d.rulesFile, []byte("rules:\n  - command: git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runUpRestart(t, upConfig(t, d.dir, d.workspace, d.rulesFile), stop.Commands{}); err == nil || !strings.Contains(err.Error(), "rules file") {
		t.Fatalf("got %v, want the rules file refused", err)
	}
	d.stillServes(t)
}
