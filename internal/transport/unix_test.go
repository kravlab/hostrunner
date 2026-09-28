package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixListenAndDialExchangeBytes(t *testing.T) {
	u := Unix{Path: filepath.Join(t.TempDir(), "h.sock")}
	l, err := u.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("pong"))
	}()

	c, err := u.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pong" {
		t.Fatalf("got %q, want %q", got, "pong")
	}
}

func TestUnixListenCreatesOwnerOnlySocket(t *testing.T) {
	u := Unix{Path: filepath.Join(t.TempDir(), "h.sock")}
	l, err := u.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(u.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestUnixListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.sock")
	// Leave a socket file behind without unlinking it, as a crashed daemon would.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	l, err := Unix{Path: path}.Listen()
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	l.Close()
}

func TestUnixListenRefusesToReplaceRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.sock")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := (Unix{Path: path}).Listen(); err == nil {
		l.Close()
		t.Fatal("expected Listen to refuse a path holding a regular file")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "data" {
		t.Fatalf("regular file was modified: %q, %v", data, err)
	}
}

func TestUnixDialFailsWithoutListener(t *testing.T) {
	u := Unix{Path: filepath.Join(t.TempDir(), "missing.sock")}
	if c, err := u.Dial(context.Background()); err == nil {
		c.Close()
		t.Fatal("expected Dial to fail when nothing listens")
	}
}

func TestUnixListenRefusesSocketOfRunningDaemon(t *testing.T) {
	u := Unix{Path: filepath.Join(t.TempDir(), "h.sock")}
	first, err := u.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if second, err := u.Listen(); !errors.Is(err, ErrAlreadyListening) {
		if err == nil {
			second.Close()
		}
		t.Fatalf("second Listen: got %v, want ErrAlreadyListening", err)
	}

	accepted := make(chan struct{})
	go func() {
		if c, err := first.Accept(); err == nil {
			c.Close()
			close(accepted)
		}
	}()
	c, err := u.Dial(context.Background())
	if err != nil {
		t.Fatalf("first daemon lost its socket: %v", err)
	}
	c.Close()
	<-accepted
}
