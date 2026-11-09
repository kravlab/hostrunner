package daemon_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/transport"
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

func (allowAll) Check([]string) (rules.Allowed, error) { return rules.Allowed{}, nil }

// panicky panics for "boom" and allows everything else.
type panicky struct{}

func (panicky) Check(argv []string) (rules.Allowed, error) {
	if argv[0] == "boom" {
		panic("policy bug")
	}
	return rules.Allowed{}, nil
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

// approvalListener listens on an approval socket in a new short
// temporary directory, for WithApprovals, and returns it and its path.
func approvalListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")
	l, err := transport.Unix{Path: socket}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	return l, socket
}

// sendArm sends a, with the protocol version set unless a sets one, on
// the socket at path, as `hostrunner up` does, and returns the daemon's
// answer.
func sendArm(t *testing.T, path string, a protocol.Arm) protocol.Frame {
	t.Helper()
	c, err := transport.Unix{Path: path}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if a.Version == 0 {
		a.Version = protocol.Version
	}
	if err := protocol.WriteJSON(c, protocol.FrameArm, a); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// An arm on the approval socket, which only the host reaches, calls the
// arm handler with the digest of the rules served.
func TestServeCallsArmHandlerOnArmRequest(t *testing.T) {
	digests := make(chan string, 1)
	al, socket := approvalListener(t)
	srv := newServer(t, daemon.WithApprovals(al, ""), daemon.WithReread("served", nil), daemon.WithArmHandler(func(a protocol.Arm, digest string) bool {
		if a.ConfigDigest == "abc" {
			digests <- digest
		}
		return false
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener())

	f := sendArm(t, socket, protocol.Arm{ConfigDigest: "abc"})
	var reply protocol.Armed
	if f.Type != protocol.FrameArmed || protocol.DecodeJSON(f, &reply) != nil || reply.Restart {
		t.Fatalf("got frame %v %+v; want Armed without step aside", f.Type, reply)
	}
	select {
	case got := <-digests:
		if got != "served" {
			t.Fatalf("arm handler got digest %q, want \"served\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("arm handler was not called")
	}
}

func TestServeRejectsArmWithOtherVersion(t *testing.T) {
	al, socket := approvalListener(t)
	srv := newServer(t, daemon.WithApprovals(al, ""), daemon.WithArmHandler(func(protocol.Arm, string) bool {
		t.Error("arm handler called for a bad version")
		return false
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener())

	if f := sendArm(t, socket, protocol.Arm{Version: protocol.Version + 1}); f.Type != protocol.FrameError {
		t.Fatalf("got frame type %d, want FrameError", f.Type)
	}
}

// The container's socket, which the container reaches, refuses an arm as
// a protocol error: the arm handler is not called, so a process in the
// container cannot re-arm the daemon or make it step aside.
func TestServeRefusesAnArmOnTheContainersSocket(t *testing.T) {
	srv := newServer(t, daemon.WithArmHandler(func(protocol.Arm, string) bool {
		t.Error("arm handler called for an arm on the container's socket")
		return true
	}))
	client, server := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, newChanListener(acceptResult{conn: server})) }()

	client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteJSON(client, protocol.FrameArm, protocol.Arm{Version: protocol.Version, ConfigDigest: "abc"}); err != nil {
		t.Fatal(err)
	}
	f := statusFrame(t, client)
	var e protocol.Error
	if f.Type != protocol.FrameError || protocol.DecodeJSON(f, &e) != nil || e.Code != protocol.ExitHostrunError {
		t.Fatalf("got frame %d %+v, want Error %d", f.Type, e, protocol.ExitHostrunError)
	}
	if want := "an arm is taken on the approval socket only"; !strings.Contains(e.Message, want) {
		t.Fatalf("message %q, want it to contain %q", e.Message, want)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve stopped (%v) after an arm on the container's socket", err)
	case <-time.After(100 * time.Millisecond):
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

func TestServeStopsAfterAnsweringArmWithStepAside(t *testing.T) {
	al, socket := approvalListener(t)
	srv := newServer(t, daemon.WithApprovals(al, ""), daemon.WithArmHandler(func(protocol.Arm, string) bool { return true }))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), newChanListener()) }()

	f := sendArm(t, socket, protocol.Arm{})
	var reply protocol.Armed
	if f.Type != protocol.FrameArmed || protocol.DecodeJSON(f, &reply) != nil || !reply.Restart {
		t.Fatalf("got frame %v %+v; want Armed with step aside", f.Type, reply)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running after stepping aside")
	}
}

// The hello of the approval socket carries the daemon settings the Server
// runs with, as WithSettings gives them, so `hostrunner up` can tell
// whether they are allowed.
func TestApprovalHelloCarriesTheDaemonSettings(t *testing.T) {
	dir, err := os.MkdirTemp("", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")
	al, err := transport.Unix{Path: socket}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"reread": "true", "grace": "15s"}
	srv := newServer(t, daemon.WithApprovals(al, ""), daemon.WithSettings(want))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener())
	info, err := daemonlist.Probe(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(info.Settings, want) {
		t.Fatalf("hello settings %v, want %v", info.Settings, want)
	}
}
