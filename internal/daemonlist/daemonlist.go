// Package daemonlist finds the hostrunner daemons of the host: each listens
// on an approval socket beside its runtime directory and says who it is
// when greeted with FrameHello. Approval sessions and `hostrunner stop`
// find the daemons they serve or stop through it.
package daemonlist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// probeTimeout bounds asking one daemon who it is; a socket that does not
// answer within it is skipped.
const probeTimeout = time.Second

// Daemon is a daemon found on its approval socket, as it says it is.
type Daemon struct {
	Socket string
	Info   protocol.DaemonInfo
}

// List returns the daemons that answer on the approval sockets under
// root, ordered by socket; a socket nobody answers on is skipped (only
// daemons remove theirs). The sockets are asked at once, so one that
// hangs holds up the list by probeTimeout at most.
func List(ctx context.Context, root string) []Daemon {
	sockets, _ := filepath.Glob(filepath.Join(root, "*"+protocol.ApprovalSuffix))
	slices.Sort(sockets)
	answers := make([]*protocol.DaemonInfo, len(sockets))
	var wg sync.WaitGroup
	for i, socket := range sockets {
		wg.Go(func() {
			if info, err := Probe(ctx, socket); err == nil {
				answers[i] = &info
			}
		})
	}
	wg.Wait()
	var found []Daemon
	for i, info := range answers {
		if info != nil {
			found = append(found, Daemon{Socket: sockets[i], Info: *info})
		}
	}
	return found
}

// Probe asks the daemon on socket who it is, without serving or stopping
// it.
func Probe(ctx context.Context, socket string) (protocol.DaemonInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return protocol.DaemonInfo{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	return Hello(conn)
}

// Hello greets the daemon on conn, a new connection to its approval
// socket, and returns who it says it is.
func Hello(conn io.ReadWriter) (protocol.DaemonInfo, error) {
	var info protocol.DaemonInfo
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		return info, err
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return info, fmt.Errorf("the daemon did not answer: %w", err)
	}
	if f.Type != protocol.FrameDaemonInfo {
		return info, ErrorOf(f)
	}
	return info, protocol.DecodeJSON(f, &info)
}

// ErrorOf returns the error a daemon sent in f, a protocol.FrameError
// frame, or one naming f as unexpected.
func ErrorOf(f protocol.Frame) error {
	var e protocol.Error
	if f.Type == protocol.FrameError && protocol.DecodeJSON(f, &e) == nil {
		return errors.New(e.Message)
	}
	return fmt.Errorf("unexpected frame type %d from the daemon", f.Type)
}

// Holds reports whether dir is workspace or lies inside it.
func Holds(workspace, dir string) bool {
	if workspace == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(workspace, dir)
	return err == nil && filepath.IsLocal(rel)
}
