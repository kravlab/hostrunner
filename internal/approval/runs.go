package approval

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// noRunOutput is what a session says of a daemon that does not report
// approved runs (protocol.DaemonInfo.Runs): it serves it all the same.
const noRunOutput = "no run output: this daemon does not report approved runs"

// defaultTailLines is how many of a run's last lines of output a session
// shows under its summary (term.tailLines).
const defaultTailLines = 3

// sessionRun is an approved run the daemon told the session of, until the
// session has reported how it ended: its number in the session (0 until
// the answer that approved it is confirmed), its command, the lines of
// output it got on each stream, the last of them on both (tail), and its
// end, once that came.
type sessionRun struct {
	n              uint64
	argv           []string
	stdout, stderr streamLines
	tail           tail
	ended          *protocol.RunEnded
}

// lines returns the lines of r's output as its report counts them: those
// of both streams, a line not yet complete included (see streamLines).
func (r *sessionRun) lines() int { return r.stdout.shown() + r.stderr.shown() }

// streamLines is what a run's stream got: its complete lines, and the line
// not yet complete it ends with, nil when it ends with a newline.
type streamLines struct {
	lines int
	open  *hostterm.OutputLine
}

// shown returns the lines of the stream as the summary counts them: a line
// not yet complete counts.
func (l streamLines) shown() int {
	if l.open != nil {
		return l.lines + 1
	}
	return l.lines
}

// tail is the last lines of a run's output, of both streams, in the order
// their first bytes arrived, at most max of them, the oldest first. A line
// pushed out while not yet complete gets the rest of it all the same,
// unseen.
type tail struct {
	max   int
	lines []tailLine
}

// tailLine is a line of a tail, of stderr or of stdout.
type tailLine struct {
	stderr bool
	line   *hostterm.OutputLine
}

// start returns a new line of the stream, stderr or stdout, the last of
// the tail, pushing its first out when the tail holds max already.
func (t *tail) start(stderr bool) *hostterm.OutputLine {
	l := &hostterm.OutputLine{}
	t.lines = append(t.lines, tailLine{stderr: stderr, line: l})
	if len(t.lines) > t.max {
		t.lines = t.lines[1:]
	}
	return l
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
// approved, a repeat, is numbered at once, and its start line is printed
// below what is shown (see printBelow), as a summary is: no answer line
// names it.
func (s *session) runStarted(m protocol.RunStarted) {
	r := &sessionRun{argv: m.Argv, tail: tail{max: s.tailLines}}
	s.runs.going[m.Run] = r
	switch n, ok := s.runs.numberByRequest[m.Request]; {
	case ok:
		delete(s.runs.numberByRequest, m.Request)
		r.n = n
	case m.Request == 0:
		r.n = s.nextRun()
		s.printBelow(startLine(r, m.Approval) + "\n")
	default:
		s.runs.startedEarly[m.Request] = r
	}
}

// startLine returns the line that tells of run r, a repeat that approval
// let through, as it starts: `run N · <cmd> · by standing approval` or
// `by session approval`, the command shown as its summary shows it.
func startLine(r *sessionRun, approval protocol.Scope) string {
	return fmt.Sprintf("run %d · %s · %s", r.n, hostterm.ArgvLine(r.argv), byApproval(approval))
}

// byApproval returns what a start line says let a repeat through: the
// scope's by text (see scopeTexts) for a standing or session approval,
// its kind as the daemon names it, shown escaped, for a scope with none.
func byApproval(approval protocol.Scope) string {
	if by := scopeTexts[approval].by; by != "" {
		return by
	}
	return "by " + hostterm.Display(string(approval))
}

// runOutput takes a chunk of an approved run's output: it counts its
// lines, continues the stream's line not yet complete, if any, and starts
// a line of the tail at each line's first byte. The output of a run the
// session was not told of (one that started before it served the daemon)
// is ignored.
func (s *session) runOutput(m protocol.RunOutput) {
	r := s.runs.going[m.Run]
	if r == nil {
		return
	}
	stream := &r.stdout
	if m.Stderr {
		stream = &r.stderr
	}
	for data := m.Data; len(data) > 0; {
		if stream.open == nil {
			stream.open = r.tail.start(m.Stderr)
		}
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			stream.open.Write(data)
			return
		}
		stream.open.Write(data[:end])
		stream.open.End()
		stream.open = nil
		stream.lines++
		data = data[end+1:]
	}
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
	s.printBelow(report(r))
}

// printBelow prints text, ending with a newline, below what is shown,
// then the prompt shown, if any, again, keeping what the human typed
// after it; no prompt is shown again while the session waits for the
// daemon to take an answer.
func (s *session) printBelow(text string) {
	fmt.Fprintf(s.out, "\n%s", text)
	if !s.awaiting.waitsForDaemon() {
		fmt.Fprint(s.out, s.asking)
	}
}

// report returns what reports run r, ended, each line ending with a
// newline: its summary line, then the lines of its tail, each cut (see
// hostterm.OutputLine) and marked │ for stdout or ! for stderr, then, when
// the tail holds fewer than the run's lines, `(<K> lines above · out N)`,
// out N being the command that shows them.
func report(r *sessionRun) string {
	var b strings.Builder
	b.WriteString(summary(r) + "\n")
	for _, l := range r.tail.lines {
		mark := "│"
		if l.stderr {
			mark = "!"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, l.line)
	}
	if above := r.lines() - len(r.tail.lines); above > 0 {
		fmt.Fprintf(&b, "(%s above · out %d)\n", counted(above, "line"), r.n)
	}
	return b.String()
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
	line := fmt.Sprintf("── run %d · %s · %s · %s · %s", r.n, hostterm.ArgvLine(r.argv), outcome, shownDuration(time.Duration(e.DurationMs)*time.Millisecond), counted(r.lines(), "line"))
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
