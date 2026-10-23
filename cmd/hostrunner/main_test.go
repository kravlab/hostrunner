package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/update"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

func TestBuildVersionReportsTheModuleVersion(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"release tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, true, "v0.1.0"},
		{"pseudo-version", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1-0.20261001120000-abcdef123456+dirty"}}, true, "v0.1.1-0.20261001120000-abcdef123456+dirty"},
		{"no VCS data", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"empty version", &debug.BuildInfo{}, true, "dev"},
		{"no build info", nil, false, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion(tt.info, tt.ok); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A test binary carries no VCS data, so its version is "dev".
func TestRunVersionPrintsTheVersion(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"version"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "dev\n" {
		t.Fatalf("got %q, want %q", got, "dev\n")
	}
}

func TestRunVersionRejectsArguments(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"version", "extra"}, &out, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected argument")
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q despite the error", out.String())
	}
}

func TestParseVersionHelpIsTheFlagHelp(t *testing.T) {
	if err := parseVersion([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}

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
func startServe(t *testing.T, cfg serveConfig, runtimes ...*watchtest.Runtime) <-chan error {
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
	rt := &watchtest.Runtime{}
	done := startServe(t, cfg, rt)
	rt.Start("c")
	time.Sleep(50 * time.Millisecond)
	rt.Stop("c")
	expectServeExit(t, done, cfg.socket)
}

func TestServeWatchExitsWhenContainerNeverStarts(t *testing.T) {
	cfg := watchConfig(t)
	expectServeExit(t, startServe(t, cfg, &watchtest.Runtime{}), cfg.socket)
}

func TestServeWatchWaitsAgainWhenArmed(t *testing.T) {
	cfg := watchConfig(t)
	rt := &watchtest.Runtime{}
	rt.Start("c")
	done := startServe(t, cfg, rt)
	time.Sleep(50 * time.Millisecond)

	rt.Stop("c") // rebuild: old container removed, image building
	if stepAside := armServe(t, cfg, rulesDigest(t, cfg)); stepAside {
		t.Fatal("serve stepped aside although the rules did not change")
	}

	select {
	case err := <-done:
		t.Fatalf("serve exited (%v) within the grace period after being armed", err)
	case <-time.After(3 * cfg.grace):
	}
	rt.Start("c") // the rebuilt container starts
	time.Sleep(cfg.startupTimeout)
	select {
	case err := <-done:
		t.Fatalf("serve exited (%v) while the rebuilt container runs", err)
	default:
	}
	rt.Stop("c")
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
// stepped aside.
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

func TestArmHandlerStepsAsideOnceTheWatchEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		discard := slog.New(slog.DiscardHandler)
		w := watch.New([]watch.Runtime{&watchtest.Runtime{}}, watch.Config{
			PollInterval: time.Second, QueryTimeout: time.Second, StartupTimeout: time.Hour, Grace: time.Second,
		}, discard)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		synctest.Wait() // the watch runs, waiting for its next poll

		onArm := armHandler("digest", "rules.yaml", w, discard)
		if onArm(protocol.Arm{ConfigDigest: "digest"}) {
			t.Fatal("stepped aside while the watch runs")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("watch ended with %v, want context.Canceled", err)
		}
		if !onArm(protocol.Arm{ConfigDigest: "digest"}) {
			t.Fatal("did not step aside after the watch ended")
		}
	})
}

func TestArmHandlerStepsAsideWhenRulesChanged(t *testing.T) {
	onArm := armHandler("digest", "rules.yaml", nil, slog.New(slog.DiscardHandler))
	if !onArm(protocol.Arm{ConfigDigest: "other"}) {
		t.Fatal("kept running with other rules")
	}
	if onArm(protocol.Arm{ConfigDigest: "digest"}) {
		t.Fatal("stepped aside with the same rules and no watch")
	}
}

func TestServeStepsAsideWhenRulesChanged(t *testing.T) {
	cfg := watchConfig(t)
	rt := &watchtest.Runtime{}
	rt.Start("c")
	done := startServe(t, cfg, rt)
	if stepAside := armServe(t, cfg, "some other digest"); !stepAside {
		t.Fatal("serve kept running with stale rules")
	}
	expectServeExit(t, done, cfg.socket)
}

// TestParseUpdateFlags checks that --check is the only flag of update.
func TestParseUpdateFlags(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{"no flags", nil, false},
		{"check", []string{"--check"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUpdate(tt.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("check: got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseUpdateRejectsArguments checks that update takes no version
// or other argument.
func TestParseUpdateRejectsArguments(t *testing.T) {
	if _, err := parseUpdate([]string{"v0.5.0"}, io.Discard); err == nil || !strings.Contains(err.Error(), updateUsage) {
		t.Fatalf("got %v, want an error with the usage", err)
	}
}

// TestParseUpdateHelpIsTheFlagHelp checks that -h is handled like the
// other subcommands' -h.
func TestParseUpdateHelpIsTheFlagHelp(t *testing.T) {
	if _, err := parseUpdate([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}

// TestRunUpdateRefusesADevBuild checks that runUpdate passes a dev build's
// refusal on with the "update: " prefix. The version is passed in, so the
// test does not depend on the test binary's build info; that the refusal
// comes before any request is tested in internal/update.
func TestRunUpdateRefusesADevBuild(t *testing.T) {
	var out strings.Builder
	err := runUpdate(context.Background(), false, "dev", &out)
	if !errors.Is(err, update.ErrDevBuild) || !strings.HasPrefix(err.Error(), "update: ") {
		t.Fatalf("got %v, want update.ErrDevBuild prefixed with \"update: \"", err)
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q despite the error", out.String())
	}
}

// TestRunDispatchesUpdate checks that run hands `update` to parseUpdate:
// its errors, not the top-level usage, come back. No case reaches the
// network.
func TestRunDispatchesUpdate(t *testing.T) {
	if err := run([]string{"update", "v0.5.0"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), updateUsage) {
		t.Fatalf("extra argument: got %v, want an error with %q", err, updateUsage)
	}
	if err := run([]string{"update", "-h"}, io.Discard, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: got %v, want flag.ErrHelp", err)
	}
}

// TestUsageListsUpdate checks that the top-level usage names update.
func TestUsageListsUpdate(t *testing.T) {
	if err := run(nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "up|serve|test|approve|update|version") {
		t.Fatalf("got %v, want the usage listing update", err)
	}
}

// TestUpdateMessage checks the line update prints for each outcome.
func TestUpdateMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		res  update.Result
		want string
	}{
		{"up to date", update.Result{Current: "v0.5.0", Latest: "v0.5.0", Outcome: update.UpToDate}, "hostrunner: already up to date (v0.5.0)"},
		{"available", update.Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: update.Available}, "hostrunner: v0.5.0 available (current v0.4.0)"},
		{"updated", update.Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: update.Updated}, "hostrunner: updated v0.4.0 -> v0.5.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateMessage(tt.res); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// serve listens for approval sessions beside its socket's directory, the
// runtime directory, which the container mounts, so the container cannot
// reach it; it removes the socket when it stops.
func TestServeListensOnTheApprovalSocket(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	cfg.config = filepath.Join(cfg.workspace, "hostrun.yaml")
	approvalSocket := launch.ApprovalSocket(filepath.Dir(cfg.socket))
	t.Cleanup(func() { os.Remove(approvalSocket) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.DiscardHandler), nil) }()
	var c net.Conn
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var err error
		if c, err = net.Dial("unix", approvalSocket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not listen on the approval socket")
		}
	}
	defer c.Close()
	info, err := os.Stat(approvalSocket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("approval socket mode %v, want 0600", info.Mode().Perm())
	}
	if err := protocol.WriteJSON(c, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	var got protocol.DaemonInfo
	if err != nil || f.Type != protocol.FrameDaemonInfo || protocol.DecodeJSON(f, &got) != nil {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	want := protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: cfg.workspace, ContainerWorkspace: cfg.containerWorkspace, RulesFile: cfg.config}
	if real, err := filepath.EvalSymlinks(cfg.workspace); err == nil {
		want.Workspace = real
	}
	if got != want {
		t.Fatalf("hello: got %+v, want %+v", got, want)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := os.Stat(approvalSocket); !os.IsNotExist(err) {
		t.Fatalf("approval socket left behind: %v", err)
	}
}

func TestParseApproveServesTheRuntimeDirectorysApprovalSocket(t *testing.T) {
	got, err := parseApprove([]string{"--dir", "/run/user/1000/hostrunner/abc"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/run/user/1000/hostrunner/abc.approval.sock"; got.Socket != want {
		t.Fatalf("socket %q, want %q", got.Socket, want)
	}
}

func TestParseApproveRequiresTheRuntimeDirectory(t *testing.T) {
	if _, err := parseApprove(nil, io.Discard); err == nil || err.Error() != approveUsage {
		t.Fatalf("got %v, want the usage", err)
	}
	if _, err := parseApprove([]string{"--dir", "/d", "extra"}, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

// An approval socket inside the workspace, which the container mounts,
// would let the container answer its own requests: serve refuses it.
func TestServeRefusesAnApprovalSocketInsideTheWorkspace(t *testing.T) {
	cfg := watchConfig(t)
	cfg.watch = false
	runtimeDir := filepath.Join(cfg.workspace, "rt")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.socket = filepath.Join(runtimeDir, "h.sock")
	err := serve(context.Background(), cfg, slog.New(slog.DiscardHandler), nil)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("got %v, want the approval socket inside the workspace refused", err)
	}
	if _, err := os.Stat(launch.ApprovalSocket(runtimeDir)); !os.IsNotExist(err) {
		t.Fatalf("approval socket created: %v", err)
	}
}

// A daemon stepping aside closes its approval socket before its socket, so
// once `up` sees the socket closed, the new daemon can take the approval
// socket.
func TestServeClosesTheApprovalSocketFirstWhenSteppingAside(t *testing.T) {
	cfg := watchConfig(t)
	approvalSocket := launch.ApprovalSocket(filepath.Dir(cfg.socket))
	t.Cleanup(func() { os.Remove(approvalSocket) })
	rt := &watchtest.Runtime{}
	rt.Start("c")
	done := startServe(t, cfg, rt)
	if stepAside := armServe(t, cfg, "some other digest"); !stepAside {
		t.Fatal("serve kept running with stale rules")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		c, err := net.Dial("unix", cfg.socket)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the socket stayed open")
		}
	}
	if c, err := net.Dial("unix", approvalSocket); err == nil {
		c.Close()
		t.Fatal("the approval socket still listens after the socket closed")
	}
	expectServeExit(t, done, cfg.socket)
}
