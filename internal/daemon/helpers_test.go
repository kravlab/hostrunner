package daemon_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// policyOf is a policy that allows every command as allowed describes.
type policyOf struct{ allowed rules.Allowed }

// Check allows every command, as p.allowed describes it.
func (p policyOf) Check([]string) (rules.Allowed, error) { return p.allowed, nil }

// newPolicyServer returns a Server over a temporary workspace, with policy
// and opts, that logs nowhere.
func newPolicyServer(t *testing.T, policy daemon.Policy, opts ...daemon.Option) *daemon.Server {
	t.Helper()
	return newLoggingServer(t, slog.New(slog.DiscardHandler), policy, opts...)
}

// newLoggingServer is newPolicyServer for a Server that logs to log.
func newLoggingServer(t *testing.T, log *slog.Logger, policy daemon.Policy, opts ...daemon.Option) *daemon.Server {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mapper, err := workspace.NewMapper(root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	return daemon.New(mapper, policy, log, opts...)
}

// startServer serves a Server of newPolicyServer until the test ends, and
// returns the listener to hand it connections on.
func startServer(t *testing.T, policy daemon.Policy, opts ...daemon.Option) *chanListener {
	t.Helper()
	srv := newPolicyServer(t, policy, opts...)
	l := newChanListener()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.Serve(ctx, l)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return l
}

// answer is what a daemon sent for one request: the stdout of its
// command, and the frame that ended the exchange.
type answer struct {
	stdout string
	end    protocol.Frame
}

// ask sends the daemon on l a frame of type ft for argv, on a connection of
// its own, and returns its answer: the exchange ends with an Exit, an
// Error or, for a dry run, an Allowed frame.
func ask(t *testing.T, l *chanListener, ft protocol.FrameType, argv ...string) answer {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	l.results <- acceptResult{conn: server}
	client.SetDeadline(time.Now().Add(10 * time.Second))
	req := protocol.Request{Version: protocol.Version, Argv: argv, Cwd: containerRoot}
	if err := protocol.WriteJSON(client, ft, req); err != nil {
		t.Fatal(err)
	}
	var got answer
	for {
		f, err := protocol.ReadFrame(client)
		if err != nil {
			t.Fatalf("no end of the exchange: %v", err)
		}
		switch f.Type {
		case protocol.FrameStdout:
			got.stdout += string(f.Payload)
		case protocol.FrameExit, protocol.FrameError, protocol.FrameAllowed:
			got.end = f
			return got
		}
	}
}

// endCase is one exchange of checkEnds: the request argv sent to l ends
// with a frame of type want.
type endCase struct {
	name string
	l    *chanListener
	argv []string
	want protocol.FrameType
}

// checkEnds sends each case's request, as a frame of type ft, to its
// listener, in a subtest of its own, and checks which frame ended the
// exchange.
func checkEnds(t *testing.T, ft protocol.FrameType, cases []endCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ask(t, tt.l, ft, tt.argv...)

			if got.end.Type != tt.want {
				t.Fatalf("the exchange ended with frame %d %s, want frame %d", got.end.Type, got.end.Payload, tt.want)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer that a logger and a test may use at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what has been written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
