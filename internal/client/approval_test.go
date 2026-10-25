package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// session is an approval session serving a harness's daemon, driven by a
// test as a human would drive it: it reads what the session prints and
// types answers.
type session struct {
	in     *io.PipeWriter
	out    *lockedBuffer
	seen   int                // how much of out the test has read
	ended  chan struct{}      // closed once the session has ended, err set
	err    error              // what Run returned
	cancel context.CancelFunc // stops the session, as Ctrl+C does
}

// startSession starts an approval session for h's daemon and waits until
// it serves the daemon. It ends when the test does.
func (h harness) startSession(t *testing.T) *session {
	t.Helper()
	return startSessionOn(t, h.approvalSocket)
}

// startSessionOn is startSession for the daemon on the approval socket
// socket.
func startSessionOn(t *testing.T, socket string) *session {
	t.Helper()
	return startSessionWith(t, approval.Config{Socket: socket})
}

// startSessionWith is startSession as cfg configures it; without a socket
// it returns at once, the session listing the daemons.
func startSessionWith(t *testing.T, cfg approval.Config) *session {
	t.Helper()
	s := launchSession(t, cfg)
	if cfg.Socket != "" {
		s.waitFor(t, "waiting for requests")
	}
	return s
}

// launchSession starts an approval session as cfg configures it and
// returns at once. It ends when the test does.
func launchSession(t *testing.T, cfg approval.Config) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	s := &session{in: pw, out: &lockedBuffer{}, ended: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.ended)
		s.err = approval.Run(ctx, cfg, pr, s.out)
	}()
	t.Cleanup(func() {
		cancel()
		pw.Close()
		select {
		case <-s.ended:
		case <-time.After(5 * time.Second):
			t.Error("the approval session did not end")
		}
	})
	return s
}

// waitFor waits until the session prints text after what the test has
// read, and returns what it printed up to the end of text.
func (s *session) waitFor(t *testing.T, text string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := s.out.String()
		if i := strings.Index(out[s.seen:], text); i >= 0 {
			got := out[s.seen : s.seen+i+len(text)]
			s.seen += i + len(text)
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session did not print %q; it printed %q", text, out[s.seen:])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// answer types line and Enter.
func (s *session) answer(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// ask runs hostrun --ask argv in the background; the result arrives on the
// channel.
func (h harness) ask(t *testing.T, argv ...string) <-chan result {
	t.Helper()
	ch := make(chan result, 1)
	go func() { ch <- h.run(t, containerRoot, "", append([]string{"--ask"}, argv...)...) }()
	return ch
}

// awaitResult waits for a hostrun started with ask.
func awaitResult(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("hostrun --ask did not finish")
		return result{}
	}
}

const waitingNoRule = "hostrun: waiting for approval on the host: no rule allows \"touch marker\"\n"

func TestAskRunsACommandApprovedOnce(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	block := s.waitFor(t, answerPromptText)
	for _, want := range []string{"\a", "── request 1 ──", "dir   " + containerRoot + "\n", `argv  "touch" "marker"`, `why   no rule allows "touch marker"`} {
		if !strings.Contains(block, want) {
			t.Errorf("the request block %q lacks %q", block, want)
		}
	}
	s.answer(t, "o")
	got := awaitResult(t, ch)
	want := result{stderr: waitingNoRule + "hostrun: approved on the host: this request only\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(h.hostRoot, "marker")); err != nil {
		t.Fatalf("the approved command did not run: %v", err)
	}
	s.waitFor(t, "approved once")
}

func TestAskReportsAHumansRefusal(t *testing.T) {
	for _, tt := range []struct{ name, note, want string }{
		{"with a note", "use echo", "hostrun: refused on the host: use echo\n"},
		{"without a note", "", "hostrun: refused on the host\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, echoOnly))
			s := h.startSession(t)
			ch := h.ask(t, "touch", "marker")
			s.waitFor(t, answerPromptText)
			s.answer(t, "r")
			s.waitFor(t, "note for the agent (Enter for none): ")
			s.answer(t, tt.note)
			got := awaitResult(t, ch)
			if want := (result{code: protocol.ExitRejected, stderr: waitingNoRule + tt.want}); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			assertNotRun(t, h, "marker")
			s.waitFor(t, "refused")
		})
	}
}

// An invalid line asks again, so a stray key never answers a request.
func TestAskSessionAsksAgainOnAnInvalidAnswer(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "x")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the approved command's result", got)
	}
}

