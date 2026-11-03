package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// An approved run is a run of a command a human approved; the session
// serving the daemon learns how it goes when it asked for runs
// (protocol.Serve.Runs): its start, its output as the container gets it,
// and its end. A session that starts serving while the run goes learns
// of its start as it does, marked earlier (see approvals.serve), and of
// its output from then on and its end. The frames reach the session
// through its runFeed, apart from the container's stream: a session that
// falls behind loses output, counted, but neither slows the command nor
// is dropped for it. The daemon log gets none of the output.

// runFeedBytes bounds the output a session's run feed holds while the
// session does not take it; output beyond it is dropped and counted.
const runFeedBytes = 256 << 10

// runChunk bounds the output one FrameRunOutput carries, well within
// protocol.MaxPayload once encoded, and small enough that a frame the
// session takes part of is soon finished (see frameWriter.tryWrite).
const runChunk = 4 << 10

// runWriteTimeout is how long the run feed gives the session to take a
// frame before it tries again, leaving the connection to other frames
// meanwhile.
const runWriteTimeout = 100 * time.Millisecond

// stopReportTimeout bounds how long a stopping daemon keeps a session that
// asked for runs, so that the approved runs the stop cancels end and the
// session learns how: a killed command's output ends within waitDelay.
const stopReportTimeout = waitDelay + time.Second

// errClientGone is the cause of a request cancelled because its client
// went away (or broke the protocol); any other cancellation is the daemon
// stopping.
var errClientGone = errors.New("the client went away")

// cancelReason is why the run of a request whose context ctx ended was
// cancelled.
func cancelReason(ctx context.Context) protocol.CancelReason {
	if errors.Is(context.Cause(ctx), errClientGone) {
		return protocol.CancelledClientGone
	}
	return protocol.CancelledStopping
}

// approvedRun is an approved run the daemon reports to the session serving
// it, whichever session that is when each frame goes; a nil one reports
// nothing (a run the rules allowed). start is its start as the session
// serving the daemon was told of it, begun when it began.
type approvedRun struct {
	a     *approvals
	id    uint64
	start protocol.RunStarted
	begun time.Time
}

// startRun records m, a run the human approved, as going, gives it its
// number and tells the session of it. It queues the start frame on the
// run feed taken under a.mu after releasing a.mu. That is safe: a
// session that starts serving in between finds the run going and is
// announced it as earlier (see approvals.serve), while the start goes
// to the feed taken under the lock, the old session's.
func (a *approvals) startRun(m protocol.RunStarted) *approvedRun {
	a.mu.Lock()
	a.lastRun++
	m.Run = a.lastRun
	r := &approvedRun{a: a, id: m.Run, start: m, begun: time.Now()}
	a.going[m.Run] = r
	if len(a.going) == 1 {
		a.noRuns = make(chan struct{})
	}
	feed := a.runFeedLocked()
	a.mu.Unlock()
	if feed != nil {
		feed.started(m)
	}
	return r
}

// announced returns r's start as told to a session that starts serving
// the daemon while r goes: marked earlier, with how long r has gone.
func (r *approvedRun) announced() protocol.RunStarted {
	m := r.start
	m.Earlier, m.ElapsedMs = true, time.Since(r.begun).Milliseconds()
	return m
}

// startRepeat starts the approved run of p, the prepared command of req,
// when a standing or session approval lets it through without asking (a
// repeat): it records it as going, tells the session of it and returns
// it. It returns nil when the rules let the command through, which is no
// approved run.
func (a *approvals) startRepeat(req protocol.Request, p *prepared) *approvedRun {
	if p.approval == "" {
		return nil
	}
	return a.startRun(protocol.RunStarted{Argv: req.Argv, Cwd: req.Cwd, Approval: p.approval})
}

// runFeed returns the run feed of the session serving the daemon, or nil
// when none does or it did not ask for runs.
func (a *approvals) runFeed() *runFeed {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runFeedLocked()
}

