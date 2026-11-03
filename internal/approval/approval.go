// Package approval implements the approval session, `hostrunner approve`:
// the host terminal process in which a human sees one daemon's approval
// requests and answers them.
//
// The session connects to the daemon's approval socket, which the
// container cannot reach, and serves the daemon: the daemon sends it each
// approval request, and it sends back the human's answer. It is a
// line-oriented log, without an alternate screen: requests are shown one
// at a time, first in, first out, each as a block, and answered with a
// letter and Enter. What came from the container is shown quoted or
// escaped as needed, so that it cannot steer the terminal.
package approval

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/textcut"
	"github.com/kravlab/hostrunner/internal/transport"
)

// Config describes one approval session.
type Config struct {
	// Socket is the approval socket of the daemon to serve; "" lists the
	// daemons under Root to choose from.
	Socket string
	// Root is where the runtime directories lie, whose approval sockets
	// the session lists; "" lists none.
	Root string
	// Cwd is the directory the session runs in: the daemon whose host
	// workspace holds it is marked and chosen by default.
	Cwd string
	// BuildVersion is this hostrunner's build version, as `hostrunner
	// version` prints it: the session says it as it starts and marks the
	// daemons of another one. "" is hostterm.DevVersion, as for a build
	// without one.
	BuildVersion string
	// TailLines is how many of an approved run's last lines of output the
	// session shows under its summary: 0 shows the summary alone. The
	// setting approve_tail_lines sets it, settings.DefaultApproveTailLines
	// by default.
	TailLines int
}

// errNoEditor is what editInEditor returns when neither $VISUAL nor
// $EDITOR names an editor.
var errNoEditor = errors.New("$EDITOR is not set")

// editInEditor opens the file at path in $VISUAL, or else $EDITOR, as a
// shell runs it, on the process's terminal, and waits for it to exit.
func editInEditor(path string) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		return errNoEditor
	}
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "editor", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Prompts and lines the session prints.
const (
	answerPrompt        = "answer [r]efuse [o]nce [s]ession stan[d]ing [a]dd rule [c]md [h]elp ? "
	guardedAnswerPrompt = "answer [r]efuse [o]nce [s]ession stan[d]ing [c]md [h]elp ? " // no rule for a guarded refusal
	addPrompt           = "[w]rite [e]dit in $EDITOR [c]ancel ? "
	editAgainPrompt     = "[e]dit again [c]ancel ? "
	rereadPrompt        = "the rules file changed since the daemon loaded it; reread it, making that change live now? [r]eread [c]ancel ? "
	notePrompt          = "note for the agent (Enter for none): "
	idlePrompt          = "> "
	takeOverPrompt      = "Take over? [y/N] "
	goneSaid            = "\nthe daemon ended: waiting for the next one\n"
)

// approvingKeys are the answers that approve a request, by the scope of
// the approval each gives.
var approvingKeys = map[string]protocol.Scope{
	"o": protocol.ScopeOnce,
	"s": protocol.ScopeSession,
	"d": protocol.ScopeStanding,
}

// helpEntry is one answer the help (h) describes: its key, what it does,
// whether a guarded request asks for "yes" before it, and whether a
// guarded request is offered it at all.
type helpEntry struct {
	key, does          string
	confirms, addsRule bool
}

// helpEntries are the answers h describes, in the prompt's order.
var helpEntries = []helpEntry{
	{key: "r", does: "refuse it; then an optional note for the agent"},
	{key: "o", does: "run this request only", confirms: true},
	{key: "s", does: "session: run this command from this directory now and whenever asked again, until this session ends", confirms: true},
	{key: "d", does: "standing: run this command from this directory now and whenever asked again, until the daemon stops", confirms: true},
	{key: "a", does: "add a rule to the rules file that allows it, then run it", addsRule: true},
	{key: "c", does: "show every argument of the command whole"},
	{key: "h", does: "this help"},
}

// helpConfirms ends the line of an answer a guarded request confirms.
const helpConfirms = "; after a warning, asks you to type yes"

// helpWidth is how wide h wraps its lines, continuation lines indented
// under the text.
const helpWidth = 65

// maxNoteBytes bounds a refusal's note, as sent: one of any length could
// not fit a frame.
const maxNoteBytes = 4096

// scopeText is what the session says of an approval of one scope.
type scopeText struct {
	// outcome is what the session says once the daemon has taken the
	// answer.
	outcome string
	// confirm is the prompt confirming the answer for a guarded request,
	// empty for a scope never confirmed.
	confirm string
	// by is what a repeat's start line says let it through, for an
	// approval that lets repeats through.
	by string
	// earlier is what the start line of a run the answer let through says
	// of it when the run started before the session served its daemon
	// (see approvedBy).
	earlier string
}

// scopeTexts are what the session says of an approval, by its scope.
var scopeTexts = map[protocol.Scope]scopeText{
	protocol.ScopeOnce: {
		outcome: "approved once",
		confirm: `Type "yes" to run it once: `,
		earlier: "approved once",
	},
	protocol.ScopeSession: {
		outcome: "approved for this session (until it ends)",
		confirm: `Type "yes" to approve it for this session: `,
		by:      "by session approval",
		earlier: "approved for a session",
	},
	protocol.ScopeStanding: {
		outcome: "approved as standing (until the daemon stops)",
		confirm: `Type "yes" to approve it as standing (until the daemon stops): `,
		by:      "by standing approval",
		earlier: "approved as standing",
	},
	protocol.ScopeRule: {
		outcome: "approved under the new rule",
		earlier: "approved under a new rule",
	},
}

