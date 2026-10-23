package client_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// session is an approval session serving a harness's daemon, driven by a
// test as a human would drive it: it reads what the session prints and
// types answers.
type session struct {
	in   *io.PipeWriter
	out  *lockedBuffer
	seen int // how much of out the test has read
	done chan error
}

// startSession starts an approval session for h's daemon and waits until
// it serves the daemon. It ends when the test does.
func (h harness) startSession(t *testing.T) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	s := &session{in: pw, out: &lockedBuffer{}, done: make(chan error, 1)}
	go func() { s.done <- approval.Run(ctx, approval.Config{Socket: h.approvalSocket}, pr, s.out) }()
	t.Cleanup(func() {
		cancel()
		pw.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Error("the approval session did not end")
		}
	})
	s.waitFor(t, "waiting for requests")
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
	block := s.waitFor(t, "answer [r]efuse [o]nce ? ")
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
			s.waitFor(t, "answer [r]efuse [o]nce ? ")
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
	s.waitFor(t, "answer [r]efuse [o]nce ? ")
	s.answer(t, "x")
	s.waitFor(t, "answer [r]efuse [o]nce ? ")
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

// Until a refusal by a matching rule or a guarded one can be approved, it
// is refused as without --ask.
func TestAskRefusesARefusalByAMatchingRuleAsWithoutAsk(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSession(t)
	got := h.run(t, containerRoot, "", "--ask", "sh", "-x", "script")
	if want := (result{code: protocol.ExitRejected, stderr: "hostrun: denied by rule \"sh\": flag -x is not allowed\n"}); got != want {
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
	s.waitFor(t, "answer [r]efuse [o]nce ? ")
	s.answer(t, "o")
	awaitResult(t, ch)
	for _, want := range []string{
		"msg=\"approval session serving\" pid=",
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

// A request whose agent went away is withdrawn: the human's later answer
// is not taken, and the session says so instead of claiming it.
func TestAskWithdrawnWhenTheClientGoesAway(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)), policy(t, echoOnly))
	s := h.startSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- client.Run(ctx, h.transport, []string{"--ask", "touch", "marker"}, containerRoot, strings.NewReader(""), io.Discard, io.Discard)
	}()
	s.waitFor(t, "answer [r]efuse [o]nce ? ")
	cancel()
	if code := <-done; code != client.ExitInterrupted {
		t.Fatalf("exit code %d, want %d", code, client.ExitInterrupted)
	}
	waitForLog(t, &log, `msg="approval not answered" approval_id=1 reason=withdrawn`)
	s.answer(t, "o")
	s.waitFor(t, "request 1: not taken: request 1 is not waiting any more")
	if strings.Contains(s.out.String(), "approved once") {
		t.Fatalf("the session claimed an answer the daemon did not take:\n%s", s.out.String())
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
	s.waitFor(t, "answer [r]efuse [o]nce ? ")
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

const notePromptText = "note for the agent (Enter for none): "

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
	block := s.waitFor(t, "answer [r]efuse [o]nce ? ")
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

// A second session cannot take a served daemon from under the first.
func TestASecondSessionIsRefused(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	h.startSession(t)
	err := approval.Run(context.Background(), approval.Config{Socket: h.approvalSocket}, strings.NewReader(""), io.Discard)
	if err == nil || err.Error() != "another approval session serves this daemon" {
		t.Fatalf("got %v, want the second session refused", err)
	}
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
