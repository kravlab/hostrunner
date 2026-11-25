package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/settings"
	"github.com/kravlab/hostrunner/internal/watch"
	"github.com/kravlab/hostrunner/internal/watch/watchtest"
)

// The rules of liveMountConfig: `touch` leaves a file on the host, which a
// test looks for to tell whether a command ran.
const touchRules = "rules:\n  - command: true\n    args: none\n  - command: touch\n    args: any\n"

// What an approval session prints that a test waits for.
const (
	answerPrompt     = "answer [r]efuse [o]nce [s]ession stan[d]ing [a]dd rule [c]md [h]elp ? "
	mountCheckNotice = "the mount check failed, so the daemon refuses every command: "
)

// liveMountConfig is mountCheckConfig whose rules file also allows `touch`.
func liveMountConfig(t *testing.T) serveConfig {
	t.Helper()
	cfg := mountCheckConfig(t)
	if err := os.WriteFile(cfg.RulesFile, []byte(touchRules), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// readOnlyFile mounts the host file source read-only in the container
// workspace of cfg, under its base name, as a devcontainer does for a file
// it keeps out of reach.
func readOnlyFile(cfg serveConfig, source string) watch.Mount {
	return watch.Mount{Source: source, Destination: cfg.containerWorkspace + "/" + filepath.Base(source), ReadOnly: true}
}

// writable returns m mounted read-write.
func writable(m watch.Mount) watch.Mount {
	m.ReadOnly = false
	return m
}

// mountPoints returns the live mount table of a container that has mounts
// as it was started: a point per mount, in order.
func mountPoints(mounts ...watch.Mount) []watch.MountPoint {
	var table []watch.MountPoint
	for _, m := range mounts {
		table = append(table, watch.MountPoint{Path: path.Clean(m.Destination), ReadOnly: m.ReadOnly})
	}
	return table
}

// armedContainer starts serve for cfg over rt, runs rt's container "c" with
// mounts and returns once serve runs the commands its rules allow, past
// its first mount check.
func armedContainer(t *testing.T, cfg serveConfig, rt *watchtest.Runtime, mounts ...watch.Mount) {
	t.Helper()
	startServe(t, cfg, rt)
	rt.SetMounts("c", mounts...)
	rt.Start("c")
	if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
		t.Fatalf("got %d, %q; want 0 while the container cannot write its config", code, stderr)
	}
}

// requireRefusedAsWritable fails the test unless code and stderr are the
// refusal of a command because the container can write the host path
// that lies suffix below its workspace: 126, naming the path in the
// container, not the host's, and how to turn the check off.
func requireRefusedAsWritable(t *testing.T, cfg serveConfig, code int, stderr, suffix string) {
	t.Helper()
	if want := cfg.containerWorkspace + suffix; code != protocol.ExitRejected || !strings.Contains(stderr, want) {
		t.Errorf("got %d, %q; want %d naming %s", code, stderr, protocol.ExitRejected, want)
	}
	if strings.Contains(stderr, cfg.workspace) {
		t.Errorf("the refusal %q names the host workspace %s", stderr, cfg.workspace)
	}
	if !strings.Contains(stderr, "mount_check: false in the host settings file, HOSTRUNNER_MOUNT_CHECK=false or --no-mount-check") {
		t.Errorf("the refusal %q does not say how to turn the check off", stderr)
	}
}

// requireMarker fails the test unless the command `touch name` has run on
// the host of cfg, or, with want false, unless it has not.
func requireMarker(t *testing.T, cfg serveConfig, name string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(cfg.workspace, name))
	if got := err == nil; got != want {
		t.Fatalf("%s exists on the host: %v (%v); want %v", name, got, err, want)
	}
}

