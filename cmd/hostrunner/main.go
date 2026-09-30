// Command hostrunner is the host side of hostrun: it runs commands sent by
// the hostrun client from inside a devcontainer.
//
// Usage:
//
//	hostrunner up --dir <runtime-dir> --workspace <host-path> --container-workspace <path>
//	hostrunner serve --socket <path> --workspace <host-path> --container-workspace <path>
//	    [--config <path>] [--watch [--startup-timeout 30m] [--grace 15s]]
//
// `up` is meant for devcontainer's initializeCommand: it installs the client
// into the runtime directory and starts a detached `serve --watch` there,
// which exits by itself once the devcontainer stops.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/workspace"
)

const (
	usage      = "usage: hostrunner up|serve [flags]; see `hostrunner <command> -h`"
	upUsage    = "usage: hostrunner up --dir <runtime-dir> --workspace <host-path> --container-workspace <path>"
	serveUsage = "usage: hostrunner serve --socket <path> --workspace <host-path> --container-workspace <path> [--config <path>] [--watch]"
)

// Defaults for following the container; see internal/watch.
const (
	defaultPollInterval   = 2 * time.Second
	queryTimeout          = 10 * time.Second // one docker/podman ps call
	defaultStartupTimeout = 30 * time.Minute // covers image builds, also after a rebuild's arm
	defaultGrace          = 15 * time.Second // how long a stopped container is tolerated
	readyTimeout          = 10 * time.Second // how long `up` waits for the daemon
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "hostrunner: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches the subcommand in args.
func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "up":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cfg, err := parseUp(args[1:], exe, os.Stderr)
		if err != nil {
			return err
		}
		return launch.Up(ctx, cfg)
	case "serve":
		cfg, err := parseServe(args[1:], os.Stderr)
		if err != nil {
			return err
		}
		var runtimes []watch.Runtime
		if cfg.watch {
			if runtimes = watch.Detect(); len(runtimes) == 0 {
				return errors.New("--watch needs docker or podman in PATH")
			}
		}
		return serve(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), runtimes)
	default:
		return errors.New(usage)
	}
}

// workspaceFlags are the flags `up` and `serve` share.
type workspaceFlags struct {
	workspace          string // workspace path on the host
	containerWorkspace string // the same workspace's path inside the container
}

// rulesFile is the default rules file of a host workspace.
func (w *workspaceFlags) rulesFile() string {
	return filepath.Join(w.workspace, ".devcontainer", "hostrun.yaml")
}

// register adds --workspace and --container-workspace to fs.
func (w *workspaceFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&w.workspace, "workspace", "", "workspace directory on the host")
	fs.StringVar(&w.containerWorkspace, "container-workspace", "", "workspace directory inside the container")
}

// parseFlags parses args with fs and rejects positional arguments.
func parseFlags(fs *flag.FlagSet, args []string, usage string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), usage)
	}
	return nil
}

// parseUp parses the flags of `hostrunner up`; all of them are required.
// exe is the running hostrunner, which becomes the daemon; the client is
// the hostrun binary installed next to it.
func parseUp(args []string, exe string, output io.Writer) (launch.Config, error) {
	var (
		dir string
		ws  workspaceFlags
	)
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&dir, "dir", "", "per-container runtime directory (mounted at /run/hostrunner)")
	ws.register(fs)
	if err := parseFlags(fs, args, upUsage); err != nil {
		return launch.Config{}, err
	}
	if dir == "" || ws.workspace == "" || ws.containerWorkspace == "" {
		return launch.Config{}, errors.New(upUsage)
	}
	return launch.Config{
		Dir:                dir,
		Workspace:          ws.workspace,
		ContainerWorkspace: ws.containerWorkspace,
		Rules:              ws.rulesFile(),
		Daemon:             exe,
		Client:             filepath.Join(filepath.Dir(exe), launch.ClientName),
		ReadyTimeout:       readyTimeout,
	}, nil
}

