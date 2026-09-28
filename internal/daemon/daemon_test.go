package daemon_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/workspace"
)

const containerRoot = "/workspaces/app"

func newServer(t *testing.T, opts ...daemon.Option) *daemon.Server {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mapper, err := workspace.NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	return daemon.New(mapper, slog.New(slog.DiscardHandler), opts...)
}

// chanListener returns whatever Accept results the test feeds it, in order,
// and net.ErrClosed once closed.
type chanListener struct {
	results chan acceptResult
	closed  chan struct{}
	once    sync.Once
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func newChanListener(results ...acceptResult) *chanListener {
	l := &chanListener{results: make(chan acceptResult, 8), closed: make(chan struct{})}
	for _, r := range results {
		l.results <- r
	}
	return l
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.results:
		return r.conn, r.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.UnixAddr{Name: "test", Net: "unix"} }

// sendRequest writes a request for argv on the client end of a pipe.
func sendRequest(t *testing.T, conn net.Conn, argv ...string) {
	t.Helper()
	req := protocol.Request{Version: protocol.Version, Argv: argv, Cwd: containerRoot}
	if err := protocol.WriteJSON(conn, protocol.FrameRequest, req); err != nil {
		t.Fatal(err)
	}
}

// statusFrame reads frames until Exit or Error and returns it.
func statusFrame(t *testing.T, conn net.Conn) protocol.Frame {
	t.Helper()
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("no status frame: %v", err)
		}
		if f.Type == protocol.FrameExit || f.Type == protocol.FrameError {
			return f
		}
	}
}

func TestServeRejectsConnectionThatSendsNoRequest(t *testing.T) {
	srv := newServer(t, daemon.WithRequestTimeout(100*time.Millisecond))
	client, server := net.Pipe()
	defer client.Close()
	l := newChanListener(acceptResult{conn: server})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	client.SetDeadline(time.Now().Add(5 * time.Second))
	if f := statusFrame(t, client); f.Type != protocol.FrameError {
		t.Fatalf("got frame type %d, want FrameError", f.Type)
	}
}

func TestServeRetriesTemporaryAcceptErrors(t *testing.T) {
	srv := newServer(t)
	client, server := net.Pipe()
	defer client.Close()
	l := newChanListener(
		acceptResult{err: os.NewSyscallError("accept", syscall.EMFILE)},
		acceptResult{conn: server},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, l)

	client.SetDeadline(time.Now().Add(5 * time.Second))
	sendRequest(t, client, "true")
	if f := statusFrame(t, client); f.Type != protocol.FrameExit {
		t.Fatalf("got frame type %d, want FrameExit", f.Type)
	}
}

func TestServeStopsRunningCommandsOnPermanentAcceptError(t *testing.T) {
	srv := newServer(t)
	client, server := net.Pipe()
	defer client.Close()
	l := newChanListener(acceptResult{conn: server})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()

	// Start a long command and keep draining its output.
	client.SetDeadline(time.Now().Add(10 * time.Second))
	sendRequest(t, client, "sh", "-c", "echo up; exec sleep 60")
	for {
		f, err := protocol.ReadFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == protocol.FrameStdout {
			break
		}
	}
	go func() {
		for {
			if _, err := protocol.ReadFrame(client); err != nil {
				return
			}
		}
	}()

	boom := errors.New("listener broke")
	l.results <- acceptResult{err: boom}
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("Serve returned %v, want %v", err, boom)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after a permanent accept error")
	}
}