// An armed container that loses a mount, or has it turned read-write,
// between two requests is refused the second: serve --watch checks the live
// mount table before every request, so a run and a dry run alike are
// refused with 126, naming the path writable in the container, not the
// host's, and nothing runs. It holds for the .devcontainer directory, the
// rules file and the workspace settings file, the last two kept read-only
// as files.
func TestServeWatchRefusesRequestsOnceALiveMountCanWriteTheConfig(t *testing.T) {
	tests := []struct {
		name   string
		adjust func(cfg *serveConfig)
		mounts func(cfg serveConfig) []watch.Mount
		after  func(mounts []watch.Mount) []watch.MountPoint // the live mount table once it changed
		want   string                                        // the container path the refusal names, under the container workspace
	}{
		{
			name:   "the .devcontainer mount dropped",
			adjust: func(*serveConfig) {},
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg)}
			},
			after: func(mounts []watch.Mount) []watch.MountPoint { return mountPoints(mounts[0]) },
			want:  "/.devcontainer",
		},
		{
			name:   "the .devcontainer mount turned read-write",
			adjust: func(*serveConfig) {},
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg)}
			},
			after: func(mounts []watch.Mount) []watch.MountPoint {
				return mountPoints(mounts[0], writable(mounts[1]))
			},
			want: "/.devcontainer",
		},
		{
			name: "the mount of a rules file outside it dropped",
			adjust: func(cfg *serveConfig) {
				cfg.RulesFile = moveFile(t, cfg.RulesFile, filepath.Join(cfg.workspace, "rules.yaml"))
			},
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyFile(cfg, cfg.RulesFile)}
			},
			after: func(mounts []watch.Mount) []watch.MountPoint { return mountPoints(mounts[:2]...) },
			want:  "/rules.yaml",
		},
		{
			name: "the mount of a rules file outside it turned read-write",
			adjust: func(cfg *serveConfig) {
				cfg.RulesFile = moveFile(t, cfg.RulesFile, filepath.Join(cfg.workspace, "rules.yaml"))
			},
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyFile(cfg, cfg.RulesFile)}
			},
			after: func(mounts []watch.Mount) []watch.MountPoint {
				return mountPoints(mounts[0], mounts[1], writable(mounts[2]))
			},
			want: "/rules.yaml",
		},
		{
			name: "the mount of a workspace settings file outside it dropped",
			adjust: func(cfg *serveConfig) {
				cfg.workspaceSettings = filepath.Join(cfg.workspace, "hostrunner.yaml")
				if err := os.WriteFile(cfg.workspaceSettings, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			mounts: func(cfg serveConfig) []watch.Mount {
				return []watch.Mount{workspaceMount(cfg), readOnlyDevcontainer(cfg), readOnlyFile(cfg, cfg.workspaceSettings)}
			},
			after: func(mounts []watch.Mount) []watch.MountPoint { return mountPoints(mounts[:2]...) },
			want:  "/hostrunner.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := liveMountConfig(t)
			tt.adjust(&cfg)
			mounts := tt.mounts(cfg)
			rt := &watchtest.Runtime{}
			armedContainer(t, cfg, rt, mounts...)
			if code, stderr := runServedOutput(cfg, "touch", "before"); code != 0 {
				t.Fatalf("got %d, %q; want 0 before the mount changed", code, stderr)
			}
			rt.SetMountTable("c", tt.after(mounts)...)
			for _, argv := range [][]string{{"touch", "marker"}, {"--dry-run", "touch", "marker"}} {
				code, stderr := runServedOutput(cfg, argv...)
				requireRefusedAsWritable(t, cfg, code, stderr, tt.want)
			}
			requireMarker(t, cfg, "marker", false)
		})
	}
}

// A container whose live mount table has a read-write mount of the
// .devcontainer directory elsewhere, which its inspection reported all
// along, is refused: the table decides what is mounted, so the mount that
// was gone does not count until it is back, and then it does wherever in
// the container it lies.
func TestServeWatchRefusesAWritableMountOfAProtectedDirectoryElsewhere(t *testing.T) {
	cfg := liveMountConfig(t)
	ws, dc := workspaceMount(cfg), readOnlyDevcontainer(cfg)
	elsewhere := watch.Mount{Source: dc.Source, Destination: "/mnt/config"}
	rt := &watchtest.Runtime{}
	rt.SetMountTable("c", mountPoints(ws, dc)...)
	armedContainer(t, cfg, rt, ws, dc, elsewhere)
	if code, stderr := runServedOutput(cfg, "touch", "before"); code != 0 {
		t.Fatalf("got %d, %q; want 0 while the container holds no such mount", code, stderr)
	}
	rt.SetMountTable("c", mountPoints(ws, dc, elsewhere)...)
	code, stderr := runServedOutput(cfg, "touch", "marker")
	if code != protocol.ExitRejected || !strings.Contains(stderr, "/mnt/config") || strings.Contains(stderr, cfg.workspace) {
		t.Errorf("got %d, %q; want %d naming /mnt/config, not the host workspace", code, stderr, protocol.ExitRejected)
	}
	requireMarker(t, cfg, "marker", false)
}

