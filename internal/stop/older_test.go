package stop_test

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
)

// olderDaemonEnv makes the test binary act as a daemon of an older
// hostrunner, which does not stop on request: it listens on the socket its
// --socket names, or on the one listenEnv names when set, and, when the
// variable is "approval", answers hellos on the approval socket beside
// the runtime directory too. SIGTERM ends it.
const (
	olderDaemonEnv = "HOSTRUNNER_STOP_TEST_OLDER_DAEMON"
	listenEnv      = "HOSTRUNNER_STOP_TEST_LISTEN"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(olderDaemonEnv); mode != "" {
		runOlderDaemon(mode)
		return
	}
	if os.Getenv(newDaemonEnv) != "" && len(os.Args) > 1 && os.Args[1] == "serve" {
		runNewDaemon()
		return
	}
	code := m.Run()
	if staticClientDir != "" {
		os.RemoveAll(staticClientDir)
	}
	os.Exit(code)
}

// runOlderDaemon is the process an olderDaemonEnv test binary runs.
func runOlderDaemon(mode string) {
	arg := func(name string) string {
		for i, a := range os.Args {
			if a == name && i+1 < len(os.Args) {
				return os.Args[i+1]
			}
		}
		return ""
	}
	socket := arg("--socket")
	info := protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: arg("--workspace"), ContainerWorkspace: arg("--container-workspace"), RulesFile: cmp.Or(arg("--rules-file"), arg("--config"))}
	if l := os.Getenv(listenEnv); l != "" {
		socket = l
	}
	serve := func(path string, hello bool) {
		l, err := transport.Unix{Path: path}.Listen()
		if err != nil {
			os.Exit(3)
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					if f, err := protocol.ReadFrame(c); hello && err == nil && f.Type == protocol.FrameHello {
						_ = protocol.WriteJSON(c, protocol.FrameDaemonInfo, info)
					}
					_, _ = io.Copy(io.Discard, c)
				}(c)
			}
		}()
	}
	if mode == "approval" {
		serve(launch.ApprovalSocket(filepath.Dir(socket)), true)
	}
	serve(socket, false)
	select {}
}

// olderDaemon is an older daemon a test runs.
type olderDaemon struct {
	dir    string // its runtime directory
	cmd    *exec.Cmd
	exited chan struct{} // closed once it has ended, cmd.ProcessState set
}

// startOlderDaemon starts an older daemon for a new runtime directory,
// run as `hostrunner serve --socket <dir>/hostrunner.sock …`, with an
// approval socket unless preApproval; listen, when set, is where it
// listens instead. It is killed when the test ends.
func startOlderDaemon(t *testing.T, preApproval bool, listen string) *olderDaemon {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	hostrunner := filepath.Join(t.TempDir(), "hostrunner")
	if err := os.Symlink(exe, hostrunner); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, launch.SocketName)
	cmd := exec.Command(hostrunner, "serve", "--socket", socket, "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app", "--config", filepath.Join(dir, "hostrun.yaml"), "--watch")
	mode := "approval"
	if preApproval {
		mode = "pre-approval"
	}
	cmd.Env = append(os.Environ(), olderDaemonEnv+"="+mode, listenEnv+"="+listen)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &olderDaemon{dir: dir, cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-d.exited
	})
	wait := socket
	if listen != "" {
		wait = listen
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := (transport.Unix{Path: wait}).Dial(context.Background()); err == nil {
			c.Close()
			return d
		}
		if time.Now().After(deadline) {
			t.Fatal("the older daemon did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// running reports whether the daemon still runs.
func (d *olderDaemon) running() bool {
	select {
	case <-d.exited:
		return false
	default:
		return true
	}
}

// waitTerminated fails the test unless the daemon ends by SIGTERM within
// 5 s.
func (d *olderDaemon) waitTerminated(t *testing.T) {
	t.Helper()
	select {
	case <-d.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the older daemon still runs")
	}
	if ws, ok := d.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("the older daemon ended with %v, want SIGTERM", d.cmd.ProcessState)
	}
}

// runStop runs a stop as cfg configures it, answering with answers, and
// returns what it printed and returned.
func runStop(t *testing.T, cfg stop.Config, answers string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- stop.Run(context.Background(), cfg, strings.NewReader(answers), &out) }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(10 * time.Second):
		t.Fatal("the stop did not end")
		return "", nil
	}
}

func TestStopKillsAnOlderDaemonWithSIGTERM(t *testing.T) {
	d := startOlderDaemon(t, false, "")
	out, err := runStop(t, stop.Config{Dir: d.dir, Commands: stop.Commands{Answer: stop.Kill}}, "")
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	if !strings.Contains(out, d.dir+" is of an older hostrunner") || !strings.HasSuffix(out, "hostrunner: stopped "+d.dir+"\n") {
		t.Fatalf("printed %q", out)
	}
	d.waitTerminated(t)
}

func TestStopAsksKillOrQuitForAnOlderDaemon(t *testing.T) {
	d := startOlderDaemon(t, false, "")
	out, err := runStop(t, stop.Config{Dir: d.dir, Commands: stop.Commands{Terminal: true}}, "w\nq\n")
	if err == nil || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("got %v, want the daemon left running", err)
	}
	// w is not offered: asked again.
	if strings.Count(out, "[k/q] ? ") != 2 {
		t.Fatalf("printed %q, want the question asked twice", out)
	}
	if !d.running() {
		t.Fatal("the older daemon was stopped")
	}
	if _, err := runStop(t, stop.Config{Dir: d.dir, Commands: stop.Commands{Terminal: true}}, "k\n"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	d.waitTerminated(t)
}

func TestStopOfAnOlderDaemonNeedsKillWithoutATerminal(t *testing.T) {
	d := startOlderDaemon(t, false, "")
	for _, answer := range []stop.Answer{stop.Ask, stop.Wait} {
		_, err := runStop(t, stop.Config{Dir: d.dir, Commands: stop.Commands{Answer: answer}}, "")
		if err == nil || !strings.Contains(err.Error(), "--kill") {
			t.Fatalf("answer %v: got %v, want an error naming --kill", answer, err)
		}
	}
	if !d.running() {
		t.Fatal("the older daemon was stopped")
	}
}

func TestStopFindsADaemonFromBeforeApprovalsByDir(t *testing.T) {
	d := startOlderDaemon(t, true, "")
	out, err := runStop(t, stop.Config{Dir: d.dir, Commands: stop.Commands{Answer: stop.Kill}}, "")
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	d.waitTerminated(t)
}

// A process listening on the daemon's socket whose command line names
// another socket is not signalled.
func TestStopRefusesAProcessThatServesAnotherSocket(t *testing.T) {
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Run for a runtime directory of its own, it listens on other's socket.
	impostor := startOlderDaemon(t, true, filepath.Join(other, launch.SocketName))
	_, err = runStop(t, stop.Config{Dir: other, Commands: stop.Commands{Answer: stop.Kill}}, "")
	if err == nil || !strings.Contains(err.Error(), "no signal sent") {
		t.Fatalf("got %v, want the process refused", err)
	}
	if !impostor.running() {
		t.Fatal("the process was signalled")
	}
}