// Run serves the daemon on cfg.Socket until ctx ends, in ends, or the
// daemon goes away: it shows each approval request on out, ringing the
// terminal bell, and sends the answer the human types on in. Requests that
// end unanswered are dropped as the daemon reports them; leaving refuses
// the ones still waiting (the daemon does, as the connection closes), and
// Run says how many. The answer "add a rule" edits the daemon's rules file
// (see proposeRule), opening it in $VISUAL or $EDITOR when the human
// asks. While no request is shown, the idle prompt takes commands:
// standing lists the daemon's standing and session approvals, revoke <n>
// revokes one, out [n] prints the output kept of a run, runs lists the
// runs kept, daemons leaves the daemon for the list, quit ends the
// session. It says its own version as it starts (see Config.BuildVersion) and
// the daemon's on each serving line, followed by a warning whenever that
// is another or unknown, so the human can restart the daemon. The runs
// the human approves are numbered, the answer's line naming its run, and
// summarised once they end (see runs), when the daemon reports them;
// approved runs already going as the session starts serving a daemon are
// numbered as the daemon announces them, marked as started before the
// session (see runStarted); the session keeps the last of their output
// in its memory for out (see keptOutput), and shows the last
// cfg.TailLines lines of each under its summary.
//
// Without cfg.Socket it lists the daemons under cfg.Root to choose from
// (see choose), and a daemon another session serves can be taken over:
// the session taken over says so and ends. With cfg.Socket, a daemon
// another session serves is taken over once the human confirms (see
// askTakeOver). When its daemon ends, the session waits for the next one
// in the same runtime directory. It returns an error when it cannot serve
// the daemon cfg.Socket names: one without an approval socket has no
// approvals, and one another session serves is left to it unless taken
// over.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer) error {
	t := &term{out: out, Input: hostterm.NewInput(in), version: cmp.Or(cfg.BuildVersion, hostterm.DevVersion), tailLines: cfg.TailLines}
	defer t.Close()
	fmt.Fprintf(out, "hostrunner approve %s\n", hostterm.Display(t.version))
	socket, takeOver := cfg.Socket, false
	first := socket != ""
	for {
		if socket == "" {
			var ok bool
			if socket, takeOver, ok = t.choose(ctx, cfg); !ok {
				return nil
			}
		}
		then, said, err := t.serveDaemon(ctx, socket, takeOver, cfg.Root != "")
		takeOver = false
		if then == nextNoDaemon && !first && cfg.Root == "" {
			// Its daemon went away between waits or at the question: the
			// session waits for the next one, as when it ends while served.
			then, said = nextAwait, goneSaid
		}
		switch {
		case then == nextNoDaemon && first:
			return fmt.Errorf("%s has no approvals: no daemon takes them there: %w", strings.TrimSuffix(socket, protocol.ApprovalSuffix), err)
		case then == nextServedElsewhere && cfg.Root == "":
			// A session given its daemon asks, as the list does.
			yes, more := t.askTakeOver(ctx, protocol.ServedRefusal+"\n"+takeOverPrompt)
			if !more {
				return err
			}
			if !yes {
				// Unless the other session left meanwhile: there is
				// nothing to take over then.
				if info, perr := daemonlist.Probe(ctx, socket); perr == nil && info.Served {
					return err
				}
			}
			takeOver, first = yes, false
			continue
		case then == nextFailed || then == nextNoDaemon || then == nextServedElsewhere:
			if cfg.Root == "" {
				return err
			}
			fmt.Fprintf(out, "cannot serve the daemon: %s\n", hostterm.Display(err.Error()))
			socket = ""
		case then == nextList:
			socket = ""
		case then == nextAwait:
			switch t.awaitDaemon(ctx, socket, cfg.Root != "", said) {
			case nextServe: // serve it
			case nextList:
				socket = ""
			default:
				return nil
			}
		default: // quit, input ended, taken over
			return err
		}
		first = false
	}
}

// errServed is why a session does not serve a daemon another session
// serves, unless it takes it over.
var errServed = errors.New(protocol.ServedRefusal)

// next is how serving a daemon ended, and so what the session does next.
type next int

const (
	nextQuit            next = iota + 1 // the human quit, or input ended
	nextTakenOver                       // another session took the daemon over
	nextList                            // the human asked for the list of daemons
	nextAwait                           // the daemon ended: the next one is awaited
	nextServe                           // a daemon answers on the socket again: it is served
	nextNoDaemon                        // no daemon answered on the socket
	nextFailed                          // the daemon would not be served, for another reason
	nextServedElsewhere                 // another session serves the daemon, not taken over
)

