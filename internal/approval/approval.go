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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// Config describes one approval session.
type Config struct {
	Socket string // the approval socket of the daemon to serve
}

// Prompts and lines the session prints.
const (
	answerPrompt = "answer [r]efuse [o]nce ? "
	notePrompt   = "note for the agent (Enter for none): "
	yesPrompt    = `Type "yes" to run it once: `
)

// Run serves the daemon on cfg.Socket until ctx ends, in ends, or the
// daemon goes away: it shows each approval request on out, ringing the
// terminal bell, and sends the answer the human types on in. It returns an
// error when it cannot serve the daemon.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer) error {
	conn, err := transport.Unix{Path: cfg.Socket}.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	info, err := hello(conn)
	if err != nil {
		return err
	}
	if info.Version != protocol.ApprovalVersion {
		return fmt.Errorf("the daemon speaks approval protocol version %d, this session %d: restart its container", info.Version, protocol.ApprovalVersion)
	}
	if err := serve(conn); err != nil {
		return err
	}
	fmt.Fprintf(out, "serving %s (container %s)\nwaiting for requests\n", info.Workspace, info.ContainerWorkspace)

	done := make(chan struct{})
	defer close(done)
	s := &session{conn: conn, out: out, in: in}
	s.lines = readLines(in, &s.read, done)
	return s.loop(ctx, readDaemon(conn, done))
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

// serve makes the session the one that serves the daemon.
func serve(conn io.ReadWriter) error {
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameServe}); err != nil {
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

// session is a serving session's state: the requests waiting in arrival
// order, the first of which is the one shown, and what it waits for: the
// human to type, or the daemon to take the answer sent.
type session struct {
	conn  io.Writer
	out   io.Writer
	in    io.Reader
	lines <-chan line
	read  atomic.Uint64 // lines read from in so far

	queue []waiting
	// note is set while the human types the note of a refusal.
	note bool
	// confirm is set while the human confirms approving a guarded request.
	confirm bool
	// sent is the outcome of the answer sent for the first request,
	// printed once the daemon has taken it; "" while none is sent.
	sent string
	// fresh is the number of the first line typed after the current
	// prompt; lines typed before it are discarded.
	fresh uint64
}

// waiting is an approval request and when its time runs out.
type waiting struct {
	protocol.ApprovalRequest
	deadline time.Time
}

// loop handles the daemon's frames and the human's lines until one of them
// ends or ctx does.
func (s *session) loop(ctx context.Context, frames <-chan protocol.Frame) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-frames:
			if !ok {
				fmt.Fprintln(s.out, "the daemon ended")
				return nil
			}
			if err := s.daemonFrame(f); err != nil {
				return err
			}
		case l, ok := <-s.lines:
			if !ok {
				return nil
			}
			if l.n >= s.fresh {
				s.typed(l.text)
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
		s.queue = append(s.queue, waiting{r, time.Now().Add(time.Duration(r.TimeoutMs) * time.Millisecond)})
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
		if a.Error != "" {
			fmt.Fprintf(s.out, "request %d: not taken: %s\n", a.ID, a.Error)
		} else {
			fmt.Fprintln(s.out, s.sent)
		}
		s.sent = ""
		s.next()
	default:
		return frameError(f)
	}
	return nil
}

// typed handles a line the human typed after the current prompt. Nothing
// is asked while an answer waits for the daemon.
func (s *session) typed(text string) {
	if len(s.queue) == 0 || s.sent != "" {
		return
	}
	r := s.queue[0]
	text = strings.TrimSpace(text)
	if s.note {
		s.note = false
		s.send(protocol.Answer{ID: r.ID, Note: text}, "refused")
		return
	}
	if s.confirm {
		s.confirm = false
		if text == "yes" {
			s.approveOnce(r.ID)
		} else {
			s.prompt(answerPrompt)
		}
		return
	}
	switch text {
	case "o":
		if len(r.Guards) == 0 {
			s.approveOnce(r.ID)
			return
		}
		// A guarded request runs only once the human has read its guards
		// again and typed "yes"; anything else asks for the answer again.
		for _, guard := range r.Guards {
			fmt.Fprintf(s.out, "⚠ %s\n", display(guard))
		}
		s.confirm = true
		s.prompt(yesPrompt)
	case "r":
		s.note = true
		s.prompt(notePrompt)
	default:
		s.prompt(answerPrompt)
	}
}

// approveOnce approves request id for this request only.
func (s *session) approveOnce(id uint64) {
	s.send(protocol.Answer{ID: id, Scope: protocol.ScopeOnce}, "approved once")
}

// send sends ans to the daemon; outcome is printed once the daemon has
// taken it.
func (s *session) send(ans protocol.Answer, outcome string) {
	s.sent = outcome
	_ = protocol.WriteJSON(s.conn, protocol.FrameAnswer, ans)
}

// next drops the answered request and shows the one after it, if any.
func (s *session) next() {
	s.queue = s.queue[1:]
	if len(s.queue) > 0 {
		s.show()
	}
}

// show prints the first waiting request as a block and asks for the
// answer.
func (s *session) show() {
	r := s.queue[0]
	left := max(time.Until(r.deadline), 0).Round(time.Second)
	head := fmt.Sprintf("── request %d ── %d:%02d left ", r.ID, int(left.Minutes()), int(left.Seconds())%60)
	fmt.Fprintf(s.out, "\n%s%s\n", head, strings.Repeat("─", max(0, 58-len([]rune(head)))))
	fmt.Fprintf(s.out, "  dir   %s\n", display(r.Cwd))
	quoted := make([]string, len(r.Argv))
	for i, arg := range r.Argv {
		quoted[i] = strconv.Quote(arg)
	}
	fmt.Fprintf(s.out, "  argv  %s\n", strings.Join(quoted, " "))
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
	s.prompt(answerPrompt)
}

// prompt prints p and discards what the human typed before it, so a key
// meant for something else never answers a request. The lines counted
// before p is printed are the ones discarded, so an answer typed once p
// shows always counts.
func (s *session) prompt(p string) {
	discardTypedAhead(s.in)
	s.fresh = s.read.Load() + 1
	fmt.Fprint(s.out, p)
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

// line is a line the human typed and its number, counting from 1.
type line struct {
	n    uint64
	text string
}

// readLines reads lines from in into the returned channel, counting each
// with read as soon as it is read, and closes the channel when in ends or
// done is closed. It reads in directly, without a buffered reader, and
// counts every line of a read before handing any on: a line is counted
// when it is typed, not when the session gets to it, so prompt can tell
// the lines typed before it. Only a read returning between prompt's count
// and its own counting escapes that.
func readLines(in io.Reader, read *atomic.Uint64, done <-chan struct{}) <-chan line {
	ch := make(chan line)
	go func() {
		defer close(ch)
		var partial []byte
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			partial = append(partial, buf[:n]...)
			var lines []line
			for {
				i := bytes.IndexByte(partial, '\n')
				if i < 0 {
					break
				}
				lines = append(lines, line{read.Add(1), string(partial[:i])})
				partial = partial[i+1:]
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
