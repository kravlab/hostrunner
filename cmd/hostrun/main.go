// Command hostrun runs a command on the devcontainer's host through the
// hostrunner daemon: `hostrun git push` runs `git push` on the host in the
// host directory that mirrors the current one, and exits with its code.
// `hostrun --dry-run git push` only asks whether it would run (see
// client.Run).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// defaultSocket is where the devcontainer mounts the daemon's socket directory.
const defaultSocket = "/run/hostrunner/hostrunner.sock"

func main() {
	os.Exit(run())
}

// run executes the command given on the command line and returns the exit
// code hostrun terminates with.
func run() int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostrun: %v\n", err)
		return protocol.ExitHostrunError
	}
	// Signal forwarding is out of scope for now: Ctrl+C ends hostrun, the
	// connection drops, and the daemon kills the host command.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client.WarnWritableRules(cwd, os.Stderr)
	tr := transport.Unix{Path: socketPath(os.Getenv)}
	return client.Run(ctx, tr, os.Args[1:], cwd, os.Stdin, os.Stdout, os.Stderr)
}

// socketPath returns HOSTRUN_SOCKET when set, the default mount path otherwise.
func socketPath(getenv func(string) string) string {
	if p := getenv("HOSTRUN_SOCKET"); p != "" {
		return p
	}
	return defaultSocket
}