// serveDaemon serves the daemon on socket, taking it over from the session
// serving it with takeOver, until the human or the daemon ends it; canList
// lets the human ask for the list of daemons. It returns once the daemon
// has dropped the session (see hangUp), so a list of daemons asked for
// right after shows the daemon unserved; an interruption (ctx) hangs up
// alike. When the daemon has gone (nextAwait), it also returns what the
// session said of that, the prompt of the wait for the next one (see
// awaitDaemon). Serving it, it says the daemon's build version and warns
// when it is not the session's (see hostterm.OtherVersion), and says so
// when the daemon does not report approved runs (noRunOutput).
// Without takeOver, a daemon another session serves is not served
// (nextServedElsewhere, errServed).
func (t *term) serveDaemon(ctx context.Context, socket string, takeOver, canList bool) (next, string, error) {
	conn, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return nextNoDaemon, "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() {
		// The daemon closing its side ends every read; should it not
		// within hangUpTimeout, the connection is closed here.
		halfClose(conn)
		time.AfterFunc(hangUpTimeout, func() { conn.Close() })
	})
	defer stop()

	info, err := daemonlist.Hello(conn)
	if err != nil {
		return nextFailed, "", err
	}
	if info.Version != protocol.ApprovalVersion {
		return nextFailed, "", fmt.Errorf("the daemon speaks approval protocol version %d, this session %d: restart its container", info.Version, protocol.ApprovalVersion)
	}
	if err := serve(conn, takeOver); err != nil {
		if err.Error() == protocol.ServedRefusal {
			return nextServedElsewhere, "", errServed
		}
		return nextFailed, "", err
	}
	daemonVersion := "daemon " + hostterm.ShownVersion(info, "version unknown")
	fmt.Fprintf(t.out, "serving %s (container %s) · %s\n", hostterm.Display(info.Workspace), hostterm.Display(info.ContainerWorkspace), daemonVersion)
	if hostterm.OtherVersion(info, t.version) {
		fmt.Fprintf(t.out, "⚠ %s, this session %s: run hostrunner restart to update\n", daemonVersion, hostterm.Display(t.version))
	}
	if !info.Runs {
		fmt.Fprintln(t.out, noRunOutput)
	}
	fmt.Fprintf(t.out, "waiting for requests · %s\n", idleCommands)

	done := make(chan struct{})
	defer close(done)
	s := &session{term: t, conn: conn, canList: canList, daemon: info, runs: newRuns()}
	s.prompt(idlePrompt)
	frames := readDaemon(conn, done)
	then, err := s.loop(ctx, frames)
	s.leftRuns()
	s.printHeld(true)
	s.dropAddition()
	hangUp(conn, frames)
	return then, s.said, err
}

// hangUpTimeout bounds how long a session that ends waits for the daemon
// to drop it (see hangUp).
const hangUpTimeout = time.Second

// hangUp lets the daemon on conn drop the session before it returns: it
// closes the session's side for writing (see halfClose) and reads what
// the daemon still sends, frames, until the daemon closes the connection,
// which it does once it no longer counts the session as serving it, or
// hangUpTimeout passes.
func hangUp(conn net.Conn, frames <-chan protocol.Frame) {
	halfClose(conn)
	timeout := time.NewTimer(hangUpTimeout)
	defer timeout.Stop()
	for {
		select {
		case _, more := <-frames:
			if !more {
				return
			}
		case <-timeout.C:
			return
		}
	}
}

