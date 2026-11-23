package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/contain"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// A panic in a goroutine Serve starts, here the watcher rereading the
// rules file, is contained under the containment of Serve's context: it
// ends that context, Serve returns, and the panic is kept with its stack.
func TestServeContainsAPanicOfItsGoroutines(t *testing.T) {
	rulesFile := filepath.Join(t.TempDir(), "hostrunner-rules.yaml")
	if err := os.WriteFile(rulesFile, []byte("rules:\n  - command: echo\n    args: any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newPolicyServer(t, policyOf{},
		daemon.WithApprovals(newChanListener(), rulesFile),
		daemon.WithReread("old", func() (daemon.Policy, string, error) { panic("reread broke") }),
		daemon.WithRereadEvery(time.Millisecond))
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, newChanListener()) }()

	pe := panicEnding(t, ctx, done)
	if pe.Value != "reread broke" || len(pe.Stack) == 0 {
		t.Fatalf("the panic is %v with a %d-byte stack, want reread broke with a stack", pe.Value, len(pe.Stack))
	}
	if !errors.Is(context.Cause(ctx), pe) {
		t.Fatalf("the containment's context ended with %v, want the panic", context.Cause(ctx))
	}
}

// A panic in a Write of a command's output, which exec.Cmd makes in a
// goroutine of its own, is contained under the containment of Serve's
// context: it ends that context, and Serve returns.
func TestServeContainsAPanicOfACommandOutputWrite(t *testing.T) {
	srv := newPolicyServer(t, allowAll{})
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()
	l := newChanListener()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()

	client, server := net.Pipe()
	defer client.Close()
	l.results <- acceptResult{conn: panicOnWrite{Conn: server, marker: "contained-boom"}}
	req := protocol.Request{Version: protocol.Version, Argv: []string{"printf", "contained-boom"}, Cwd: containerRoot}
	if err := protocol.WriteJSON(client, protocol.FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, client) // net.Pipe blocks every write until it is read

	if pe := panicEnding(t, ctx, done); pe.Value != "write broke" {
		t.Fatalf("the containment holds %v, want the panic of the write", pe)
	}
}

// panicEnding returns the panic ctx's containment keeps once Serve, which
// sends its error to done, returned: it fails t unless Serve returns nil
// within 10 s, and the containment then keeps a panic.
func panicEnding(t *testing.T, ctx context.Context, done <-chan error) *contain.PanicError {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the panic")
	}
	pe := contain.Panic(ctx)
	if pe == nil {
		t.Fatal("the containment keeps no panic, want the one Serve ended with")
	}
	return pe
}

// panicOnWrite is a net.Conn whose Write panics with "write broke" when
// what it writes holds marker.
type panicOnWrite struct {
	net.Conn
	marker string
}

// Write panics when p holds c.marker, and writes p to c.Conn otherwise.
func (c panicOnWrite) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(c.marker)) {
		panic("write broke")
	}
	return c.Conn.Write(p)
}
