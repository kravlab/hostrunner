package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// pathEnvironment is an environment that has only the PATH of the process,
// so that the daemon finds the same programs, and vars.
func pathEnvironment(vars ...string) *daemon.Environment {
	return daemon.NewEnvironment(append([]string{"PATH=" + os.Getenv("PATH")}, vars...))
}

// A command gets the given environment, and nothing of the process's: its
// script prints the variable only the given environment has, and the one only
// the process has as unset.
func TestWithEnvironmentRunsCommandsWithTheGivenEnvironment(t *testing.T) {
	t.Setenv("HR_PROCESS_ONLY", "from-process")
	l := startServer(t, allowAll{}, daemon.WithEnvironment(pathEnvironment("HR_GIVEN=from-daemon")))

	got := ask(t, l, protocol.FrameRequest, "sh", "-c", `printf '%s|%s' "$HR_GIVEN" "${HR_PROCESS_ONLY-unset}"`)

	if want := "from-daemon|unset"; got.stdout != want {
		t.Fatalf("the command printed %q, want %q", got.stdout, want)
	}
}

// commandSource is a policy whose every command has one secret, which the
// command source gives: the output of the program argv names.
func commandSource(argv ...string) policyOf {
	return policyOf{rules.Allowed{Secrets: []rules.Secret{{
		Name: "TOKEN", Kind: rules.SourceCommand, Command: argv, Timeout: 10 * time.Second,
	}}}}
}

// A secret's command source gets the given environment, and nothing of
// the process's: its script prints a variable only the given environment
// has, and fails if one only the process's has is set, so the request
// runs only if both hold.
func TestWithEnvironmentRunsSecretCommandSourcesWithTheGivenEnvironment(t *testing.T) {
	t.Setenv("HR_PROCESS_ONLY", "from-process")
	l := startServer(t, commandSource("sh", "-c", `printf '%s' "$HR_GIVEN" && test -z "${HR_PROCESS_ONLY+set}"`),
		daemon.WithEnvironment(pathEnvironment("HR_GIVEN=from-the-daemon")))

	got := ask(t, l, protocol.FrameRequest, "true")

	if got.end.Type != protocol.FrameExit {
		t.Fatalf("the request ended with frame %d %s, want Exit", got.end.Type, got.end.Payload)
	}
}

// envSecret is a policy whose every command has one secret, the value of
// the variable name.
func envSecret(name string) policyOf {
	return policyOf{rules.Allowed{Secrets: []rules.Secret{{Name: "TOKEN", Kind: rules.SourceEnv, Env: name}}}}
}

// An env secret is the given environment's variable: one only the process
// has is not set.
func TestWithEnvironmentReadsEnvSecretsFromTheGivenEnvironment(t *testing.T) {
	t.Setenv("HR_PROCESS_ONLY", "from-the-process")
	lGiven := startServer(t, envSecret("HR_GIVEN"), daemon.WithEnvironment(pathEnvironment("HR_GIVEN=from-the-daemon")))
	lProcess := startServer(t, envSecret("HR_PROCESS_ONLY"), daemon.WithEnvironment(pathEnvironment()))

	checkEnds(t, protocol.FrameRequest, []endCase{
		{"a variable of the given environment", lGiven, []string{"true"}, protocol.FrameExit},
		{"a variable of the process only", lProcess, []string{"true"}, protocol.FrameError},
	})
}

// A dry run checks an env secret against the given environment too.
func TestWithEnvironmentChecksEnvSecretsOfADryRunInTheGivenEnvironment(t *testing.T) {
	t.Setenv("HR_PROCESS_ONLY", "from-the-process")
	lGiven := startServer(t, envSecret("HR_GIVEN"), daemon.WithEnvironment(pathEnvironment("HR_GIVEN=from-the-daemon")))
	lProcess := startServer(t, envSecret("HR_PROCESS_ONLY"), daemon.WithEnvironment(pathEnvironment()))

	checkEnds(t, protocol.FrameDryRun, []endCase{
		{"a variable of the given environment", lGiven, []string{"true"}, protocol.FrameAllowed},
		{"a variable of the process only", lProcess, []string{"true"}, protocol.FrameError},
	})
}

