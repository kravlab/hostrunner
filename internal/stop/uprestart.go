package stop

import (
	"context"
	"errors"
	"io"

	"github.com/kravlab/hostrunner/internal/launch"
)

// UpRestart is `hostrunner up --restart`: as launch.Up, but a daemon
// serving up.Dir, whatever its rules and whether its container runs, is
// first stopped as Run stops it, its commands treated as commands says,
// and then replaced by one for up. Invalid rules, or a busy daemon nobody
// says what to do with, leave the daemon running.
func UpRestart(ctx context.Context, up launch.Config, commands Commands, in io.Reader, out io.Writer) error {
	commands = commands.withStopTimeout()
	p, err := launch.Prepare(up)
	if err != nil {
		return err
	}
	cfg := Config{Dir: up.Dir, Commands: commands, up: p}
	t := &term{in: in, out: out, interrupts: commands.Interrupts}
	defer t.close()
	err = t.stopDaemon(ctx, cfg, launch.ApprovalSocket(up.Dir))
	if errors.Is(err, errGone) {
		// No daemon to replace: as up.
		return launch.Up(ctx, up)
	}
	return err
}
