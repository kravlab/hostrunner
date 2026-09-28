// Package client implements the container side of hostrun: it sends one
// command to the hostrunner daemon, relays stdio, and yields the exit code.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/kravlab/hostrunner/internal/protocol"
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

// Run executes argv on the host with the container working directory cwd
// and returns the exit code hostrun must terminate with: the command's own
// code, or one of the protocol.Exit* codes when hostrun or the daemon fails.
// Failures of hostrun itself are reported on stderr prefixed with "hostrun:".
//
// stdin is forwarded until it returns EOF, within the credit the daemon
// grants. Run does not wait for that: a command that ignores stdin finishes
// while a terminal read may still block. Cancelling ctx drops the connection,
// which makes the daemon kill the command, and Run returns ExitInterrupted.
func Run(ctx context.Context, d Dialer, argv []string, cwd string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		return fail(stderr, protocol.ExitHostrunError, "usage: hostrun <command> [args...]")
	}
	conn, err := d.Dial(ctx)
	if err != nil {
		return fail(stderr, protocol.ExitHostrunError, "cannot reach the hostrunner daemon: %v", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req := protocol.Request{Version: protocol.Version, Argv: argv, Cwd: cwd}
	if err := protocol.WriteJSON(conn, protocol.FrameRequest, req); err != nil {
		return fail(stderr, protocol.ExitHostrunError, "send request: %v", err)
	}
	credit := newStdinCredit()
	defer credit.stop()
	// After the request this goroutine is the only writer on conn.
	go forwardStdin(conn, stdin, credit)

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
		default:
			return fail(stderr, protocol.ExitHostrunError, "unexpected frame type %d from the daemon", f.Type)
		}
	}
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
