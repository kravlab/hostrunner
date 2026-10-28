package client_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/stop"
	"github.com/kravlab/hostrunner/internal/transport"
)

// launchStop runs `hostrunner stop` as cfg configures it, answering on
// the returned session's in, and returns at once; interrupts reach it
// through cfg.Interrupts.
func launchStop(t *testing.T, cfg stop.Config) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	s := &session{in: pw, out: &lockedBuffer{}, ended: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.ended)
		s.err = stop.Run(ctx, cfg, pr, s.out)
	}()
	t.Cleanup(func() {
		cancel()
		pw.Close()
		select {
		case <-s.ended:
		case <-time.After(10 * time.Second):
			t.Error("the stop did not end")
		}
	})
	return s
}

// runStop runs `hostrunner stop` as cfg configures it, with nothing to
// read, and returns what it printed and returned.
func runStop(t *testing.T, cfg stop.Config) (string, error) {
	t.Helper()
	s := launchStop(t, cfg)
	s.in.Close()
	waitEnded(t, s)
	return s.out.String(), s.err
}

// stillServes fails the test unless h's daemon answers on its approval
// socket.
func (h harness) stillServes(t *testing.T) {
	t.Helper()
	if _, err := daemonlist.Probe(context.Background(), h.approvalSocket); err != nil {
		t.Fatalf("the daemon is gone: %v", err)
	}
}

// fakeDaemonFor answers every hello on the approval socket of the runtime
// directory dir with a daemon serving workspace, which stops; it stops
// nothing.
func fakeDaemonFor(t *testing.T, dir, workspace string) {
	t.Helper()
	l, err := transport.Unix{Path: launch.ApprovalSocket(dir)}.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if f, err := protocol.ReadFrame(c); err == nil && f.Type == protocol.FrameHello {
					_ = protocol.WriteJSON(c, protocol.FrameDaemonInfo, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: workspace, Stops: true})
				}
			}(c)
		}
	}()
}

func TestStopStopsTheDaemonOfTheCurrentDirectory(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	otherBase, _ := newBase(t)
	other := startEchoDaemon(t, slog.New(slog.DiscardHandler), otherBase)

	out, err := runStop(t, stop.Config{Root: root, Cwd: filepath.Join(h.hostRoot, "sub")})
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	if want := "hostrunner: stopped " + base + "\n"; out != want {
		t.Fatalf("printed %q, want %q", out, want)
	}
	h.gone(t)
	h.waitServed(t)
	other.stillServes(t)
}

func TestStopWithoutADaemonForTheCurrentDirectoryListsThem(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)

	out, err := runStop(t, stop.Config{Root: root, Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no daemon serves") {
		t.Fatalf("got %v, want no daemon for the directory", err)
	}
	if !strings.Contains(out, base+"   "+h.hostRoot) {
		t.Fatalf("printed %q, want the daemons found", out)
	}
	h.stillServes(t)
}

func TestStopRefusesSeveralDaemonsForTheCurrentDirectory(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	twin, _ := newBase(t)
	fakeDaemonFor(t, twin, h.hostRoot)

	out, err := runStop(t, stop.Config{Root: root, Cwd: h.hostRoot})
	if err == nil || !strings.Contains(err.Error(), "2 daemons serve") {
		t.Fatalf("got %v, want several daemons for the directory", err)
	}
	if !strings.Contains(out, base+"   "+h.hostRoot) || !strings.Contains(out, twin+"   "+h.hostRoot) {
		t.Fatalf("printed %q, want both daemons", out)
	}
	h.stillServes(t)
}

func TestStopDirStopsThatDaemon(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	if out, err := runStop(t, stop.Config{Dir: base, Cwd: t.TempDir()}); err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	h.waitServed(t)
}

func TestStopDirWithoutADaemonFails(t *testing.T) {
	dir := t.TempDir()
	_, err := runStop(t, stop.Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "no daemon answers") {
		t.Fatalf("got %v, want no daemon", err)
	}
}

func TestStopAllStopsEveryDaemon(t *testing.T) {
	base1, root := newBase(t)
	h1 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base1)
	base2, _ := newBase(t)
	h2 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base2)

	out, err := runStop(t, stop.Config{Root: root, All: true})
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	for _, base := range []string{base1, base2} {
		if !strings.Contains(out, "hostrunner: stopped "+base+"\n") {
			t.Errorf("printed %q, want %s stopped", out, base)
		}
	}
	h1.waitServed(t)
	h2.waitServed(t)
}

