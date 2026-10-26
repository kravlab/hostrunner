package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/client"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// countingDialer counts the connections Run asks for and refuses them.
type countingDialer struct{ dials int }

// Dial counts the connection asked for and refuses it.
func (d *countingDialer) Dial(context.Context) (net.Conn, error) {
	d.dials++
	return nil, errors.New("dial refused by the test")
}

// runOffline runs hostrun with args in cwd against a dialer that refuses
// every connection, and returns what the user observes and how often it
// dialled.
func runOffline(t *testing.T, cwd string, args ...string) (result, int) {
	t.Helper()
	var d countingDialer
	var stdout, stderr bytes.Buffer
	code := client.Run(context.Background(), &d, args, cwd, strings.NewReader(""), &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}, d.dials
}

func TestRunRefusesAnArgumentThatIsNotUTF8(t *testing.T) {
	got, dials := runOffline(t, containerRoot, "printf", "%s", "a\xffb")
	want := result{code: protocol.ExitHostrunError, stderr: "hostrun: argument 3 is not valid UTF-8: \"a\\xffb\"\n"}
	if got != want || dials != 0 {
		t.Fatalf("got %+v after %d dials, want %+v without dialling", got, dials, want)
	}
}

func TestRunRefusesACurrentDirectoryThatIsNotUTF8(t *testing.T) {
	got, dials := runOffline(t, containerRoot+"/\xe9t\xe9", "pwd")
	want := result{code: protocol.ExitHostrunError, stderr: "hostrun: the current directory is not valid UTF-8: \"/workspaces/app/\\xe9t\\xe9\"\n"}
	if got != want || dials != 0 {
		t.Fatalf("got %+v after %d dials, want %+v without dialling", got, dials, want)
	}
}

func TestRunListsEveryValueThatIsNotUTF8(t *testing.T) {
	got, dials := runOffline(t, containerRoot+"/\xff", "\xfe", "ok", "\xfd")
	want := result{code: protocol.ExitHostrunError, stderr: "" +
		"hostrun: argument 1 is not valid UTF-8: \"\\xfe\"\n" +
		"hostrun: argument 3 is not valid UTF-8: \"\\xfd\"\n" +
		"hostrun: the current directory is not valid UTF-8: \"/workspaces/app/\\xff\"\n"}
	if got != want || dials != 0 {
		t.Fatalf("got %+v after %d dials, want %+v without dialling", got, dials, want)
	}
}

func TestRunCutsALongValueThatIsNotUTF8(t *testing.T) {
	cases := map[string]struct{ arg, shown string }{
		"invalid byte counts as a character": {
			arg:   "\xff" + strings.Repeat("a", 300),
			shown: `"\xff` + strings.Repeat("a", 199) + `"…(+101 bytes)`,
		},
		"cut between characters": {
			arg:   strings.Repeat("é", 250) + "\xff",
			shown: `"` + strings.Repeat("é", 200) + `"…(+101 bytes)`,
		},
		"200 characters are not cut": {
			arg:   "\xff" + strings.Repeat("a", 199),
			shown: `"\xff` + strings.Repeat("a", 199) + `"`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, _ := runOffline(t, containerRoot, "echo", c.arg)
			want := "hostrun: argument 2 is not valid UTF-8: " + c.shown + "\n"
			if got.code != protocol.ExitHostrunError || got.stderr != want {
				t.Fatalf("got %+v, want code %d, stderr %q", got, protocol.ExitHostrunError, want)
			}
		})
	}
}

func TestDryRunAndAskRefuseAnArgumentThatIsNotUTF8(t *testing.T) {
	for _, flag := range []string{"--dry-run", "--ask", "--ask=1m"} {
		t.Run(flag, func(t *testing.T) {
			got, dials := runOffline(t, containerRoot, flag, "cat", "\xff")
			want := result{code: protocol.ExitHostrunError, stderr: "hostrun: argument 2 is not valid UTF-8: \"\\xff\"\n"}
			if got != want || dials != 0 {
				t.Fatalf("got %+v after %d dials, want %+v without dialling", got, dials, want)
			}
		})
	}
}

