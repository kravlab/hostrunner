// Package fakedaemon is for tests: it stands in for a daemon on its
// approval socket, answering each hello with the daemon information a
// test gives and nothing else.
package fakedaemon

import (
	"net"
	"testing"

	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// Serve listens on the approval socket of the runtime directory dir until
// the test ends, and answers a connection that opens with FrameHello with
// FrameDaemonInfo carrying what info returns, called anew for each hello.
// It reads nothing more from a connection and then closes it.
func Serve(t testing.TB, dir string, info func() protocol.DaemonInfo) {
	t.Helper()
	l, err := transport.Unix{Path: launch.ApprovalSocket(dir)}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameHello {
					_ = protocol.WriteJSON(c, protocol.FrameDaemonInfo, info())
				}
			}(c)
		}
	}()
}
