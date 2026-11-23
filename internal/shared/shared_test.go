package shared_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/contain"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/shared"
	"github.com/kravlab/hostrunner/internal/transport"
)

// logBuffer is a log the test reads while the process writes it.
type logBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed chan struct{}
}

func newLogBuffer() *logBuffer { return &logBuffer{closed: make(chan struct{})} }

// Write appends p to the log.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Close marks the log closed.
func (b *logBuffer) Close() error {
	close(b.closed)
	return nil
}

// String returns what the log holds.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A daemon of the shared process that panics, in its Serve or in a
// goroutine it started under its context, once or twice, ends alone: the
// other keeps running, and each panic, with its stack, is in the daemon's
// log and in the process log, not in the other daemon's log.
func TestServeEndsOnlyTheDaemonThatPanics(t *testing.T) {
	cases := []struct {
		name      string
		panicking func(ctx context.Context) // called from the daemon's Serve
		panics    []string                  // the values it panics with
	}{
		{"in its Serve", func(context.Context) { panic("the daemon broke") }, []string{"the daemon broke"}},
		{"in a goroutine it started", func(ctx context.Context) {
			contain.Go(ctx, func() { panic("the daemon broke") })
		}, []string{"the daemon broke"}},
		{"twice in a goroutine it started", func(ctx context.Context) {
			var wg sync.WaitGroup
			contain.GoIn(ctx, &wg, func() {
				contain.Run(ctx, func() { panic("the daemon broke") })
				panic("and broke again")
			})
			wg.Wait()
		}, []string{"the daemon broke", "and broke again"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testServeEndsOnlyTheDaemonThatPanics(t, tc.panicking, tc.panics) })
	}
}

