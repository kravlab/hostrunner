package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/shared"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

// A panic in the watch of one daemon of the shared process ends that
// daemon alone: the other one still runs commands and approval sessions,
// and the panic, with its stack, reaches the log of the daemon that
// panicked and the process log, not the other daemon's log.
func TestSharedProcessEndsOnlyTheDaemonWhoseWatchPanics(t *testing.T) {
	cfgs := map[string]serveConfig{"a": sharedWatchConfig(t), "b": sharedWatchConfig(t)}
	runtimes := map[string]*watchtest.Runtime{"a": {}, "b": {}}
	prepare := func(args, _ []string) (shared.Daemon, error) {
		return newSharedDaemon(cfgs[args[0]], []watch.Runtime{runtimes[args[0]]})
	}
	dir, err := os.MkdirTemp("", "hrs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	control := filepath.Join(dir, "c.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	processLog := &logBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- shared.Serve(ctx, l, prepare, time.Minute, slog.New(slog.NewTextHandler(processLog, nil)))
	}()
	for _, name := range []string{"a", "b"} {
		if err := shared.Add(ctx, control, protocol.AddDaemon{Args: []string{name}}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		waitListening(t, cfgs[name].socket)
		waitListening(t, launch.ApprovalSocket(filepath.Dir(cfgs[name].socket)))
	}

	runtimes["a"].Panic("the runtime query broke")
	processLog.waitFor(t, `msg="daemon ended" socket=`+cfgs["a"].socket)
	if c, err := net.Dial("unix", cfgs["a"].socket); err == nil {
		c.Close()
		t.Error("the daemon whose watch panicked still accepts connections")
	}
	b := cfgs["b"]
	tr := transport.Unix{Path: b.socket}
	if code := client.Run(context.Background(), tr, []string{"true"}, b.containerWorkspace, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Errorf("the other daemon ran an allowed command with exit %d, want 0", code)
	}
	approvalSession(t, b)
	select {
	case err := <-served:
		t.Fatalf("the process ended with the daemon that panicked: %v", err)
	default:
	}
	for name, log := range map[string]string{"daemon": daemonLog(t, cfgs["a"]), "process": processLog.String()} {
		if !strings.Contains(log, "the runtime query broke") || !strings.Contains(log, "watchtest.go") {
			t.Errorf("the %s log lacks the panic and its stack:\n%s", name, log)
		}
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("shared.Serve: %v", err)
	}
	if log := daemonLog(t, b); strings.Contains(log, "the runtime query broke") {
		t.Errorf("the other daemon's log has the panic:\n%s", log)
	}
}

// sharedWatchConfig is a watchConfig whose watch waits for its container
// for longer than any test, and whose rules allow `true` with no
// arguments, unchecked by the mount check, so the daemon serves until it
// is stopped.
func sharedWatchConfig(t *testing.T) serveConfig {
	t.Helper()
	cfg := watchConfig(t)
	cfg.StartupTimeout = time.Hour
	cfg.MountCheck = false
	cfg.RulesFile = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: true\n    args: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// approvalSession serves the daemon of cfg from an approval session,
// which must see, within 5 s, its hello answered with FrameDaemonInfo,
// its FrameServe with FrameServing and then its FrameStanding with a
// FrameStandingList without error.
func approvalSession(t *testing.T, cfg serveConfig) {
	t.Helper()
	c, err := transport.Unix{Path: launch.ApprovalSocket(filepath.Dir(cfg.socket))}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(c); err != nil || f.Type != protocol.FrameDaemonInfo {
		t.Fatalf("hello: frame %v, err %v; want FrameDaemonInfo", f.Type, err)
	}
	if err := protocol.WriteJSON(c, protocol.FrameServe, protocol.Serve{}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(c); err != nil || f.Type != protocol.FrameServing {
		t.Fatalf("serve: frame %v, err %v; want FrameServing", f.Type, err)
	}
	if err := protocol.WriteFrame(c, protocol.Frame{Type: protocol.FrameStanding}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	if err != nil || f.Type != protocol.FrameStandingList {
		t.Fatalf("standing: frame %v, err %v; want FrameStandingList", f.Type, err)
	}
	var list protocol.StandingList
	if err := protocol.DecodeJSON(f, &list); err != nil || list.Error != "" {
		t.Fatalf("standing: list %+v, err %v; want one served", list, err)
	}
}

// daemonLog returns the daemon.log of the daemon of cfg, beside its
// socket.
func daemonLog(t *testing.T, cfg serveConfig) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.socket), launch.LogName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
