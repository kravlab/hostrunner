// Package client implements the container side of hostrun: it sends one
// command to the hostrunner daemon, relays stdio, and yields the exit code.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/textcut"
)

// Dialer opens a connection to the daemon; transport.Transport satisfies it.
type Dialer interface {
	Dial(ctx context.Context) (net.Conn, error)
}

// stdinChunk is the read size for forwarding stdin; well under MaxPayload.
const stdinChunk = 32 * 1024

// ExitInterrupted is returned when ctx is cancelled (Ctrl+C) before the
// command finishes: 128 + SIGINT, as a shell would report it.
const ExitInterrupted = 130

// dryRunFlag, as hostrun's first argument, makes Run a dry run.
const dryRunFlag = "--dry-run"

// askFlag, as hostrun's first argument, makes Run ask for an approval when
// the rules refuse the command; askFlag=<duration> also sets how long the
// request waits for an answer (see isAskFlag).
const askFlag = "--ask"

// isAskFlag reports whether arg is askFlag, with a limit or without.
func isAskFlag(arg string) bool { return arg == askFlag || strings.HasPrefix(arg, askFlag+"=") }

// Run executes the command in args, hostrun's arguments, on the host with
// the container working directory cwd and returns the exit code hostrun
// must terminate with: the command's own code, or one of the protocol.Exit*
// codes when hostrun or the daemon fails. Failures of hostrun itself are
// reported on stderr prefixed with "hostrun:".
//
// When args starts with dryRunFlag, the rest is the command, and Run makes
// a dry run of it: the daemon runs every check the command would meet
// before it starts, but never starts it. Run then reports the allowing
// rule on stderr and returns 0, or fails as a run of the command would
// have been refused. After the program nothing is hostrun's own, so a
// command's --dry-run reaches the command.
//
// When args starts with askFlag, a command the rules refuse but a human may
// approve waits for an approval session's answer: Run reports on stderr
// that it waits, then that it was approved and how far, and runs it as
// usual; a refusal exits as any other. Cancelling ctx withdraws the
// request. The two flags exclude each other: one followed by the other
// fails with ExitHostrunError.
//
// stdin is forwarded until it returns EOF, within the credit the daemon
// grants; a dry run does not read it, and an approval request reads it only
// once approved. Run does not wait for that: a command that ignores stdin
// finishes while a terminal read may still block. Cancelling ctx drops the
// connection, which makes the daemon kill the command, and Run returns
// ExitInterrupted.
func Run(ctx context.Context, d Dialer, args []string, cwd string, stdin io.Reader, stdout, stderr io.Writer) int {
	argv, dryRun, ask := args, false, false
	var limit time.Duration // how long an approval request waits; 0: the daemon's default
	if len(args) > 0 {
		switch {
		case args[0] == dryRunFlag:
			argv, dryRun = args[1:], true
		case isAskFlag(args[0]):
			argv, ask = args[1:], true
			if value, set := strings.CutPrefix(args[0], askFlag+"="); set {
				d, err := time.ParseDuration(value)
				if err != nil || d <= 0 {
					return fail(stderr, protocol.ExitHostrunError, "%s limit %q is not a positive duration", askFlag, value)
				}
				limit = d
			}
		}
	}
	// The other flag as the next argument is taken for a mistake rather
	// than for a program of that name.
	if len(argv) > 0 && (dryRun && isAskFlag(argv[0]) || ask && argv[0] == dryRunFlag) {
		return fail(stderr, protocol.ExitHostrunError, "%s and %s cannot be combined", dryRunFlag, askFlag)
	}
	if len(argv) == 0 {
		return fail(stderr, protocol.ExitHostrunError, "usage: hostrun [%s|%s] <command> [args...]", dryRunFlag, askFlag)
	}
	if lines := utf8Errors(argv, cwd); len(lines) > 0 {
		for _, line := range lines {
			fail(stderr, protocol.ExitHostrunError, "%s", line)
		}
		return protocol.ExitHostrunError
	}
	conn, err := d.Dial(ctx)
	if err != nil {
		return fail(stderr, protocol.ExitHostrunError, "cannot reach the hostrunner daemon: %v", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req := protocol.Request{Version: protocol.Version, Argv: argv, Cwd: cwd}
	switch {
	case dryRun:
		err = protocol.WriteJSON(conn, protocol.FrameDryRun, req)
	case ask:
		err = protocol.WriteJSON(conn, protocol.FrameAsk, protocol.Ask{Request: req, TimeoutMs: askTimeoutMs(limit)})
	default:
		err = protocol.WriteJSON(conn, protocol.FrameRequest, req)
	}
	if err != nil {
		return fail(stderr, protocol.ExitHostrunError, "send request: %v", err)
	}
	credit := newStdinCredit()
	defer credit.stop()
	// After the request this goroutine is the only writer on conn. An
	// approval request sends nothing until it is approved, so a client
	// that goes away is all the daemon reads meanwhile.
	startStdin := func() { go forwardStdin(conn, stdin, credit) }
	if !dryRun && !ask {
		startStdin()
	}

	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				return fail(stderr, ExitInterrupted, "interrupted")
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("connection closed before the command finished")
			}
			return fail(stderr, protocol.ExitHostrunError, "%v", err)
		}
		switch f.Type {
		case protocol.FrameStdout:
			_, _ = stdout.Write(f.Payload)
		case protocol.FrameStderr:
			_, _ = stderr.Write(f.Payload)
		case protocol.FrameStdinCredit:
			var c protocol.Credit
			if err := protocol.DecodeJSON(f, &c); err != nil {
				return fail(stderr, protocol.ExitHostrunError, "%v", err)
			}
			credit.grant(c.Bytes)
		case protocol.FrameExit:
			var exit protocol.Exit
			if err := protocol.DecodeJSON(f, &exit); err != nil {
				return fail(stderr, protocol.ExitHostrunError, "%v", err)
			}
			return exit.Code
		case protocol.FrameError:
			var e protocol.Error
			if err := protocol.DecodeJSON(f, &e); err != nil {
				return fail(stderr, protocol.ExitHostrunError, "%v", err)
			}
			return fail(stderr, e.Code, "%s", e.Message)
		case protocol.FrameAllowed: // only a dry run is answered with it
			if !dryRun {
				return unexpectedFrame(stderr, f)
			}
			var a protocol.Allowed
			if err := protocol.DecodeJSON(f, &a); err != nil {
				return fail(stderr, protocol.ExitHostrunError, "%v", err)
			}
			fmt.Fprintf(stderr, "hostrun: dry run: %s\n", a)
			return 0
		case protocol.FrameWaiting: // only an approval request gets it
			if !ask {
				return unexpectedFrame(stderr, f)
			}
			var w protocol.Waiting
			if err := protocol.DecodeJSON(f, &w); err != nil {
				return fail(stderr, protocol.ExitHostrunError, "%v", err)
			}
			fmt.Fprintf(stderr, "hostrun: %s\n", w)
		case protocol.FrameApproved: // likewise; the run stream follows
			if !ask {
				return unexpectedFrame(stderr, f)
			}
			var a protocol.Approved
			if err := protocol.DecodeJSON(f, &a); err != nil {
				return fail(stderr, protocol.ExitHostrunError, "%v", err)
			}
			fmt.Fprintf(stderr, "hostrun: %s\n", a)
			startStdin()
		default:
			return unexpectedFrame(stderr, f)
		}
	}
}