// testServeEndsOnlyTheDaemonThatPanics runs TestServeEndsOnlyTheDaemonThatPanics
// for a daemon that panics with the values panics, calling panicking from
// its Serve.
func testServeEndsOnlyTheDaemonThatPanics(t *testing.T, panicking func(ctx context.Context), panics []string) {
	dir := t.TempDir()
	control := filepath.Join(dir, "control.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	logs := map[string]*logBuffer{"panics": newLogBuffer(), "serves": newLogBuffer()}
	prepare := func(args, _ []string) (shared.Daemon, error) {
		name := args[0]
		return shared.Daemon{Socket: name, Log: logs[name], Serve: func(ctx context.Context) error {
			if name == "panics" {
				panicking(ctx)
			}
			<-ctx.Done()
			return nil
		}}, nil
	}
	processLog := newLogBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- shared.Serve(ctx, l, prepare, time.Minute, slog.New(slog.NewTextHandler(processLog, nil)))
	}()

	for _, name := range []string{"serves", "panics"} {
		if err := shared.Add(ctx, control, protocol.AddDaemon{Args: []string{name}}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	select {
	case <-logs["panics"].closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon that panicked did not end")
	}
	select {
	case err := <-served:
		t.Fatalf("the process ended with the daemon that panicked: %v", err)
	case <-logs["serves"].closed:
		t.Fatal("the other daemon ended with the one that panicked")
	case <-time.After(100 * time.Millisecond):
	}
	for _, value := range panics {
		for name, log := range map[string]string{"daemon": logs["panics"].String(), "process": processLog.String()} {
			if !strings.Contains(log, value) || !strings.Contains(log, "shared_test.go") {
				t.Errorf("the %s log lacks the panic %q and its stack:\n%s", name, value, log)
			}
		}
		if log := logs["serves"].String(); strings.Contains(log, value) {
			t.Errorf("the other daemon's log has the panic %q:\n%s", value, log)
		}
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// A panic while the shared process prepares the daemon of an add request
// refuses that add alone: its peer gets a refusal that does not carry the
// stack, the panic with its stack is in the process log, and the process
// goes on, with the daemon it runs and with later adds.
func TestServeRefusesAnAddWhosePreparationPanics(t *testing.T) {
	control := filepath.Join(t.TempDir(), "control.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	logs := map[string]*logBuffer{"first": newLogBuffer(), "later": newLogBuffer()}
	prepare := func(args, _ []string) (shared.Daemon, error) {
		name := args[0]
		if name == "panics" {
			panic("the preparation broke")
		}
		return shared.Daemon{Socket: name, Log: logs[name], Serve: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}}, nil
	}
	processLog := newLogBuffer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- shared.Serve(ctx, l, prepare, time.Minute, slog.New(slog.NewTextHandler(processLog, nil)))
	}()

	if err := shared.Add(ctx, control, protocol.AddDaemon{Args: []string{"first"}}); err != nil {
		t.Fatalf("add first: %v", err)
	}
	err = shared.Add(ctx, control, protocol.AddDaemon{Args: []string{"panics"}})
	if err == nil || errors.Is(err, shared.ErrEnding) {
		t.Errorf("the add whose preparation panicked: got %v, want a refusal that is not ErrEnding", err)
	} else if strings.Contains(err.Error(), "shared_test.go") {
		t.Errorf("the refusal carries the stack:\n%v", err)
	}
	if err := shared.Add(ctx, control, protocol.AddDaemon{Args: []string{"later"}}); err != nil {
		t.Errorf("add later, after the panic: %v", err)
	}
	select {
	case err := <-served:
		t.Fatalf("the process ended with the add that panicked: %v", err)
	case <-logs["first"].closed:
		t.Fatal("the daemon already running ended with the add that panicked")
	case <-time.After(100 * time.Millisecond):
	}
	if log := processLog.String(); !strings.Contains(log, "the preparation broke") || !strings.Contains(log, "shared_test.go") {
		t.Errorf("the process log lacks the panic and its stack:\n%s", log)
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// A shared process that gets no daemon within its first-add timeout
// ends on its own: Serve returns, its control socket is removed and the
// process log says why.
func TestServeEndsWhenNoDaemonIsAddedInTime(t *testing.T) {
	control := filepath.Join(t.TempDir(), "control.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(_, _ []string) (shared.Daemon, error) {
		t.Error("a daemon was prepared with no add sent")
		return shared.Daemon{}, nil
	}
	processLog := newLogBuffer()
	served := make(chan error, 1)
	go func() {
		served <- shared.Serve(context.Background(), l, prepare, 100*time.Millisecond, slog.New(slog.NewTextHandler(processLog, nil)))
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the process runs on with no daemon added past its timeout")
	}
	if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket after Serve returned: %v, want it removed", err)
	}
	if log := processLog.String(); !strings.Contains(log, `msg="no daemon added" timeout=100ms`) {
		t.Errorf("the process log does not say no daemon was added in time:\n%s", log)
	}
}

// A connection that sends nothing, or only part of a request, does not
// keep a shared process whose end is decided alive for the time a request
// may take: once its first-add timeout ends it, Serve returns at once,
// and the connection is answered FrameEnding, or closed if the process
// ended before it accepted it.
func TestServeEndsAtOnceWithAConnectionNotYetRead(t *testing.T) {
	cases := []struct {
		name string
		sent []byte // what the connection sends before it waits
	}{
		{"sending nothing", nil},
		{"sending part of a request", []byte{byte(protocol.FrameAddDaemon)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testServeEndsAtOnceWithAConnectionNotYetRead(t, tc.sent) })
	}
}

// testServeEndsAtOnceWithAConnectionNotYetRead runs
// TestServeEndsAtOnceWithAConnectionNotYetRead for a connection that
// sends sent, then waits.
func testServeEndsAtOnceWithAConnectionNotYetRead(t *testing.T, sent []byte) {
	const firstAdd = 100 * time.Millisecond
	control := filepath.Join(t.TempDir(), "control.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := net.Dial("unix", control)
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Close()
	if _, err := waiting.Write(sent); err != nil {
		t.Fatal(err)
	}
	prepare := func(_, _ []string) (shared.Daemon, error) {
		t.Error("a daemon was prepared with no add sent")
		return shared.Daemon{}, nil
	}
	start := time.Now()
	served := make(chan error, 1)
	go func() {
		served <- shared.Serve(context.Background(), l, prepare, firstAdd, slog.New(slog.NewTextHandler(newLogBuffer(), nil)))
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve does not return")
	}
	// Room for a loaded machine, yet clearly less than the 5 s a request
	// may take, which a process that waited for the connection would.
	const slack = 2500 * time.Millisecond
	if elapsed := time.Since(start); elapsed > firstAdd+slack {
		t.Errorf("Serve returned %v after its start, want within %v of its first-add timeout of %v", elapsed, slack, firstAdd)
	}
	_ = waiting.SetReadDeadline(time.Now().Add(time.Second))
	f, err := protocol.ReadFrame(waiting)
	switch {
	case err == nil && f.Type != protocol.FrameEnding:
		t.Errorf("the connection got frame type %d, want FrameEnding", f.Type)
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET):
		t.Errorf("the connection: %v, want FrameEnding or the connection closed", err)
	}
}

// An add that reaches a shared process whose end is decided while its
// daemon is being prepared gets ErrEnding, which the process log records
// as a refused add, while an ordinary refusal does not get it.
func TestAddToAnEndingProcessGetsErrEnding(t *testing.T) {
	control := filepath.Join(t.TempDir(), "control.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	preparing, release := make(chan struct{}), make(chan struct{})
	prepare := func(args, _ []string) (shared.Daemon, error) {
		if args[0] == "refused" {
			return shared.Daemon{}, errors.New("the arguments are refused")
		}
		close(preparing)
		<-release
		return shared.Daemon{Socket: args[0], Log: newLogBuffer(), Serve: func(ctx context.Context) error {
			t.Error("a daemon started in a process whose end was decided")
			return nil
		}}, nil
	}
	processLog := newLogBuffer()
	served := make(chan error, 1)
	go func() {
		served <- shared.Serve(context.Background(), l, prepare, time.Minute, slog.New(slog.NewTextHandler(processLog, nil)))
	}()
	added := make(chan error, 1)
	go func() { added <- shared.Add(context.Background(), control, protocol.AddDaemon{Args: []string{"late"}}) }()
	<-preparing

	// Refused while no daemon runs, this add decides the end.
	if err := shared.Add(context.Background(), control, protocol.AddDaemon{Args: []string{"refused"}}); err == nil || errors.Is(err, shared.ErrEnding) {
		t.Errorf("the ordinary refusal: got %v, want an error that is not ErrEnding", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Lstat(control); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket is not removed after the refused add")
		}
	}
	close(release)

	if err := <-added; !errors.Is(err, shared.ErrEnding) {
		t.Errorf("the add to the ending process: got %v, want ErrEnding", err)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
	if log := processLog.String(); !strings.Contains(log, `msg="add refused"`) || !strings.Contains(log, `error="the shared process is ending"`) {
		t.Errorf("the process log does not record the add refused as the process is ending:\n%s", log)
	}
}

// Add takes a control connection closed before any byte of an answer, as
// a process ending with the request still in its listener's backlog
// closes it, for ErrEnding.
func TestAddGetsErrEndingWhenClosedUnanswered(t *testing.T) {
	cases := []struct {
		name string
		peer func(c net.Conn) // what the peer does with the connection before it closes it
	}{
		{"after reading the request", func(c net.Conn) { _, _ = protocol.ReadFrame(c) }},
		// Closed with the rest of the request unread, the connection is
		// reset rather than closed.
		{"with the request unread", func(c net.Conn) { _, _ = c.Read(make([]byte, 1)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			control := servePeer(t, tc.peer)
			if err := shared.Add(context.Background(), control, protocol.AddDaemon{}); !errors.Is(err, shared.ErrEnding) {
				t.Errorf("Add: got %v, want ErrEnding", err)
			}
		})
	}
}

// Add takes a control connection the process closes while the request is
// still being written, before any byte of an answer, for ErrEnding: the
// request is larger than the socket buffers, so the write is still in
// progress when the peer, having read one byte of it, closes.
func TestAddGetsErrEndingWhenClosedWhileTheRequestIsWritten(t *testing.T) {
	control := servePeer(t, func(c net.Conn) { _, _ = c.Read(make([]byte, 1)) })
	req := protocol.AddDaemon{Env: []string{strings.Repeat("x", protocol.MaxPayload-protocol.MaxPayload/8)}}
	if err := shared.Add(context.Background(), control, req); !errors.Is(err, shared.ErrEnding) {
		t.Errorf("Add: got %v, want ErrEnding", err)
	}
}

// Add takes a control connection closed in the middle of an answer for
// an ordinary error, not ErrEnding: the process answered.
func TestAddDoesNotGetErrEndingForAPartialAnswer(t *testing.T) {
	partial := []byte{byte(protocol.FrameDaemonAdded), 0}
	cases := []struct {
		name string
		peer func(c net.Conn) // what the peer does with the connection before it closes it
	}{
		{"closed after reading the request", func(c net.Conn) {
			_, _ = protocol.ReadFrame(c)
			_, _ = c.Write(partial)
		}},
		{"reset with the request unread", func(c net.Conn) {
			_, _ = c.Read(make([]byte, 1))
			_, _ = c.Write(partial)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			control := servePeer(t, tc.peer)
			if err := shared.Add(context.Background(), control, protocol.AddDaemon{}); err == nil || errors.Is(err, shared.ErrEnding) {
				t.Errorf("Add: got %v, want an error that is not ErrEnding", err)
			}
		})
	}
}

// servePeer listens on a control socket of its own, whose path it
// returns, and hands peer the first connection, which it closes after.
func servePeer(t *testing.T, peer func(c net.Conn)) string {
	t.Helper()
	control := filepath.Join(t.TempDir(), "control.sock")
	l, err := transport.Unix{Path: control}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		peer(c)
	}()
	return control
}