func TestStopAllWithoutDaemonsDoesNothing(t *testing.T) {
	if out, err := runStop(t, stop.Config{Root: t.TempDir(), All: true}); err != nil || out != "" {
		t.Fatalf("got %v, printed %q; want nothing", err, out)
	}
}

func TestStopChooseStopsTheChosenDaemon(t *testing.T) {
	base1, root := newBase(t)
	h1 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base1)
	base2, _ := newBase(t)
	h2 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base2)
	chosen, kept := h2, h1
	if base1 > base2 { // the list is ordered by socket
		chosen, kept = h1, h2
	}

	s := launchStop(t, stop.Config{Root: root, Choose: true, Cwd: t.TempDir(), Commands: stop.Commands{Terminal: true}})
	s.waitFor(t, "choose [1-2, q quit] ? ")
	s.answer(t, "2")
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	chosen.waitServed(t)
	kept.stillServes(t)
}

func TestStopChooseQuitStopsNothing(t *testing.T) {
	base, root := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	h.startSession(t) // served: stopping takes no session over, so the list does not say
	s := launchStop(t, stop.Config{Root: root, Choose: true, Cwd: h.hostRoot, Commands: stop.Commands{Terminal: true}})
	s.waitFor(t, "choose [1-1, Enter = 1, q quit] ? ")
	if strings.Contains(s.out.String(), "served by another session") {
		t.Errorf("the list shows the serving session: %q", s.out.String())
	}
	s.answer(t, "q")
	waitEnded(t, s)
	if s.err == nil {
		t.Fatal("quitting the list stopped")
	}
	h.stillServes(t)
}

func TestStopOfABusyDaemonWithoutATerminalFails(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	pid := h.startSleeper(t)

	out, err := runStop(t, stop.Config{Dir: base})
	if err == nil || !strings.Contains(err.Error(), "--kill or --wait") {
		t.Fatalf("got %v, want an error naming --kill and --wait", err)
	}
	if !strings.Contains(out, "  sh -c 'echo $$; exec sleep 60'   in "+containerRoot+"\n") {
		t.Fatalf("printed %q, want the running command", out)
	}
	h.stillServes(t)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the running command is gone: %v", err)
	}
}

func TestStopKillKillsRunningCommands(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	pid := h.startSleeper(t)

	out, err := runStop(t, stop.Config{Dir: base, Commands: stop.Commands{Answer: stop.Kill}})
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	h.gone(t)
	if !strings.Contains(out, "exec sleep 60") || !strings.HasSuffix(out, "hostrunner: stopped "+base+"\n") {
		t.Fatalf("printed %q", out)
	}
	h.waitServed(t)
	waitForExit(t, pid)
}

func TestStopWaitLetsRunningCommandsFinish(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	conn := h.startRaw(t, "sh", "-c", "echo $$; exec sleep 1")
	readStdoutLine(t, conn)

	start := time.Now()
	out, err := runStop(t, stop.Config{Dir: base, Commands: stop.Commands{Answer: stop.Wait}})
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	if strings.Contains(out, "Ctrl+C") {
		t.Errorf("printed %q: no terminal to answer Ctrl+C on", out)
	}
	h.gone(t)
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Fatalf("the stop returned after %v, before the command finished", took)
	}
	if f := statusFrameOf(t, conn); f.Type != protocol.FrameExit {
		t.Fatalf("the command ended with frame %v, want its exit", f.Type)
	}
	h.waitServed(t)
}

func TestStopAsksWhatToDoWithRunningCommands(t *testing.T) {
	for _, tt := range []struct {
		answer  string
		stopped bool
	}{
		{"q", false},
		{"k", true},
	} {
		t.Run(tt.answer, func(t *testing.T) {
			base, _ := newBase(t)
			h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
			pid := h.startSleeper(t)
			s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Terminal: true}})
			s.waitFor(t, "exec sleep 60")
			s.waitFor(t, "[k/w/q] ? ")
			s.answer(t, "x") // not an answer: asked again
			s.waitFor(t, "[k/w/q] ? ")
			s.answer(t, tt.answer)
			waitEnded(t, s)
			if tt.stopped {
				if s.err != nil {
					t.Fatalf("stop: %v", s.err)
				}
				h.waitServed(t)
				waitForExit(t, pid)
				return
			}
			if s.err == nil || !strings.Contains(s.err.Error(), "left running") {
				t.Fatalf("got %v, want the daemon left running", s.err)
			}
			h.stillServes(t)
		})
	}
}

