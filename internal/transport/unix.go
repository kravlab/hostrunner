package transport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"
)

// ErrAlreadyListening means another process is serving the socket path.
var ErrAlreadyListening = errors.New("another daemon is already listening on the socket")

// probeTimeout bounds the liveness check of an existing socket file.
const probeTimeout = time.Second

// Unix is a Transport over a Unix domain socket at Path. Access control is
// the filesystem: the socket is created owner-only (mode 0600) and reaches
// the container only through an explicit bind mount of its directory.
type Unix struct {
	Path string
}

var _ Transport = Unix{}

// Listen binds the socket, first removing a stale socket left by a daemon
// that did not shut down cleanly. It returns ErrAlreadyListening if a live
// daemon still answers on Path; any other kind of file at Path is left
// untouched and reported as an error.
//
// The socket is created under umask 0177 so it is never reachable with
// looser permissions, not even briefly. The umask is process-wide, so Listen
// must run before the process starts goroutines that create files.
func (u Unix) Listen() (net.Listener, error) {
	if err := removeStaleSocket(u.Path); err != nil {
		return nil, err
	}
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", u.Path)
	syscall.Umask(old)
	return l, err
}

// Dial connects to the socket.
func (u Unix) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", u.Path)
}

// removeStaleSocket deletes the socket file at path if no daemon answers on
// it. A missing path is fine; a live socket or a non-socket file is an error.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	c, err := net.DialTimeout("unix", path, probeTimeout)
	if err == nil {
		c.Close()
		return fmt.Errorf("%w: %s", ErrAlreadyListening, path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("probe existing socket %s: %w", path, err)
	}
	return os.Remove(path)
}