// tableSwapRuntime is a watchtest.Runtime whose MountTable counts its calls
// and can run a function once the nth call after armSwap has read its
// table. serve --watch reads the table once per armed running container
// for each check it makes, nothing else reading it between attaches, so
// such a function runs right after one chosen check and before the next.
type tableSwapRuntime struct {
	*watchtest.Runtime
	mu     sync.Mutex
	calls  int
	swapAt int // the call after which swap runs; 0: none
	swap   func()
}

// MountTable reads the table as watchtest.Runtime does, then runs the swap
// armed for this call, if any, which changes what the next read returns.
func (r *tableSwapRuntime) MountTable(ctx context.Context, pid int) ([]watch.MountPoint, error) {
	table, err := r.Runtime.MountTable(ctx, pid)
	r.mu.Lock()
	r.calls++
	swap := r.swap
	if r.calls != r.swapAt {
		swap = nil
	}
	r.mu.Unlock()
	if swap != nil {
		swap()
	}
	return table, err
}

// armSwap makes the nth MountTable call from now run swap once it has read
// its table.
func (r *tableSwapRuntime) armSwap(n int, swap func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.swapAt, r.swap = r.calls+n, swap
}

// count returns how many times MountTable has been called.
func (r *tableSwapRuntime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// startServeOn runs serve --watch of cfg over rt, which any runtime may
// be, until the test ends, once its sockets accept connections.
func startServeOn(t *testing.T, cfg serveConfig, rt watch.Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		_ = serve(ctx, cfg, slog.New(slog.DiscardHandler), []watch.Runtime{rt})
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
	})
	waitListening(t, cfg.socket)
	waitListening(t, launch.ApprovalSocket(filepath.Dir(cfg.socket)))
}

// A mount that is gone from the container after a run passed the check as
// it arrived, but before its command would start, is caught by the check
// made right before the start: the request is refused with 126 and nothing
// runs. The change is made deterministically, as the table read by the
// arrival check is returned: the very next read is the check before the
// start, so that two reads made the request. A dry run reads the table
// once, as its last check, so a mount lost before it is caught there.
func TestServeWatchChecksTheLiveMountsAgainRightBeforeTheStart(t *testing.T) {
	for _, tc := range []struct {
		argv   []string
		swapAt int // the read of the request after which the mount is lost; 0: before the request
		reads  int // the reads the request makes
	}{
		{argv: []string{"touch", "marker"}, swapAt: 1, reads: 2},
		{argv: []string{"--dry-run", "touch", "marker"}, reads: 1},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			cfg := liveMountConfig(t)
			ws, dc := workspaceMount(cfg), readOnlyDevcontainer(cfg)
			rt := &tableSwapRuntime{Runtime: &watchtest.Runtime{}}
			startServeOn(t, cfg, rt)
			rt.SetMounts("c", ws, dc)
			rt.Start("c")
			if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
				t.Fatalf("got %d, %q; want 0 before the mount is lost", code, stderr)
			}
			lose := func() { rt.SetMountTable("c", mountPoints(ws)...) }
			before := rt.count()
			if tc.swapAt == 0 {
				lose()
			} else {
				rt.armSwap(tc.swapAt, lose)
			}
			code, stderr := runServedOutput(cfg, tc.argv...)
			requireRefusedAsWritable(t, cfg, code, stderr, "/.devcontainer")
			if calls := rt.count() - before; calls != tc.reads {
				t.Errorf("the mount table was read %d times for the request, want %d", calls, tc.reads)
			}
			requireMarker(t, cfg, "marker", false)
		})
	}
}

