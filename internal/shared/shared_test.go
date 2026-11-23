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

// A daemon of the shared process that panics ends alone: the other keeps
// running, and the panic, with its stack, is in the daemon's log and in
// the process log.
func TestServeEndsOnlyTheDaemonThatPanics(t *testing.T) {
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
				panic("the daemon broke")
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
	for name, log := range map[string]string{"daemon": logs["panics"].String(), "process": processLog.String()} {
		if !strings.Contains(log, "the daemon broke") || !strings.Contains(log, "shared_test.go") {
			t.Errorf("the %s log lacks the panic and its stack:\n%s", name, log)
		}
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}
