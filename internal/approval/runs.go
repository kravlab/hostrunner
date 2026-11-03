package approval

import (
	"bytes"
	"fmt"
	"time"

	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// noRunOutput is what a session says of a daemon that does not report
// approved runs (protocol.DaemonInfo.Runs): it serves it all the same.
const noRunOutput = "no run output: this daemon does not report approved runs"

// sessionRun is an approved run the daemon told the session of, until the
// session has reported how it ended: its number in the session (0 until
// the answer that approved it is confirmed), its command, the lines of
// output it got on each stream, and its end, once that came.
type sessionRun struct {
	n              uint64
	argv           []string
	stdout, stderr streamLines
	ended          *protocol.RunEnded
}

// streamLines is what a run's stream got: its complete lines, and whether
// it ends with a line not yet complete.
type streamLines struct {
	lines int
	open  bool
}

// shown returns the lines of the stream as the summary counts them: a line
// not yet complete counts.
func (l streamLines) shown() int {
	if l.open {
		return l.lines + 1
	}
	return l.lines
}

// runs are the approved runs of the daemon served that the session has not
// reported yet. A fresh approval's run gets its number when the daemon
// confirms the answer (FrameAnswered), which may come before or after the
// run's start: going holds the runs by the daemon's number, startedEarly
// those started before their answer was confirmed, by request, and
// numberByRequest the numbers given to answers confirmed before their run
// started, by request.
type runs struct {
	going           map[uint64]*sessionRun
	startedEarly    map[uint64]*sessionRun
	numberByRequest map[uint64]uint64
}

// newRuns returns runs holding none.
func newRuns() runs {
	return runs{going: map[uint64]*sessionRun{}, startedEarly: map[uint64]*sessionRun{}, numberByRequest: map[uint64]uint64{}}
}

// nextRun returns the number of the session's next run: runs are numbered
// 1, 2, 3… across the daemons the session serves, apart from requests.
func (t *term) nextRun() uint64 {
	t.lastRun++
	return t.lastRun
}

// numberRun gives the run that the answer to request id approved its
// number, as the daemon confirms the answer, and returns it and the run,
// when the run started already, or nil.
func (s *session) numberRun(id uint64) (uint64, *sessionRun) {
	n := s.nextRun()
	r := s.runs.startedEarly[id]
	if r == nil {
		s.runs.numberByRequest[id] = n
		return n, nil
	}
	delete(s.runs.startedEarly, id)
	r.n = n
	return n, r
}

// runStarted takes the start of approved run m: numbered already when its
// answer was confirmed, numbered then otherwise. A run no request
// approved is numbered at once.
func (s *session) runStarted(m protocol.RunStarted) {
	r := &sessionRun{argv: m.Argv}
	s.runs.going[m.Run] = r
	switch n, ok := s.runs.numberByRequest[m.Request]; {
	case ok:
		delete(s.runs.numberByRequest, m.Request)
		r.n = n
	case m.Request == 0:
		r.n = s.nextRun()
	default:
		s.runs.startedEarly[m.Request] = r
	}
}

// runOutput counts the lines of a chunk of an approved run's output. The
// output of a run the session was not told of (one that started before
// it served the daemon) is ignored.
func (s *session) runOutput(m protocol.RunOutput) {
	r := s.runs.going[m.Run]
	if r == nil || len(m.Data) == 0 {
		return
	}
	stream := &r.stdout
	if m.Stderr {
		stream = &r.stderr
	}
	stream.lines += bytes.Count(m.Data, []byte("\n"))
	stream.open = m.Data[len(m.Data)-1] != '\n'
}

// runEnded takes the end of an approved run and, once it is numbered,
// reports it below what is shown, the prompt shown, if any, shown again,
// keeping what the human typed after it: a summary that arrives as they
// type takes none of their answer. A run whose answer is not confirmed yet
// is reported with the answer's line (see numberRun).
func (s *session) runEnded(m protocol.RunEnded) {
	r := s.runs.going[m.Run]
	if r == nil {
		return
	}
	r.ended = &m
	if r.n == 0 {
		return
	}
	delete(s.runs.going, m.Run)
	fmt.Fprintf(s.out, "\n%s\n", summary(r))
	if !s.awaiting.waitsForDaemon() {
		fmt.Fprint(s.out, s.asking)
	}
}

// summary returns the line that reports run r, ended: `── run N · <cmd> ·
// exit <code> · <duration> · <lines> lines ──`, `not started: <why>` or
// `cancelled: <why>` in place of the exit code, and `· <N> bytes not
// received` before the end when the daemon dropped output.
func summary(r *sessionRun) string {
	e := r.ended
	outcome := fmt.Sprintf("exit %d", e.Code)
	switch {
	case e.Error != "":
		outcome = "not started: " + hostterm.Display(e.Error)
	case e.Cancelled != "":
		outcome = "cancelled: " + hostterm.Display(string(e.Cancelled))
	}
	lines := r.stdout.shown() + r.stderr.shown()
	line := fmt.Sprintf("── run %d · %s · %s · %s · %s", r.n, hostterm.ArgvLine(r.argv), outcome, shownDuration(time.Duration(e.DurationMs)*time.Millisecond), counted(lines, "line"))
	if e.Dropped > 0 {
		line += " · " + counted(int(e.Dropped), "byte") + " not received"
	}
	return line + " ──"
}

// shownDuration returns d as a run's summary gives it: in seconds with one
// decimal under a minute (12.3s), in minutes and seconds under an hour
// (3m07s), in hours and minutes above (1h02m).
func shownDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Truncate(100*time.Millisecond).Seconds())
	case d < time.Hour:
		d = d.Truncate(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	d = d.Truncate(time.Minute)
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// counted returns n and noun, plural unless n is 1.
func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