func TestAskWithoutASessionIsRefusedAtOnce(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	got := h.run(t, containerRoot, "", "--ask", "touch", "marker")
	want := result{code: protocol.ExitRejected, stderr: "hostrun: not approved: no approval session serves this devcontainer\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertNotRun(t, h, "marker")
}

func TestAskRunsAnAllowedCommandWithoutAsking(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	if got := h.run(t, containerRoot, "", "--ask", "echo", "hi"); got != (result{stdout: "hi\n"}) {
		t.Fatalf("got %+v, want the allowed command's output alone", got)
	}
	if out := s.out.String()[s.seen:]; strings.Contains(out, "request") {
		t.Fatalf("the session was asked about an allowed command: %q", out)
	}
}

// A request that could not run anyway is refused as without --ask, and
// never reaches the human.
func TestAskRefusesWhatCannotBeApprovedWithoutAsking(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	tests := []struct {
		name string
		cwd  string
		argv []string
		want result
	}{
		{"program not found", containerRoot, []string{"no-such-program-hostrunner"},
			result{code: protocol.ExitNotFound, stderr: "hostrun: command no-such-program-hostrunner not found on the host\n"}},
		{"directory outside the workspace", "/etc", []string{"touch", "marker"},
			result{code: protocol.ExitRejected, stderr: "hostrun: working directory /etc is outside the workspace\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.run(t, tt.cwd, "", append([]string{"--ask"}, tt.argv...)...)
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if out := s.out.String()[s.seen:]; strings.Contains(out, "request") {
		t.Fatalf("the session was asked about a command that cannot run: %q", out)
	}
}

// A refusal by a matching rule's allow list is approvable, and not guarded:
// the rule's name leads the reasons.
func TestAskAsksForARefusalByAMatchingRule(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "sh", "-x", "script")
	block := s.waitFor(t, answerPromptText)
	if want := "why   denied by rule \"sh\": flag -x is not allowed\n"; !strings.Contains(block, want) {
		t.Errorf("the block %q lacks %q", block, want)
	}
	if strings.Contains(block, "guarded") {
		t.Errorf("an allow-list refusal is shown guarded: %q", block)
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	got := awaitResult(t, ch)
	want := result{code: protocol.ExitRejected, stderr: "hostrun: waiting for approval on the host: denied by rule \"sh\": flag -x is not allowed\nhostrun: refused on the host\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// The container's socket takes no session frames, and the approval
// socket no container frames, so the container cannot answer its own
// requests even if it reached the approval socket.
func TestSocketsRefuseEachOthersFrames(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	tests := []struct {
		name   string
		socket string
		frame  protocol.FrameType
		v      any
	}{
		{"hello on the container's socket", h.transport.Path, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}},
		{"answer on the container's socket", h.transport.Path, protocol.FrameAnswer, protocol.Answer{ID: 1, Scope: protocol.ScopeOnce}},
		{"request on the approval socket", h.approvalSocket, protocol.FrameRequest, protocol.Request{Version: protocol.Version, Argv: []string{"echo"}, Cwd: containerRoot}},
		{"ask on the approval socket", h.approvalSocket, protocol.FrameAsk, protocol.Ask{Request: protocol.Request{Version: protocol.Version, Argv: []string{"echo"}, Cwd: containerRoot}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := transport.Unix{Path: tt.socket}.Dial(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := protocol.WriteJSON(conn, tt.frame, tt.v); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			f, err := protocol.ReadFrame(conn)
			if err != nil {
				t.Fatalf("no answer: %v", err)
			}
			var e protocol.Error
			if f.Type != protocol.FrameError || protocol.DecodeJSON(f, &e) != nil || e.Code != protocol.ExitHostrunError {
				t.Fatalf("got frame %d %q, want an error with code %d", f.Type, f.Payload, protocol.ExitHostrunError)
			}
		})
	}
}

// A daemon older than --ask rejects the frame type it does not know with
// 125 and runs nothing; hostrun exits with that.
func TestAskAgainstAnOlderDaemonFailsWith125(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sock")
	l, err := transport.Unix{Path: path}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	opened := make(chan protocol.FrameType, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var header [5]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		opened <- protocol.FrameType(header[0])
		// What protocol.ReadFrame of an older daemon answers a type past
		// its last one with.
		_ = protocol.WriteJSON(conn, protocol.FrameError, protocol.Error{Code: protocol.ExitHostrunError, Message: "protocol: unknown frame type 13"})
	}()
	var stdout, stderr strings.Builder
	code := client.Run(context.Background(), transport.Unix{Path: path}, []string{"--ask", "touch", "marker"}, containerRoot, strings.NewReader(""), &stdout, &stderr)
	if code != protocol.ExitHostrunError || stderr.String() != "hostrun: protocol: unknown frame type 13\n" {
		t.Fatalf("exit code %d, stderr %q; want 125 and the daemon's error", code, stderr.String())
	}
	// The daemon this release replaces knew frame types up to FrameAllowed.
	if got := <-opened; got <= protocol.FrameAllowed {
		t.Fatalf("--ask opened with frame type %d, which an older daemon knows", got)
	}
}

// The daemon log records the request, the answer and the run it allowed.
func TestAskIsLogged(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	awaitResult(t, ch)
	for _, want := range []string{
		// The session runs in this process, which the kernel names.
		"msg=\"approval session serving\" pid=" + strconv.Itoa(os.Getpid()) + "\n",
		"msg=\"approval requested\" approval_id=1 argv=\"[touch marker]\"",
		"msg=\"approval answered\" approval_id=1 scope=once",
		"approved=once approval_id=1",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, log.String())
		}
	}
}

// A session that cannot reach the daemon fails with the reason.
func TestSessionWithoutADaemonFails(t *testing.T) {
	err := approval.Run(context.Background(), approval.Config{Socket: filepath.Join(t.TempDir(), "none.approval.sock")}, strings.NewReader(""), io.Discard)
	var dialErr *net.OpError
	if err == nil || !errors.As(err, &dialErr) {
		t.Fatalf("got %v, want the dial error", err)
	}
}

// A request too large to show the human is refused at once, rather than
// lost on the way to the session while the agent waits.
func TestAskTooLargeToShowIsRefused(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSession(t)
	got := awaitResult(t, h.ask(t, "touch", strings.Repeat("x", 700*1024)))
	want := result{code: protocol.ExitRejected, stderr: "hostrun: not approved: the request is too large to show\n"}
	if got != want {
		t.Fatalf("got code %d, stderr %q; want %+v", got.code, got.stderr, want)
	}
}

// serveRaw makes a connection to h's approval socket the serving session,
// as a session that then stops reading would.
func (h harness) serveRaw(t *testing.T) net.Conn {
	t.Helper()
	conn, err := transport.Unix{Path: h.approvalSocket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameDaemonInfo {
		t.Fatalf("hello: frame %v, err %v", f.Type, err)
	}
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameServe}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameServing {
		t.Fatalf("serve: frame %v, err %v", f.Type, err)
	}
	return conn
}

// A session that stops reading (suspended, its terminal stopped) holds up
// neither other sessions nor its daemon: the daemon gives up on it and
// takes it for gone.
func TestAStalledSessionIsDropped(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.serveRaw(t) // never read again
	// Large enough to fill the socket's buffer, small enough to show.
	ch := h.ask(t, "touch", strings.Repeat("x", 400*1024))
	time.Sleep(100 * time.Millisecond) // the request is being written
	conn, err := transport.Unix{Path: h.approvalSocket}.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.FrameHello, protocol.Hello{Version: protocol.ApprovalVersion}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameDaemonInfo {
		t.Fatalf("a hello while a session stalls: frame %v, err %v", f.Type, err)
	}
	got := awaitResult(t, ch)
	if want := (result{code: protocol.ExitRejected, stderr: "hostrun: not approved: no approval session serves this devcontainer\n"}); got != want {
		t.Fatalf("got code %d, stderr %q; want %+v", got.code, got.stderr, want)
	}
}

// A request whose agent went away is withdrawn: the session announces it
// and drops it, and a later answer reaches nothing.
func TestAskWithdrawnWhenTheClientGoesAway(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- client.Run(ctx, h.transport, []string{"--ask", "touch", "marker"}, containerRoot, strings.NewReader(""), io.Discard, io.Discard)
	}()
	s.waitFor(t, answerPromptText)
	cancel()
	if code := <-done; code != client.ExitInterrupted {
		t.Fatalf("exit code %d, want %d", code, client.ExitInterrupted)
	}
	waitForLog(t, &log, `msg="approval not answered" approval_id=1 reason=withdrawn`)
	s.waitFor(t, "request 1 withdrawn\n")
	s.answer(t, "o")
	s.waitFor(t, `unknown command "o"`)
	if out := s.out.String(); strings.Contains(out, "approved once") {
		t.Fatalf("the session answered a withdrawn request:\n%s", out)
	}
	assertNotRun(t, h, "marker")
}

// waitForLog waits until the daemon log holds text.
func waitForLog(t *testing.T, log *lockedBuffer, text string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(log.String(), text); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log lacks %q:\n%s", text, log.String())
		}
	}
}

// What the human typed before a prompt was shown never answers it: not a
// line typed while no request waited, nor one typed with the answer before
// the note's prompt.
func TestAskSessionDiscardsInputTypedBeforeThePrompt(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	s.answer(t, "o") // nothing waits yet
	time.Sleep(50 * time.Millisecond)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	if _, err := io.WriteString(s.in, "r\nfoo\n"); err != nil { // foo typed before the note's prompt
		t.Fatal(err)
	}
	s.waitFor(t, notePromptText)
	time.Sleep(50 * time.Millisecond)
	s.answer(t, "use echo")
	got := awaitResult(t, ch)
	if want := (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: refused on the host: use echo\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// The prompts the session asks for an answer, to a request and to a
// guarded one, and for a refusal's note with.
const (
	answerPromptText  = "answer [r]efuse [o]nce [l]ifetime [a]dd rule [?]more ? "
	guardedPromptText = "answer [r]efuse [o]nce [l]ifetime [?]more ? " // a guarded request gets no rule
	notePromptText    = "note for the agent (Enter for none): "
)

// What came from the container is shown escaped, so an argument or a
// directory name cannot steer the human's terminal.
func TestAskSessionShowsTerminalEscapesEscaped(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	if err := os.Mkdir(filepath.Join(h.hostRoot, "d\x1b[2J"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := make(chan result, 1)
	go func() { ch <- h.run(t, containerRoot+"/d\x1b[2J", "", "--ask", "touch", "\x1b]0;owned\a") }()
	block := s.waitFor(t, answerPromptText)
	for _, want := range []string{`dir   "/workspaces/app/d\x1b[2J"`, `argv  "touch" "\x1b]0;owned\a"`} {
		if !strings.Contains(block, want) {
			t.Errorf("the block %q lacks %q", block, want)
		}
	}
	if strings.Contains(block, "\x1b") {
		t.Errorf("the block holds a raw escape: %q", block)
	}
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, "")
	awaitResult(t, ch)
}

// takeOverPromptText is what a session given a served daemon asks.
const takeOverPromptText = "another approval session serves this daemon\nTake over? [y/N] "

// A second session given a served daemon asks whether to take it over:
// "y" or "yes" takes it over; anything else, input ending or Ctrl+C
// leaves the first serving it and ends the second.
func TestASecondSessionAsksToTakeOver(t *testing.T) {
	for _, tt := range []struct {
		name       string
		typed      string
		closeInput bool // input ends instead
		interrupt  bool // Ctrl+C instead
		takesOver  bool
	}{
		{name: "y", typed: "y", takesOver: true},
		{name: "yes", typed: "yes", takesOver: true},
		{name: "n", typed: "n"},
		{name: "Enter", typed: ""},
		{name: "input ends", closeInput: true},
		{name: "Ctrl+C", interrupt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, echoOnly))
			first := h.startSession(t)
			second := launchSession(t, approval.Config{Socket: h.approvalSocket})
			second.waitFor(t, takeOverPromptText)
			switch {
			case tt.closeInput:
				second.in.Close()
			case tt.interrupt:
				second.cancel()
			default:
				second.answer(t, tt.typed)
			}
			if tt.takesOver {
				first.waitFor(t, "taken over by another session\n")
				second.waitFor(t, "serving "+h.hostRoot)
				return
			}
			waitEnded(t, second)
			if second.err == nil || second.err.Error() != "another approval session serves this daemon" {
				t.Fatalf("got %v, want the second session refused", second.err)
			}
			if out := first.out.String(); strings.Contains(out, "taken over") {
				t.Fatalf("the first session was taken over: %q", out)
			}
		})
	}
}

// A session given a served daemon that takes it over gets the requests
// waiting, each with the rest of its limit.
func TestADirSessionTakingOverPassesTheRequestsOn(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	first := h.startSession(t)
	ch := h.askFor(t, "10m", "touch", "marker")
	first.waitFor(t, answerPromptText)
	// Past a second, the rest of the limit shows under 10:00.
	time.Sleep(1100 * time.Millisecond)
	second := launchSession(t, approval.Config{Socket: h.approvalSocket})
	second.waitFor(t, takeOverPromptText)
	second.answer(t, "y")
	if block := second.waitFor(t, answerPromptText); !strings.Contains(block, "── request 1 ── 9:5") {
		t.Fatalf("the moved request %q, want it with the rest of its limit", block)
	}
	second.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved by the new session", got)
	}
}