// Every armed container is checked: when one of the containers of a
// Compose project cannot be trusted any more, a request is refused whatever
// the others mount, and runs again once it can, while one that does not
// mount the workspace at all is never the reason.
func TestServeWatchRefusesRequestsWhileAnyArmedContainerCanWriteItsConfig(t *testing.T) {
	cfg := liveMountConfig(t)
	ws, dc := workspaceMount(cfg), readOnlyDevcontainer(cfg)
	var log logBuffer
	rt := &watchtest.Runtime{}
	startLoggingServe(t, cfg, slog.New(slog.NewTextHandler(&log, nil)), rt)
	rt.SetMounts("app", ws, dc)
	rt.Start("app")
	if code, stderr := runOnceChecked(t, cfg, "true"); code != 0 {
		t.Fatalf("got %d, %q; want 0 for the first container", code, stderr)
	}
	// Each one joins the arm before the next starts, so that both are
	// armed once the second has.
	rt.SetMounts("db", watch.Mount{Source: t.TempDir(), Destination: "/var/lib/data"})
	rt.Start("db")
	waitForLogCount(t, &log, "a container joined the armed containers", 1)
	rt.SetMounts("web", ws, dc)
	rt.Start("web")
	waitForLogCount(t, &log, "a container joined the armed containers", 2)

	if code, stderr := runServedOutput(cfg, "touch", "before"); code != 0 {
		t.Fatalf("got %d, %q; want 0 while no container can write its config", code, stderr)
	}
	rt.SetMountTable("web", mountPoints(ws)...)
	code, stderr := runServedOutput(cfg, "touch", "marker")
	requireRefusedAsWritable(t, cfg, code, stderr, "/.devcontainer")
	requireMarker(t, cfg, "marker", false)

	rt.SetMountTable("web", mountPoints(ws, dc)...)
	if code, stderr := runServedOutput(cfg, "touch", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want 0 once the container mounts .devcontainer read-only again", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
}

// waitForLogCount waits until log holds text n times, for 5 s at most.
func waitForLogCount(t *testing.T, log *logBuffer, text string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); strings.Count(log.String(), text) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log lacks %d of %q:\n%s", n, text, log.String())
		}
	}
}

// A container that mounts nothing of the workspace, as one that only keeps
// data, cannot write the config: serve --watch runs the commands its rules
// allow.
func TestServeWatchRunsCommandsInAContainerThatDoesNotMountTheWorkspace(t *testing.T) {
	cfg := liveMountConfig(t)
	rt := &watchtest.Runtime{}
	startServe(t, cfg, rt)
	rt.SetMounts("c", watch.Mount{Source: t.TempDir(), Destination: "/var/lib/data"})
	rt.Start("c")
	if code, stderr := runOnceChecked(t, cfg, "touch", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want 0", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
}

// A live mount table serve --watch cannot read leaves it unable to tell
// whether the container can write its config: it refuses every run and dry
// run with 126, saying so and not naming the host, and runs the commands its
// rules allow again once the table can be read.
func TestServeWatchRefusesRequestsWhileTheLiveMountTableCannotBeRead(t *testing.T) {
	cfg := liveMountConfig(t)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg))
	rt.FailMountTable("c", true)
	for _, argv := range [][]string{{"touch", "marker"}, {"--dry-run", "touch", "marker"}} {
		code, stderr := runServedOutput(cfg, argv...)
		if code != protocol.ExitRejected || !strings.Contains(stderr, "the daemon cannot read the container's mounts to check them") {
			t.Errorf("%q: got %d, %q; want %d, the mounts unreadable", argv, code, stderr, protocol.ExitRejected)
		}
		if strings.Contains(stderr, cfg.workspace) {
			t.Errorf("%q: the refusal %q names the host workspace %s", argv, stderr, cfg.workspace)
		}
	}
	requireMarker(t, cfg, "marker", false)
	rt.FailMountTable("c", false)
	if code, stderr := runServedOutput(cfg, "touch", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want 0 once the table can be read", code, stderr)
	}
}

// A container that restarts keeps being checked: its old process, whose
// table can no longer be read, is not the reason for a refusal, the runtime
// is inspected again, and the request is judged by the mounts and the table
// of the new process.
func TestServeWatchChecksARestartedContainerByItsNewProcess(t *testing.T) {
	tests := []struct {
		name     string
		restart  func(cfg serveConfig, rt *watchtest.Runtime) // between the stop and the start
		wantCode int
	}{
		{
			name:     "with the mounts it had",
			restart:  func(serveConfig, *watchtest.Runtime) {},
			wantCode: 0,
		},
		{
			name: "with mounts that can write its config",
			restart: func(cfg serveConfig, rt *watchtest.Runtime) {
				rt.SetMounts("c", workspaceMount(cfg))
				rt.SetMountTable("c", mountPoints(workspaceMount(cfg))...)
			},
			wantCode: protocol.ExitRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := liveMountConfig(t)
			rt := &watchtest.Runtime{}
			armedContainer(t, cfg, rt, workspaceMount(cfg), readOnlyDevcontainer(cfg))
			rt.Stop("c")
			tt.restart(cfg, rt)
			rt.Start("c")
			code, stderr := runServedOutput(cfg, "touch", "marker")
			if code != tt.wantCode {
				t.Fatalf("got %d, %q; want %d", code, stderr, tt.wantCode)
			}
			if tt.wantCode != 0 {
				requireRefusedAsWritable(t, cfg, code, stderr, "/.devcontainer")
			}
			requireMarker(t, cfg, "marker", tt.wantCode == 0)
		})
	}
}

