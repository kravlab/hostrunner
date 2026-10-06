package daemon_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// pathWorkspace is a host workspace for path check tests, with a directory
// outside it that a check must not let a command reach.
type pathWorkspace struct {
	base    string // holds root and outside; symlink-free
	root    string // the host workspace, mapped to containerRoot
	outside string // a directory next to the workspace
}

// newPathWorkspace creates a workspace holding app.txt ("inside") and sub/,
// and outside it secret.txt ("secret").
func newPathWorkspace(t *testing.T) pathWorkspace {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := pathWorkspace{base: base, root: filepath.Join(base, "app"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{filepath.Join(w.root, "sub"), w.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.write(t, filepath.Join(w.root, "app.txt"), "inside")
	w.write(t, filepath.Join(w.outside, "secret.txt"), "secret")
	return w
}

// write creates the file path with content.
func (pathWorkspace) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// symlink creates link pointing at target, as written.
func (pathWorkspace) symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// outcome is what the client saw of one request.
type outcome struct {
	code    int    // exit code of the command, or the error frame's code
	stdout  string // the command's stdout
	message string // the error frame's message; empty if the command ran
}

// run sends argv, run in the container directory cwd, to a daemon serving w
// with the rules in config, and returns what the client saw.
func (w pathWorkspace) run(t *testing.T, config, cwd string, argv ...string) outcome {
	t.Helper()
	policy, err := rules.Parse([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := workspace.NewMapper(w.root, containerRoot)
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.New(mapper, policy, slog.New(slog.DiscardHandler))
	client, server := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, newChanListener(acceptResult{conn: server}))

	client.SetDeadline(time.Now().Add(10 * time.Second))
	req := protocol.Request{Version: protocol.Version, Argv: argv, Cwd: cwd}
	if err := protocol.WriteJSON(client, protocol.FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	var out outcome
	var stdout strings.Builder
	for {
		f, err := protocol.ReadFrame(client)
		if err != nil {
			t.Fatalf("no status frame: %v", err)
		}
		switch f.Type {
		case protocol.FrameStdout:
			stdout.Write(f.Payload)
		case protocol.FrameExit:
			var e protocol.Exit
			if err := protocol.DecodeJSON(f, &e); err != nil {
				t.Fatal(err)
			}
			out.code, out.stdout = e.Code, stdout.String()
			return out
		case protocol.FrameError:
			var e protocol.Error
			if err := protocol.DecodeJSON(f, &e); err != nil {
				t.Fatal(err)
			}
			out.code, out.stdout, out.message = e.Code, stdout.String(), e.Message
			return out
		}
	}
}

// expectRefused checks that the daemon refused the command with message,
// and that the message gives away no host path.
func (w pathWorkspace) expectRefused(t *testing.T, got outcome, message string) {
	t.Helper()
	if got.code != protocol.ExitRejected || got.message != message {
		t.Fatalf("got code %d, message %q; want %d, %q", got.code, got.message, protocol.ExitRejected, message)
	}
	if strings.Contains(got.message, w.base) {
		t.Fatalf("message %q reveals a host path", got.message)
	}
}

const catCheck = "rules:\n  - command: cat\n    positional:\n      path: check\n"

// With path: check the program gets the argument as given, after the
// daemon has checked that it names a workspace file.
func TestPathCheckPassesAWorkspaceFileAsGiven(t *testing.T) {
	w := newPathWorkspace(t)
	got := w.run(t, "rules:\n  - command: echo\n    positional:\n      path: check\n", containerRoot+"/sub", "echo", "../app.txt")
	if got.code != 0 || got.stdout != "../app.txt\n" {
		t.Fatalf("got %+v, want ../app.txt echoed", got)
	}
	got = w.run(t, catCheck, containerRoot+"/sub", "cat", "../app.txt")
	if got.code != 0 || got.stdout != "inside" {
		t.Fatalf("got %+v, want the workspace file read", got)
	}
}

func TestPathCheckFollowsSymlinksThatStayInside(t *testing.T) {
	w := newPathWorkspace(t)
	w.symlink(t, "../app.txt", filepath.Join(w.root, "sub", "link.txt"))
	got := w.run(t, catCheck, containerRoot, "cat", "sub/link.txt")
	if got.code != 0 || got.stdout != "inside" {
		t.Fatalf("got %+v, want the workspace file read", got)
	}
}

func TestPathCheckRefusesWhatIsNotAWorkspaceFile(t *testing.T) {
	w := newPathWorkspace(t)
	w.symlink(t, "../../outside/secret.txt", filepath.Join(w.root, "sub", "escape.txt"))
	w.symlink(t, filepath.Join(w.root, "app.txt"), filepath.Join(w.root, "absolute.txt"))
	if err := syscall.Mkfifo(filepath.Join(w.root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ arg, message string }{
		{"/workspaces/app/app.txt", `argument "/workspaces/app/app.txt" is not a workspace file: it is an absolute path`},
		{"", `argument "" is not a workspace file: it is empty`},
		{"missing.txt", `argument "missing.txt" is not a workspace file: it does not exist`},
		{"sub", `argument "sub" is not a workspace file: it is not a regular file`},
		{"fifo", `argument "fifo" is not a workspace file: it is not a regular file`},
		{"sub/escape.txt", `argument "sub/escape.txt" is not a workspace file: it leads outside the workspace`},
		{"absolute.txt", `argument "absolute.txt" is not a workspace file: it leads outside the workspace`},
		{"../outside/secret.txt", `argument "../outside/secret.txt" is not a workspace file: it leads outside the workspace`},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			w.expectRefused(t, w.run(t, catCheck, containerRoot, "cat", tc.arg), tc.message)
		})
	}
}

// The list's patterns see the argument as given; both have to pass.
func TestPathCheckCombinesWithTheList(t *testing.T) {
	w := newPathWorkspace(t)
	w.write(t, filepath.Join(w.root, "notes.md"), "notes")
	config := "rules:\n  - command: cat\n    positional:\n      allow: ['*.txt']\n      path: check\n"
	w.expectRefused(t, w.run(t, config, containerRoot, "cat", "notes.md"),
		`denied by rule "cat": argument "notes.md" is not allowed`)
	w.expectRefused(t, w.run(t, config, containerRoot, "cat", "missing.txt"),
		`argument "missing.txt" is not a workspace file: it does not exist`)
	if got := w.run(t, config, containerRoot, "cat", "app.txt"); got.code != 0 || got.stdout != "inside" {
		t.Fatalf("got %+v, want the workspace file read", got)
	}
}

// With path: open the program gets each checked file as a descriptor, in
// argument order, named by its /proc/self/fd path.
func TestPathOpenPassesTheOpenedFiles(t *testing.T) {
	w := newPathWorkspace(t)
	w.write(t, filepath.Join(w.root, "sub", "b.txt"), "b")
	got := w.run(t, "rules:\n  - command: echo\n    positional:\n      path: open\n", containerRoot, "echo", "app.txt", "sub/b.txt")
	if got.code != 0 || got.stdout != "/proc/self/fd/3 /proc/self/fd/4\n" {
		t.Fatalf("got %+v, want the descriptors of both files", got)
	}
	got = w.run(t, "rules:\n  - command: cat\n    positional:\n      path: open\n", containerRoot, "cat", "app.txt", "sub/b.txt")
	if got.code != 0 || got.stdout != "insideb" {
		t.Fatalf("got %+v, want both workspace files read", got)
	}
}

func TestPathOpenRefusesWhatIsNotAWorkspaceFile(t *testing.T) {
	w := newPathWorkspace(t)
	got := w.run(t, "rules:\n  - command: cat\n    positional:\n      path: open\n", containerRoot, "cat", "../outside/secret.txt")
	w.expectRefused(t, got, `argument "../outside/secret.txt" is not a workspace file: it leads outside the workspace`)
}

// swapAndRead is a rule whose program swaps a symlink to the outside file
// into the checked path before reading it ($0), as a container could do
// between the check and the program opening the path.
const swapAndRead = "mv app.txt old.txt; ln -s ../outside/secret.txt app.txt; cat \"$0\""

// swapRule allows `sh -c <script> <$0>` with a path check in mode on $0.
func swapRule(mode string) string {
	return "rules:\n  - command: sh\n    flags:\n      values:\n        -c: {}\n    positional:\n      path: " + mode + "\n"
}

func TestPathOpenIsNotRedirectedBySwapAfterTheCheck(t *testing.T) {
	w := newPathWorkspace(t)
	got := w.run(t, swapRule("open"), containerRoot, "sh", "-c", swapAndRead, "app.txt")
	if got.code != 0 || got.stdout != "inside" {
		t.Fatalf("got %+v, want the file the check opened", got)
	}
}

// path: check leaves the window open: this is the documented race.
func TestPathCheckIsRedirectedBySwapAfterTheCheck(t *testing.T) {
	w := newPathWorkspace(t)
	got := w.run(t, swapRule("check"), containerRoot, "sh", "-c", swapAndRead, "app.txt")
	if got.code != 0 || got.stdout != "secret" {
		t.Fatalf("got %+v, want the swapped-in file", got)
	}
}

// A path check on a value flag applies to every spelling of its value, and
// path: open replaces only the value in the flag's token.
func TestPathCheckAppliesToEverySpellingOfAFlagValue(t *testing.T) {
	w := newPathWorkspace(t)
	open := "rules:\n  - command: echo\n    flags:\n      values:\n        --file: { path: open }\n        -f: { path: open }\n"
	cases := []struct{ argv, want string }{
		{"--file app.txt", "--file /proc/self/fd/3\n"},
		{"--file=app.txt", "--file=/proc/self/fd/3\n"},
		{"--fi=app.txt", "--fi=/proc/self/fd/3\n"},
		{"-f app.txt", "-f /proc/self/fd/3\n"},
		{"-fapp.txt", "-f/proc/self/fd/3\n"},
		{"-xfapp.txt", "-xf/proc/self/fd/3\n"},
		{"-f app.txt --file=sub/../app.txt", "-f /proc/self/fd/3 --file=/proc/self/fd/4\n"},
	}
	for _, tc := range cases {
		t.Run(tc.argv, func(t *testing.T) {
			got := w.run(t, open, containerRoot, append([]string{"echo"}, strings.Fields(tc.argv)...)...)
			if got.code != 0 || got.stdout != tc.want {
				t.Fatalf("got %+v, want stdout %q", got, tc.want)
			}
		})
	}
	for _, argv := range []string{"--file ../outside/secret.txt", "--file=../outside/secret.txt", "--fi=../outside/secret.txt", "-f ../outside/secret.txt", "-f../outside/secret.txt", "-xf../outside/secret.txt"} {
		t.Run(argv, func(t *testing.T) {
			got := w.run(t, open, containerRoot, append([]string{"echo"}, strings.Fields(argv)...)...)
			w.expectRefused(t, got, `argument "../outside/secret.txt" is not a workspace file: it leads outside the workspace`)
		})
	}
}

func TestPathCheckOnAFlagValuePassesItAsGiven(t *testing.T) {
	w := newPathWorkspace(t)
	check := "rules:\n  - command: echo\n    flags:\n      values:\n        --file: { allow: ['*.txt'], path: check }\n"
	if got := w.run(t, check, containerRoot, "echo", "--file=app.txt"); got.code != 0 || got.stdout != "--file=app.txt\n" {
		t.Fatalf("got %+v, want the flag as given", got)
	}
	w.expectRefused(t, w.run(t, check, containerRoot, "echo", "--file", "missing.txt"),
		`argument "missing.txt" is not a workspace file: it does not exist`)
	w.expectRefused(t, w.run(t, check, containerRoot, "echo", "--file", "app.md"),
		`denied by rule "echo": value "app.md" of flag --file is not allowed`)
}

// Descriptors follow the order of the arguments, flag values and positional
// arguments alike.
func TestPathOpenNumbersDescriptorsInArgumentOrder(t *testing.T) {
	w := newPathWorkspace(t)
	w.write(t, filepath.Join(w.root, "sub", "b.txt"), "b")
	config := "rules:\n  - command: echo\n    flags:\n      values:\n        --file: { path: open }\n    positional:\n      path: open\n"
	got := w.run(t, config, containerRoot, "echo", "app.txt", "--file", "sub/b.txt")
	if got.code != 0 || got.stdout != "/proc/self/fd/3 --file /proc/self/fd/4\n" {
		t.Fatalf("got %+v, want descriptors in argument order", got)
	}
}

// A path is resolved from the working directory's real location, as the
// program resolves it: from a symlinked working directory, ".." climbs from
// the symlink's target.
func TestPathCheckResolvesFromASymlinkedWorkingDirectory(t *testing.T) {
	w := newPathWorkspace(t)
	if err := os.MkdirAll(filepath.Join(w.root, "deep", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.write(t, filepath.Join(w.root, "deep", "app.txt"), "deep")
	w.symlink(t, "deep/dir", filepath.Join(w.root, "link"))
	got := w.run(t, catCheck, containerRoot+"/link", "cat", "../app.txt")
	if got.code != 0 || got.stdout != "deep" {
		t.Fatalf("got %+v, want the file next to the symlink's target", got)
	}
}