// A daemon that ends while the session asks whether to take it over is
// waited for, as one that ends while served: "y" finds no daemon.
func TestADirSessionWaitsWhenTheDaemonEndsAtTheQuestion(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSession(t)
	second := launchSession(t, approval.Config{Socket: h.approvalSocket})
	second.waitFor(t, takeOverPromptText)
	h.stop()
	second.answer(t, "y")
	second.waitFor(t, "the daemon ended: waiting for the next one\n")
}

// A daemon the other session left while the session asked is served
// even on "no": there is nothing to take over.
func TestADirSessionServesADaemonLeftWhileAsking(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	first := h.startSession(t)
	second := launchSession(t, approval.Config{Socket: h.approvalSocket})
	second.waitFor(t, takeOverPromptText)
	first.answer(t, "quit")
	waitEnded(t, first)
	second.answer(t, "n")
	second.waitFor(t, "serving "+h.hostRoot)
}

// A session given its daemon that waits for the next one asks again when
// another session serves the daemon that comes.
func TestADirSessionAsksAgainWhenItsNextDaemonIsServed(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	s := startSessionOn(t, h.approvalSocket)
	h.stop()
	s.waitFor(t, "the daemon ended: waiting for the next one\n")
	next := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	startSessionOn(t, next.approvalSocket)
	s.waitForWithin(t, takeOverPromptText, listWait)
}

