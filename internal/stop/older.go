package stop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/transport"
)

// stopOlder stops the daemon of dir, of an older hostrunner, which does
// not stop on request, by SIGTERM, which kills what it runs: the process
// on the other end of conn, a connection to one of its sockets, once it
// is the user's and its command line is `hostrunner serve --socket
// <dir>/hostrunner.sock …`. The process is held by a pidfd (see
// os.FindProcess) from before its command line is read, so a process
// taking its PID meanwhile is not the one signalled. What it runs is
// unknown, so the human can only kill it or leave it running; --wait is
// an error.
func (t *term) stopOlder(ctx context.Context, cfg Config, dir string, conn net.Conn, e ending) error {
	name := hostterm.Display(dir)
	uid, pid, err := transport.PeerCred(conn)
	if err != nil {
		return fmt.Errorf("%s: who serves its socket: %w", name, err)
	}
	socket := launch.Socket(dir)
	if uid != os.Getuid() {
		return fmt.Errorf("%s: process %d on its socket belongs to another user: no signal sent", name, pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("%s: process %d on its socket: %w", name, pid, err)
	}
	defer proc.Release()
	argv, err := cmdline(pid)
	if err == nil {
		err = checkServe(argv, socket)
	}
	if err != nil {
		return fmt.Errorf("%s: process %d on its socket is not `hostrunner serve --socket %s`: %v; no signal sent", name, pid, hostterm.Display(socket), err)
	}
	fmt.Fprintf(t.out, "%s is of an older hostrunner: it cannot tell what it runs, and stopping it kills that\n", name)
	switch cfg.Answer {
	case Wait:
		return fmt.Errorf("%s is of an older hostrunner, which cannot wait for its commands: give --kill", name)
	case Ask:
		if !cfg.Terminal {
			return fmt.Errorf("%s is of an older hostrunner: give --kill to stop it", name)
		}
		if err := t.askKill(ctx, name); err != nil {
			return err
		}
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("%s: signal process %d: %w", name, pid, err)
	}
	return t.stopped(ctx, cfg, dir, e)
}

// askKill asks the human whether to kill the older daemon named name;
// anything but k leaves it running, an error.
func (t *term) askKill(ctx context.Context, name string) error {
	text, err := t.pick(ctx, "k kills whatever it runs and stops it, q leaves it running [k/q] ? ", "kq", nil)
	switch {
	case errors.Is(err, errInterrupted):
		return fmt.Errorf("%w: %s left running", err, name)
	case err != nil || text == "q":
		return fmt.Errorf("%s left running", name)
	}
	return nil
}

// cmdline returns the command line of process pid.
func cmdline(pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	return strings.Split(string(bytes.TrimSuffix(b, []byte{0})), "\x00"), nil
}

// checkServe returns an error unless argv is the command line of
// `hostrunner serve` with --socket socket, the flag in any form the flag
// package reads.
func checkServe(argv []string, socket string) error {
	if len(argv) < 2 || filepath.Base(argv[0]) != "hostrunner" || argv[1] != "serve" {
		return fmt.Errorf("it runs %q", argv)
	}
	for i, arg := range argv[2:] {
		var value string
		switch {
		case arg == "--socket" || arg == "-socket":
			if i+3 >= len(argv) {
				continue
			}
			value = argv[i+3]
		case strings.HasPrefix(arg, "--socket="):
			value = strings.TrimPrefix(arg, "--socket=")
		case strings.HasPrefix(arg, "-socket="):
			value = strings.TrimPrefix(arg, "-socket=")
		default:
			continue
		}
		if filepath.Clean(value) == filepath.Clean(socket) {
			return nil
		}
		return fmt.Errorf("it serves %q", value)
	}
	return fmt.Errorf("it names no socket")
}
