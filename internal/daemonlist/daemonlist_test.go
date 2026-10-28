package daemonlist_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

func TestHolds(t *testing.T) {
	tests := []struct {
		workspace, dir string
		want           bool
	}{
		{"/home/u/app", "/home/u/app", true},
		{"/home/u/app", "/home/u/app/sub/dir", true},
		{"/home/u/app", "/home/u/application", false},
		{"/home/u/app", "/home/u", false},
		{"", "/home/u/app", false},
		{"/home/u/app", "", false},
	}
	for _, tt := range tests {
		if got := daemonlist.Holds(tt.workspace, tt.dir); got != tt.want {
			t.Errorf("Holds(%q, %q) = %v, want %v", tt.workspace, tt.dir, got, tt.want)
		}
	}
}

// fakeDaemon answers every hello on socket with info, or refuses it with
// the message refusal when set, until the test ends. It refuses anything
// but a FrameHello of the approval protocol's version, as hello must be.
func fakeDaemon(t *testing.T, socket string, info protocol.DaemonInfo, refusal string) {
	t.Helper()
	l, err := transport.Unix{Path: socket}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				f, err := protocol.ReadFrame(conn)
				if err != nil {
					return
				}
				var hello protocol.Hello
				if f.Type != protocol.FrameHello || protocol.DecodeJSON(f, &hello) != nil || hello.Version != protocol.ApprovalVersion {
					_ = protocol.WriteJSON(conn, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "not a hello"})
					return
				}
				if refusal != "" {
					_ = protocol.WriteJSON(conn, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: refusal})
					return
				}
				_ = protocol.WriteJSON(conn, protocol.FrameDaemonInfo, info)
			}(conn)
		}
	}()
}

func TestListFindsTheDaemonsBySocketSkippingDeadOnes(t *testing.T) {
	root := t.TempDir()
	b := protocol.DaemonInfo{Version: 1, Workspace: "/home/u/b"}
	a := protocol.DaemonInfo{Version: 1, Workspace: "/home/u/a", Stops: true}
	fakeDaemon(t, filepath.Join(root, "b"+protocol.ApprovalSuffix), b, "")
	fakeDaemon(t, filepath.Join(root, "a"+protocol.ApprovalSuffix), a, "")
	// A socket file nobody answers on, as a daemon killed by SIGKILL leaves.
	dead := filepath.Join(root, "c"+protocol.ApprovalSuffix)
	l, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := os.WriteFile(filepath.Join(root, "d.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := daemonlist.List(context.Background(), root)
	want := []daemonlist.Daemon{
		{Socket: filepath.Join(root, "a"+protocol.ApprovalSuffix), Info: a},
		{Socket: filepath.Join(root, "b"+protocol.ApprovalSuffix), Info: b},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A socket that accepts and never answers holds the list up by the probe
// timeout at most, and is left out.
func TestListSkipsADaemonThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	live := protocol.DaemonInfo{Version: 1, Workspace: "/home/u/live"}
	fakeDaemon(t, filepath.Join(root, "live"+protocol.ApprovalSuffix), live, "")
	hung, err := transport.Unix{Path: filepath.Join(root, "hung"+protocol.ApprovalSuffix)}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hung.Close() })
	go func() {
		for {
			conn, err := hung.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	start := time.Now()
	got := daemonlist.List(context.Background(), root)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("List took %v with a socket that does not answer", took)
	}
	if len(got) != 1 || got[0].Info != live {
		t.Fatalf("got %+v, want only the daemon that answers", got)
	}
}

func TestProbeReportsTheDaemonsRefusal(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "x"+protocol.ApprovalSuffix)
	fakeDaemon(t, socket, protocol.DaemonInfo{}, "approval socket connection refused")
	_, err := daemonlist.Probe(context.Background(), socket)
	if err == nil || err.Error() != "approval socket connection refused" {
		t.Fatalf("got %v, want the daemon's refusal", err)
	}
}

func TestProbeFailsWithoutADaemon(t *testing.T) {
	_, err := daemonlist.Probe(context.Background(), filepath.Join(t.TempDir(), "none"+protocol.ApprovalSuffix))
	if err == nil {
		t.Fatal("Probe found a daemon on a missing socket")
	}
}

func TestErrorOf(t *testing.T) {
	payload := []byte(`{"code":125,"message":"approval protocol version mismatch"}`)
	if err := daemonlist.ErrorOf(protocol.Frame{Type: protocol.FrameError, Payload: payload}); err == nil || err.Error() != "approval protocol version mismatch" {
		t.Fatalf("error frame: got %v, want its message", err)
	}
	want := fmt.Sprintf("unexpected frame type %d from the daemon", protocol.FrameServing)
	if err := daemonlist.ErrorOf(protocol.Frame{Type: protocol.FrameServing}); err == nil || err.Error() != want {
		t.Fatalf("other frame: got %v, want %q", err, want)
	}
	want = fmt.Sprintf("unexpected frame type %d from the daemon", protocol.FrameError)
	if err := daemonlist.ErrorOf(protocol.Frame{Type: protocol.FrameError, Payload: []byte("{")}); err == nil || err.Error() != want {
		t.Fatalf("malformed error frame: got %v, want %q", err, want)
	}
}