// The daemon honours the client's limit on the wait.
func TestAskHonoursTheClientsLimit(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.serveRaw(t)
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ask := protocol.Ask{Request: protocol.Request{Version: protocol.Version, Argv: []string{"touch", "marker"}, Cwd: containerRoot}, TimeoutMs: 200}
	if err := protocol.WriteJSON(conn, protocol.FrameAsk, ask); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameWaiting {
		t.Fatalf("frame %v, err %v; want waiting", f.Type, err)
	}
	f, err := protocol.ReadFrame(conn)
	var e protocol.Error
	if err != nil || f.Type != protocol.FrameError || protocol.DecodeJSON(f, &e) != nil || e.Message != "not approved: nobody answered on the host" {
		t.Fatalf("frame %v %q, err %v; want nobody answered", f.Type, f.Payload, err)
	}
}

// The daemon log records a request no session could take, and a session
// leaving.
func TestAskWithoutASessionAndASessionLeavingAreLogged(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	h.run(t, containerRoot, "", "--ask", "touch", "marker")
	waitForLog(t, &log, `msg="approval not answered" reason="no session" argv="[touch marker]"`)
	conn := h.serveRaw(t)
	conn.Close()
	waitForLog(t, &log, `msg="approval session left" pid=`)
}

// guardedRules refuse each kind of guarded refusal: a deny list, a path
// check, an argv the rules cannot read, and a rule with secrets.
const guardedRules = `
rules:
  - command: touch
    positional:
      deny: ["+*"]
  - command: cat
    positional:
      path: open
  - command: env
    flags:
      values:
        --unset: {}
        --unset-other: {}
  - command: printf
    secrets:
      token: {env: TOKEN}
    inject:
      env: {API_TOKEN: "${token}"}
    flags:
      allow: []
    positional:
      allow: ["ok"]
`

// A guarded refusal is marked as such, and approving it needs "yes" after a
// warning naming why; anything else returns to the answer prompt.
func TestAskGuardedRefusalsNeedYes(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	tests := []struct {
		name  string
		argv  []string
		guard string
	}{
		{"explicitly denied", []string{"touch", "+marker"}, `explicitly denied: argument "+marker" is denied`},
		{"path check", []string{"cat", "/etc/passwd"}, `path check: argument "/etc/passwd" is not a workspace file: it is an absolute path`},
		{"unreadable argv", []string{"env", "--unse", "x"}, `unreadable argv: flag --unse is ambiguous`},
		{"secrets", []string{"printf", "other"}, `injects secrets: token`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, guardedRules))
			s := h.startSession(t)
			ch := h.ask(t, tt.argv...)
			block := s.waitFor(t, guardedPromptText)
			if want := "⚠ guarded: " + tt.guard + "\n"; !strings.Contains(block, want) {
				t.Errorf("the block %q lacks %q", block, want)
			}
			s.answer(t, "o")
			warning := s.waitFor(t, `Type "yes" to run it once: `)
			if !strings.Contains(warning, "⚠ "+tt.guard+"\n") {
				t.Errorf("the warning %q lacks %q", warning, tt.guard)
			}
			s.answer(t, "no")
			s.waitFor(t, guardedPromptText)
			s.answer(t, "r")
			s.waitFor(t, notePromptText)
			s.answer(t, "")
			got := awaitResult(t, ch)
			if got.code != protocol.ExitRejected || !strings.HasPrefix(got.stderr, "hostrun: waiting for approval on the host (guarded): ") {
				t.Fatalf("got %+v, want a guarded wait and a refusal", got)
			}
		})
	}
}

// Typing "yes" approves a guarded refusal; the log records it guarded.
func TestAskApprovesAGuardedRefusalWithYes(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, guardedRules))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "+marker")
	s.waitFor(t, guardedPromptText)
	s.answer(t, "o")
	s.waitFor(t, `Type "yes" to run it once: `)
	s.answer(t, "yes")
	got := awaitResult(t, ch)
	want := result{stderr: "hostrun: waiting for approval on the host (guarded): denied by rule \"touch\": argument \"+marker\" is denied\nhostrun: approved on the host: this request only\n"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(h.hostRoot, "+marker")); err != nil {
		t.Fatalf("the approved command did not run: %v", err)
	}
	waitForLog(t, &log, "guarded=true")
}

// approveYes approves the shown request once, through its warning.
func (s *session) approveYes(t *testing.T) {
	t.Helper()
	s.waitFor(t, guardedPromptText)
	s.answer(t, "o")
	s.waitFor(t, `Type "yes" to run it once: `)
	s.answer(t, "yes")
}

// An approved command a rule refused runs as the rule's allowed commands
// do: in its fixed directory, with its injection, its secrets masked in
// the output.
func TestAskRunsAnApprovedCommandAsItsRuleWould(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	dir := fixedDir(t)
	h := startDaemon(t, policy(t, `
rules:
  - command: sh
    dir: `+dir+`
    secrets:
      token: {env: TOKEN}
    inject:
      env: {API_TOKEN: "${token}"}
    flags:
      allow: [-c]
    positional:
      allow: ["true"]
`))
	s := h.startSession(t)
	ch := h.ask(t, "sh", "-c", `pwd; echo "$API_TOKEN"`)
	s.approveYes(t)
	got := awaitResult(t, ch)
	if want := dir + "\n***\n"; got.code != 0 || got.stdout != want {
		t.Fatalf("got %+v, want stdout %q", got, want)
	}
}