// With the mount check off (mount_check false), serve --watch looks at no
// mount table: a run and a dry run go ahead although the .devcontainer mount
// is gone from the container.
func TestServeWatchWithTheMountCheckOffRunsCommandsWhenAMountIsLost(t *testing.T) {
	cfg := liveMountConfig(t)
	cfg.MountCheck = false
	ws, dc := workspaceMount(cfg), readOnlyDevcontainer(cfg)
	rt := &watchtest.Runtime{}
	startServe(t, cfg, rt)
	rt.SetMounts("c", ws, dc)
	rt.Start("c")
	rt.SetMountTable("c", mountPoints(ws)...)
	for _, argv := range [][]string{{"--dry-run", "touch", "marker"}, {"touch", "marker"}} {
		if code, stderr := runServedOutput(cfg, argv...); code != 0 {
			t.Fatalf("%q: got %d, %q; want 0", argv, code, stderr)
		}
	}
	requireMarker(t, cfg, "marker", true)
}

// humanSession is an approval session serving the daemon of a serve, as a
// human on the host drives it: a test reads what it prints and types the
// answers.
type humanSession struct {
	in   io.Writer
	out  *logBuffer
	seen int // how much of out the test has read
}

// startHumanSession starts an approval session for the daemon of cfg and
// waits until it serves it; it ends with the test.
func startHumanSession(t *testing.T, cfg serveConfig) *humanSession {
	t.Helper()
	pr, pw := io.Pipe()
	s := &humanSession{in: pw, out: &logBuffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = approval.Run(ctx, approval.Config{Socket: launch.ApprovalSocket(filepath.Dir(cfg.socket)), TailLines: settings.DefaultApproveTailLines}, pr, s.out)
	}()
	t.Cleanup(func() {
		cancel()
		pw.Close()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Error("the approval session did not end")
		}
	})
	s.waitFor(t, "waiting for requests")
	return s
}

// waitFor waits until the session prints text after what the test has
// read, for 5 s at most.
func (s *humanSession) waitFor(t *testing.T, text string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		out := s.out.String()
		if i := strings.Index(out[s.seen:], text); i >= 0 {
			s.seen += i + len(text)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session did not print %q; it printed %q", text, out[s.seen:])
		}
	}
}

// answer types line and Enter.
func (s *humanSession) answer(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// servedResult is what a hostrun printed and exited with.
type servedResult struct {
	code   int
	stderr string
}

// askServed runs argv with --ask in the container workspace of cfg's serve
// in the background; the result arrives on the channel.
func askServed(cfg serveConfig, argv ...string) <-chan servedResult {
	ch := make(chan servedResult, 1)
	go func() {
		code, stderr := runServedOutput(cfg, append([]string{"--ask"}, argv...)...)
		ch <- servedResult{code, stderr}
	}()
	return ch
}

// awaitServed waits for a hostrun started with askServed, for 10 s at most.
func awaitServed(t *testing.T, ch <-chan servedResult) servedResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("hostrun --ask did not finish")
		return servedResult{}
	}
}

// An --ask that arrives while the live mount check fails is refused with
// 126 before it is queued: the approval session is shown no request, only
// the notice of the failed check.
func TestServeWatchRefusesAnAskBeforeQueuingItWhenTheLiveMountsFail(t *testing.T) {
	cfg := mountCheckConfig(t)
	ws := workspaceMount(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, ws, readOnlyDevcontainer(cfg))
	s := startHumanSession(t, cfg)
	rt.SetMountTable("c", mountPoints(ws)...)
	got := awaitServed(t, askServed(cfg, "touch", "marker"))
	requireRefusedAsWritable(t, cfg, got.code, got.stderr, "/.devcontainer")
	s.waitFor(t, mountCheckNotice)
	if out := s.out.String(); strings.Contains(out, "── request") {
		t.Fatalf("the session was shown a request:\n%s", out)
	}
	requireMarker(t, cfg, "marker", false)
}