// runFeedLocked is runFeed with a.mu held.
func (a *approvals) runFeedLocked() *runFeed {
	if a.serving == nil {
		return nil
	}
	return a.serving.runs.Load()
}

// awaitRuns waits until no approved run is going, or until deadline.
func (a *approvals) awaitRuns(deadline time.Time) {
	a.mu.Lock()
	none := a.noRuns
	a.mu.Unlock()
	if none == nil {
		return
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-none:
	case <-timer.C:
	}
}

// feed returns where r's frames go: nil for a nil r, a run the rules
// allowed, and while no session serving the daemon asked for runs. end
// takes the feed itself, under a.mu (see end).
func (r *approvedRun) feed() *runFeed {
	if r == nil {
		return nil
	}
	return r.a.runFeed()
}

// output tells the session of p, a chunk of r's output on stderr or
// stdout.
func (r *approvedRun) output(stderr bool, p []byte) {
	if feed := r.feed(); feed != nil && len(p) > 0 {
		feed.output(r.id, stderr, p)
	}
}

// exited ends r: its command exited with code.
func (r *approvedRun) exited(code int) {
	r.end(protocol.RunEnded{Code: code})
}

// failed ends r, whose command never started: its client exits with code,
// told message.
func (r *approvedRun) failed(code int, message string) {
	r.end(protocol.RunEnded{Code: code, Error: message})
}

// cancelled ends r, whose command was cancelled as ctx ended.
func (r *approvedRun) cancelled(ctx context.Context) {
	r.end(protocol.RunEnded{Cancelled: cancelReason(ctx)})
}

// end tells the session that r ended as m says and forgets r, both under
// a.mu, which a session that starts serving the daemon takes the runs
// going under (see approvals.serve): that session is told either of r
// and its end, or of neither. Queuing the end takes the run feed's lock
// under a.mu; nothing takes them the other way round. A daemon that
// stops waits for the end (see awaitRuns).
func (r *approvedRun) end(m protocol.RunEnded) {
	if r == nil {
		return
	}
	m.Run, m.DurationMs = r.id, time.Since(r.begun).Milliseconds()
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	if feed := r.a.runFeedLocked(); feed != nil {
		feed.ended(m)
	}
	delete(r.a.going, r.id)
	if len(r.a.going) == 0 && r.a.noRuns != nil {
		close(r.a.noRuns)
		r.a.noRuns = nil
	}
}

// runTee writes a command's output, masked, to the container through w,
// and tells the session of what the container took, as run's output on
// stderr or stdout.
type runTee struct {
	w      io.Writer
	run    *approvedRun
	stderr bool
}

// Write writes p to the container, then hands the session what it took.
func (t runTee) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.run.output(t.stderr, p[:n])
	return n, err
}

// runFeed is the queue of one session's run frames and the goroutine that
// writes them (pump), each with tryWrite, so a session that does not take
// them holds up neither the command nor the session's other frames. Output
// beyond runFeedBytes waiting is dropped and counted per run, the count
// going out with the run's end; starts and ends are never dropped.
type runFeed struct {
	out *frameWriter

	// mu may be taken under approvals.mu (approvedRun.end does);
	// approvals.mu is never taken under it.
	mu      sync.Mutex
	queue   []runFrame
	held    int              // the output bytes in queue
	dropped map[uint64]int64 // the output bytes dropped, by run, until it ends
	drained []chan struct{}  // closed once queue is empty; see drain

	wake     chan struct{} // tells pump that a frame was queued
	done     chan struct{} // closed by stop
	finished chan struct{} // closed once pump returns
}

// runFrame is a frame the feed holds: of type t with the JSON of v, and
// size output bytes.
type runFrame struct {
	t    protocol.FrameType
	v    any
	size int
}

// newRunFeed returns a feed writing to out; its owner starts pump and
// calls stop.
func newRunFeed(out *frameWriter) *runFeed {
	return &runFeed{
		out: out, dropped: map[uint64]int64{},
		wake: make(chan struct{}, 1), done: make(chan struct{}), finished: make(chan struct{}),
	}
}

