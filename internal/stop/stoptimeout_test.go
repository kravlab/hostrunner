package stop_test

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
)

// startStuckDaemon starts, for a new runtime directory, a daemon that
// answers every connection to its approval socket, confirms a stop, but
// closes neither the socket nor the connection; it returns the
// directory.
func startStuckDaemon(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l, err := transport.Unix{Path: launch.ApprovalSocket(dir)}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	serve := func(conn net.Conn) { // until the stop hangs up
		defer conn.Close()
		answers := map[protocol.FrameType]func() error{
			protocol.FrameHello: func() error {
				return protocol.WriteJSON(conn, protocol.FrameDaemonInfo, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Stops: true})
			},
			protocol.FrameRunning: func() error {
				return protocol.WriteJSON(conn, protocol.FrameRunningList, protocol.RunningList{})
			},
			protocol.FrameStop: func() error { return protocol.WriteJSON(conn, protocol.FrameStopping, struct{}{}) },
		}
		for {
			f, err := protocol.ReadFrame(conn)
			if err != nil {
				return
			}
			if answer := answers[f.Type]; answer == nil || answer() != nil {
				return
			}
		}
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return dir
}

// A daemon that has not closed its sockets once the stop timeout is over
// fails the stop, which waits no longer than the timeout.
func TestStopWaitsTheStopTimeoutForTheDaemonToClose(t *testing.T) {
	dir := startStuckDaemon(t)
	start := time.Now()
	_, err := runStop(t, stop.Config{Dir: dir, Commands: stop.Commands{StopTimeout: 200 * time.Millisecond}}, "")
	if err == nil || !strings.Contains(err.Error(), "the daemon did not stop within 200ms") {
		t.Fatalf("got %v, want the stop timeout's error", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("the stop took %v, far beyond the stop timeout", waited)
	}
}
