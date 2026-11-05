package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

// mountCheckConfig is a `serve --watch` over a temporary workspace whose
// rules file, .devcontainer/hostrunner-rules.yaml, allows `true`, and
// whose workspace settings file is .devcontainer/hostrunner.yaml, which
// does not exist. It waits long enough for a container that a test can
// ask before one starts.
func mountCheckConfig(t *testing.T) serveConfig {
	t.Helper()
	cfg := watchConfig(t)
	cfg.StartupTimeout = 10 * time.Second
	dc := filepath.Join(cfg.workspace, ".devcontainer")
	if err := os.Mkdir(dc, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.RulesFile = filepath.Join(dc, "hostrunner-rules.yaml")
	if err := os.WriteFile(cfg.RulesFile, []byte("rules:\n  - command: true\n    args: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.workspaceSettings = filepath.Join(dc, "hostrunner.yaml")
	return cfg
}

// workspaceMount mounts the workspace of cfg writable at its container
// path, as a devcontainer does.
func workspaceMount(cfg serveConfig) watch.Mount {
	return watch.Mount{Source: cfg.workspace, Destination: cfg.containerWorkspace}
}

// readOnlyDevcontainer mounts the .devcontainer directory of cfg's
// workspace read-only over the workspace mount.
func readOnlyDevcontainer(cfg serveConfig) watch.Mount {
	return watch.Mount{
		Source:      filepath.Join(cfg.workspace, ".devcontainer"),
		Destination: cfg.containerWorkspace + "/.devcontainer",
		ReadOnly:    true,
	}
}

// runServedOutput runs argv in the container workspace of cfg's serve and
// returns its exit code and what the client wrote to stderr.
func runServedOutput(cfg serveConfig, argv ...string) (int, string) {
	var stderr strings.Builder
	code := client.Run(context.Background(), transport.Unix{Path: cfg.socket}, argv, cfg.containerWorkspace, strings.NewReader(""), &stderr, &stderr)
	return code, stderr.String()
}

// runOnceChecked runs argv on cfg's serve once its mount check has run,
// retrying while the client is told it is pending (125), for 5 s at
// most, and returns the exit code and stderr of the first run past it.
func runOnceChecked(t *testing.T, cfg serveConfig, argv ...string) (int, string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(cfg.PollInterval) {
		code, stderr := runServedOutput(cfg, argv...)
		if code != protocol.ExitHostrunError {
			return code, stderr
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mount check is still pending: %s", stderr)
		}
	}
}

// Before the watcher attaches to the armed container, serve --watch has
// not checked its mounts: it refuses every run as a hostrun failure the
// client may retry.
func TestServeWatchRefusesCommandsBeforeTheMountCheck(t *testing.T) {
	cfg := mountCheckConfig(t)
	startServe(t, cfg, &watchtest.Runtime{})
	code, stderr := runServedOutput(cfg, "true")
	if code != protocol.ExitHostrunError || !strings.Contains(stderr, "mount check") {
		t.Fatalf("got %d, %q; want %d naming the mount check", code, stderr, protocol.ExitHostrunError)
	}
}

// Once attached to a container that mounts .devcontainer, where the rules
// file and the workspace settings file lie, read-only, serve --watch runs
// the commands its rules allow.
func TestServeWatchRunsCommandsWhenTheContainerCannotWriteItsConfig(t *testing.T) {
	cfg := mountCheckConfig(t)
	rt := &watchtest.Runtime{}
	startServe(t, cfg, rt)
	rt.SetMounts("c", workspaceMount(cfg), readOnlyDevcontainer(cfg))
	rt.Start("c")
	if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
		t.Fatalf("got %d, %q; want 0", code, stderr)
	}
}

// Once attached to a container that can write the .devcontainer directory,
// the rules file or the workspace settings file, serve --watch refuses
// every run with 126, naming the path writable in the container, not the
// host's.
func TestServeWatchRefusesCommandsWhenTheContainerCanWriteItsConfig(t *testing.T) {
	tests := []struct {
		name   string
		adjust func(cfg *serveConfig)
		mounts func(cfg serveConfig) []watch.Mount
		want   string // the container path the refusal names, under the container workspace
	}{
		{
			name:   "the .devcontainer directory",
			adjust: func(*serveConfig) {},
			mounts: func(cfg serveConfig) []watch.Mount { return []watch.Mount{workspaceMount(cfg)} },
			want:   "/.devcontainer",
		},
		{
			name: "a rules file outside it",
			adjust: func(cfg *serveConfig) {
				cfg.RulesFile = moveFile(t, cfg.RulesFile, filepath.Join(cfg.workspace, "rules.yaml"))
			},
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg)}
			},
			want: "/rules.yaml",
		},
		{
			name:   "a workspace settings file outside it",
			adjust: func(cfg *serveConfig) { cfg.workspaceSettings = filepath.Join(cfg.workspace, "hostrunner.yaml") },
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg)}
			},
			want: "/hostrunner.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mountCheckConfig(t)
			tt.adjust(&cfg)
			rt := &watchtest.Runtime{}
			startServe(t, cfg, rt)
			rt.SetMounts("c", tt.mounts(cfg)...)
			rt.Start("c")
			code, stderr := runOnceChecked(t, cfg, "true")
			if want := cfg.containerWorkspace + tt.want; code != protocol.ExitRejected || !strings.Contains(stderr, want) {
				t.Fatalf("got %d, %q; want %d naming %s", code, stderr, protocol.ExitRejected, want)
			}
			if strings.Contains(stderr, cfg.workspace) {
				t.Fatalf("the refusal %q names the host workspace %s", stderr, cfg.workspace)
			}
		})
	}
}

