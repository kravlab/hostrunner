// Package approval implements the approval session, `hostrunner approve`:
// the host terminal process in which a human sees one daemon's approval
// requests and answers them.
//
// The session connects to the daemon's approval socket, which the
// container cannot reach, and serves the daemon: the daemon sends it each
// approval request, and it sends back the human's answer. It is a
// line-oriented log, without an alternate screen: requests are shown one
// at a time, first in, first out, each as a block, and answered with a
// letter and Enter. What came from the container is shown quoted, so that
// it cannot steer the terminal.
package approval

import (
	"bytes"
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
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/kravlab/hostrunner/internal/protocol"
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
}

// ErrNoEditor is what EditInEditor returns when neither $VISUAL nor
// $EDITOR names an editor.
var ErrNoEditor = errors.New("$EDITOR is not set")

// EditInEditor opens the file at path in $VISUAL, or else $EDITOR, as a
// shell runs it, on the process's terminal, and waits for it to exit.
func EditInEditor(path string) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		return ErrNoEditor
	}
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "editor", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Prompts and lines the session prints.
const (
	answerPrompt        = "answer [r]efuse [o]nce [l]ifetime [a]dd rule [?]more ? "
	guardedAnswerPrompt = "answer [r]efuse [o]nce [l]ifetime [?]more ? " // no rule for a guarded refusal
	addPrompt           = "[w]rite [e]dit in $EDITOR [c]ancel ? "
	editAgainPrompt     = "[e]dit again [c]ancel ? "
	notePrompt          = "note for the agent (Enter for none): "
	idlePrompt          = "> "
	idleCommands        = "commands: standing, revoke <n>, daemons, quit"
)

// scopeTexts are what the session says of an approval, by its scope: the
// outcome once the daemon has taken it, and the prompt confirming one for
// a guarded request.
var scopeTexts = map[protocol.Scope]struct{ outcome, confirm string }{
	protocol.ScopeOnce:     {"approved once", `Type "yes" to run it once: `},
	protocol.ScopeLifetime: {"approved for the daemon's lifetime", `Type "yes" to approve it for the daemon's lifetime: `},
	protocol.ScopeRule:     {"approved under the new rule", ""},
}

