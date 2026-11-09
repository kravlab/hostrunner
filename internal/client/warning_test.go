package client_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// scriptedDialer connects Run to a server that reads the request and then
// plays script, a function writing the daemon's frames to the connection.
type scriptedDialer struct {
	script func(t *testing.T, conn net.Conn)
	t      *testing.T
}

// Dial returns the client end of a pipe served by the script.
func (d scriptedDialer) Dial(context.Context) (net.Conn, error) {
	cl, srv := net.Pipe()
	go func() {
		defer srv.Close()
		if _, err := protocol.ReadFrame(srv); err != nil {
			return
		}
		go func() { _, _ = io.Copy(io.Discard, srv) }() // stdin frames
		d.script(d.t, srv)
	}()
	return cl, nil
}

// send writes one JSON frame of the script.
func send(t *testing.T, conn net.Conn, ft protocol.FrameType, v any) {
	t.Helper()
	if err := protocol.WriteJSON(conn, ft, v); err != nil {
		t.Errorf("script write: %v", err)
	}
}

// runScripted runs hostrun with args against a daemon that plays script.
func runScripted(t *testing.T, script func(t *testing.T, conn net.Conn), args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := client.Run(context.Background(), scriptedDialer{script: script, t: t}, args, containerRoot, strings.NewReader(""), &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// The warning the daemon sends in the initial credit is printed once,
// before the command's output, though a later credit repeats it.
func TestRunPrintsTheWarningOfTheInitialCreditOnceBeforeTheOutput(t *testing.T) {
	got := runScripted(t, func(t *testing.T, conn net.Conn) {
		send(t, conn, protocol.FrameStdinCredit, protocol.Credit{Bytes: 10, Warning: "w"})
		_ = protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStderr, Payload: []byte("err\n")})
		_ = protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdout, Payload: []byte("out")})
		send(t, conn, protocol.FrameStdinCredit, protocol.Credit{Bytes: 10, Warning: "w"})
		send(t, conn, protocol.FrameExit, protocol.Exit{Code: 3})
	}, "cmd")
	want := result{code: 3, stdout: "out", stderr: "hostrun: warning: w\nerr\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A dry run prints the warning of its allowed frame before it says which
// rule allows the command.
func TestRunPrintsTheWarningOfADryRunBeforeTheAllowingRule(t *testing.T) {
	got := runScripted(t, func(t *testing.T, conn net.Conn) {
		send(t, conn, protocol.FrameAllowed, protocol.Allowed{Rule: "cmd", Warning: "w"})
	}, "--dry-run", "cmd")
	want := result{stderr: "hostrun: warning: w\nhostrun: dry run: allowed by rule \"cmd\"\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A refusal prints the warning of its error frame before the message and
// exits with its code.
func TestRunPrintsTheWarningOfARefusalBeforeTheMessage(t *testing.T) {
	got := runScripted(t, func(t *testing.T, conn net.Conn) {
		send(t, conn, protocol.FrameError, protocol.Error{Code: 126, Message: "m", Warning: "w"})
	}, "cmd")
	want := result{code: 126, stderr: "hostrun: warning: w\nhostrun: m\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An approval request prints the warning of its waiting frame once,
// before the waiting line, though a later credit repeats it.
func TestRunPrintsTheWarningOfAnApprovalRequestOnceBeforeTheWaitingLine(t *testing.T) {
	got := runScripted(t, func(t *testing.T, conn net.Conn) {
		send(t, conn, protocol.FrameWaiting, protocol.Waiting{Reasons: []string{"r"}, Warning: "w"})
		send(t, conn, protocol.FrameApproved, protocol.Approved{Scope: protocol.ScopeSession})
		send(t, conn, protocol.FrameStdinCredit, protocol.Credit{Bytes: 10, Warning: "w"})
		send(t, conn, protocol.FrameExit, protocol.Exit{})
	}, "--ask", "cmd")
	want := "hostrun: warning: w\nhostrun: waiting for approval on the host: r\nhostrun: " + protocol.Approved{Scope: protocol.ScopeSession}.String() + "\n"
	if got.code != 0 || got.stdout != "" || got.stderr != want {
		t.Fatalf("got %+v, want stderr %q", got, want)
	}
}

// A daemon that sends no warning makes hostrun print none.
func TestRunPrintsNoWarningWhenTheDaemonSendsNone(t *testing.T) {
	got := runScripted(t, func(t *testing.T, conn net.Conn) {
		send(t, conn, protocol.FrameStdinCredit, protocol.Credit{Bytes: 10})
		_ = protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdout, Payload: []byte("out")})
		send(t, conn, protocol.FrameExit, protocol.Exit{})
	}, "cmd")
	if want := (result{stdout: "out"}); got != want || strings.Contains(got.stderr, "warning") {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