// serveConfig holds the settings of `hostrunner serve`.
type serveConfig struct {
	socket string // Unix socket to listen on
	workspaceFlags
	config         string        // rules file; default <workspace>/.devcontainer/hostrun.yaml
	watch          bool          // exit once the devcontainer stops
	startupTimeout time.Duration // with watch: wait this long for the container
	grace          time.Duration // with watch: tolerate its absence this long
	pollInterval   time.Duration // with watch: how often runtimes are asked (not a flag)
}

// parseServe parses the flags of `hostrunner serve`; the socket and both
// workspace paths are required.
func parseServe(args []string, output io.Writer) (serveConfig, error) {
	cfg := serveConfig{pollInterval: defaultPollInterval}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.socket, "socket", "", "Unix socket path to listen on")
	cfg.workspaceFlags.register(fs)
	fs.StringVar(&cfg.config, "config", "", "rules file (default <workspace>/.devcontainer/hostrun.yaml)")
	fs.BoolVar(&cfg.watch, "watch", false, "exit once the workspace's devcontainer stops")
	fs.DurationVar(&cfg.startupTimeout, "startup-timeout", defaultStartupTimeout, "with --watch: how long to wait for the container to start")
	fs.DurationVar(&cfg.grace, "grace", defaultGrace, "with --watch: how long the container may be gone before exiting")
	if err := parseFlags(fs, args, serveUsage); err != nil {
		return cfg, err
	}
	if cfg.socket == "" || cfg.workspace == "" || cfg.containerWorkspace == "" {
		return cfg, errors.New(serveUsage)
	}
	if cfg.config == "" {
		cfg.config = cfg.rulesFile()
	}
	return cfg, nil
}

// serve runs the daemon until ctx is cancelled or, with watch, until the
// devcontainer has stopped (as reported by runtimes). An arm request from
// `hostrunner up` puts the watcher back into waiting for the container.
//
// The rules file is read once, here: an invalid file stops the daemon from
// starting (fail closed), a missing one makes it deny every command. Later
// edits, e.g. by a `git pull` on the host, take effect only on restart.
func serve(ctx context.Context, cfg serveConfig, log *slog.Logger, runtimes []watch.Runtime) error {
	policy, err := rules.Load(cfg.config)
	if err != nil {
		return err
	}
	mapper, err := workspace.NewMapper(cfg.workspace, cfg.containerWorkspace)
	if err != nil {
		return err
	}
	l, err := transport.Unix{Path: cfg.socket}.Listen()
	if err != nil {
		return err
	}
	log.Info("listening", "socket", cfg.socket, "workspace", cfg.workspace, "container_workspace", cfg.containerWorkspace, "config", cfg.config, "rules_digest", policy.Digest(), "watch", cfg.watch)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var watcher *watch.Watcher
	if cfg.watch {
		watcher = watch.New(runtimes, watch.Config{
			Workspace:      cfg.workspace,
			PollInterval:   cfg.pollInterval,
			QueryTimeout:   queryTimeout,
			StartupTimeout: cfg.startupTimeout,
			Grace:          cfg.grace,
		}, log)
		go func() {
			defer cancel()
			if err := watcher.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("stopping", "reason", err)
			}
		}()
	}
	// `hostrunner up` arms the daemon on every container start with the
	// digest of the rules it validated: other rules mean this daemon is
	// stale and steps aside for a fresh one; the same rules mean a (re)start
	// of the container, which the watcher must wait for.
	onArm := func(a protocol.Arm) (restart bool) {
		if a.ConfigDigest != policy.Digest() {
			log.Info("rules file changed; stopping for a fresh daemon", "config", cfg.config)
			return true
		}
		if watcher != nil {
			watcher.Rearm()
		}
		return false
	}
	return daemon.New(mapper, policy, log, daemon.WithArmHandler(onArm)).Serve(ctx, l)
}