// toolDir returns a new directory that holds an executable script named
// name, which prints out.
func toolDir(t *testing.T, name, out string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withPath is the process's PATH with dir in front.
func withPath(dir string) string {
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// A dry run looks a program up in the PATH of the given environment: a
// program only the process's PATH has is not found.
func TestWithEnvironmentLooksProgramsUpInTheGivenPathInADryRun(t *testing.T) {
	givenPath := withPath(toolDir(t, "hr-given-tool", "given"))
	t.Setenv("PATH", withPath(toolDir(t, "hr-process-tool", "process")))
	l := startServer(t, allowAll{}, daemon.WithEnvironment(daemon.NewEnvironment([]string{"PATH=" + givenPath})))

	checkEnds(t, protocol.FrameDryRun, []endCase{
		{"a program on the given PATH", l, []string{"hr-given-tool"}, protocol.FrameAllowed},
		{"a program on the process's PATH only", l, []string{"hr-process-tool"}, protocol.FrameError},
	})
}

// A command's program is the one the PATH of the given environment finds,
// not the process's.
func TestWithEnvironmentStartsProgramsFoundInTheGivenPath(t *testing.T) {
	givenPath := withPath(toolDir(t, "hr-tool", "given"))
	t.Setenv("PATH", withPath(toolDir(t, "hr-tool", "process")))
	l := startServer(t, allowAll{}, daemon.WithEnvironment(daemon.NewEnvironment([]string{"PATH=" + givenPath})))

	got := ask(t, l, protocol.FrameRequest, "hr-tool")

	if want := "given"; got.stdout != want {
		t.Fatalf("the program printed %q, want %q", got.stdout, want)
	}
}

// A program the given PATH does not have is not started, though the
// process's PATH has it.
func TestWithEnvironmentDoesNotStartProgramsOnlyTheProcessPathHas(t *testing.T) {
	givenPath := withPath(t.TempDir())
	t.Setenv("PATH", withPath(toolDir(t, "hr-process-tool", "process")))
	l := startServer(t, allowAll{}, daemon.WithEnvironment(daemon.NewEnvironment([]string{"PATH=" + givenPath})))

	got := ask(t, l, protocol.FrameRequest, "hr-process-tool")

	var e protocol.Error
	if got.end.Type != protocol.FrameError || protocol.DecodeJSON(got.end, &e) != nil || e.Code != protocol.ExitNotFound {
		t.Fatalf("the request ended with frame %d %s, want Error %d", got.end.Type, got.end.Payload, protocol.ExitNotFound)
	}
}

// A secret's command source is the program the PATH of the given
// environment finds, not the process's: the given one prints a value long
// enough to be a secret, the process's one is too short.
func TestWithEnvironmentLooksSecretCommandSourcesUpInTheGivenPath(t *testing.T) {
	givenPath := withPath(toolDir(t, "hr-source", "given-secret-value"))
	t.Setenv("PATH", withPath(toolDir(t, "hr-source", "short")))
	l := startServer(t, commandSource("hr-source"), daemon.WithEnvironment(daemon.NewEnvironment([]string{"PATH=" + givenPath})))

	got := ask(t, l, protocol.FrameRequest, "true")

	if got.end.Type != protocol.FrameExit {
		t.Fatalf("the request ended with frame %d %s, want Exit", got.end.Type, got.end.Payload)
	}
}

// A dry run looks a secret's command source up in the given PATH.
func TestWithEnvironmentChecksSecretCommandSourcesOfADryRunInTheGivenPath(t *testing.T) {
	givenPath := withPath(toolDir(t, "hr-given-source", "given-secret-value"))
	t.Setenv("PATH", withPath(toolDir(t, "hr-process-source", "process-secret-value")))
	env := daemon.WithEnvironment(daemon.NewEnvironment([]string{"PATH=" + givenPath}))
	lGiven, lProcess := startServer(t, commandSource("hr-given-source"), env), startServer(t, commandSource("hr-process-source"), env)

	checkEnds(t, protocol.FrameDryRun, []endCase{
		{"a source on the given PATH", lGiven, []string{"true"}, protocol.FrameAllowed},
		{"a source on the process's PATH only", lProcess, []string{"true"}, protocol.FrameError},
	})
}

// A directory the given PATH names relatively is the daemon's working
// directory, not the command's, so a program found there is not run, as
// exec.LookPath refuses it.
func TestWithEnvironmentRefusesProgramsFoundThroughARelativePath(t *testing.T) {
	t.Chdir(toolDir(t, "hr-dot-tool", "dot"))
	l := startServer(t, allowAll{}, daemon.WithEnvironment(daemon.NewEnvironment([]string{"PATH=."})))

	got := ask(t, l, protocol.FrameDryRun, "hr-dot-tool")

	if got.end.Type != protocol.FrameError {
		t.Fatalf("the dry run ended with frame %d %s, want Error", got.end.Type, got.end.Payload)
	}
}

// secretSourceRun is a Server that serves while a request waits for its
// secret source to give the secret.
type secretSourceRun struct {
	dir    string       // where the script's files are; its $HR_DIR
	client net.Conn     // the connection of the request
	done   <-chan error // what Serve returns
}

// startSecretSource serves a Server that logs to log, under ctx, sends it a
// request whose only secret the shell script gives, and returns once the
// script has started: it creates the file started in $HR_DIR then, as the
// first thing it does.
func startSecretSource(t *testing.T, log *slog.Logger, ctx context.Context, script string) secretSourceRun {
	t.Helper()
	dir := t.TempDir()
	srv := newLoggingServer(t, log, commandSource("sh", "-c", `touch "$HR_DIR/started"; `+script), daemon.WithEnvironment(pathEnvironment("HR_DIR="+dir)))
	l := newChanListener()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close() })
	l.results <- acceptResult{conn: server}
	go io.Copy(io.Discard, client)
	sendRequest(t, client, "true")
	awaitFile(t, filepath.Join(dir, "started"), "the secret source did not start")
	return secretSourceRun{dir, client, done}
}

