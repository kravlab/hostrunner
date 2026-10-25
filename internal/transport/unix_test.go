package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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

// Listen leaves the process's umask alone: a file another goroutine
// creates meanwhile gets the mode it would get anyway.
func TestUnixListenLeavesTheUmaskAlone(t *testing.T) {
	dir := t.TempDir()
	files := t.TempDir()
	ref := filepath.Join(files, "ref")
	if err := os.WriteFile(ref, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	want := info.Mode().Perm()
	stop := make(chan struct{})
	var wrong atomic.Uint32
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			name := filepath.Join(files, strconv.Itoa(i))
			if err := os.WriteFile(name, nil, 0o666); err != nil {
				t.Error(err)
				return
			}
			info, err := os.Stat(name)
			if err == nil {
				err = os.Remove(name)
			}
			if err != nil {
				t.Error(err)
				return
			}
			if info.Mode().Perm() != want {
				wrong.Add(1)
			}
		}
	})
	for i := range 300 {
		l, err := Unix{Path: filepath.Join(dir, strconv.Itoa(i)+".sock")}.Listen()
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
	close(stop)
	wg.Wait()
	if n := wrong.Load(); n > 0 {
		t.Fatalf("%d files created while Listen ran did not get mode %o", n, want)
	}
}

// Of two Listens on one path at once, one listens there and the other is
// refused: neither takes the other's socket.
func TestUnixListenTwiceAtOnceListensOnce(t *testing.T) {
	dir := t.TempDir()
	for i := range 100 {
		path := filepath.Join(dir, strconv.Itoa(i)+".sock")
		var ls [2]net.Listener
		var errs [2]error
		var wg sync.WaitGroup
		for j := range 2 {
			wg.Go(func() { ls[j], errs[j] = Unix{Path: path}.Listen() })
		}
		wg.Wait()
		listening := 0
		for j := range 2 {
			if errs[j] == nil {
				listening++
				defer ls[j].Close()
			} else if !errors.Is(errs[j], ErrAlreadyListening) {
				t.Fatalf("Listen %d: %v, want ErrAlreadyListening", j, errs[j])
			}
		}
		if listening != 1 {
			t.Fatalf("pair %d: %d Listens listen on %s, want 1", i, listening, path)
		}
	}
}

// Closing the listener removes its socket and leaves nothing else behind.
func TestUnixListenerCloseRemovesTheSocket(t *testing.T) {
	dir := t.TempDir()
	l, err := Unix{Path: filepath.Join(dir, "h.sock")}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) > 0 {
		t.Fatalf("after Close the directory holds %v (%v), want nothing", entries, err)
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

// The kernel reports who connected; with both ends in this test that is
// this process.
func TestPeerCredReportsTheConnectingProcess(t *testing.T) {
	u := Unix{Path: filepath.Join(t.TempDir(), "h.sock")}
	l, err := u.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	c, err := u.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer server.Close()
	uid, pid, err := PeerCred(server)
	if err != nil {
		t.Fatal(err)
	}
	if uid != os.Getuid() || pid != os.Getpid() {
		t.Fatalf("PeerCred = uid %d pid %d, want uid %d pid %d", uid, pid, os.Getuid(), os.Getpid())
	}
}