// started queues the start of a run.
func (f *runFeed) started(m protocol.RunStarted) {
	f.mu.Lock()
	f.queueLocked(runFrame{t: protocol.FrameRunStarted, v: m})
	f.mu.Unlock()
	f.signal()
}

// output queues a copy of p, a chunk of run's output on stderr or stdout,
// in pieces of runChunk at most, or counts what does not fit.
func (f *runFeed) output(run uint64, stderr bool, p []byte) {
	f.mu.Lock()
	for len(p) > 0 {
		n := min(len(p), runChunk)
		if f.held+n > runFeedBytes {
			f.dropped[run] += int64(n)
		} else {
			f.queueLocked(runFrame{t: protocol.FrameRunOutput, v: protocol.RunOutput{Run: run, Stderr: stderr, Data: bytes.Clone(p[:n])}, size: n})
		}
		p = p[n:]
	}
	f.mu.Unlock()
	f.signal()
}

// ended queues the end of a run, with the bytes of its output dropped.
func (f *runFeed) ended(m protocol.RunEnded) {
	f.mu.Lock()
	m.Dropped = f.dropped[m.Run]
	delete(f.dropped, m.Run)
	f.queueLocked(runFrame{t: protocol.FrameRunEnded, v: m})
	f.mu.Unlock()
	f.signal()
}

// queueLocked queues fr, unless the feed has stopped; the caller holds
// f.mu.
func (f *runFeed) queueLocked(fr runFrame) {
	select {
	case <-f.done:
		return
	default:
	}
	f.queue = append(f.queue, fr)
	f.held += fr.size
}

// signal tells pump that frames were queued.
func (f *runFeed) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// pump writes the frames queued, in order, until stop or until the
// connection fails.
func (f *runFeed) pump() {
	defer close(f.finished)
	for {
		fr, ok := f.next()
		if !ok {
			return
		}
		// A frame that does not encode cannot be: chunks keep frames small.
		// It would be dropped.
		if buf, err := protocol.EncodeJSON(fr.t, fr.v); err == nil && !f.write(buf) {
			return
		}
		f.pop()
	}
}

// write writes buf, one frame, trying again after runWriteTimeout while
// the session does not take it, and reports false once the feed stops or
// the connection fails (its handleSession sees that too).
func (f *runFeed) write(buf []byte) bool {
	retry := time.NewTimer(runWriteTimeout)
	defer retry.Stop()
	for {
		sent, err := f.out.tryWrite(buf, runWriteTimeout)
		if err != nil {
			return false
		}
		if sent {
			return true
		}
		retry.Reset(runWriteTimeout)
		select {
		case <-retry.C:
		case <-f.done:
			return false
		}
	}
}

// next returns the first frame queued, once there is one, or false once
// the feed stops.
func (f *runFeed) next() (runFrame, bool) {
	for {
		f.mu.Lock()
		if len(f.queue) > 0 {
			fr := f.queue[0]
			f.mu.Unlock()
			return fr, true
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-f.done:
			return runFrame{}, false
		}
	}
}

// pop drops the first frame queued, written, and tells drain once none is
// left.
func (f *runFeed) pop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held -= f.queue[0].size
	f.queue = f.queue[1:]
	if len(f.queue) == 0 {
		for _, ch := range f.drained {
			close(ch)
		}
		f.drained = nil
	}
}

// drain waits until the feed has written every frame it holds, can write
// no more, or deadline passes.
func (f *runFeed) drain(deadline time.Time) {
	f.mu.Lock()
	if len(f.queue) == 0 {
		f.mu.Unlock()
		return
	}
	empty := make(chan struct{})
	f.drained = append(f.drained, empty)
	f.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-empty:
	case <-f.finished:
	case <-timer.C:
	}
}

// stop ends pump and waits for it; what is still queued is dropped.
func (f *runFeed) stop() {
	close(f.done)
	<-f.finished
}