// awaitFile waits for the file path to exist, and fails with msg if it does
// not.
func awaitFile(t *testing.T, path, msg string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatal(msg)
		}
	}
}

// awaitServe waits for Serve to return, and fails with msg if it does not.
func (r secretSourceRun) awaitServe(t *testing.T, msg string) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// A secret's command source runs under the daemon's context: once it
// ends, the source is killed and Serve returns, though the source has
// time left.
func TestServeEndsRunningSecretSourcesWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := startSecretSource(t, slog.New(slog.DiscardHandler), ctx, "exec sleep 60")

	cancel()

	run.awaitServe(t, "Serve did not return after its context ended, while a secret source ran")
}

// The log says that a secret source was cancelled with the daemon's
// context, not that it timed out.
func TestServeLogsASecretSourceCancelledWithItsContext(t *testing.T) {
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := startSecretSource(t, slog.New(slog.NewTextHandler(&out, nil)), ctx, "exec sleep 60")

	cancel()
	run.awaitServe(t, "Serve did not return after its context ended")

	if got := out.String(); !strings.Contains(got, "cancelled") || strings.Contains(got, "timed out") {
		t.Fatalf("the log of the cancelled secret source is\n%s\nwant it to say cancelled, not timed out", got)
	}
}

// A deadline of the daemon's context is its context ending, not the
// source's timeout: the log says cancelled.
func TestServeLogsASecretSourceCancelledByTheDeadlineOfItsContext(t *testing.T) {
	var out syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run := startSecretSource(t, slog.New(slog.NewTextHandler(&out, nil)), ctx, "exec sleep 60")

	run.awaitServe(t, "Serve did not return after the deadline of its context")

	if got := out.String(); !strings.Contains(got, "cancelled") || strings.Contains(got, "timed out") {
		t.Fatalf("the log of the secret source cancelled by a deadline is\n%s\nwant it to say cancelled, not timed out", got)
	}
}

// A client that goes away does not kill the secret source of its request:
// it runs on, to its own timeout, as it always did.
func TestServeKeepsASecretSourceRunningWhenItsClientGoesAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := startSecretSource(t, slog.New(slog.DiscardHandler), ctx, `sleep 1; touch "$HR_DIR/finished"; printf 'a-long-enough-secret'`)

	run.client.Close()

	awaitFile(t, filepath.Join(run.dir, "finished"), "the secret source was killed when its client went away")
}

// Without WithEnvironment, a daemon alone in its process runs commands
// with the process's environment as it is when each starts, as it always
// did.
func TestServerWithoutAnEnvironmentRunsCommandsWithTheProcessEnvironment(t *testing.T) {
	l := startServer(t, allowAll{})
	t.Setenv("HR_SET_LATER", "from-the-process")

	got := ask(t, l, protocol.FrameRequest, "sh", "-c", `printf '%s' "$HR_SET_LATER"`)

	if want := "from-the-process"; got.stdout != want {
		t.Fatalf("the command printed %q, want %q", got.stdout, want)
	}
}

// WithEnvironment takes the environment of the daemon: a nil one is a
// mistake of the caller, which panics as New does for a nil Policy, not a
// silent fall back to the process's.
func TestWithEnvironmentPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithEnvironment(nil) did not panic")
		}
	}()

	newPolicyServer(t, allowAll{}, daemon.WithEnvironment(nil))
}