// halfClose closes the session's side of conn for writing: the daemon
// reads the end of it and drops the session. A connection to a daemon is
// a Unix one, which can; any other is left open, and hangUp waits out
// hangUpTimeout.
func halfClose(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// serve makes the session the one that serves the daemon, taking it over
// from the session serving it with takeOver; the session reads the
// daemon's notices of a reread (see rulesReread) and the frames of the
// approved runs (see runs).
func serve(conn io.ReadWriter, takeOver bool) error {
	if err := protocol.WriteJSON(conn, protocol.FrameServe, protocol.Serve{TakeOver: takeOver, Notices: true, Runs: true}); err != nil {
		return err
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("the daemon did not answer: %w", err)
	}
	if f.Type != protocol.FrameServing {
		return daemonlist.ErrorOf(f)
	}
	return nil
}

// term is the human's side of a session, kept across the daemons it
// serves: where it prints, the lines typed, and the prompt they answer.
// The lines are read only while the session waits for the human, never
// while an editor has the terminal.
type term struct {
	out io.Writer
	*hostterm.Input
	// asking is the current prompt.
	asking string
	// version is the session's build version (Config.BuildVersion, or
	// hostterm.DevVersion), against which a daemon of another one is
	// marked.
	version string
	// lastRun is the number of the last approved run (see nextRun).
	lastRun uint64
	// tailLines is how many of a run's last lines of output its report
	// shows (see report).
	tailLines int
	// kept are the last runs, by number, whose output out shows and runs
	// lists (see keep); droppedThrough is the number of the last run no
	// longer kept.
	kept           []*sessionRun
	droppedThrough uint64
}

// session is the state of a session serving one daemon: the requests
// waiting in arrival order, the first of which is the one shown, and what
// it waits for: the human to type, or the daemon to take the answer sent.
// While no request is shown, the human types commands at the idle prompt.
type session struct {
	*term
	conn io.Writer
	// canList lets the human leave the daemon for the list of daemons.
	canList bool
	// after is set once the human ends serving the daemon: what is next.
	after next
	// said is what the session said of its daemon gone, the prompt of the
	// wait for the next one (see serveDaemon).
	said string
	// failed is why a write to the daemon failed, which ends serving it.
	failed error

	queue []waiting
	// awaiting is what the session takes next (see awaiting); confirm,
	// sent, adding, rereadDigest and rereadFor carry what the states that
	// need them hold.
	awaiting awaiting
	// confirm is the scope the human confirms approving a guarded request
	// for (awaitConfirm).
	confirm protocol.Scope
	// sent is the answer sent for the first request, its outcome printed
	// once the daemon has taken it (awaitAnswered).
	sent sentAnswer
	// runs are the approved runs not reported yet, of a daemon that
	// reports them (DaemonInfo.Runs).
	runs runs
	// held are the notices and the commands' outcomes that arrived while
	// a request was shown, in arrival order, each ending with a newline,
	// to be printed once it is answered or withdrawn (see hold).
	held []string

	// daemon is what the daemon said of itself when the session connected
	// (see daemonlist.Hello): a rule is added to its RulesFile, in its host
	// Workspace; RulesDigest, which a reread updates, is the digest of the
	// rules it serves, which the file must have for a rule to be proposed.
	daemon protocol.DaemonInfo
	// adding is the rule being added (awaitAdd, awaitReread).
	adding *addition
	// rereadDigest is the digest of a rules file changed by hand, found
	// as a rule is proposed (awaitRereadChoice); rereadFor is the request
	// the rule is then proposed for, once the daemon rereads the file
	// (awaitProposeReread).
	rereadDigest string
	rereadFor    uint64
}

// awaiting is what a session takes next: while a request is shown, a
// line the human types, or first a frame of the daemon's, nothing typed
// being taken meanwhile (see waitsForDaemon). A frame awaited outlasts the
// request it is for, the queue emptied meanwhile.
type awaiting int

const (
	awaitAnswer        awaiting = iota // the answer to the shown request
	awaitNote                          // the note of a refusal
	awaitConfirm                       // "yes" to approving a guarded request
	awaitAdd                           // write, edit or cancel the rule being added
	awaitRereadChoice                  // whether the daemon rereads a hand-edited rules file
	awaitAnswered                      // the daemon taking the answer sent (Answered)
	awaitReread                        // the daemon rereading the rule written (RereadResult)
	awaitProposeReread                 // the daemon rereading before a proposal (RereadResult)
)

// waitsForDaemon reports whether a waits for a frame of the daemon's.
func (a awaiting) waitsForDaemon() bool {
	return a == awaitAnswered || a == awaitReread || a == awaitProposeReread
}

// waiting is an approval request and when its time runs out. ended is why
// it ended unanswered (a protocol.EndReason) while its answer was
// on the way to the daemon, which will not take it.
type waiting struct {
	protocol.ApprovalRequest
	deadline time.Time
	ended    protocol.EndReason
}

// sentAnswer is an answer sent and not yet confirmed: its outcome, and its
// scope, "" for a refusal: an approval's outcome names its run.
type sentAnswer struct {
	outcome string
	scope   protocol.Scope
}

// loop handles the daemon's frames and the human's lines until one of them
// ends serving the daemon, a write to the daemon fails, or ctx ends it,
// and returns how. Leaving refuses the requests still waiting.
func (s *session) loop(ctx context.Context, frames <-chan protocol.Frame) (next, error) {
	for {
		if s.failed != nil && ctx.Err() != nil {
			// Ctrl+C hung up, which is why the write failed.
			s.leave()
			return nextQuit, nil
		}
		if s.failed != nil {
			s.connectionFailed()
			return nextAwait, nil
		}
		select {
		case <-ctx.Done():
			s.leave()
			return nextQuit, nil
		case f, ok := <-frames:
			if !ok && ctx.Err() != nil {
				// Ctrl+C hung up, and the daemon closed the connection,
				// before ctx.Done was seen.
				s.leave()
				return nextQuit, nil
			}
			if !ok {
				s.said = goneSaid
				return nextAwait, nil
			}
			if f.Type == protocol.FrameTakenOver {
				fmt.Fprintln(s.out, "\ntaken over by another session")
				return nextTakenOver, nil
			}
			if err := s.daemonFrame(f); err != nil {
				return nextFailed, err
			}
		case l, ok := <-s.Lines():
			if !ok {
				s.leave()
				return nextQuit, nil
			}
			if s.Fresh(l) {
				s.typed(l.Text)
			}
			s.Handled(l)
			if s.after != 0 {
				return s.after, nil
			}
		}
	}
}

// daemonFrame handles one frame from the daemon.
func (s *session) daemonFrame(f protocol.Frame) error {
	switch f.Type {
	case protocol.FrameApprovalRequest:
		var r protocol.ApprovalRequest
		if err := protocol.DecodeJSON(f, &r); err != nil {
			return err
		}
		fmt.Fprint(s.out, "\a")
		s.queue = append(s.queue, waiting{ApprovalRequest: r, deadline: time.Now().Add(time.Duration(r.TimeoutMs) * time.Millisecond)})
		if len(s.queue) == 1 {
			s.show()
		}
	case protocol.FrameAnswered:
		var a protocol.Answered
		if err := protocol.DecodeJSON(f, &a); err != nil {
			return err
		}
		if s.awaiting != awaitAnswered || len(s.queue) == 0 || s.queue[0].ID != a.ID {
			return fmt.Errorf("the daemon confirmed request %d, which was not answered", a.ID)
		}
		// The request may have ended as the answer came: the daemon says so
		// here, or told the session already (withdrawn).
		ended := cmp.Or(a.Ended, s.queue[0].ended)
		switch {
		case a.Error != "" && ended != "":
			fmt.Fprintf(s.out, "request %d %s\n", a.ID, ended)
		case a.Error != "":
			fmt.Fprintf(s.out, "request %d: not taken: %s\n", a.ID, a.Error)
		case s.sent.scope != "" && s.daemon.Runs:
			// The run approved: reported with its answer when it ended
			// already.
			n, ended := s.answeredRun(a.ID)
			fmt.Fprintf(s.out, "%s · run %d\n", s.sent.outcome, n)
			if ended != "" {
				fmt.Fprintln(s.out, ended)
			}
		default:
			fmt.Fprintln(s.out, s.sent.outcome)
		}
		s.awaiting, s.sent = awaitAnswer, sentAnswer{}
		s.advance()
	case protocol.FrameWithdrawn:
		var w protocol.Withdrawn
		if err := protocol.DecodeJSON(f, &w); err != nil {
			return err
		}
		s.withdrawn(w)
	case protocol.FrameStandingList:
		var l protocol.StandingList
		if err := protocol.DecodeJSON(f, &l); err != nil {
			return err
		}
		var b strings.Builder
		switch {
		case l.Error != "":
			fmt.Fprintf(&b, "standing: %s\n", l.Error)
		case len(l.Approvals) == 0:
			fmt.Fprintln(&b, "no approvals")
		}
		for _, a := range l.Approvals {
			line := fmt.Sprintf("  %d  %s   in %s", a.N, hostterm.ArgvLine(a.Argv), hostterm.Display(a.Cwd))
			if a.Guarded {
				line += "   guarded"
			}
			if a.Session {
				line += "   session"
			}
			fmt.Fprintln(&b, line)
			printMarks(&b, a.Argv)
		}
		s.commandOutcome(b.String())
	case protocol.FrameRereadResult:
		var r protocol.RereadResult
		if err := protocol.DecodeJSON(f, &r); err != nil {
			return err
		}
		if s.awaiting != awaitReread && s.awaiting != awaitProposeReread {
			return errors.New("the daemon reread the rules, which the session did not ask for")
		}
		s.reread(r)
	case protocol.FrameRulesReread:
		var r protocol.RulesReread
		if err := protocol.DecodeJSON(f, &r); err != nil {
			return err
		}
		s.rulesReread(r)
	case protocol.FrameRunStarted:
		var m protocol.RunStarted
		if err := protocol.DecodeJSON(f, &m); err != nil {
			return err
		}
		s.runStarted(m)
	case protocol.FrameRunOutput:
		var m protocol.RunOutput
		if err := protocol.DecodeJSON(f, &m); err != nil {
			return err
		}
		s.runOutput(m)
	case protocol.FrameRunEnded:
		var m protocol.RunEnded
		if err := protocol.DecodeJSON(f, &m); err != nil {
			return err
		}
		s.runEnded(m)
	case protocol.FrameRevoked:
		var r protocol.Revoked
		if err := protocol.DecodeJSON(f, &r); err != nil {
			return err
		}
		if r.Error != "" {
			s.commandOutcome(fmt.Sprintf("revoke %d: %s\n", r.N, r.Error))
		} else {
			s.commandOutcome(fmt.Sprintf("revoked %d\n", r.N))
		}
	default:
		return daemonlist.ErrorOf(f)
	}
	return nil
}

// askAgain asks again what the session asks while it awaits nothing of
// the daemon's, after a reread of it that came as the human typed, none
// of which was taken: the answer to the request shown, or, when none is,
// a command at the idle prompt.
func (s *session) askAgain() {
	if len(s.queue) == 0 {
		s.prompt(idlePrompt)
		return
	}
	s.askAnswer()
}

// typed handles a line the human typed after the current prompt: what the
// session awaits while a request is shown, a command while none is.
// Nothing is taken while it awaits the daemon.
func (s *session) typed(text string) {
	text = strings.TrimSpace(text)
	if len(s.queue) == 0 {
		s.command(text)
		return
	}
	r := s.queue[0]
	switch s.awaiting {
	case awaitAnswered, awaitReread, awaitProposeReread:
	case awaitRereadChoice:
		s.rereadTyped(text)
	case awaitAdd:
		s.addTyped(r, text)
	case awaitNote:
		note := hostterm.Display(text)
		if len(note) > maxNoteBytes {
			fmt.Fprintf(s.out, "note too long: at most %d bytes\n", maxNoteBytes)
			s.prompt(notePrompt)
			return
		}
		s.send(protocol.Answer{ID: r.ID, Note: note}, "refused")
	case awaitConfirm:
		scope := s.confirm
		s.awaiting, s.confirm = awaitAnswer, ""
		if text == "yes" {
			s.approve(r.ID, scope)
		} else {
			s.askAnswer()
		}
	default:
		s.answerTyped(r, text)
	}
}

// answerTyped handles text typed as the answer to r: r refuses it, asking
// for a note; o, s and d approve it (this request only, for this session,
// standing), once a guarded one is confirmed; a adds a rule; c shows all
// of its argv; h prints what each answer offered for it does. Anything
// else asks again.
func (s *session) answerTyped(r waiting, text string) {
	if scope, ok := approvingKeys[text]; ok {
		if !r.guarded() {
			s.approve(r.ID, scope)
			return
		}
		// A guarded request is approved only once the human has read its
		// guards again and typed "yes"; anything else asks for the answer
		// again. A standing or session approval is confirmed so once, here.
		for _, guard := range r.Guards {
			fmt.Fprintf(s.out, "⚠ %s\n", hostterm.Display(guard))
		}
		s.awaiting, s.confirm = awaitConfirm, scope
		s.prompt(scopeTexts[scope].confirm)
		return
	}
	switch text {
	case "a":
		if r.guarded() {
			s.askAnswer()
			return
		}
		s.proposeRule(r)
	case "c":
		s.showAll(r)
		s.askAnswer()
	case "h":
		s.showHelp(r)
		s.askAnswer()
	case "r":
		s.awaiting = awaitNote
		s.prompt(notePrompt)
	default:
		s.askAnswer()
	}
}

// idleCommand is a command the idle prompt takes: its name, how the
// command list (idleCommands) shows it, and what runs it, given the
// argument typed after the name, spaces trimmed.
type idleCommand struct {
	name, syntax string
	run          func(s *session, arg string)
}

// idleCommandTable are the commands the idle prompt takes, in the order
// the command list names them: standing lists the standing and session
// approvals, revoke <n> revokes one, the daemon's answer printing the
// outcome; out [n] prints the output kept of a run (see showOutput), runs
// lists the runs kept (see listRuns), their outcome held as the daemon's
// is; daemons leaves the daemon for the list of daemons, quit ends the
// session.
var idleCommandTable = []idleCommand{
	{"standing", "standing", func(s *session, _ string) { s.write(protocol.FrameStanding, nil) }},
	{"revoke", "revoke <n>", (*session).revoke},
	{"out", "out [n]", func(s *session, arg string) { s.commandOutcome(s.showOutput(arg)) }},
	{"runs", "runs", func(s *session, _ string) { s.commandOutcome(s.listRuns()) }},
	{"daemons", "daemons", (*session).leaveForList},
	{"quit", "quit", func(s *session, _ string) { s.after = nextQuit }},
}

// idleCommands is the command list the session prints as it starts
// serving a daemon and of a command it does not know: `commands: ` and
// the syntax of each command of idleCommandTable.
var idleCommands = commandList(idleCommandTable)

// commandList returns the command list of the commands cmds.
func commandList(cmds []idleCommand) string {
	syntaxes := make([]string, len(cmds))
	for i, c := range cmds {
		syntaxes[i] = c.syntax
	}
	return "commands: " + strings.Join(syntaxes, ", ")
}

// command runs a command typed at the idle prompt (see
// idleCommandTable); an empty line shows the prompt again, a command
// that takes no argument, its syntax being its name, given one prints its
// usage (`usage: runs`), and any other says it is unknown, with the
// command list.
func (s *session) command(text string) {
	name, arg, _ := strings.Cut(text, " ")
	if name == "" {
		s.prompt(idlePrompt)
		return
	}
	i := slices.IndexFunc(idleCommandTable, func(c idleCommand) bool { return c.name == name })
	if i < 0 {
		fmt.Fprintf(s.out, "unknown command %q: %s\n", hostterm.Display(text), idleCommands)
		s.prompt(idlePrompt)
		return
	}
	c := idleCommandTable[i]
	arg = strings.TrimSpace(arg)
	if arg != "" && c.syntax == c.name {
		fmt.Fprintf(s.out, "usage: %s\n", c.name)
		s.prompt(idlePrompt)
		return
	}
	c.run(s, arg)
}

// revoke runs revoke arg: it asks the daemon to revoke the approval arg
// numbers in the standing list, or prints the usage for an arg that is
// not a number.
func (s *session) revoke(arg string) {
	n, err := strconv.ParseUint(arg, 10, 64)
	if err != nil {
		fmt.Fprintln(s.out, "usage: revoke <n>, n from standing")
		s.prompt(idlePrompt)
		return
	}
	s.write(protocol.FrameRevoke, protocol.Revoke{N: n})
}

// leaveForList runs daemons: it leaves the daemon for the list of
// daemons, unless the session serves the one daemon it was given.
func (s *session) leaveForList(string) {
	if !s.canList {
		fmt.Fprintln(s.out, "this session serves the one daemon it was given")
		s.prompt(idlePrompt)
		return
	}
	s.after = nextList
}

// approve approves request id for scope.
func (s *session) approve(id uint64, scope protocol.Scope) {
	s.send(protocol.Answer{ID: id, Scope: scope}, scopeTexts[scope].outcome)
}

// send sends ans to the daemon; outcome is printed once the daemon has
// taken it.
func (s *session) send(ans protocol.Answer, outcome string) {
	s.awaiting, s.sent = awaitAnswered, sentAnswer{outcome: outcome, scope: ans.Scope}
	s.write(protocol.FrameAnswer, ans)
}

// write sends the daemon a frame of type t carrying v, none when v is nil.
// A write that fails ends serving the daemon (see loop): what it carried
// never reached it.
func (s *session) write(t protocol.FrameType, v any) {
	if s.failed != nil {
		return
	}
	var err error
	if v == nil {
		err = protocol.WriteFrame(s.conn, protocol.Frame{Type: t})
	} else {
		err = protocol.WriteJSON(s.conn, t, v)
	}
	s.failed = err
}

// withdrawn drops the request w names, which ended unanswered. A queued
// one drops out silently; the current one is announced, and the next is
// shown, its prompt discarding what was typed for the one that ended. One
// whose answer is on the way is announced once the daemon refuses it.
func (s *session) withdrawn(w protocol.Withdrawn) {
	i := slices.IndexFunc(s.queue, func(r waiting) bool { return r.ID == w.ID })
	switch {
	case i < 0: // answered meanwhile, or its end came with the answer
	case i > 0:
		s.queue = slices.Delete(s.queue, i, i+1)
	case s.awaiting == awaitAnswered:
		s.queue[0].ended = w.Reason
	default:
		fmt.Fprintf(s.out, "\nrequest %d %s\n", w.ID, w.Reason)
		// What was asked of it goes with it. A reread of the daemon's is
		// still awaited, nothing typed being taken till it comes: of the
		// rule written (its addition kept for the result), or of a file
		// changed by hand, before a proposal no longer due.
		if s.awaiting != awaitReread {
			s.dropAddition()
		}
		if !s.awaiting.waitsForDaemon() {
			s.awaiting = awaitAnswer
		}
		s.confirm, s.rereadDigest = "", ""
		s.advance()
	}
}

// rulesReread says, as a notice (see notify), that the daemon found its
// rules file changed and reread it, keeping the rules it now serves for
// later proposals at once, or why it kept its old ones.
func (s *session) rulesReread(r protocol.RulesReread) {
	if r.Error != "" {
		s.notify("the rules file changed, but the daemon cannot reread it, so the old rules stay: " + hostterm.Display(r.Error))
		return
	}
	s.daemon.RulesDigest = r.Digest
	s.notify("the rules file changed; the daemon reread it")
}

// notify prints text, a notice, on lines of its own, adding the newline
// that ends its last: what the daemon tells the session of its own accord,
// not as the outcome of what the human typed (a run's start line or
// report, a reread of the rules). It is held while a request is shown
// (see hold); at the idle prompt it is printed at once, below what is
// shown (see printBelow).
func (s *session) notify(text string) {
	text += "\n"
	if !s.hold(text) {
		s.printBelow(text)
	}
}

// printBelow prints text, ending with a newline, below what is shown,
// then the prompt shown, if any, again, keeping what the human typed
// after it: a notice that arrives as they type a command at the idle
// prompt takes none of it, even on a terminal, whose line typed in part
// a prompt would discard. No prompt is shown again while the session
// waits for the daemon.
func (s *session) printBelow(text string) {
	fmt.Fprintf(s.out, "\n%s", text)
	s.showAgain()
}

// commandOutcome prints text, the outcome of a command typed at the
// idle prompt (standing, revoke, out, runs), each line ending with a
// newline.
// It is held while a request is shown, as a notice is (see hold); at the
// idle prompt it is printed at once, right below the command's line, and
// the prompt shown again, keeping what the human typed after it, as
// printBelow does.
func (s *session) commandOutcome(text string) {
	if !s.hold(text) {
		fmt.Fprint(s.out, text)
		s.showAgain()
	}
}

// showAgain prints the prompt shown again, keeping what the human typed
// after it, unless the session waits for the daemon, which shows none.
func (s *session) showAgain() {
	if !s.awaiting.waitsForDaemon() {
		fmt.Fprint(s.out, s.asking)
	}
}

// hold holds text, ending with a newline, while a request is shown, and
// reports whether it did. Nothing is printed in the middle of a request:
// from its block until the daemon has taken its answer or it is
// withdrawn, with every prompt it asks meanwhile (a note, "yes", adding a
// rule), what arrives is printed with what was held before it once the
// request is gone (see printHeld).
func (s *session) hold(text string) bool {
	if len(s.queue) == 0 {
		return false
	}
	s.held = append(s.held, text)
	return true
}

// printHeld prints what was held while a request was shown (see hold), in
// the order it arrived, and holds nothing any more. It is called once
// that request is gone, before what comes next is shown: the next
// request or the idle prompt, right below the request's last line (the
// answer's line, or its end), or, apart, the end of serving the daemon,
// after a blank line below what the session said of that, on lines of
// their own even when the session left at a request's prompt.
func (s *session) printHeld(apart bool) {
	if len(s.held) == 0 {
		return
	}
	if apart {
		fmt.Fprintln(s.out)
	}
	for _, text := range s.held {
		fmt.Fprint(s.out, text)
	}
	s.held = nil
}

// leave says how many requests leaving refuses: the daemon refuses every
// one still waiting once the session's connection closes. One whose answer
// is on the way may be taken first, so it does not count. The count is the
// session's view: a request or an end still on its way from the daemon is
// not in it.
func (s *session) leave() {
	n := len(s.queue)
	if s.awaiting == awaitAnswered {
		n--
	}
	if n > 0 {
		fmt.Fprintln(s.out)
	}
	s.sayRefused(n)
}

// connectionFailed says that a write to the daemon failed (see write):
// the daemon, if it is there, has dropped the session and refused every
// request waiting, the one whose answer did not reach it among them; a
// rule written but not reread takes effect at the next container start.
// The daemon may well be there: it is served again once it answers on
// its socket.
func (s *session) connectionFailed() {
	fmt.Fprintf(s.out, "\nthe connection to the daemon failed: %s\n", hostterm.Display(s.failed.Error()))
	if s.awaiting == awaitReread {
		fmt.Fprintln(s.out, "rule written, but the daemon did not reread it: it takes effect at the next container start")
	}
	s.sayRefused(len(s.queue))
	s.said = "waiting for the next one\n"
}

// sayRefused says that the daemon refused n waiting requests, if any.
func (s *session) sayRefused(n int) {
	switch {
	case n == 1:
		fmt.Fprintln(s.out, "refused 1 waiting request")
	case n > 1:
		fmt.Fprintf(s.out, "refused %d waiting requests\n", n)
	}
}

// advance drops the answered request, prints what was held while it was
// shown, and shows the one after it, if any.
func (s *session) advance() {
	s.queue = s.queue[1:]
	s.printHeld(false)
	if len(s.queue) > 0 {
		s.show()
		return
	}
	s.prompt(idlePrompt)
}

// show prints the first waiting request as a block and asks for the
// answer.
func (s *session) show() {
	r := s.queue[0]
	left := max(time.Until(r.deadline), 0).Round(time.Second)
	head := fmt.Sprintf("── request %d ── %d:%02d left ", r.ID, int(left.Minutes()), int(left.Seconds())%60)
	fmt.Fprintf(s.out, "\n%s%s\n", head, strings.Repeat("─", max(0, 58-len([]rune(head)))))
	fmt.Fprintf(s.out, "  dir   %s\n", hostterm.Display(r.Cwd))
	fmt.Fprintf(s.out, "  cmd   %s\n", hostterm.ArgvLine(r.Argv))
	printMarks(s.out, r.Argv)
	if len(r.Argv) > hostterm.MaxShownArgs || slices.ContainsFunc(hostterm.ShownArgs(r.Argv), hostterm.IsLong) {
		fmt.Fprintln(s.out, "  cmd truncated: c shows all")
	}
	for i, reason := range r.Reasons {
		label := "why "
		if i > 0 {
			label = "    "
		}
		fmt.Fprintf(s.out, "  %s  %s\n", label, hostterm.Display(reason))
	}
	for _, guard := range r.Guards {
		fmt.Fprintf(s.out, "  ⚠ guarded: %s\n", hostterm.Display(guard))
	}
	if more := len(s.queue) - 1; more > 0 {
		fmt.Fprintf(s.out, "  (%d more waiting)\n", more)
	}
	s.askAnswer()
}

// prompt prints p and discards what the human typed before it, so a key
// meant for something else never answers a request. The lines counted
// before p is printed are the ones discarded, so an answer typed once p
// shows always counts.
func (t *term) prompt(p string) {
	t.asking = p
	t.Prompt(t.out, p)
}

// printMarks prints on w a `⚠ non-ASCII` line for every argument argvLine
// shows that holds non-ASCII: the argument as argvLine shows it, and its
// ASCII form, where a look-alike letter shows its code, cut alike.
func printMarks(w io.Writer, argv []string) {
	for i, arg := range hostterm.ShownArgs(argv) {
		if hostterm.HoldsNonASCII(arg) {
			fmt.Fprintf(w, "  ⚠ non-ASCII: argument %d %s is %s\n", i+1, textcut.Cut(arg, hostterm.BashQuote), textcut.Cut(arg, hostterm.ASCIIQuote))
		}
	}
}

// showHelp prints what each answer offered for r does, one per answer,
// wrapped at helpWidth: a guarded request is offered no rule, and its o, s
// and d ask for yes.
func (s *session) showHelp(r waiting) {
	for _, e := range helpEntries {
		does := e.does
		if r.guarded() {
			if e.addsRule {
				continue
			}
			if e.confirms {
				does += helpConfirms
			}
		}
		line := "  " + e.key + " "
		for _, word := range strings.Fields(does) {
			if len([]rune(line))+1+len([]rune(word)) > helpWidth {
				fmt.Fprintln(s.out, line)
				line = "    "
			}
			line += " " + word
		}
		fmt.Fprintln(s.out, line)
	}
}

// showAll prints every argument of r whole, one numbered per line, an
// argument holding non-ASCII with its ASCII form.
func (s *session) showAll(r waiting) {
	for i, arg := range r.Argv {
		line := fmt.Sprintf("  %d  %s", i+1, hostterm.BashQuote(arg))
		if hostterm.HoldsNonASCII(arg) {
			line += "   ⚠ non-ASCII: " + hostterm.ASCIIQuote(arg)
		}
		fmt.Fprintln(s.out, line)
	}
}

// readDaemon reads the daemon's frames into the returned channel and closes
// it when the connection ends or done is closed.
func readDaemon(conn io.Reader, done <-chan struct{}) <-chan protocol.Frame {
	ch := make(chan protocol.Frame)
	go func() {
		defer close(ch)
		for {
			f, err := protocol.ReadFrame(conn)
			if err != nil {
				return
			}
			select {
			case ch <- f:
			case <-done:
				return
			}
		}
	}()
	return ch
}
