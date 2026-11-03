package approval

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// noRunOutput is what a session says of a daemon that does not report
// approved runs (protocol.DaemonInfo.Runs): it serves it all the same.
const noRunOutput = "no run output: this daemon does not report approved runs"

// outUsage is what out says of an argument that is not a run's number.
const outUsage = "usage: out [n], n from runs"

// sessionRun is an approved run the daemon told the session of: its
// number in the session (0 until the answer that approved it is
// confirmed), its command, when it started (when the session was told
// of its start, or as the daemon counts it for a run that started
// earlier), whether it started before the session served its daemon
// (earlier), its output from before then not received, the lines of
// output it got (see outputLines), the output kept for out (see
// outputKept), and its end, once that came, or when the session stopped
// serving its daemon before it came (left). The session holds it until
// it has reported how the run ended, and keeps it for out and runs while
// it is one of the last runs (see term.keep).
type sessionRun struct {
	n       uint64
	argv    []string
	started time.Time
	earlier bool
	outputLines
	output *keptOutput
	ended  *protocol.RunEnded
	left   time.Time
}

// lines returns the lines of r's output as its report counts them: those
// of both streams, a line not yet complete included (see streamLines).
func (r *sessionRun) lines() int { return r.streams[stdout].shown() + r.streams[stderr].shown() }

// stream is one of a run's two streams of output, stdout or stderr; it
// indexes what the session holds of each.
type stream int

const (
	stdout stream = iota
	stderr
)

// streamOf returns the stream a chunk of output came on, as the daemon
// tells it (protocol.RunOutput.Stderr).
func streamOf(isStderr bool) stream {
	if isStderr {
		return stderr
	}
	return stdout
}

// mark returns what marks a line of the stream where the session shows
// it: │ for stdout, ! for stderr.
func (s stream) mark() string {
	if s == stderr {
		return "!"
	}
	return "│"
}

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
// unseen. Its lines are cut (see hostterm.OutputLine), or held whole.
type tail struct {
	max   int
	whole bool
	lines []tailLine
}

// tailLine is a line of a tail, of one stream.
type tailLine struct {
	stream stream
	line   *hostterm.OutputLine
}

// start returns a new line of stream s, the last of the tail, pushing its
// first out when the tail holds max already.
func (t *tail) start(s stream) *hostterm.OutputLine {
	l := &hostterm.OutputLine{}
	if t.whole {
		l = hostterm.WholeOutputLine()
	}
	t.lines = append(t.lines, tailLine{stream: s, line: l})
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
	s.keep(r)
	return n, r
}

// answeredRun numbers the run that the answer to request id approved,
// as the daemon confirms the answer (see numberRun), and returns its
// number and, when the run ended already, its report (see report),
// the run then no longer going; "" otherwise.
func (s *session) answeredRun(id uint64) (uint64, string) {
	n, r := s.numberRun(id)
	if r == nil || r.ended == nil {
		return n, ""
	}
	delete(s.runs.going, r.ended.Run)
	return n, report(r)
}

// runStarted takes the start of approved run m: numbered already when its
// answer was confirmed, numbered then otherwise. A run no request
// approved, a repeat, and a run that started before the session served
// its daemon (m.Earlier), whose answer, if any, was confirmed to another
// session, are numbered at once, and their start line is a notice (see
// notify), as a summary is: no answer line names them. A run that
// started before is dated back by m.ElapsedMs, how long it had gone. A
// run is kept for out from the start, its output held before it is
// numbered too.
func (s *session) runStarted(m protocol.RunStarted) {
	started := time.Now()
	if m.Earlier {
		started = started.Add(-time.Duration(m.ElapsedMs) * time.Millisecond)
	}
	r := &sessionRun{argv: m.Argv, started: started, earlier: m.Earlier, outputLines: outputLines{tail: tail{max: s.tailLines}}, output: &keptOutput{}}
	s.runs.going[m.Run] = r
	switch n, ok := s.runs.numberByRequest[m.Request]; {
	case m.Repeat() || m.Earlier:
		r.n = s.nextRun()
		s.keep(r)
		s.notify(startLine(r, m))
	case ok:
		delete(s.runs.numberByRequest, m.Request)
		r.n = n
		s.keep(r)
	default:
		s.runs.startedEarly[m.Request] = r
	}
}

// startLine returns the line that tells of run r, started as m says, as
// the session learns of it: `run N · <cmd> · by standing approval` or
// `by session approval` for a repeat, and for a run that started before
// the session served its daemon the approval that let it through (see
// approvedBy), then `· started before this session`, the command shown
// as its summary shows it.
func startLine(r *sessionRun, m protocol.RunStarted) string {
	line := fmt.Sprintf("run %d · %s · %s", r.n, hostterm.ArgvLine(r.argv), approvedBy(m))
	if m.Earlier {
		line += " · started before this session"
	}
	return line
}

// approvedBy returns what a start line says let the run m through: for a
// fresh approval, the answer to request m.Request, the scope's earlier
// text (see scopeTexts); for a repeat, the scope's by text; and for a
// scope without that text, its kind as the daemon names it, shown
// escaped.
func approvedBy(m protocol.RunStarted) string {
	texts := scopeTexts[m.Approval]
	text := texts.earlier
	if m.Repeat() {
		text = texts.by
	}
	if text != "" {
		return text
	}
	return "by " + hostterm.Display(string(m.Approval))
}

