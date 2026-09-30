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
	return daemon.New(mapper, allowAll{}, slog.New(slog.DiscardHandler), opts...)
}

// allowAll is a policy that allows every command.
type allowAll struct{}

func (allowAll) Check([]string) error { return nil }

// panicky panics for "boom" and allows everything else.
type panicky struct{}

func (panicky) Check(argv []string) error {
	if argv[0] == "boom" {
		panic("policy bug")
	}
	return nil
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

func TestServeRearmsOnArmRequest(t *testing.T) {
	armed := make(chan struct{}, 1)
	srv := newServer(t, daemon.WithArmHandler(func(a protocol.Arm) bool {
		if a.ConfigDigest == "abc" {
			armed <- struct{}{}
		}
		return false
	}))
	client, server := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener(acceptResult{conn: server}))

	client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(client, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: "abc"}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(client)
	if err != nil || f.Type != protocol.FrameArmed {
		t.Fatalf("got frame %v, err %v; want FrameArmed", f.Type, err)
	}
	var reply protocol.Armed
	if err := protocol.DecodeJSON(f, &reply); err != nil || reply.Restart {
		t.Fatalf("reply %+v, err %v; want no restart", reply, err)
	}
	select {
	case <-armed:
	case <-time.After(5 * time.Second):
		t.Fatal("arm handler was not called")
	}
}

func TestServeRejectsArmWithOtherVersion(t *testing.T) {
	srv := newServer(t, daemon.WithArmHandler(func(protocol.Arm) bool {
		t.Error("arm handler called for a bad version")
		return false
	}))
	client, server := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener(acceptResult{conn: server}))

	client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(client, protocol.FrameArm, protocol.Arm{Version: protocol.Version + 1}); err != nil {
		t.Fatal(err)
	}
	if f := statusFrame(t, client); f.Type != protocol.FrameError {
		t.Fatalf("got frame type %d, want FrameError", f.Type)
	}
}

func TestServeSurvivesAPanickingRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mapper, err := workspace.NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.New(mapper, panicky{}, slog.New(slog.DiscardHandler))
	first, firstServer := net.Pipe()
	second, secondServer := net.Pipe()
	defer first.Close()
	defer second.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener(acceptResult{conn: firstServer}, acceptResult{conn: secondServer}))

	first.SetDeadline(time.Now().Add(5 * time.Second))
	sendRequest(t, first, "boom")
	f := statusFrame(t, first)
	var e protocol.Error
	if f.Type != protocol.FrameError || protocol.DecodeJSON(f, &e) != nil || e.Code != protocol.ExitHostrunError {
		t.Fatalf("panicking request: got frame %d %+v, want Error %d", f.Type, e, protocol.ExitHostrunError)
	}

	second.SetDeadline(time.Now().Add(5 * time.Second))
	sendRequest(t, second, "true")
	if f := statusFrame(t, second); f.Type != protocol.FrameExit {
		t.Fatalf("daemon did not serve the next request: frame %d", f.Type)
	}
}

func TestServeStopsAfterAnsweringArmThatAsksForRestart(t *testing.T) {
	srv := newServer(t, daemon.WithArmHandler(func(protocol.Arm) bool { return true }))
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), newChanListener(acceptResult{conn: server})) }()

	client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(client, protocol.FrameArm, protocol.Arm{Version: protocol.Version}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(client)
	var reply protocol.Armed
	if err != nil || f.Type != protocol.FrameArmed || protocol.DecodeJSON(f, &reply) != nil || !reply.Restart {
		t.Fatalf("got frame %v %+v, err %v; want Armed with restart", f.Type, reply, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running after asking for a restart")
	}
}