func TestRunPassesValidUTF8AsGiven(t *testing.T) {
	h := startDaemon(t)
	// U+FFFD itself, non-ASCII text and an empty argument are valid UTF-8.
	args := []string{string(utf8.RuneError), "été", ""}
	got := h.run(t, containerRoot, "", append([]string{"printf", "%s|%s|%s"}, args...)...)
	if want := (result{stdout: strings.Join(args, "|")}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// requestJSON is a request, as another client may write it, that runs
// argv, given as JSON string literals, in the container workspace.
func requestJSON(argv ...string) string {
	return `{"version":1,"argv":[` + strings.Join(argv, ",") + `],"cwd":"` + containerRoot + `"}`
}

// esc is the JSON escape of the UTF-16 code unit u.
func esc(u uint16) string { return fmt.Sprintf(`\u%04x`, u) }

// notUTF8Message is the daemon's refusal of a request that is not valid
// UTF-8.
const notUTF8Message = "the request is not valid UTF-8: arguments and the current directory must be valid UTF-8"

// expectError asserts that f is a FrameError carrying want.
func expectError(t *testing.T, f protocol.Frame, want protocol.Error) {
	t.Helper()
	if f.Type != protocol.FrameError {
		t.Fatalf("got frame type %d, want FrameError", f.Type)
	}
	var e protocol.Error
	if err := protocol.DecodeJSON(f, &e); err != nil {
		t.Fatal(err)
	}
	if e != want {
		t.Fatalf("got %+v, want %+v", e, want)
	}
}

func TestDaemonRefusesARequestThatIsNotUTF8(t *testing.T) {
	payloads := map[string]string{
		"invalid byte in an argument":       requestJSON(`"touch"`, `"marker"`, "\"a\xffb\""),
		"invalid byte in the directory":     `{"version":1,"argv":["touch","marker"],"cwd":"` + containerRoot + "/\xff\"}",
		"lone high surrogate":               requestJSON(`"touch"`, `"marker"`, `"a`+esc(0xd800)+`b"`),
		"lone low surrogate":                requestJSON(`"touch"`, `"marker"`, `"a`+esc(0xdc00)+`b"`),
		"high surrogate at the end":         requestJSON(`"touch"`, `"marker"`, `"a`+esc(0xd800)+`"`),
		"two high surrogates":               requestJSON(`"touch"`, `"marker"`, `"`+esc(0xd800)+esc(0xd800)+`"`),
		"high surrogate before an escape":   requestJSON(`"touch"`, `"marker"`, `"`+esc(0xd800)+`\n"`),
		"high surrogate before a character": requestJSON(`"touch"`, `"marker"`, `"`+esc(0xd800)+`A"`),
	}
	frames := map[string]protocol.FrameType{"run": protocol.FrameRequest, "dry run": protocol.FrameDryRun, "ask": protocol.FrameAsk}
	for fname, typ := range frames {
		for pname, payload := range payloads {
			t.Run(fname+"/"+pname, func(t *testing.T) {
				h := startDaemon(t)
				f, _ := h.rawFrame(t, typ, payload)
				expectError(t, f, protocol.Error{Code: protocol.ExitHostrunError, Message: notUTF8Message})
				if typ == protocol.FrameRequest { // the others would not run it anyway
					assertNotRun(t, h, "marker")
				}
			})
		}
	}
}

func TestDaemonAsksNoApprovalForARequestThatIsNotUTF8(t *testing.T) {
	h := startDaemon(t, policy(t, echoOnly))
	s := h.startSession(t)
	f, _ := h.rawFrame(t, protocol.FrameAsk, requestJSON(`"touch"`, `"marker"`, `"`+esc(0xd800)+`"`))
	expectError(t, f, protocol.Error{Code: protocol.ExitHostrunError, Message: notUTF8Message})
	if out := s.out.String(); strings.Contains(out, "request 1") {
		t.Fatalf("the session was asked for an approval: %q", out)
	}
}

func TestDaemonLogsARefusedRequestThatIsNotUTF8(t *testing.T) {
	var log lockedBuffer
	h := startLoggingDaemon(t, slog.New(slog.NewTextHandler(&log, nil)))
	f, _ := h.rawFrame(t, protocol.FrameRequest, requestJSON(`"true"`, "\"\xff\""))
	expectError(t, f, protocol.Error{Code: protocol.ExitHostrunError, Message: notUTF8Message})
	h.stop()
	if got := log.String(); !strings.Contains(got, `msg="request rejected"`) || !strings.Contains(got, notUTF8Message) {
		t.Fatalf("the log %q does not record the refusal", got)
	}
}

func TestDaemonRunsARequestOfValidUTF8(t *testing.T) {
	cases := map[string]struct{ arg, stdout string }{
		"surrogate pair":                 {`"` + esc(0xd83d) + esc(0xde00) + `"`, "\U0001F600"},
		"escaped backslash before ud800": {`"\\ud800"`, `\ud800`},
		"U+FFFD as UTF-8":                {`"` + string(utf8.RuneError) + `"`, string(utf8.RuneError)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := startDaemon(t)
			f, stdout := h.rawFrame(t, protocol.FrameRequest, requestJSON(`"printf"`, `"%s"`, c.arg))
			var exit protocol.Exit
			if f.Type != protocol.FrameExit {
				t.Fatalf("got frame type %d, want FrameExit", f.Type)
			}
			if err := protocol.DecodeJSON(f, &exit); err != nil {
				t.Fatal(err)
			}
			if exit.Code != 0 || stdout != c.stdout {
				t.Fatalf("got code %d, stdout %q; want 0, %q", exit.Code, stdout, c.stdout)
			}
		})
	}
}