func TestStopInterruptedWhileDrainingAsksAgain(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	pid := h.startSleeper(t)
	interrupts := make(chan os.Signal, 1)
	s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Answer: stop.Wait, Terminal: true, Interrupts: interrupts}})
	s.waitFor(t, "waiting for the running commands")
	interrupts <- os.Interrupt
	s.waitFor(t, "[k/w] ? ")
	s.answer(t, "w")
	s.waitFor(t, "waiting for the running commands")
	interrupts <- os.Interrupt
	s.waitFor(t, "[k/w] ? ")
	s.answer(t, "k")
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	h.waitServed(t)
	waitForExit(t, pid)
}

func TestStopReportsTheApprovalsThatEnd(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	sess := h.startSession(t)
	h.grantStanding(t, sess, false, "touch", "a")
	h.grantAs(t, sess, sessionAnswer, false, "touch", "b")

	out, err := runStop(t, stop.Config{Dir: base})
	if err != nil {
		t.Fatalf("stop: %v; printed %q", err, out)
	}
	if !strings.Contains(out, "1 standing and 1 session approvals end with the daemon\n") {
		t.Fatalf("printed %q, want the approvals that end", out)
	}
	h.waitServed(t)
}

func TestStopWaitsForTheRuntimeDirectoryLock(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	unlock, err := launch.LockDir(base)
	if err != nil {
		t.Fatal(err)
	}
	s := launchStop(t, stop.Config{Dir: base})
	s.in.Close()
	select {
	case <-s.ended:
		unlock()
		t.Fatalf("the stop ran while `up` held the lock: %v", s.err)
	case <-time.After(300 * time.Millisecond):
	}
	h.stillServes(t)
	if !strings.Contains(s.out.String(), "waiting for another hostrunner up or stop on "+base) {
		t.Errorf("printed %q, want it waiting for the lock", s.out.String())
	}
	unlock()
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v", s.err)
	}
	h.waitServed(t)
}

// gone fails the test unless nothing accepts on h's sockets any more.
func (h harness) gone(t *testing.T) {
	t.Helper()
	for _, tr := range []transport.Unix{{Path: h.approvalSocket}, h.transport} {
		if c, err := tr.Dial(context.Background()); err == nil {
			c.Close()
			t.Fatalf("%s still accepts", tr.Path)
		}
	}
}

func TestStopWithoutAnyDaemonSaysSo(t *testing.T) {
	root := t.TempDir()
	out, err := runStop(t, stop.Config{Root: root, Cwd: root})
	if err == nil || err.Error() != "no daemons run under "+root {
		t.Fatalf("got %v, want no daemons", err)
	}
	if out != "" {
		t.Fatalf("printed %q, want nothing more", out)
	}
}

func TestStopAllGoesOnPastADaemonThatFails(t *testing.T) {
	base1, root := newBase(t)
	h1 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base1)
	base2, _ := newBase(t)
	h2 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base2)
	h1.startSleeper(t)

	out, err := runStop(t, stop.Config{Root: root, All: true})
	if err == nil || err.Error() != "1 of 2 daemons not stopped" {
		t.Fatalf("got %v, want one daemon not stopped; printed %q", err, out)
	}
	if !strings.Contains(out, "hostrunner: "+base1+" runs commands: give --kill or --wait to stop it\n") || !strings.Contains(out, "hostrunner: stopped "+base2+"\n") {
		t.Fatalf("printed %q", out)
	}
	h1.stillServes(t)
	h2.waitServed(t)
}

