package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/launch"
)

// lockedBuffer is a bytes.Buffer that a logger and a test may use at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what has been written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serve ends when the context it is given ends, by returning, not by
// ending the process: the test goes on afterwards.
func TestServeReturnsOnceItsContextEnds(t *testing.T) {
	cfg := printenvConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.DiscardHandler), nil) }()
	waitListening(t, cfg.socket)
	waitListening(t, launch.ApprovalSocket(filepath.Dir(cfg.socket)))

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after its context ended, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after its context ended")
	}
}

// serve logs to the logger it is given, which it does not replace with a
// logger of the process's.
func TestServeLogsToTheLoggerItIsGiven(t *testing.T) {
	var out lockedBuffer
	startLoggingServe(t, printenvConfig(t), slog.New(slog.NewTextHandler(&out, nil)))

	if got := out.String(); !strings.Contains(got, "msg=listening") {
		t.Fatalf("the given logger got\n%s\nwant the line listening", got)
	}
}
