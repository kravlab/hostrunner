package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
)

func TestParseServeAcceptsAllFlags(t *testing.T) {
	got, err := parseServe([]string{"--socket", "/run/h.sock", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := serveConfig{
		socket:         "/run/h.sock",
		workspaceFlags: workspaceFlags{workspace: "/home/u/app", containerWorkspace: "/workspaces/app"},
		config:         "/home/u/app/.devcontainer/hostrun.yaml",
		startupTimeout: 30 * time.Minute,
		grace:          15 * time.Second,
		pollInterval:   2 * time.Second,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseServeAcceptsWatchSettings(t *testing.T) {
	got, err := parseServe([]string{
		"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c",
		"--watch", "--startup-timeout", "5m", "--grace", "3s",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !got.watch || got.startupTimeout != 5*time.Minute || got.grace != 3*time.Second {
		t.Fatalf("got %+v, want watch with 5m startup timeout and 3s grace", got)
	}
}

func TestParseServeRequiresEveryFlag(t *testing.T) {
	full := map[string]string{"--socket": "/run/h.sock", "--workspace": "/home/u/app", "--container-workspace": "/workspaces/app"}
	for missing := range full {
		var args []string
		for flag, value := range full {
			if flag != missing {
				args = append(args, flag, value)
			}
		}
		if _, err := parseServe(args, io.Discard); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

func TestParseServeRejectsPositionalArguments(t *testing.T) {
	args := []string{"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c", "extra"}
	if _, err := parseServe(args, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

func TestParseUpBuildsLaunchConfig(t *testing.T) {
	exe := "/home/u/.local/bin/hostrunner"
	got, err := parseUp([]string{"--dir", "/run/user/1000/hostrunner/abc", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app"}, exe, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := launch.Config{
		Dir:                "/run/user/1000/hostrunner/abc",
		Workspace:          "/home/u/app",
		ContainerWorkspace: "/workspaces/app",
		Rules:              "/home/u/app/.devcontainer/hostrun.yaml",
		Daemon:             exe,
		Client:             filepath.Join("/home/u/.local/bin", "hostrun"),
		ReadyTimeout:       10 * time.Second,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseUpRequiresEveryFlag(t *testing.T) {
	full := map[string]string{"--dir": "/d", "--workspace": "/w", "--container-workspace": "/c"}
	for missing := range full {
		var args []string
		for flag, value := range full {
			if flag != missing {
				args = append(args, flag, value)
			}
		}
		if _, err := parseUp(args, "/bin/hostrunner", io.Discard); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

func TestParseUpRejectsPositionalArguments(t *testing.T) {
	if _, err := parseUp([]string{"--dir", "/d", "--workspace", "/w", "--container-workspace", "/c", "extra"}, "/bin/hostrunner", io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

// fakeRuntime reports whatever the test stores in present.
type fakeRuntime struct{ present atomic.Bool }

func (f *fakeRuntime) Running(context.Context, string) (bool, error) { return f.present.Load(), nil }

// watchConfig is a `serve --watch` over a temporary workspace with short
// timings.
func watchConfig(t *testing.T) serveConfig {
	t.Helper()
	base, err := os.MkdirTemp("", "hrs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	return serveConfig{
		socket:         filepath.Join(base, "h.sock"),
		workspaceFlags: workspaceFlags{workspace: base, containerWorkspace: "/workspaces/app"},
		watch:          true,
		startupTimeout: 400 * time.Millisecond,
		grace:          100 * time.Millisecond,
		pollInterval:   5 * time.Millisecond,
	}
}

// startServe runs serve in the background and returns its result channel
// once the socket accepts connections.
func startServe(t *testing.T, cfg serveConfig, runtimes ...*fakeRuntime) <-chan error {
	t.Helper()
	rts := make([]watch.Runtime, len(runtimes))
	for i, rt := range runtimes {
		rts[i] = rt
	}
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), cfg, slog.New(slog.DiscardHandler), rts) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.Dial("unix", cfg.socket); err == nil {
			c.Close()
			return done
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not listen")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expectServeExit waits for serve to return nil and checks the socket is gone.
func expectServeExit(t *testing.T, done <-chan error, socket string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve --watch did not exit")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestServeWatchExitsWhenContainerStops(t *testing.T) {
	cfg := watchConfig(t)
	rt := &fakeRuntime{}
	rt.present.Store(true)
	done := startServe(t, cfg, rt)
	time.Sleep(50 * time.Millisecond)
	rt.present.Store(false)
	expectServeExit(t, done, cfg.socket)
}

func TestServeWatchExitsWhenContainerNeverStarts(t *testing.T) {
	cfg := watchConfig(t)
	expectServeExit(t, startServe(t, cfg, &fakeRuntime{}), cfg.socket)
}

func TestServeWatchWaitsAgainWhenArmed(t *testing.T) {
	cfg := watchConfig(t)
	rt := &fakeRuntime{}
	rt.present.Store(true)
	done := startServe(t, cfg, rt)
	time.Sleep(50 * time.Millisecond)

	rt.present.Store(false) // rebuild: old container removed, image building
	if restart := armServe(t, cfg, rulesDigest(t, cfg)); restart {
		t.Fatal("serve asked for a restart although the rules did not change")
	}

	select {
	case err := <-done:
		t.Fatalf("serve exited (%v) within the grace period after being armed", err)
	case <-time.After(3 * cfg.grace):
	}
	rt.present.Store(true) // the rebuilt container starts
	time.Sleep(cfg.startupTimeout)
	select {
	case err := <-done:
		t.Fatalf("serve exited (%v) while the rebuilt container runs", err)
	default:
	}
	rt.present.Store(false)
	expectServeExit(t, done, cfg.socket)
}

func TestParseServeAcceptsConfigPath(t *testing.T) {
	got, err := parseServe([]string{"--socket", "/s", "--workspace", "/w", "--container-workspace", "/c", "--config", "/etc/rules.yaml"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.config != "/etc/rules.yaml" {
		t.Fatalf("config = %q, want /etc/rules.yaml", got.config)
	}
}

func TestServeRefusesInvalidConfig(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.config = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.config, []byte("rules:\n  - command: git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := serve(context.Background(), cfg, slog.New(slog.DiscardHandler), nil)
	if err == nil || !strings.Contains(err.Error(), cfg.config) {
		t.Fatalf("got %v, want an error naming the config", err)
	}
	if _, err := os.Stat(cfg.socket); !os.IsNotExist(err) {
		t.Fatal("serve listened despite an invalid config")
	}
}

func TestServeEnforcesConfig(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.config = filepath.Join(cfg.workspace, "hostrun.yaml")
	if err := os.WriteFile(cfg.config, []byte("rules:\n  - command: true\n    args: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.DiscardHandler), nil) }()
	defer func() { cancel(); <-done }()
	tr := transport.Unix{Path: cfg.socket}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c, err := tr.Dial(ctx); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not listen")
		}
	}
	run := func(argv ...string) int {
		return client.Run(ctx, tr, argv, cfg.containerWorkspace, strings.NewReader(""), io.Discard, io.Discard)
	}
	if code := run("true"); code != 0 {
		t.Errorf("allowed command exited %d", code)
	}
	if code := run("false"); code != protocol.ExitRejected {
		t.Errorf("unlisted command exited %d, want %d", code, protocol.ExitRejected)
	}
}

// rulesDigest is the digest of the rules serve loaded for cfg.
func rulesDigest(t *testing.T, cfg serveConfig) string {
	t.Helper()
	p, err := rules.Load(cfg.config)
	if err != nil {
		t.Fatal(err)
	}
	return p.Digest()
}

// armServe arms the daemon as `hostrunner up` would and returns whether it
// asked for a restart.
func armServe(t *testing.T, cfg serveConfig, digest string) bool {
	t.Helper()
	c, err := transport.Unix{Path: cfg.socket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := protocol.WriteJSON(c, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: digest}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	var reply protocol.Armed
	if err != nil || f.Type != protocol.FrameArmed || protocol.DecodeJSON(f, &reply) != nil {
		t.Fatalf("arm: frame %v, err %v", f.Type, err)
	}
	return reply.Restart
}

func TestServeStepsAsideWhenRulesChanged(t *testing.T) {
	cfg := watchConfig(t)
	rt := &fakeRuntime{}
	rt.present.Store(true)
	done := startServe(t, cfg, rt)
	if restart := armServe(t, cfg, "some other digest"); !restart {
		t.Fatal("serve kept running with stale rules")
	}
	expectServeExit(t, done, cfg.socket)
}