// Arguments that pass their path check are handed over as the files the
// check opened; one approved after failing it is passed as given.
func TestAskHandsPassingPathsOverAndFailingOnesAsGiven(t *testing.T) {
	h := startDaemon(t, policy(t, `
rules:
  - command: echo
    positional:
      path: open
`))
	if err := os.WriteFile(filepath.Join(h.hostRoot, "inside"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	s := h.startSession(t)
	ch := h.ask(t, "echo", "inside", outside)
	s.approveYes(t)
	got := awaitResult(t, ch)
	if want := "/proc/self/fd/3 " + outside + "\n"; got.code != 0 || got.stdout != want {
		t.Fatalf("got %+v, want stdout %q", got, want)
	}
}

// An argument its list refuses is path-checked too: one that fails the
// check makes the request guarded.
func TestAskPathChecksArgumentsTheListsRefuse(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{"positional", []string{"cat", "/etc/passwd"}},
		{"flag value", []string{"cat", "--file", "/etc/passwd"}},
		{"go positional", []string{"tail", "/etc/passwd"}},
		{"go flag value", []string{"tail", "-file", "/etc/passwd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, `
rules:
  - command: cat
    flags:
      values:
        --file: {allow: ["foo"], path: open}
    positional:
      allow: ["foo"]
      path: open
  - command: tail
    flag_style: go
    flags:
      values:
        -file: {allow: ["foo"], path: open}
    positional:
      allow: ["foo"]
      path: open
`))
			s := h.startSession(t)
			ch := h.ask(t, tt.argv...)
			block := s.waitFor(t, guardedPromptText)
			if want := `⚠ guarded: path check: argument "/etc/passwd" is not a workspace file: it is an absolute path` + "\n"; !strings.Contains(block, want) {
				t.Errorf("the block %q lacks %q", block, want)
			}
			s.answer(t, "r")
			s.waitFor(t, notePromptText)
			s.answer(t, "")
			awaitResult(t, ch)
		})
	}
}

// An argument that passed its path check when the human was asked, and
// fails it when the approved command starts, refuses the command: only
// the failures the human saw are passed as given.
func TestAskRefusesAPathThatFailsOnlyOnceApproved(t *testing.T) {
	h := startDaemon(t, policy(t, `
rules:
  - command: cat
    flags:
      allow: []
    positional:
      path: open
`))
	inside := filepath.Join(h.hostRoot, "inside")
	if err := os.WriteFile(inside, []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSession(t)
	ch := h.ask(t, "cat", "-n", "inside")
	s.waitFor(t, answerPromptText)
	if err := os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	s.answer(t, "o")
	got := awaitResult(t, ch)
	if got.code != protocol.ExitRejected || !strings.Contains(got.stderr, `hostrun: argument "inside" is not a workspace file`) || strings.Contains(got.stdout, "outside") {
		t.Fatalf("got %+v, want the path check to refuse it", got)
	}
}

// While a session serves the daemon, a refusal --ask would ask a human
// about hints at it, marked guarded when any of its reasons is; a dry run
// reports what the run gets.
func TestRefusalsHintAtAskWhileASessionServes(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	const ask = " (ask for an approval: hostrun --ask …)\n"
	const guarded = " (ask for an approval: hostrun --ask …; guarded: needs a second confirmation)\n"
	tests := []struct {
		name   string
		rules  string
		argv   []string
		marker string // what the command would create in the workspace; "" if nothing
		stderr string
	}{
		{"no rule", echoOnly, []string{"touch", "marker"}, "marker", `hostrun: no rule allows "touch marker"` + ask},
		{"guarded by a later reason", `
rules:
  - command: touch
    flags:
      allow: []
    positional:
      deny: ["+*"]
`, []string{"touch", "-a", "+marker"}, "+marker", `hostrun: denied by rule "touch": flag -a is not allowed` + guarded},
		{"path check", guardedRules, []string{"cat", "/etc/passwd"}, "", `hostrun: argument "/etc/passwd" is not a workspace file: it is an absolute path` + guarded},
		{"rule with secrets", guardedRules, []string{"printf", "other"}, "", `hostrun: denied by rule "printf": argument "other" is not allowed` + guarded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startDaemon(t, policy(t, tt.rules))
			h.startSession(t)
			want := result{code: protocol.ExitRejected, stderr: tt.stderr}
			for _, mode := range [][]string{nil, {"--dry-run"}} {
				if got := h.run(t, containerRoot, "", append(mode, tt.argv...)...); got != want {
					t.Errorf("%v: got %+v, want %+v", mode, got, want)
				}
			}
			if tt.marker != "" {
				assertNotRun(t, h, tt.marker)
			}
		})
	}
}

// Without a session nothing can be approved, so nothing hints at --ask.
func TestRefusalsDoNotHintWithoutASession(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	want := result{code: protocol.ExitRejected, stderr: "hostrun: no rule allows \"touch marker\"\n"}
	if got := h.run(t, containerRoot, "", "touch", "marker"); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A refusal --ask would refuse anyway, before asking anyone, does not hint
// at it, in a run or a dry run.
func TestRefusalsAskWouldRefuseAnywayDoNotHint(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly+`
  - command: touch
    dir: /no-such-dir-hostrunner
    flags:
      allow: []
  - command: printf
    secrets:
      token: {env: HOSTRUNNER_TEST_UNSET_TOKEN}
    inject:
      env: {API_TOKEN: "${token}"}
    flags:
      allow: []
    positional:
      allow: ["ok"]
`))
	h.startSession(t)
	tests := []struct {
		name string
		cwd  string
		argv []string
		want string
	}{
		{"program not found", containerRoot, []string{"no-such-program-hostrunner"}, "hostrun: no rule allows \"no-such-program-hostrunner\"\n"},
		{"directory outside the workspace", "/etc", []string{"sh", "-x"}, "hostrun: denied by rule \"sh\": flag -x is not allowed\n"},
		{"rule's dir missing", containerRoot, []string{"touch", "-a", "x"}, "hostrun: denied by rule \"touch\": flag -a is not allowed\n"},
		{"secret unavailable", containerRoot, []string{"printf", "-v"}, "hostrun: denied by rule \"printf\": flag -v is not allowed\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := result{code: protocol.ExitRejected, stderr: tt.want}
			for _, mode := range [][]string{nil, {"--dry-run"}} {
				if got := h.run(t, tt.cwd, "", append(mode, tt.argv...)...); got != want {
					t.Errorf("%v: got %+v, want %+v", mode, got, want)
				}
			}
		})
	}
}

// askFor runs hostrun --ask=limit argv in the background, as ask does.
func (h harness) askFor(t *testing.T, limit string, argv ...string) <-chan result {
	t.Helper()
	ch := make(chan result, 1)
	go func() { ch <- h.run(t, containerRoot, "", append([]string{"--ask=" + limit}, argv...)...) }()
	return ch
}

// A limit that is not a positive Go duration fails before hostrun
// connects: no daemon listens on the socket it is given.
func TestAskLimitMustBeAPositiveDuration(t *testing.T) {
	nowhere := transport.Unix{Path: filepath.Join(t.TempDir(), "none.sock")}
	for _, limit := range []string{"abc", "", "0s", "-1m", "10"} {
		var stderr strings.Builder
		code := client.Run(context.Background(), nowhere, []string{"--ask=" + limit, "touch", "marker"}, containerRoot, strings.NewReader(""), io.Discard, &stderr)
		want := "hostrun: --ask limit " + strconv.Quote(limit) + " is not a positive duration\n"
		if code != protocol.ExitHostrunError || stderr.String() != want {
			t.Errorf("%q: code %d, stderr %q; want %d and %q", limit, code, stderr.String(), protocol.ExitHostrunError, want)
		}
	}
}

// The daemon refuses a limit over 1 hour without asking anyone; 1 hour
// itself is taken.
func TestAskLimitOverAnHourIsRefused(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	want := result{code: protocol.ExitHostrunError, stderr: "hostrun: --ask limit 1h0m1s exceeds 1h0m0s\n"}
	if got := h.run(t, containerRoot, "", "--ask=1h0m1s", "touch", "marker"); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if out := s.out.String()[s.seen:]; strings.Contains(out, "request") {
		t.Fatalf("the session was asked: %q", out)
	}
	ch := h.askFor(t, "1h", "touch", "marker")
	s.waitFor(t, "── request 1 ── 60:00 left")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the approved command's result", got)
	}
}

// A request nobody answers within its limit is refused, announced in the
// session, which drops it, and logged.
func TestAskExpiresAtItsLimit(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.askFor(t, "300ms", "touch", "marker")
	s.waitFor(t, answerPromptText)
	got := awaitResult(t, ch)
	if want := (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: not approved: nobody answered on the host\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	s.waitFor(t, "request 1 expired\n")
	waitForLog(t, &log, `msg="approval not answered" approval_id=1 reason=expired`)
	assertNotRun(t, h, "marker")
}

// Requests are shown one at a time, first in, first out: a new one rings
// the bell, prints nothing in the middle of the prompt, and is counted at
// the next block.
func TestAskSessionShowsRequestsInTurn(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	first := h.ask(t, "touch", "first")
	block := s.waitFor(t, answerPromptText)
	if strings.Contains(block, "more waiting") {
		t.Errorf("the first block counts others: %q", block)
	}
	second := h.ask(t, "touch", "second")
	s.waitFor(t, "\a")
	third := h.ask(t, "touch", "third")
	s.waitFor(t, "\a")
	time.Sleep(50 * time.Millisecond)
	if out := s.out.String()[s.seen:]; out != "" {
		t.Fatalf("the session printed in the middle of the prompt: %q", out)
	}
	s.answer(t, "o")
	if got := awaitResult(t, first); got.code != 0 {
		t.Fatalf("first: got %+v", got)
	}
	block = s.waitFor(t, answerPromptText)
	for _, want := range []string{"── request 2 ──", `"touch" "second"`, "  (1 more waiting)\n"} {
		if !strings.Contains(block, want) {
			t.Errorf("the second block %q lacks %q", block, want)
		}
	}
	s.answer(t, "o")
	awaitResult(t, second)
	block = s.waitFor(t, answerPromptText)
	if !strings.Contains(block, `"touch" "third"`) || strings.Contains(block, "more waiting") {
		t.Errorf("the third block %q", block)
	}
	s.answer(t, "o")
	awaitResult(t, third)
}

// A queued request that expires drops out silently; a current one that is
// withdrawn is announced, what was typed for it is discarded, and the
// next is shown.
func TestAskSessionDropsRequestsThatEnd(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := make(chan int, 1)
	go func() {
		current <- client.Run(ctx, h.transport, []string{"--ask", "touch", "current"}, containerRoot, strings.NewReader(""), io.Discard, io.Discard)
	}()
	s.waitFor(t, answerPromptText)
	expiring := h.askFor(t, "200ms", "touch", "expiring")
	s.waitFor(t, "\a")
	if got := awaitResult(t, expiring); got.code != protocol.ExitRejected {
		t.Fatalf("expiring: got %+v", got)
	}
	time.Sleep(50 * time.Millisecond)
	if out := s.out.String()[s.seen:]; out != "" {
		t.Fatalf("a queued request that expired was announced: %q", out)
	}
	last := h.ask(t, "touch", "last")
	s.waitFor(t, "\a")
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	cancel()
	<-current
	s.waitFor(t, "request 1 withdrawn\n")
	block := s.waitFor(t, answerPromptText)
	if !strings.Contains(block, "── request 3 ──") || strings.Contains(block, "request 2") || strings.Contains(block, "more waiting") {
		t.Fatalf("after the withdrawn request the session showed %q, want request 3 alone", block)
	}
	// The note's prompt went with request 1: "o" answers request 3.
	s.answer(t, "o")
	if got := awaitResult(t, last); got.code != 0 {
		t.Fatalf("last: got %+v, want it approved", got)
	}
	assertNotRun(t, h, "current")
	assertNotRun(t, h, "expiring")
}

// A session that leaves refuses every request still waiting, says how
// many, and the daemon logs why.
func TestASessionLeavingRefusesWaitingRequests(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	first := h.ask(t, "touch", "first")
	s.waitFor(t, answerPromptText)
	second := h.ask(t, "touch", "second")
	s.waitFor(t, "\a")
	s.in.Close()
	s.waitFor(t, "refused 2 waiting requests\n")
	for _, ch := range []<-chan result{first, second} {
		if got := awaitResult(t, ch); got.code != protocol.ExitRejected || !strings.HasSuffix(got.stderr, "hostrun: refused on the host\n") {
			t.Errorf("got %+v, want a refusal", got)
		}
	}
	waitForLog(t, &log, `msg="approval not answered" approval_id=1 reason="session left"`)
	waitForLog(t, &log, `msg="approval not answered" approval_id=2 reason="session left"`)
}

// A daemon that stops answers every waiting request before it closes.
func TestAStoppingDaemonAnswersWaitingRequests(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	h.stop()
	got := awaitResult(t, ch)
	if want := (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: not approved: the hostrunner daemon is stopping\n"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	waitForLog(t, &log, `msg="approval not answered" approval_id=1 reason="daemon stopping"`)
}

// Ctrl+C in the session refuses the requests still waiting, as leaving
// does, and says how many.
func TestASessionInterruptedRefusesWaitingRequests(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.cancel()
	s.waitFor(t, "refused 1 waiting request\n")
	if got := awaitResult(t, ch); got != (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: refused on the host\n"}) {
		t.Fatalf("got %+v, want a refusal", got)
	}
}

// A daemon that steps aside for a new one answers every waiting request
// before it closes, as a stopping one does.
func TestADaemonSteppingAsideAnswersWaitingRequests(t *testing.T) {
	h := startDaemonWith(t, slog.New(slog.DiscardHandler), policy(t, echoOnly), daemon.WithArmHandler(func(protocol.Arm) bool { return true }))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	conn, err := h.transport.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.FrameArm, protocol.Arm{Version: protocol.Version}); err != nil {
		t.Fatal(err)
	}
	if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameArmed {
		t.Fatalf("frame %v, err %v; want armed", f.Type, err)
	}
	if got := awaitResult(t, ch); got != (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: not approved: the hostrunner daemon is stopping\n"}) {
		t.Fatalf("got %+v, want the daemon stopping", got)
	}
}

// A limit under a millisecond is a limit of a millisecond, not the
// daemon's default.
func TestAskLimitUnderAMillisecondExpiresAtOnce(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSession(t)
	if got := awaitResult(t, h.askFor(t, "500us", "touch", "marker")); got != (result{code: protocol.ExitRejected, stderr: waitingNoRule + "hostrun: not approved: nobody answered on the host\n"}) {
		t.Fatalf("got %+v, want it expired", got)
	}
}

// fakeDaemon serves one approval session on a socket of its own, sending
// it request 1, and returns the socket and the connection once the session
// serves it; the test plays the daemon's part from there.
func fakeDaemon(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	return fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion})
}

// fakeDaemonWith is fakeDaemon saying it is info.
func fakeDaemonWith(t *testing.T, info protocol.DaemonInfo) (string, <-chan net.Conn) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "fake.approval.sock")
	l, err := transport.Unix{Path: socket}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	served := make(chan net.Conn, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { conn.Close() })
		for _, step := range []struct {
			read  protocol.FrameType
			reply protocol.FrameType
			v     any
		}{
			{protocol.FrameHello, protocol.FrameDaemonInfo, info},
			{protocol.FrameServe, protocol.FrameServing, nil},
		} {
			if f, err := protocol.ReadFrame(conn); err != nil || f.Type != step.read {
				return
			}
			if step.v == nil {
				_ = protocol.WriteFrame(conn, protocol.Frame{Type: step.reply})
			} else {
				_ = protocol.WriteJSON(conn, step.reply, step.v)
			}
		}
		_ = protocol.WriteJSON(conn, protocol.FrameApprovalRequest, protocol.ApprovalRequest{ID: 1, Argv: []string{"touch", "marker"}, Cwd: containerRoot, Reasons: []string{"no rule"}, TimeoutMs: 60000})
		served <- conn
	}()
	return socket, served
}

// An answer that crosses its request's end is reported as the end, in
// whichever order the daemon's two frames arrive.
func TestAskSessionReportsAnAnswerTooLate(t *testing.T) {
	for _, withdrawnFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("withdrawn first %v", withdrawnFirst), func(t *testing.T) {
			socket, served := fakeDaemon(t)
			s := startSessionOn(t, socket)
			conn := <-served
			s.waitFor(t, answerPromptText)
			s.answer(t, "o")
			if f, err := protocol.ReadFrame(conn); err != nil || f.Type != protocol.FrameAnswer {
				t.Fatalf("frame %v, err %v; want the answer", f.Type, err)
			}
			withdrawn := func() {
				_ = protocol.WriteJSON(conn, protocol.FrameWithdrawn, protocol.Withdrawn{ID: 1, Reason: protocol.WithdrawnExpired})
			}
			answered := func() {
				_ = protocol.WriteJSON(conn, protocol.FrameAnswered, protocol.Answered{ID: 1, Error: "request 1 is not waiting any more", Ended: protocol.WithdrawnExpired})
			}
			if withdrawnFirst {
				withdrawn()
				answered()
			} else {
				answered()
				withdrawn()
			}
			s.waitFor(t, "request 1 expired\n")
			time.Sleep(50 * time.Millisecond)
			if out := s.out.String(); strings.Contains(out, "not taken") || strings.Contains(out, "approved once") {
				t.Fatalf("the session reported the answer as such:\n%s", out)
			}
		})
	}
}

// A write to the daemon that fails ends serving it, even while reading
// from it still works: the session says so, and that the daemon refused
// the requests waiting, the one whose answer did not reach it among them,
// and waits for the daemon as when it ends.
func TestSessionEndsWhenAWriteToTheDaemonFails(t *testing.T) {
	for _, tt := range []struct{ name, typed string }{
		{"an answer", "o"},
		{"a refusal with a note", "use echo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket, served := fakeDaemon(t)
			s := startSessionOn(t, socket)
			conn := <-served
			s.waitFor(t, answerPromptText)
			// The daemon reads no more: a write to it fails, a read waits.
			if err := conn.(*net.UnixConn).CloseRead(); err != nil {
				t.Fatal(err)
			}
			if tt.typed != "o" {
				s.answer(t, "r")
				s.waitFor(t, notePromptText)
			}
			s.answer(t, tt.typed)
			s.waitFor(t, "\nthe connection to the daemon failed: ")
			if out := s.waitFor(t, "\nrefused 1 waiting request\nwaiting for the next one\n"); strings.Contains(out, "approved once") || strings.Contains(out, "\nrefused\n") {
				t.Errorf("the session said the answer was taken: %q", out)
			}
		})
	}
}

// A rule written whose reread the daemon was never asked for is in the
// rules file only: the session says it takes effect at the next start.
func TestSessionSaysARuleWasNotRereadWhenTheWriteFails(t *testing.T) {
	rulesFile := filepath.Join(t.TempDir(), "hostrun.yaml")
	if err := os.WriteFile(rulesFile, []byte("rules:\n  - command: echo\n    args: any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	socket, served := fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, RulesFile: rulesFile})
	s := startSessionOn(t, socket)
	conn := <-served
	s.waitFor(t, answerPromptText)
	s.answer(t, "a")
	s.waitFor(t, addPromptText)
	if err := conn.(*net.UnixConn).CloseRead(); err != nil {
		t.Fatal(err)
	}
	s.answer(t, "w")
	s.waitFor(t, "\nthe connection to the daemon failed: ")
	s.waitFor(t, "\nrule written, but the daemon did not reread it: it takes effect at the next container start\n")
	if file := readFile(t, rulesFile); !strings.Contains(file, "- command: touch") {
		t.Fatalf("the rules file lacks the rule:\n%s", file)
	}
}

// A note too long for a frame is not sent: the session says so and asks
// for the note again.
func TestAskNoteTooLongIsAskedAgain(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "r")
	s.waitFor(t, notePromptText)
	s.answer(t, strings.Repeat("x", 4097))
	s.waitFor(t, "note too long: at most 4096 bytes\n"+notePromptText)
	s.answer(t, "shorter")
	if got := awaitResult(t, ch); !strings.HasSuffix(got.stderr, "hostrun: refused on the host: shorter\n") {
		t.Fatalf("got %+v, want the shorter note", got)
	}
}

// Run stops reading its input when it returns, a pipe as well: a line
// typed after it is left for whoever reads the input next.
func TestRunLeavesItsInputOnceItReturns(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{out: &lockedBuffer{}, ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		s.err = approval.Run(ctx, approval.Config{Socket: h.approvalSocket}, r, s.out)
	}()
	s.waitFor(t, "waiting for requests")
	s.waitFor(t, "> ")
	// A line answered: the session reads on, after a prompt.
	if _, err := io.WriteString(w, "x\n"); err != nil {
		t.Fatal(err)
	}
	s.waitFor(t, "unknown command")
	s.waitFor(t, "> ")
	cancel()
	waitEnded(t, s)
	if _, err := io.WriteString(w, "late\n"); err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, _ := r.Read(buf)
		read <- string(buf[:n])
	}()
	select {
	case got := <-read:
		if got != "late\n" {
			t.Fatalf("read %q after Run returned, want the line typed after it", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the line typed after Run returned was taken")
	}
}

// An approved command that injects a secret leaves its value out of the
// daemon log: the request and the run are logged by the argv the
// container sent, and the secret is shown injected, masked, on stdout.
func TestAskLogsNoSecretValue(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, `
rules:
  - command: printf
    secrets:
      token: {env: TOKEN}
    inject:
      trailing_args: ["${token}"]
    flags:
      allow: []
    positional:
      allow: ["ok"]
`))
	s := h.startSession(t)
	ch := h.ask(t, "printf", "%s")
	s.approveYes(t)
	if got := awaitResult(t, ch); got.code != 0 || got.stdout != "***" {
		t.Fatalf("got %+v, want the injected secret printed masked", got)
	}
	waitForLog(t, &log, `msg="command finished"`)
	if strings.Contains(log.String(), testToken) {
		t.Fatalf("the log holds the secret's value:\n%s", log.String())
	}
	waitForLog(t, &log, `secrets=[token]`)
}

// An approved command of a rule with dir: inherit runs in the directory
// it inherits, as an allowed one does.
func TestAskRunsAnApprovedCommandInAnInheritedDirectory(t *testing.T) {
	dir := fixedDir(t)
	h := startDaemon(t, policy(t, fmt.Sprintf("rules:\n  - command: sh\n    dir: %s\n    args: none\n  - command: sh -c\n    dir: inherit\n    positional:\n      allow: [\"true\"]\n", dir)))
	s := h.startSession(t)
	ch := make(chan result, 1)
	go func() { ch <- h.run(t, containerRoot+"/sub", "", "--ask", "sh", "-c", "pwd -P") }()
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 || got.stdout != dir+"\n" {
		t.Fatalf("got %+v, want it run in %s", got, dir)
	}
}

// An answer of a scope there is not is refused, and the request still
// waits for an answer.
func TestAnAnswerOfAnUnknownScopeIsRefused(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	conn := h.serveRaw(t)
	ch := h.ask(t, "touch", "marker")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := protocol.ReadFrame(conn)
	var req protocol.ApprovalRequest
	if err != nil || f.Type != protocol.FrameApprovalRequest || protocol.DecodeJSON(f, &req) != nil {
		t.Fatalf("frame %v, err %v; want the request", f.Type, err)
	}
	if err := protocol.WriteJSON(conn, protocol.FrameAnswer, protocol.Answer{ID: req.ID, Scope: "forever"}); err != nil {
		t.Fatal(err)
	}
	f, err = protocol.ReadFrame(conn)
	var answered protocol.Answered
	if err != nil || f.Type != protocol.FrameAnswered || protocol.DecodeJSON(f, &answered) != nil || answered.Error != `unknown scope "forever"` {
		t.Fatalf("frame %v %+v, err %v; want the answer refused for its scope", f.Type, answered, err)
	}
	if err := protocol.WriteJSON(conn, protocol.FrameAnswer, protocol.Answer{ID: req.ID, Scope: protocol.ScopeOnce}); err != nil {
		t.Fatal(err)
	}
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want the request approved by the next answer", got)
	}
}

// A reread result the session did not ask for breaks the protocol: the
// session stops serving the daemon and says why.
func TestSessionRefusesARereadResultItDidNotAskFor(t *testing.T) {
	socket, served := fakeDaemon(t)
	s := startSessionOn(t, socket)
	conn := <-served
	s.waitFor(t, answerPromptText)
	if err := protocol.WriteJSON(conn, protocol.FrameRereadResult, protocol.RereadResult{Digest: "x"}); err != nil {
		t.Fatal(err)
	}
	waitEnded(t, s)
	if s.err == nil || !strings.Contains(s.err.Error(), "did not ask for") {
		t.Fatalf("got %v, want the session to refuse the reread result", s.err)
	}
}