func TestStopAnswerWaitLetsCommandsFinish(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	conn := h.startRaw(t, "sh", "-c", "echo $$; exec sleep 1")
	readStdoutLine(t, conn)
	s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Terminal: true}})
	s.waitFor(t, "[k/w/q] ? ")
	s.answer(t, "w")
	s.waitFor(t, base+": waiting for the running commands to finish (Ctrl+C asks again)")
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	if f := statusFrameOf(t, conn); f.Type != protocol.FrameExit {
		t.Fatalf("the command ended with frame %v, want its exit", f.Type)
	}
	h.waitServed(t)
}

// A daemon that stops by itself while the human is asked counts as
// stopped.
func TestStopOfADaemonThatStopsWhileAsked(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	h.startSleeper(t)
	s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Terminal: true}})
	s.waitFor(t, "[k/w/q] ? ")
	h.stop()
	s.answer(t, "k")
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	if !strings.HasSuffix(s.out.String(), "hostrunner: stopped "+base+"\n") {
		t.Fatalf("printed %q", s.out.String())
	}
}

// A daemon that stops by itself while the human is asked ends the
// question: there is nothing left to answer for, and it counts as stopped.
func TestStopOfADaemonThatStopsWhileAskedDropsTheQuestion(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	h.startSleeper(t)
	s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Terminal: true}})
	s.waitFor(t, "[k/w/q] ? ")
	h.stop()
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	if !strings.HasSuffix(s.out.String(), "hostrunner: stopped "+base+"\n") {
		t.Fatalf("printed %q", s.out.String())
	}
}

// Ctrl+C at a question stops nothing more, with --all too.
func TestStopInterruptedAtTheQuestionStopsNothingMore(t *testing.T) {
	base1, root := newBase(t)
	h1 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base1)
	base2, _ := newBase(t)
	h2 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base2)
	h1.startSleeper(t)
	h2.startSleeper(t)
	interrupts := make(chan os.Signal, 1)
	s := launchStop(t, stop.Config{Root: root, All: true, Commands: stop.Commands{Terminal: true, Interrupts: interrupts}})
	s.waitFor(t, "[k/w/q] ? ")
	interrupts <- os.Interrupt
	waitEnded(t, s)
	if s.err == nil || !strings.Contains(s.err.Error(), "interrupted") {
		t.Fatalf("got %v, want interrupted", s.err)
	}
	h1.stillServes(t)
	h2.stillServes(t)
}

// Commands that finish while the human is asked whether to kill them end
// the question: the daemon has stopped.
func TestStopDrainEndingDuringTheQuestionStops(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	conn := h.startRaw(t, "sh", "-c", "echo $$; exec sleep 1")
	readStdoutLine(t, conn)
	interrupts := make(chan os.Signal, 1)
	s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Answer: stop.Wait, Terminal: true, Interrupts: interrupts}})
	s.waitFor(t, "waiting for the running commands")
	interrupts <- os.Interrupt
	s.waitFor(t, "[k/w] ? ")
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	h.waitServed(t)
}

func TestStopChooseEnterPicksTheDaemonOfTheCurrentDirectory(t *testing.T) {
	base1, root := newBase(t)
	h1 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base1)
	base2, _ := newBase(t)
	h2 := startEchoDaemon(t, slog.New(slog.DiscardHandler), base2)
	s := launchStop(t, stop.Config{Root: root, Choose: true, Cwd: h2.hostRoot, Commands: stop.Commands{Terminal: true}})
	s.waitFor(t, ", q quit] ? ")
	s.answer(t, "")
	waitEnded(t, s)
	if s.err != nil {
		t.Fatalf("stop: %v; printed %q", s.err, s.out.String())
	}
	h2.waitServed(t)
	h1.stillServes(t)
}

func TestStopInterruptedWhileWaitingForTheLock(t *testing.T) {
	base, _ := newBase(t)
	h := startEchoDaemon(t, slog.New(slog.DiscardHandler), base)
	unlock, err := launch.LockDir(base)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	interrupts := make(chan os.Signal, 1)
	s := launchStop(t, stop.Config{Dir: base, Commands: stop.Commands{Interrupts: interrupts}})
	s.waitFor(t, "waiting for another hostrunner up or stop on "+base)
	interrupts <- os.Interrupt
	waitEnded(t, s)
	if s.err == nil || !strings.Contains(s.err.Error(), "interrupted") {
		t.Fatalf("got %v, want interrupted", s.err)
	}
	h.stillServes(t)
}
