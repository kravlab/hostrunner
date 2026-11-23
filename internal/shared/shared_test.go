package shared_test

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
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
	go func() { served <- shared.Serve(ctx, l, prepare, slog.New(slog.NewTextHandler(processLog, nil))) }()

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