// askTimeoutMs is limit as Ask.TimeoutMs carries it: 0 for the daemon's
// default, and never 0 for a limit under a millisecond.
func askTimeoutMs(limit time.Duration) int64 {
	if limit <= 0 {
		return 0
	}
	return max(limit.Milliseconds(), 1)
}

// forwardStdin streams stdin as FrameStdin frames, never exceeding the
// granted credit, and marks its end with FrameStdinClose. It stops silently
// once the connection is gone or credit is stopped.
func forwardStdin(conn net.Conn, stdin io.Reader, credit *stdinCredit) {
	buf := make([]byte, stdinChunk)
	for {
		n, err := stdin.Read(buf)
		for data := buf[:n]; len(data) > 0; {
			k, ok := credit.take(len(data))
			if !ok {
				return
			}
			if werr := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdin, Payload: data[:k]}); werr != nil {
				return
			}
			data = data[k:]
		}
		if err != nil {
			_ = protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdinClose})
			return
		}
	}
}

// stdinCredit tracks how many stdin bytes the daemon currently accepts.
type stdinCredit struct {
	mu      sync.Mutex
	changed sync.Cond
	bytes   int
	stopped bool
}

// newStdinCredit returns a tracker with no credit; the daemon grants the
// initial window once the command has started.
func newStdinCredit() *stdinCredit {
	c := &stdinCredit{}
	c.changed.L = &c.mu
	return c
}

// grant adds n bytes of credit.
func (c *stdinCredit) grant(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes += n
	c.changed.Broadcast()
}

// take blocks until credit is available and consumes up to limit bytes of
// it. It reports false once stop has been called.
func (c *stdinCredit) take(limit int) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.bytes == 0 && !c.stopped {
		c.changed.Wait()
	}
	if c.stopped {
		return 0, false
	}
	n := min(c.bytes, limit)
	c.bytes -= n
	return n, true
}

// stop wakes and ends any pending take; Run calls it when it returns.
func (c *stdinCredit) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	c.changed.Broadcast()
}

// fail prints a hostrun-level error and returns code.
func fail(stderr io.Writer, code int, format string, args ...any) int {
	fmt.Fprintf(stderr, "hostrun: "+format+"\n", args...)
	return code
}

// unexpectedFrame fails on a frame the daemon must not send at this point.
func unexpectedFrame(stderr io.Writer, f protocol.Frame) int {
	return fail(stderr, protocol.ExitHostrunError, "unexpected frame type %d from the daemon", f.Type)
}

// utf8Errors returns an error line for each argument of argv, then for
// cwd, that is not valid UTF-8, an argument numbered from 1 as an approval
// session numbers them, each value Go-quoted, so no byte of it reaches the
// terminal as it is, and cut as textcut.Cut cuts it. The request is JSON,
// whose encoding would replace such bytes with U+FFFD, so the host would
// run another argv, or in another directory, than the one given.
func utf8Errors(argv []string, cwd string) []string {
	var lines []string
	for i, arg := range argv {
		if !utf8.ValidString(arg) {
			lines = append(lines, fmt.Sprintf("argument %d is not valid UTF-8: %s", i+1, textcut.Cut(arg, strconv.Quote)))
		}
	}
	if !utf8.ValidString(cwd) {
		lines = append(lines, fmt.Sprintf("the current directory is not valid UTF-8: %s", textcut.Cut(cwd, strconv.Quote)))
	}
	return lines
}