// Run serves the daemon on cfg.Socket until ctx ends, in ends, or the
// daemon goes away: it shows each approval request on out, ringing the
// terminal bell, and sends the answer the human types on in. Requests that
// end unanswered are dropped as the daemon reports them; leaving refuses
// the ones still waiting (the daemon does, as the connection closes), and
// Run says how many. The answer "add a rule" edits the daemon's rules file
// (see proposeRule), opening it in $VISUAL or $EDITOR when the human
// asks. While no request is shown, the idle prompt takes commands:
// standing lists the daemon's standing approvals, revoke <n> revokes one,
// daemons leaves the daemon for the list, quit ends the session.
//
// Without cfg.Socket it lists the daemons under cfg.Root to choose from
// (see choose), and a daemon another session serves can be taken over:
// the session taken over says so and ends. When its daemon ends, the
// session waits for the next one in the same runtime directory. It returns
// an error when it cannot serve the daemon cfg.Socket names: one without
// an approval socket has no approvals.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer) error {
	done := make(chan struct{})
	defer close(done)
	t := &term{out: out, in: in, want: make(chan struct{}, 1)}
	t.want <- struct{}{}
	t.lines = readLines(in, &t.read, t.want, done)
	socket, takeOver := cfg.Socket, false
	first := socket != ""
	for {
		if socket == "" {
			var ok bool
			if socket, takeOver, ok = t.choose(ctx, cfg); !ok {
				return nil
			}
		}
		end, err := t.serveDaemon(ctx, socket, takeOver, cfg.Root != "")
		takeOver = false
		switch {
		case end == endNoDaemon && first:
			return fmt.Errorf("%s has no approvals: no daemon takes them there: %w", strings.TrimSuffix(socket, approvalSuffix), err)
		case end == endFailed || end == endNoDaemon:
			if cfg.Root == "" {
				return err
			}
			fmt.Fprintf(out, "cannot serve the daemon: %s\n", display(err.Error()))
			socket = ""
		case end == endList:
			socket = ""
		case end == endGone:
			switch t.awaitDaemon(ctx, socket, cfg.Root != "") {
			case endBack: // serve it
			case endList:
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

// approvalSuffix ends an approval socket's path: it lies beside the
// runtime directory it is named after (see launch.ApprovalSocket).
const approvalSuffix = ".approval.sock"

// end is how serving a daemon ended.
type end int

const (
	endQuit      end = iota + 1 // the human quit, or input ended
	endTakenOver                // another session took the daemon over
	endList                     // the human asked for the list of daemons
	endGone                     // the daemon ended
	endBack                     // a daemon answers on the socket again
	endNoDaemon                 // no daemon answered on the socket
	endFailed                   // the daemon would not be served
)

// serveDaemon serves the daemon on socket, taking it over from the session
// serving it with takeOver, until the human or the daemon ends it; canList
// lets the human ask for the list of daemons. It returns once the daemon
// has dropped the session (see hangUp), so a list of daemons asked for
// right after shows the daemon unserved; an interruption (ctx) hangs up
// alike.
func (t *term) serveDaemon(ctx context.Context, socket string, takeOver, canList bool) (end, error) {
	conn, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return endNoDaemon, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() {
		// The daemon closing its side ends every read; should it not
		// within hangUpTimeout, the connection is closed here.
		halfClose(conn)
		time.AfterFunc(hangUpTimeout, func() { conn.Close() })
	})
	defer stop()

	info, err := hello(conn)
	if err != nil {
		return endFailed, err
	}
	if info.Version != protocol.ApprovalVersion {
		return endFailed, fmt.Errorf("the daemon speaks approval protocol version %d, this session %d: restart its container", info.Version, protocol.ApprovalVersion)
	}
	if err := serve(conn, takeOver); err != nil {
		return endFailed, err
	}
	fmt.Fprintf(t.out, "serving %s (container %s)\nwaiting for requests · %s\n", display(info.Workspace), display(info.ContainerWorkspace), idleCommands)

	done := make(chan struct{})
	defer close(done)
	s := &session{term: t, conn: conn, canList: canList, rulesFile: info.RulesFile, rulesDigest: info.RulesDigest, workspace: info.Workspace}
	s.prompt(idlePrompt)
	frames := readDaemon(conn, done)
	end, err := s.loop(ctx, frames)
	s.dropAddition()
	hangUp(conn, frames)
	return end, err
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

// hello opens the connection and returns the daemon's answer.
func hello(conn io.ReadWriter) (protocol.DaemonInfo, error) {
	var info protocol.DaemonInfo
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		return info, err
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return info, fmt.Errorf("the daemon did not answer: %w", err)
	}
	if f.Type != protocol.FrameDaemonInfo {
		return info, frameError(f)
	}
	return info, protocol.DecodeJSON(f, &info)
}

// serve makes the session the one that serves the daemon, taking it over
// from the session serving it with takeOver.
func serve(conn io.ReadWriter, takeOver bool) error {
	if err := protocol.WriteJSON(conn, protocol.FrameServe, protocol.Serve{TakeOver: takeOver}); err != nil {
		return err
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("the daemon did not answer: %w", err)
	}
	if f.Type != protocol.FrameServing {
		return frameError(f)
	}
	return nil
}

// frameError turns a frame the daemon sent instead of the expected one into
// an error.
func frameError(f protocol.Frame) error {
	var e protocol.Error
	if f.Type == protocol.FrameError && protocol.DecodeJSON(f, &e) == nil {
		return errors.New(e.Message)
	}
	return fmt.Errorf("unexpected frame type %d from the daemon", f.Type)
}

// term is the human's side of a session, kept across the daemons it
// serves: where it prints, the lines typed, and the prompt they answer.
type term struct {
	out   io.Writer
	in    io.Reader
	lines <-chan line
	read  atomic.Uint64 // lines read from in so far
	// fresh is the number of the first line typed after the current
	// prompt; lines typed before it are discarded.
	fresh uint64
	// asking is the current prompt.
	asking string
	// want lets readLines read on: it reads only while the session waits
	// for the human, never while an editor has the terminal.
	want chan struct{}
}

// handled lets readLines read on once the lines of its last read are
// handled, l being one of them.
func (t *term) handled(l line) {
	if l.last {
		t.want <- struct{}{}
	}
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
	// end is set once the human ends serving the daemon.
	end end

	queue []waiting
	// note is set while the human types the note of a refusal.
	note bool
	// confirm is the scope the human confirms approving a guarded request
	// for; "" while no confirmation is asked.
	confirm protocol.Scope
	// sent is the outcome of the answer sent for the first request,
	// printed once the daemon has taken it; "" while none is sent.
	sent string

	// rulesFile is the daemon's rules file, in its host workspace, which
	// a rule is added to; rulesDigest is the digest of the rules the daemon
	// serves, which the file must have for a rule to be proposed.
	rulesFile, rulesDigest, workspace string
	// adding is the rule being added, while the human writes, edits or
	// cancels it; rereading is set while the daemon rereads the rules.
	adding    *addition
	rereading bool
}

// waiting is an approval request and when its time runs out. ended is why
// it ended unanswered (a protocol.Withdrawn reason) while its answer was
// on the way to the daemon, which will not take it.
type waiting struct {
	protocol.ApprovalRequest
	deadline time.Time
	ended    string
}

// loop handles the daemon's frames and the human's lines until one of them
// ends serving the daemon, or ctx does, and returns how. Leaving refuses
// the requests still waiting.
func (s *session) loop(ctx context.Context, frames <-chan protocol.Frame) (end, error) {
	for {
		select {
		case <-ctx.Done():
			s.leave()
			return endQuit, nil
		case f, ok := <-frames:
			if !ok && ctx.Err() != nil {
				// Ctrl+C hung up, and the daemon closed the connection,
				// before ctx.Done was seen.
				s.leave()
				return endQuit, nil
			}
			if !ok {
				fmt.Fprintln(s.out, "\nthe daemon ended: waiting for the next one")
				return endGone, nil
			}
			if f.Type == protocol.FrameTakenOver {
				fmt.Fprintln(s.out, "\ntaken over by another session")
				return endTakenOver, nil
			}
			if err := s.daemonFrame(f); err != nil {
				return endFailed, err
			}
		case l, ok := <-s.lines:
			if !ok {
				s.leave()
				return endQuit, nil
			}
			if l.n >= s.fresh {
				s.typed(l.text)
			}
			s.handled(l)
			if s.end != 0 {
				return s.end, nil
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
		if s.sent == "" || len(s.queue) == 0 || s.queue[0].ID != a.ID {
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
		default:
			fmt.Fprintln(s.out, s.sent)
		}
		s.sent = ""
		s.next()
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
		switch {
		case l.Error != "":
			fmt.Fprintf(s.out, "standing: %s\n", l.Error)
		case len(l.Approvals) == 0:
			fmt.Fprintln(s.out, "no standing approvals")
		}
		for _, a := range l.Approvals {
			line := fmt.Sprintf("  %d  %s   in %s", a.N, argvLine(a.Argv), display(a.Cwd))
			if a.Guarded {
				line += "   guarded"
			}
			fmt.Fprintln(s.out, line)
			s.printMarks(a.Argv)
		}
		s.idle()
	case protocol.FrameRereadResult:
		var r protocol.RereadResult
		if err := protocol.DecodeJSON(f, &r); err != nil {
			return err
		}
		s.reread(r)
	case protocol.FrameRevoked:
		var r protocol.Revoked
		if err := protocol.DecodeJSON(f, &r); err != nil {
			return err
		}
		if r.Error != "" {
			fmt.Fprintf(s.out, "revoke %d: %s\n", r.N, r.Error)
		} else {
			fmt.Fprintf(s.out, "revoked %d\n", r.N)
		}
		s.idle()
	default:
		return frameError(f)
	}
	return nil
}

// idle asks again, after a command's outcome, what was asked before it:
// the idle prompt, or, when a request arrived meanwhile, its prompt, which
// the outcome printed below would bury. Nothing is asked while an answer
// waits for the daemon.
func (s *session) idle() {
	switch {
	case len(s.queue) == 0:
		s.prompt(idlePrompt)
	case s.sent == "":
		s.prompt(s.asking)
	}
}

// typed handles a line the human typed after the current prompt: an
// answer while a request is shown, a command while none is. Nothing is
// asked while an answer waits for the daemon.
func (s *session) typed(text string) {
	text = strings.TrimSpace(text)
	if len(s.queue) == 0 {
		s.command(text)
		return
	}
	if s.sent != "" || s.rereading {
		return
	}
	r := s.queue[0]
	if s.adding != nil {
		s.addTyped(r, text)
		return
	}
	if s.note {
		s.note = false
		s.send(protocol.Answer{ID: r.ID, Note: display(text)}, "refused")
		return
	}
	if scope := s.confirm; scope != "" {
		s.confirm = ""
		if text == "yes" {
			s.approve(r.ID, scope)
		} else {
			s.askAnswer()
		}
		return
	}
	switch text {
	case "o", "l":
		scope := protocol.ScopeOnce
		if text == "l" {
			scope = protocol.ScopeLifetime
		}
		if !r.guarded() {
			s.approve(r.ID, scope)
			return
		}
		// A guarded request is approved only once the human has read its
		// guards again and typed "yes"; anything else asks for the answer
		// again. A standing approval is confirmed so once, here.
		for _, guard := range r.Guards {
			fmt.Fprintf(s.out, "⚠ %s\n", display(guard))
		}
		s.confirm = scope
		s.prompt(scopeTexts[scope].confirm)
	case "a":
		if r.guarded() {
			s.askAnswer()
			return
		}
		s.proposeRule(r)
	case "?":
		s.showAll(r)
		s.askAnswer()
	case "r":
		s.note = true
		s.prompt(notePrompt)
	default:
		s.askAnswer()
	}
}

// command runs a command typed at the idle prompt: standing lists the
// standing approvals, revoke <n> revokes one, the daemon's answer printing
// the outcome; daemons leaves the daemon for the list of daemons, quit
// ends the session.
func (s *session) command(text string) {
	name, arg, _ := strings.Cut(text, " ")
	switch name {
	case "":
		s.prompt(idlePrompt)
	case "standing":
		_ = protocol.WriteFrame(s.conn, protocol.Frame{Type: protocol.FrameStanding})
	case "revoke":
		n, err := strconv.ParseUint(strings.TrimSpace(arg), 10, 64)
		if err != nil {
			fmt.Fprintln(s.out, "usage: revoke <n>, n from standing")
			s.prompt(idlePrompt)
			return
		}
		_ = protocol.WriteJSON(s.conn, protocol.FrameRevoke, protocol.Revoke{N: n})
	case "daemons":
		if !s.canList {
			fmt.Fprintln(s.out, "this session serves the one daemon it was given")
			s.prompt(idlePrompt)
			return
		}
		s.end = endList
	case "quit":
		s.end = endQuit
	default:
		fmt.Fprintf(s.out, "unknown command %q: %s\n", display(text), idleCommands)
		s.prompt(idlePrompt)
	}
}

// approve approves request id for scope.
func (s *session) approve(id uint64, scope protocol.Scope) {
	s.send(protocol.Answer{ID: id, Scope: scope}, scopeTexts[scope].outcome)
}

// send sends ans to the daemon; outcome is printed once the daemon has
// taken it.
func (s *session) send(ans protocol.Answer, outcome string) {
	s.sent = outcome
	_ = protocol.WriteJSON(s.conn, protocol.FrameAnswer, ans)
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
	case s.sent != "":
		s.queue[0].ended = w.Reason
	default:
		fmt.Fprintf(s.out, "\nrequest %d %s\n", w.ID, w.Reason)
		s.note, s.confirm = false, ""
		if !s.rereading {
			s.dropAddition()
		}
		s.next()
	}
}

// leave says how many requests leaving refuses: the daemon refuses every
// one still waiting once the session's connection closes. One whose answer
// is on the way may be taken first, so it does not count. The count is the
// session's view: a request or an end still on its way from the daemon is
// not in it.
func (s *session) leave() {
	n := len(s.queue)
	if s.sent != "" {
		n--
	}
	switch {
	case n == 1:
		fmt.Fprintln(s.out, "\nrefused 1 waiting request")
	case n > 1:
		fmt.Fprintf(s.out, "\nrefused %d waiting requests\n", n)
	}
}

// next drops the answered request and shows the one after it, if any.
func (s *session) next() {
	s.queue = s.queue[1:]
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
	fmt.Fprintf(s.out, "  dir   %s\n", display(r.Cwd))
	fmt.Fprintf(s.out, "  argv  %s\n", argvLine(r.Argv))
	s.printMarks(r.Argv)
	if len(r.Argv) > maxShownArgs || slices.ContainsFunc(shownArgs(r.Argv), isLong) {
		fmt.Fprintln(s.out, "  argv truncated: ? shows all")
	}
	for i, reason := range r.Reasons {
		label := "why "
		if i > 0 {
			label = "    "
		}
		fmt.Fprintf(s.out, "  %s  %s\n", label, display(reason))
	}
	for _, guard := range r.Guards {
		fmt.Fprintf(s.out, "  ⚠ guarded: %s\n", display(guard))
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
	discardTypedAhead(t.in)
	t.fresh = t.read.Load() + 1
	t.asking = p
	fmt.Fprint(t.out, p)
}

// How much of an argv a request's block, and the list of standing
// approvals, show; `?` shows all of a request's.
const (
	maxShownArgs  = 20  // arguments; more are counted, not shown
	maxShownRunes = 200 // characters of one argument; more are counted
)

// isLong reports whether arg is longer than a block shows.
func isLong(arg string) bool { return utf8.RuneCountInString(arg) > maxShownRunes }

// shownArgs returns the arguments of argv a block shows.
func shownArgs(argv []string) []string { return argv[:min(len(argv), maxShownArgs)] }

// argvLine shows argv as a block does: the arguments shownArgs returns,
// each Go-quoted, so none can steer the terminal or hide where it ends,
// and cut (see quoteCut), the arguments left out counted.
func argvLine(argv []string) string {
	shown := shownArgs(argv)
	cut := make([]string, len(shown))
	for i, arg := range shown {
		cut[i] = quoteCut(arg, strconv.Quote)
	}
	line := strings.Join(cut, " ")
	if more := len(argv) - len(shown); more > 0 {
		line += fmt.Sprintf(" …(+%d args)", more)
	}
	return line
}

// printMarks prints a `⚠ non-ASCII` line for every argument argvLine
// shows that holds non-ASCII: the argument as argvLine shows it, and its
// ASCII form, where a look-alike letter shows its code, cut alike.
func (t *term) printMarks(argv []string) {
	for i, arg := range shownArgs(argv) {
		if holdsNonASCII(arg) {
			fmt.Fprintf(t.out, "  ⚠ non-ASCII: argument %d %s is %s\n", i+1, quoteCut(arg, strconv.Quote), quoteCut(arg, strconv.QuoteToASCII))
		}
	}
}

// quoteCut shows arg quoted by quote, cut after maxShownRunes
// characters, the bytes left out counted. The cut falls between two
// characters, so an argument of megabytes shows in a few hundred bytes.
func quoteCut(arg string, quote func(string) string) string {
	n := 0
	for i := range arg {
		if n == maxShownRunes {
			return fmt.Sprintf("%s…(+%d bytes)", quote(arg[:i]), len(arg)-i)
		}
		n++
	}
	return quote(arg)
}

// holdsNonASCII reports whether arg holds a character outside ASCII.
func holdsNonASCII(arg string) bool {
	return strings.IndexFunc(arg, func(r rune) bool { return r > unicode.MaxASCII }) >= 0
}

// showAll prints every argument of r whole, one numbered per line, an
// argument holding non-ASCII with its ASCII form.
func (s *session) showAll(r waiting) {
	for i, arg := range r.Argv {
		line := fmt.Sprintf("  %d  %s", i+1, strconv.Quote(arg))
		if holdsNonASCII(arg) {
			line += "   ⚠ non-ASCII: " + strconv.QuoteToASCII(arg)
		}
		fmt.Fprintln(s.out, line)
	}
}

// display returns text as it may be shown on the terminal: as it is when
// every character in it is printable, Go-quoted otherwise, so control
// characters and terminal escapes appear escaped.
func display(text string) string {
	if !utf8.ValidString(text) || strings.IndexFunc(text, func(r rune) bool { return !strconv.IsPrint(r) }) >= 0 {
		return strconv.Quote(text)
	}
	return text
}

// line is a line the human typed, its number, counting from 1, and
// whether it is the last of what one read returned.
type line struct {
	n    uint64
	text string
	last bool
}

// readLines reads lines from in into the returned channel, counting each
// with read as soon as it is read, and closes the channel when in ends or
// done is closed. It reads in directly, without a buffered reader, and
// counts every line of a read before handing any on: a line is counted
// when it is typed, not when the session gets to it, so prompt can tell
// the lines typed before it. Only a read returning between prompt's count
// and its own counting escapes that.
//
// It reads only once want lets it, after the lines of the last read are
// handled: an editor the session runs meanwhile has the terminal to
// itself.
func readLines(in io.Reader, read *atomic.Uint64, want <-chan struct{}, done <-chan struct{}) <-chan line {
	ch := make(chan line)
	go func() {
		defer close(ch)
		var partial []byte
		buf := make([]byte, 4096)
		for {
			select {
			case <-want:
			case <-done:
				return
			}
			var lines []line
			var err error
			for len(lines) == 0 && err == nil {
				var n int
				n, err = in.Read(buf)
				partial = append(partial, buf[:n]...)
				for {
					i := bytes.IndexByte(partial, '\n')
					if i < 0 {
						break
					}
					lines = append(lines, line{n: read.Add(1), text: string(partial[:i])})
					partial = partial[i+1:]
				}
			}
			if len(lines) > 0 {
				lines[len(lines)-1].last = true
			}
			for _, l := range lines {
				select {
				case ch <- l:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
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

// discardTypedAhead drops the input a terminal holds that nobody has read
// yet. Input that is not a terminal (a pipe, a file) is left alone.
func discardTypedAhead(in io.Reader) {
	f, ok := in.(*os.File)
	if !ok {
		return
	}
	fd := int(f.Fd())
	if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err != nil {
		return
	}
	_ = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