// runOutput takes a chunk of an approved run's output: its lines (see
// outputLines.write), and the output kept for out while the run is kept.
// The output of a run the session was not told of (one that started
// before it served the daemon) is ignored.
func (s *session) runOutput(m protocol.RunOutput) {
	r := s.runs.going[m.Run]
	if r == nil {
		return
	}
	st := streamOf(m.Stderr)
	if r.outputKept() {
		r.output.write(st, m.Data)
	}
	r.write(st, m.Data)
}

// runEnded takes the end of an approved run and, once it is numbered,
// reports it in a notice (see notify). A run whose answer is not
// confirmed yet is reported with the answer's line (see answeredRun).
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
	s.notify(report(r))
}

// leftRuns marks the runs still going as the session stops serving their
// daemon: their end will not come (see runLine).
func (s *session) leftRuns() {
	now := time.Now()
	for _, r := range s.runs.going {
		if r.ended == nil {
			r.left = now
		}
	}
}

// showOutput returns what out arg prints, ending with a newline: the
// output kept of the run arg numbers, or of the last run kept without
// arg, below its line as runs lists it (see runLine), preceded, when
// output was dropped beyond what is kept, by `(<B> bytes dropped: out
// keeps a run's last 64 KiB)`; its lines are whole, of both streams in
// the order their first bytes arrived, marked and sanitised as a tail's,
// but not cut. Otherwise it says why it shows none: no runs yet, the
// usage for an arg that is not a number, `run N: output dropped` for a
// number in the range of runs no longer kept (up to droppedThrough,
// whether its run started or not), `run N: no such run` for any other,
// one too large for a number of a run included, shown as typed.
func (t *term) showOutput(arg string) string {
	var r *sessionRun
	if arg == "" {
		if len(t.kept) == 0 {
			return "no runs yet\n"
		}
		r = t.kept[len(t.kept)-1]
	} else {
		n, err := strconv.ParseUint(arg, 10, 64)
		switch {
		case errors.Is(err, strconv.ErrRange):
			return fmt.Sprintf("run %s: no such run\n", arg)
		case err != nil:
			return outUsage + "\n"
		}
		i, found := t.findKept(n)
		switch {
		case found:
			r = t.kept[i]
		case n != 0 && n <= t.droppedThrough:
			return fmt.Sprintf("run %d: output dropped\n", n)
		default:
			return fmt.Sprintf("run %d: no such run\n", n)
		}
	}
	var b strings.Builder
	b.WriteString(runLine(r))
	lines, dropped := r.output.lines()
	if dropped > 0 {
		fmt.Fprintf(&b, "\n(%s dropped: out keeps a run's last %d KiB)", counted(dropped, "byte"), keptBytes>>10)
	}
	writeLines(&b, lines)
	return b.String() + "\n"
}

// listRuns returns what runs prints: the line of each run kept (see
// runLine), the oldest first, each ending with a newline, or `no runs
// yet`.
func (t *term) listRuns() string {
	if len(t.kept) == 0 {
		return "no runs yet\n"
	}
	var b strings.Builder
	for _, r := range t.kept {
		b.WriteString(runLine(r) + "\n")
	}
	return b.String()
}

// runLine returns the line that runs lists run r with: its summary once
// it ended; `── run N · <cmd> · running · <duration so far> · <lines>
// lines ──` while it goes, and `end not received` in place of running,
// with its duration until then, once the session stopped serving its
// daemon before its end came.
func runLine(r *sessionRun) string {
	switch {
	case r.ended != nil:
		return summary(r)
	case !r.left.IsZero():
		return statusLine(r, "end not received", r.left.Sub(r.started))
	}
	return statusLine(r, "running", time.Since(r.started))
}

// report returns what reports run r, ended, each line but the last
// ending with a newline: its summary line, then the lines of its tail,
// each cut (see hostterm.OutputLine) and marked │ for stdout or ! for
// stderr, then, when the tail holds fewer than the run's lines, `(<K>
// lines above · out N)`, out N being the command that shows them. A
// session showing no tail lines reports the summary alone: the summary
// counts the lines already.
func report(r *sessionRun) string {
	var b strings.Builder
	b.WriteString(summary(r))
	writeLines(&b, r.tail.lines)
	if above := r.lines() - len(r.tail.lines); above > 0 && r.tail.max > 0 {
		fmt.Fprintf(&b, "\n(%s above · out %d)", counted(above, "line"), r.n)
	}
	return b.String()
}

// writeLines writes lines on b, each on a line of its own after a
// newline, marked as its stream's (see stream.mark).
func writeLines(b *strings.Builder, lines []tailLine) {
	for _, l := range lines {
		fmt.Fprintf(b, "\n%s %s", l.stream.mark(), l.line)
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
	return statusLine(r, outcome, time.Duration(e.DurationMs)*time.Millisecond)
}

// statusLine returns the line of run r that says how it went, outcome, in
// duration d: `── run N · <cmd> · <outcome> · <duration> · <lines> lines
// ──`, `· earlier output not received` before the end for a run that
// started before the session served its daemon, whose lines count only
// the output since, then `· <N> bytes not received` when the daemon
// dropped any of its output. The daemon counts those in a run's end
// alone (protocol.RunEnded.Dropped), so the line of a run without one
// says none.
func statusLine(r *sessionRun, outcome string, d time.Duration) string {
	line := fmt.Sprintf("── run %d · %s · %s · %s · %s", r.n, hostterm.ArgvLine(r.argv), outcome, shownDuration(d), counted(r.lines(), "line"))
	if r.earlier {
		line += " · earlier output not received"
	}
	if r.ended != nil && r.ended.Dropped > 0 {
		line += " · " + counted(int(r.ended.Dropped), "byte") + " not received"
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
