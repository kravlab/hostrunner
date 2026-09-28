// Command hostrunner is the host-side daemon that runs commands sent by the
// hostrun client from inside a devcontainer.
//
// Usage:
//
//	hostrunner serve --socket <path> --workspace <host-path> --container-workspace <path>
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
	"syscall"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/workspace"
)

const usage = "usage: hostrunner serve --socket <path> --workspace <host-path> --container-workspace <path>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "hostrunner: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches the subcommand in args; only `serve` exists so far.
func run(args []string) error {
	if len(args) == 0 || args[0] != "serve" {
		return errors.New(usage)
	}
	cfg, err := parseServe(args[1:], os.Stderr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// serveConfig holds the settings of `hostrunner serve`.
type serveConfig struct {
	socket             string // Unix socket to listen on
	workspace          string // workspace path on the host
	containerWorkspace string // the same workspace's path inside the container
}

// parseServe parses the flags of `hostrunner serve`; all of them are required.
func parseServe(args []string, output io.Writer) (serveConfig, error) {
	var cfg serveConfig
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.socket, "socket", "", "Unix socket path to listen on")
	fs.StringVar(&cfg.workspace, "workspace", "", "workspace directory on the host")
	fs.StringVar(&cfg.containerWorkspace, "container-workspace", "", "workspace directory inside the container")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), usage)
	}
	if cfg.socket == "" || cfg.workspace == "" || cfg.containerWorkspace == "" {
		return cfg, errors.New(usage)
	}
	return cfg, nil
}

// serve runs the daemon until ctx is cancelled.
func serve(ctx context.Context, cfg serveConfig, log *slog.Logger) error {
	mapper, err := workspace.NewMapper(cfg.workspace, cfg.containerWorkspace)
	if err != nil {
		return err
	}
	l, err := transport.Unix{Path: cfg.socket}.Listen()
	if err != nil {
		return err
	}
	log.Info("listening", "socket", cfg.socket, "workspace", cfg.workspace, "container_workspace", cfg.containerWorkspace)
	return daemon.New(mapper, log).Serve(ctx, l)
}