// An --ask whose container loses a mount while the human decides is
// refused with 126 once the human approves it: the check made after the
// answer, right before the approved run starts, finds it, and nothing runs.
func TestServeWatchRefusesAnApprovedRunWhenAMountIsLostWhileTheHumanDecides(t *testing.T) {
	cfg := mountCheckConfig(t)
	ws := workspaceMount(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, ws, readOnlyDevcontainer(cfg))
	s := startHumanSession(t, cfg)
	ch := askServed(cfg, "touch", "marker")
	s.waitFor(t, answerPrompt)
	rt.SetMountTable("c", mountPoints(ws)...)
	s.answer(t, "o")
	got := awaitServed(t, ch)
	requireRefusedAsWritable(t, cfg, got.code, got.stderr, "/.devcontainer")
	requireMarker(t, cfg, "marker", false)
}

// A repeat that a standing approval lets through is checked like any other
// request: once its container loses a mount, the repeat, with or without
// --ask, is refused with 126 and does not run, and it runs again once the
// mount is back, the approval standing meanwhile.
func TestServeWatchChecksTheRepeatsOfAStandingApproval(t *testing.T) {
	cfg := mountCheckConfig(t)
	ws, dc := workspaceMount(cfg), readOnlyDevcontainer(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, ws, dc)
	s := startHumanSession(t, cfg)
	ch := askServed(cfg, "touch", "marker")
	s.waitFor(t, answerPrompt)
	s.answer(t, "d")
	if got := awaitServed(t, ch); got.code != 0 {
		t.Fatalf("got %d, %q; want the approved command to run", got.code, got.stderr)
	}
	s.waitFor(t, "approved as standing (until the daemon stops) · run ")
	s.waitFor(t, " ──\n")
	requireMarker(t, cfg, "marker", true)
	if err := os.Remove(filepath.Join(cfg.workspace, "marker")); err != nil {
		t.Fatal(err)
	}

	rt.SetMountTable("c", mountPoints(ws)...)
	for _, argv := range [][]string{{"touch", "marker"}, {"--ask", "touch", "marker"}} {
		code, stderr := runServedOutput(cfg, argv...)
		requireRefusedAsWritable(t, cfg, code, stderr, "/.devcontainer")
	}
	requireMarker(t, cfg, "marker", false)

	rt.SetMountTable("c", mountPoints(ws, dc)...)
	if code, stderr := runServedOutput(cfg, "touch", "marker"); code != 0 {
		t.Fatalf("got %d, %q; want the repeat to run once the mount is back", code, stderr)
	}
	requireMarker(t, cfg, "marker", true)
}

// The approval session is told once when a per-request check starts
// failing: not again for the requests the same failure refuses, but again
// when the same failure follows a check that passed, and for another one.
func TestServeWatchTellsTheApprovalSessionOfEachChangeOfTheLiveMountCheck(t *testing.T) {
	cfg := liveMountConfig(t)
	ws, dc := workspaceMount(cfg), readOnlyDevcontainer(cfg)
	rt := &watchtest.Runtime{}
	armedContainer(t, cfg, rt, ws, dc)
	s := startHumanSession(t, cfg)
	writableNotice := mountCheckNotice + "container c can write "
	steps := []struct {
		name     string
		change   func()
		wantCode int
	}{
		{"the .devcontainer mount dropped", func() { rt.SetMountTable("c", mountPoints(ws)...) }, protocol.ExitRejected},
		{"the same failure again", func() {}, protocol.ExitRejected},
		{"the mount back", func() { rt.SetMountTable("c", mountPoints(ws, dc)...) }, 0},
		{"dropped again", func() { rt.SetMountTable("c", mountPoints(ws)...) }, protocol.ExitRejected},
	}
	for _, step := range steps {
		step.change()
		if code, stderr := runServedOutput(cfg, "true"); code != step.wantCode {
			t.Fatalf("%s: got %d, %q; want %d", step.name, code, stderr, step.wantCode)
		}
	}
	// A failure of another kind ends the run: notices come in order, so
	// once it is shown, so is every notice before it.
	rt.FailMountTable("c", true)
	if code, stderr := runServedOutput(cfg, "true"); code != protocol.ExitRejected {
		t.Fatalf("got %d, %q; want %d while the table cannot be read", code, stderr, protocol.ExitRejected)
	}
	s.waitFor(t, mountCheckNotice+"container c: cannot tell whether")
	out := s.out.String()
	if n := strings.Count(out, writableNotice); n != 2 {
		t.Errorf("the session was told of the writable .devcontainer %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, mountCheckNotice); n != 3 {
		t.Errorf("the session was told of %d failed checks, want 3:\n%s", n, out)
	}
}