// A container that starts after the arm and joins the attached arm, as one
// a Compose devcontainer recreated, is checked too: when it can write the
// config, serve --watch refuses every run again.
func TestServeWatchChecksAContainerThatJoinsTheArm(t *testing.T) {
	cfg := mountCheckConfig(t)
	rt := &watchtest.Runtime{}
	startServe(t, cfg, rt)
	rt.SetMounts("c", workspaceMount(cfg), readOnlyDevcontainer(cfg))
	rt.Start("c")
	if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
		t.Fatalf("got %d, %q; want 0 while the armed container cannot write its config", code, stderr)
	}

	rt.SetMounts("recreated", workspaceMount(cfg))
	rt.Start("recreated")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(cfg.PollInterval) {
		code, stderr := runServedOutput(cfg, "true")
		if want := cfg.containerWorkspace + "/.devcontainer"; code == protocol.ExitRejected && strings.Contains(stderr, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d, %q; want %d naming %s once the recreated container joined", code, stderr, protocol.ExitRejected, cfg.containerWorkspace+"/.devcontainer")
		}
	}
}

// A re-arm puts the mount check back to pending: serve --watch refuses
// every run with 125 until it attaches for the arm, then serves or refuses
// it by the mounts of the container it attached to, whether `up` reused the
// running container or started a new one.
func TestServeWatchMountCheckIsPendingAfterARearm(t *testing.T) {
	tests := []struct {
		name     string
		start    func(rt *watchtest.Runtime, cfg serveConfig) // after the re-arm
		wantCode int
	}{
		{
			name:     "the running container reused",
			start:    func(*watchtest.Runtime, serveConfig) {},
			wantCode: 0,
		},
		{
			name: "a new container that can write its config",
			start: func(rt *watchtest.Runtime, cfg serveConfig) {
				rt.Stop("c")
				rt.SetMounts("new", workspaceMount(cfg))
				rt.Start("new")
			},
			wantCode: protocol.ExitRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mountCheckConfig(t)
			// Long enough to see the pending check before the reused
			// container is attached to again.
			cfg.Grace = 2 * time.Second
			rt := &watchtest.Runtime{}
			startServe(t, cfg, rt)
			rt.SetMounts("c", workspaceMount(cfg), readOnlyDevcontainer(cfg))
			rt.Start("c")
			if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
				t.Fatalf("got %d, %q; want 0 before the re-arm", code, stderr)
			}
			if armServe(t, cfg, rulesDigest(t, cfg)) {
				t.Fatal("the daemon stepped aside on the re-arm")
			}
			code, stderr := runServedOutput(cfg, "true")
			if code != protocol.ExitHostrunError || !strings.Contains(stderr, "mount check has not run yet") {
				t.Fatalf("got %d, %q right after the re-arm; want %d, the check pending", code, stderr, protocol.ExitHostrunError)
			}
			tt.start(rt, cfg)
			if code, stderr := runOnceChecked(t, cfg, "true"); code != tt.wantCode {
				t.Fatalf("got %d, %q once attached again; want %d", code, stderr, tt.wantCode)
			}
		})
	}
}

// moveFile renames from to to and returns to.
func moveFile(t *testing.T, from, to string) string {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	return to
}
