package client_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// stopper opens a connection to h's approval socket as `hostrunner stop`
// does, with a hello of version, and returns it with the daemon's info.
func (h harness) stopper(t *testing.T, version int) (*stopConn, protocol.DaemonInfo) {
	t.Helper()
	conn, err := transport.Unix{Path: h.approvalSocket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: version}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil || f.Type != protocol.FrameDaemonInfo {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	var info protocol.DaemonInfo
	if err := protocol.DecodeJSON(f, &info); err != nil {
		t.Fatal(err)
	}
	return &stopConn{conn: conn}, info
}

// stopConn is a stopper's connection to the approval socket.
type stopConn struct{ conn io.ReadWriteCloser }

// running asks the daemon what it runs.
func (c *stopConn) running(t *testing.T) protocol.RunningList {
	t.Helper()
	if err := protocol.WriteFrame(c.conn, protocol.Frame{Type: protocol.FrameRunning}); err != nil {
		t.Fatal(err)
	}
	f, err := protocol.ReadFrame(c.conn)
	if err != nil || f.Type != protocol.FrameRunningList {
		t.Fatalf("running: frame %v, err %v", f.Type, err)
	}
	var list protocol.RunningList
	if err := protocol.DecodeJSON(f, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

// stop sends a Stop, and expects FrameStopping when confirm is set.
func (c *stopConn) stop(t *testing.T, drain, confirm bool) {
	t.Helper()
	if err := protocol.WriteJSON(c.conn, protocol.FrameStop, protocol.Stop{Drain: drain}); err != nil {
		t.Fatal(err)
	}
	if !confirm {
		return
	}
	if f, err := protocol.ReadFrame(c.conn); err != nil || f.Type != protocol.FrameStopping {
		t.Fatalf("stop: frame %v, err %v", f.Type, err)
	}
}

// expectClosed expects the daemon to close the connection, sending
// nothing more: it has stopped.
func (c *stopConn) expectClosed(t *testing.T) {
	t.Helper()
	if f, err := protocol.ReadFrame(c.conn); !errors.Is(err, io.EOF) {
		t.Fatalf("got frame %v, err %v; want the connection closed", f.Type, err)
	}
}

// startSleeper runs a command on h that prints its process ID and sleeps,
// and returns the ID.
func (h harness) startSleeper(t *testing.T) (pid int) {
	t.Helper()
	conn := h.startRaw(t, "sh", "-c", "echo $$; exec sleep 60")
	pid, err := strconv.Atoi(readStdoutLine(t, conn))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestDaemonInfoSaysTheDaemonStops(t *testing.T) {
	h := startDaemon(t)
	_, info := h.stopper(t, protocol.ApprovalVersion)
	if !info.Stops || info.ContainerStopped {
		t.Fatalf("got Stops=%v ContainerStopped=%v, want true and false", info.Stops, info.ContainerStopped)
	}
	stopped := startDaemonWith(t, slog.New(slog.DiscardHandler), allowAll{}, daemon.WithContainerStopped(func() bool { return true }))
	if _, info := stopped.stopper(t, protocol.ApprovalVersion); !info.ContainerStopped {
		t.Fatal("got ContainerStopped=false from a daemon that saw its armed container stopped")
	}
}

func TestRunningListsCommandsAndApprovals(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	h.grantStanding(t, s, false, "touch", "a")
	h.grantAs(t, s, sessionAnswer, false, "touch", "b")
	h.grantAs(t, s, sessionAnswer, false, "touch", "c")
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	if got := c.running(t); got.Standing != 1 || got.Session != 2 {
		t.Fatalf("got %d standing and %d session approvals, want 1 and 2", got.Standing, got.Session)
	}

	busy := startDaemon(t)
	// A finished command is no longer listed; the others are, oldest first.
	if got := busy.run(t, containerRoot, "", "true"); got != (result{}) {
		t.Fatalf("true: got %+v", got)
	}
	busy.startSleeper(t)
	conn := busy.startRaw(t, "sh", "-c", "echo second; exec sleep 60")
	readStdoutLine(t, conn)
	c, _ = busy.stopper(t, protocol.ApprovalVersion)
	got := c.running(t)
	want := []protocol.RunningCommand{
		{Argv: []string{"sh", "-c", "echo $$; exec sleep 60"}, Cwd: containerRoot},
		{Argv: []string{"sh", "-c", "echo second; exec sleep 60"}, Cwd: containerRoot},
	}
	if !slices.EqualFunc(got.Commands, want, func(a, b protocol.RunningCommand) bool { return slices.Equal(a.Argv, b.Argv) && a.Cwd == b.Cwd }) {
		t.Fatalf("got commands %+v, want %+v", got.Commands, want)
	}
}

// A daemon of any approval protocol version can be stopped.
func TestStopFramesIgnoreTheHelloVersion(t *testing.T) {
	h := startDaemon(t)
	c, _ := h.stopper(t, protocol.ApprovalVersion+1)
	if got := c.running(t); len(got.Commands) != 0 {
		t.Fatalf("got commands %+v, want none", got.Commands)
	}
}

func TestStopKillsRunningCommands(t *testing.T) {
	h := startDaemon(t)
	pid := h.startSleeper(t)
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	c.stop(t, false, true)
	waitForExit(t, pid)
	c.expectClosed(t)
	h.waitServed(t)
}

func TestDrainLetsRunningCommandsFinish(t *testing.T) {
	h := startDaemon(t)
	conn := h.startRaw(t, "sh", "-c", "echo go; read line; echo got $line")
	if got := readStdoutLine(t, conn); got != "go" {
		t.Fatalf("got %q, want go", got)
	}
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	c.stop(t, true, true)
	if _, err := h.transport.Dial(context.Background()); err == nil {
		t.Fatal("a draining daemon accepted a new connection")
	}
	if _, err := (transport.Unix{Path: h.approvalSocket}).Dial(context.Background()); err == nil {
		t.Fatal("a draining daemon accepted a connection on its approval socket")
	}
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameStdin, Payload: []byte("x\n")}); err != nil {
		t.Fatal(err)
	}
	if got := readStdoutLine(t, conn); got != "got x" {
		t.Fatalf("got %q, want the command to finish", got)
	}
	f := statusFrameOf(t, conn)
	var exit protocol.Exit
	if f.Type != protocol.FrameExit || protocol.DecodeJSON(f, &exit) != nil || exit.Code != 0 {
		t.Fatalf("got frame %v %q, want exit 0", f.Type, f.Payload)
	}
	c.expectClosed(t)
	h.waitServed(t)
}

func TestDrainOfAnIdleDaemonStopsIt(t *testing.T) {
	h := startDaemon(t)
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	c.stop(t, true, true)
	c.expectClosed(t)
	h.waitServed(t)
}

func TestDrainAnswersWaitingApprovalRequests(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSleeper(t) // keeps the drain going
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	c.stop(t, true, true)
	// The session, which stays connected while the daemon drains, drops the
	// request at once.
	s.waitFor(t, "request 1 ended: the daemon is stopping")
	got := awaitResult(t, ch)
	if want := (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: not approved: the hostrunner daemon is stopping\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	c.stop(t, false, false) // the sleeper would keep the drain going
	c.expectClosed(t)
}

// Ctrl+C and k while draining: a Stop without Drain kills what still runs,
// and gets no second FrameStopping.
func TestStopWhileDrainingKills(t *testing.T) {
	h := startDaemon(t)
	pid := h.startSleeper(t)
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	c.stop(t, true, true)
	c.stop(t, false, false)
	waitForExit(t, pid)
	c.expectClosed(t)
	h.waitServed(t)
}

// statusFrameOf reads frames until Exit or Error and returns it.
func statusFrameOf(t *testing.T, conn io.Reader) protocol.Frame {
	t.Helper()
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("no status frame: %v", err)
		}
		if f.Type == protocol.FrameExit || f.Type == protocol.FrameError {
			return f
		}
	}
}

// A request accepted before a drain whose command has not started yet is
// refused: a draining daemon starts nothing new.
func TestDrainRefusesCommandsNotStartedYet(t *testing.T) {
	h := startDaemon(t)
	h.startSleeper(t) // keeps the drain going
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, _ := h.stopper(t, protocol.ApprovalVersion)
	c.stop(t, true, true)
	req := protocol.Request{Version: protocol.Version, Argv: []string{"true"}, Cwd: containerRoot}
	if err := protocol.WriteJSON(conn, protocol.FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	f := statusFrameOf(t, conn)
	var e protocol.Error
	if f.Type != protocol.FrameError || protocol.DecodeJSON(f, &e) != nil || e.Code != protocol.ExitHostrunError || e.Message != "the hostrunner daemon is stopping" {
		t.Fatalf("got frame %v %q, want error 125 \"the hostrunner daemon is stopping\"", f.Type, f.Payload)
	}
}
