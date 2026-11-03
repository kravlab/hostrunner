package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// collectFrames reads frames from r until it fails, then sends them all
// on the channel it returns.
func collectFrames(r io.Reader) <-chan []protocol.Frame {
	done := make(chan []protocol.Frame, 1)
	go func() {
		var frames []protocol.Frame
		for {
			f, err := protocol.ReadFrame(r)
			if err != nil {
				done <- frames
				return
			}
			frames = append(frames, f)
		}
	}()
	return done
}

// decoded returns the payload of f, a JSON frame of type typ, decoded.
func decoded[T any](t *testing.T, f protocol.Frame, typ protocol.FrameType) T {
	t.Helper()
	var v T
	if f.Type != typ {
		t.Fatalf("got frame type %d, want %d", f.Type, typ)
	}
	if err := protocol.DecodeJSON(f, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// encoded returns v as a JSON frame of type typ, encoded.
func encoded(t *testing.T, typ protocol.FrameType, v any) []byte {
	t.Helper()
	buf, err := protocol.EncodeJSON(typ, v)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

// A peer that takes none of a frame in time leaves it unsent, without an
// error: the connection is still good, and the frame goes once the peer
// reads.
func TestTryWriteGivesUpOnAPeerThatDoesNotRead(t *testing.T) {
	daemonEnd, peer := net.Pipe()
	defer daemonEnd.Close()
	defer peer.Close()
	w := &frameWriter{conn: daemonEnd, timeout: time.Second}
	buf := encoded(t, protocol.FrameRunStarted, protocol.RunStarted{Run: 1, Argv: []string{"true"}})

	begun := time.Now()
	sent, err := w.tryWrite(buf, 50*time.Millisecond)
	if sent || err != nil {
		t.Fatalf("tryWrite to a peer that does not read: sent %v, %v; want unsent, no error", sent, err)
	}
	if took := time.Since(begun); took < 50*time.Millisecond || took > time.Second {
		t.Fatalf("tryWrite gave up after %v, want after its 50ms", took)
	}

	frames := collectFrames(peer)
	if sent, err := w.tryWrite(buf, time.Second); !sent || err != nil {
		t.Fatalf("tryWrite to a peer that reads: sent %v, %v; want sent", sent, err)
	}
	daemonEnd.Close()
	got := <-frames
	if len(got) != 1 {
		t.Fatalf("the peer got %d frames, want 1", len(got))
	}
	if m := decoded[protocol.RunStarted](t, got[0], protocol.FrameRunStarted); m.Run != 1 {
		t.Fatalf("the peer got run %d, want 1", m.Run)
	}
}

// The short deadline of a tryWrite does not outlive it: a writer without
// a timeout of its own then writes with none, however late the peer reads.
func TestTryWriteLeavesNoDeadlineBehind(t *testing.T) {
	daemonEnd, peer := net.Pipe()
	defer daemonEnd.Close()
	defer peer.Close()
	w := &frameWriter{conn: daemonEnd}
	if sent, err := w.tryWrite(encoded(t, protocol.FrameRunStarted, protocol.RunStarted{Run: 1}), 10*time.Millisecond); sent || err != nil {
		t.Fatalf("tryWrite to a peer that does not read: sent %v, %v; want unsent, no error", sent, err)
	}

	written := make(chan error, 1)
	go func() { written <- w.writeJSON(protocol.FrameRunEnded, protocol.RunEnded{Run: 1}) }()
	time.Sleep(50 * time.Millisecond) // well past the tryWrite's deadline
	frames := collectFrames(peer)
	if err := <-written; err != nil {
		t.Fatalf("a write after tryWrite, read late: %v; want it written", err)
	}
	daemonEnd.Close()
	if got := <-frames; len(got) != 1 || got[0].Type != protocol.FrameRunEnded {
		t.Fatalf("the peer got %d frames, want the one written", len(got))
	}
}

// A frame the peer took part of in time counts as sent; its rest goes
// before the next frame, so that frames stay whole on the wire.
func TestAFramePartlyTakenIsFinishedBeforeTheNext(t *testing.T) {
	daemonEnd, peer := net.Pipe()
	defer daemonEnd.Close()
	defer peer.Close()
	w := &frameWriter{conn: daemonEnd, timeout: time.Second}
	first := encoded(t, protocol.FrameRunOutput, protocol.RunOutput{Run: 1, Data: []byte("first frame")})

	// The peer takes the first 3 bytes, then stops reading.
	head := make(chan []byte, 1)
	go func() {
		b := make([]byte, 3)
		_, _ = io.ReadFull(peer, b)
		head <- b
	}()
	if sent, err := w.tryWrite(first, 100*time.Millisecond); !sent || err != nil {
		t.Fatalf("tryWrite of a frame partly taken: sent %v, %v; want sent", sent, err)
	}
	if len(w.rest) != len(first)-3 {
		t.Fatalf("rest holds %d bytes, want the %d not taken", len(w.rest), len(first)-3)
	}

	frames := collectFrames(io.MultiReader(bytes.NewReader(<-head), peer))
	if err := w.writeJSON(protocol.FrameExit, protocol.Exit{Code: 7}); err != nil {
		t.Fatal(err)
	}
	daemonEnd.Close()
	got := <-frames
	if len(got) != 2 {
		t.Fatalf("the peer got %d whole frames, want 2", len(got))
	}
	if m := decoded[protocol.RunOutput](t, got[0], protocol.FrameRunOutput); string(m.Data) != "first frame" {
		t.Fatalf("the first frame carries %q, want %q", m.Data, "first frame")
	}
	if m := decoded[protocol.Exit](t, got[1], protocol.FrameExit); m.Code != 7 {
		t.Fatalf("the second frame carries code %d, want 7", m.Code)
	}
}

// Output beyond runFeedBytes waiting is dropped and counted per run, the
// count going with the run's end; starts and ends are never dropped.
func TestARunFeedDropsOutputBeyondItsBoundAndCountsIt(t *testing.T) {
	daemonEnd, peer := net.Pipe()
	defer daemonEnd.Close()
	defer peer.Close()
	feed := newRunFeed(&frameWriter{conn: daemonEnd, timeout: time.Second})

	// Nothing is written until pump starts: the feed fills.
	feed.started(protocol.RunStarted{Run: 1})
	feed.started(protocol.RunStarted{Run: 2})
	feed.output(1, false, bytes.Repeat([]byte("a"), runFeedBytes-2*runChunk))
	feed.output(2, true, bytes.Repeat([]byte("b"), 5*runChunk))
	feed.output(1, false, bytes.Repeat([]byte("c"), runChunk))
	feed.ended(protocol.RunEnded{Run: 1})
	feed.ended(protocol.RunEnded{Run: 2, Code: 3})

	frames := collectFrames(peer)
	go feed.pump()
	feed.drain(time.Now().Add(5 * time.Second))
	feed.stop()
	daemonEnd.Close()

	var starts, ends []uint64
	got := map[uint64][]byte{}
	dropped := map[uint64]int64{}
	for _, f := range <-frames {
		switch f.Type {
		case protocol.FrameRunStarted:
			starts = append(starts, decoded[protocol.RunStarted](t, f, f.Type).Run)
		case protocol.FrameRunOutput:
			m := decoded[protocol.RunOutput](t, f, f.Type)
			got[m.Run] = append(got[m.Run], m.Data...)
		case protocol.FrameRunEnded:
			m := decoded[protocol.RunEnded](t, f, f.Type)
			ends = append(ends, m.Run)
			dropped[m.Run] = m.Dropped
		default:
			t.Fatalf("unexpected frame type %d", f.Type)
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("got the starts of runs %v and the ends of %v, want both of runs 1 and 2", starts, ends)
	}
	if want := bytes.Repeat([]byte("a"), runFeedBytes-2*runChunk); !bytes.Equal(got[1], want) {
		t.Fatalf("run 1 got %d bytes of output, want its first %d", len(got[1]), len(want))
	}
	if want := bytes.Repeat([]byte("b"), 2*runChunk); !bytes.Equal(got[2], want) {
		t.Fatalf("run 2 got %d bytes of output, want the %d that fit", len(got[2]), len(want))
	}
	if dropped[1] != runChunk || dropped[2] != 3*runChunk {
		t.Fatalf("dropped %d bytes of run 1 and %d of run 2, want %d and %d", dropped[1], dropped[2], runChunk, 3*runChunk)
	}
}

// approvalsServer returns a Server with approvals whose serving session
// asked for runs, its frames going to the peer it returns.
func approvalsServer(t *testing.T) (*Server, *sessionConn, net.Conn) {
	t.Helper()
	mapper, err := workspace.NewMapper(t.TempDir(), "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	p, err := rules.Parse([]byte("rules:\n  - command: echo\n    args: any\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(mapper, p, slog.New(slog.DiscardHandler), WithApprovals(nil, ""))
	daemonEnd, peer := net.Pipe()
	t.Cleanup(func() {
		daemonEnd.Close()
		peer.Close()
	})
	out := &frameWriter{conn: daemonEnd, timeout: sessionWriteTimeout}
	c := &sessionConn{conn: daemonEnd, out: out}
	feed := newRunFeed(out)
	c.runs.Store(feed)
	go feed.pump()
	t.Cleanup(feed.stop)
	s.approvals.serving = c
	return s, c, peer
}

// A stopping daemon closes a session that asked for runs once the
// approved runs going have ended and their ends reached it.
func TestClosingAStoppingSessionWaitsForTheEndsOfRuns(t *testing.T) {
	s, c, peer := approvalsServer(t)
	frames := collectFrames(peer)
	run := s.approvals.startRun(protocol.RunStarted{Argv: []string{"sleep", "1"}, Request: 1})

	closed := make(chan struct{})
	go func() {
		s.closeStopping(c)
		close(closed)
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("the session was closed while its run was going")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run.cancelled(ctx)
	select {
	case <-closed:
	case <-time.After(stopReportTimeout):
		t.Fatal("the session was not closed once its run ended")
	}
	got := <-frames
	if len(got) != 2 {
		t.Fatalf("the session got %d frames, want the start and the end of its run", len(got))
	}
	if m := decoded[protocol.RunEnded](t, got[1], protocol.FrameRunEnded); m.Cancelled != protocol.CancelledStopping {
		t.Fatalf("the run ended cancelled for %q, want %q", m.Cancelled, protocol.CancelledStopping)
	}
}

// A run that does not end holds a stopping daemon's session no longer
// than stopReportTimeout.
func TestClosingAStoppingSessionGivesUpOnARunAtItsDeadline(t *testing.T) {
	s, c, peer := approvalsServer(t)
	frames := collectFrames(peer)
	s.approvals.startRun(protocol.RunStarted{Argv: []string{"sleep", "60"}, Request: 1})

	begun := time.Now()
	s.closeStopping(c)
	if took := time.Since(begun); took < stopReportTimeout || took > stopReportTimeout+time.Second {
		t.Fatalf("closeStopping returned after %v, want after stopReportTimeout (%v)", took, stopReportTimeout)
	}
	if got := <-frames; len(got) != 1 || got[0].Type != protocol.FrameRunStarted {
		t.Fatalf("the session got %d frames, want the start of its run alone", len(got))
	}
}

// An approved command that fails to start because its request was
// cancelled meanwhile ends its run as cancelled, not as not started; the
// client gets the start failure as before.
func TestARunWhoseRequestIsCancelledAsItStartsEndsCancelled(t *testing.T) {
	s, _, peer := approvalsServer(t)
	frames := collectFrames(peer)
	run := s.approvals.startRun(protocol.RunStarted{Argv: []string{"true"}, Request: 1})

	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errClientGone)
	daemonEnd, client := net.Pipe()
	defer client.Close()
	result := collectFrames(client)
	x := exchange{cancel: func() {}, conn: daemonEnd, out: &frameWriter{conn: daemonEnd}}
	next := func() (protocol.Frame, error) { return protocol.Frame{}, io.EOF }
	req := protocol.Request{Argv: []string{"true"}, Cwd: "/workspaces/app"}
	s.start(ctx, x, req, &prepared{dir: dir, hostPath: dir.Name(), argv: []string{"true"}}, next, run)
	daemonEnd.Close()

	if got := <-result; len(got) != 1 || got[0].Type != protocol.FrameError {
		t.Fatalf("the client got %d frames, want its start failure alone", len(got))
	} else if e := decoded[protocol.Error](t, got[0], protocol.FrameError); e.Code != protocol.ExitRejected {
		t.Fatalf("the client got code %d, want %d", e.Code, protocol.ExitRejected)
	}
	s.closeStopping(s.approvals.serving)
	got := <-frames
	if len(got) != 2 {
		t.Fatalf("the session got %d frames, want the start and the end of the run", len(got))
	}
	m := decoded[protocol.RunEnded](t, got[1], protocol.FrameRunEnded)
	if m.Cancelled != protocol.CancelledClientGone || m.Error != "" {
		t.Fatalf("the run ended cancelled for %q, error %q; want cancelled for %q", m.Cancelled, m.Error, protocol.CancelledClientGone)
	}
}
