package transport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
// The socket is never reachable with permissions looser than the owner's,
// not even briefly: it is bound in a new directory only the owner can
// enter, beside Path, made owner-only there and then linked to Path,
// already listening. The link never replaces what is at Path: of two
// Listens at once, the second gets ErrAlreadyListening. The process's
// umask, which is process-wide, is left alone, so Listen may run while
// other goroutines create files. Closing the listener removes the socket.
func (u Unix) Listen() (net.Listener, error) {
	if err := removeStaleSocket(u.Path); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(u.Path), ".hr")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// The socket leaves tmp, which goes with dir: Close removes it at Path.
	l.SetUnlinkOnClose(false)
	if err = os.Chmod(tmp, 0o600); err == nil {
		err = os.Link(tmp, u.Path)
	}
	if errors.Is(err, fs.ErrExist) {
		if info, lerr := os.Lstat(u.Path); lerr == nil && info.Mode().Type() == fs.ModeSocket {
			err = fmt.Errorf("%w: %s", ErrAlreadyListening, u.Path)
		}
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	return &unixListener{UnixListener: l, path: u.Path}, nil
}

// unixListener is a listener on the Unix socket at path, which Close
// removes, once.
type unixListener struct {
	*net.UnixListener
	path   string
	remove sync.Once
}

// Close removes the socket and stops listening, in that order, as
// net.UnixListener does: once Accept returns, the socket is gone.
func (l *unixListener) Close() error {
	var err error
	l.remove.Do(func() {
		if rerr := os.Remove(l.path); !errors.Is(rerr, fs.ErrNotExist) {
			err = rerr
		}
	})
	if cerr := l.UnixListener.Close(); err == nil {
		err = cerr
	}
	return err
}

// Addr returns the socket's address, at its path.
func (l *unixListener) Addr() net.Addr { return &net.UnixAddr{Name: l.path, Net: "unix"} }

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

// PeerCred returns the uid and pid of the process on the other end of conn,
// a Unix socket connection, as the kernel recorded them when it connected
// (SO_PEERCRED). The daemon checks the uid on its approval socket.
func PeerCred(conn net.Conn) (uid, pid int, err error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("peer credentials of a %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if credErr != nil {
		return 0, 0, credErr
	}
	return int(cred.Uid), int(cred.Pid), nil
}
