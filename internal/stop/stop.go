// Package stop implements `hostrunner stop`, `hostrunner restart` and
// `hostrunner up --restart`: a human on the host stops a daemon, or
// several, over its approval socket, and decides what becomes of the
// commands it runs: they are killed, or the daemon drains, taking no new
// requests until they finish. The approvals the daemon holds end with it.
// A restart then starts a new daemon in its place, as `hostrunner up`
// does.
package stop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/launch"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
	"github.com/kravlab/hostrunner/internal/watch"
)

// Answer is what becomes of the commands a daemon runs when it is
// stopped.
type Answer int

const (
	Ask  Answer = iota // ask the human: kill, wait or leave the daemon running
	Kill               // kill them (--kill)
	Wait               // drain: let them finish (--wait)
)

// Commands says what becomes of the commands a daemon runs when it is
// stopped, and how the human is asked.
type Commands struct {
	Answer Answer // what becomes of running commands; Ask asks the human, on a Terminal
	// Terminal tells that a human answers on in; without one, a daemon
	// running commands is not stopped unless Answer says what becomes of
	// them.
	Terminal bool
	// Interrupts delivers Ctrl+C: while a daemon drains, it asks whether to
	// kill what still runs; at a question, or while waiting for another
	// `up` or stop, it ends the stop, which stops no more daemons.
	Interrupts <-chan os.Signal
	// StopTimeout bounds how long a daemon may take to close its sockets
	// once it has stopped; zero means defaultStopTimeout.
	StopTimeout time.Duration
}

// Config describes one `hostrunner stop` or `hostrunner restart`. Dir,
// All and Choose exclude each other; without any, the daemon whose host
// workspace holds Cwd is stopped.
type Config struct {
	Root   string // where the daemons' approval sockets are: $XDG_RUNTIME_DIR/hostrunner
	Dir    string // the runtime directory of the daemon to stop
	All    bool   // stop every daemon under Root
	Choose bool   // let the human choose from the daemons under Root
	Cwd    string // the directory the human runs stop in
	Commands
	// Restart, when set, makes it `hostrunner restart`: each daemon
	// stopped is replaced by a new one.
	Restart *Restart

	// up is the new daemon of `up --restart` (see UpRestart), started once
	// the old one has stopped, however it stopped.
	up *launch.Prepared
}

// Restart describes how `hostrunner restart` starts the new daemon of a
// runtime directory, as `hostrunner up` does, for the workspace,
// container workspace and rules file the old one served. Its rules file
// is validated before the old daemon stops: invalid rules leave it
// running. A daemon that saw its armed container stop, and so is about
// to exit, is only stopped, and so is one that stops by itself meanwhile;
// the next `up` starts a new one.
type Restart struct {
	Daemon       string        // hostrunner executable to start as the daemon
	Client       string        // hostrun binary to install into the runtime directory
	ReadyTimeout time.Duration // how long to wait for the new daemon's socket
	// Runtimes are asked whether the container of a daemon of an older
	// hostrunner, which does not say, is running.
	Runtimes []watch.Runtime
}

// runtimeTimeout bounds asking one container runtime about a workspace.
const runtimeTimeout = 5 * time.Second

// endingKind is what follows a daemon stopping.
type endingKind int

const (
	justStop   endingKind = iota // a stop: nothing follows
	notRunning                   // a restart of a daemon whose container stopped: the next `up` starts one
	startNew                     // a restart: the new daemon starts
)

// ending is what follows a daemon stopping, with, for startNew, the new
// daemon, prepared.
type ending struct {
	kind endingKind
	up   *launch.Prepared
}

// defaultStopTimeout is the StopTimeout of a Config without one.
const defaultStopTimeout = 10 * time.Second

// exchangeTimeout bounds one question to a daemon (its hello, what it
// runs, the confirmation of a stop), connecting included.
const exchangeTimeout = 5 * time.Second

// goneTimeout bounds how long a daemon that failed an exchange may take
// to close its sockets for the stop to count it stopped by itself.
const goneTimeout = time.Second

// lockPoll is how often a stop waiting for the runtime directory's lock
// tries it again.
const lockPoll = 100 * time.Millisecond

var (
	// errGone means the daemon is no longer there to be stopped.
	errGone = errors.New("no daemon answers")
	// errInterrupted means the human pressed Ctrl+C, or ctx ended: the
	// stop stops no more daemons.
	errInterrupted = errors.New("interrupted")
	// errInputEnded means in ended before the human answered.
	errInputEnded = errors.New("input ended")
	// errAbandoned means a question was dropped as its answer no longer
	// matters.
	errAbandoned = errors.New("question abandoned")
	// errClosed means the daemon closed the connection.
	errClosed = errors.New("the daemon closed the connection")
)

// Run stops the daemons cfg chooses, writing what it does to out and
// reading the human's answers from in. It returns an error unless every
// daemon chosen was stopped; with All, one failing does not keep the
// others from being stopped, a daemon gone meanwhile is not a failure, and
// Ctrl+C stops no more.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer) error {
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = defaultStopTimeout
	}
	t := &term{in: in, out: out, interrupts: cfg.Interrupts}
	defer t.close()
	chosen, err := t.choose(ctx, cfg)
	if err != nil {
		return err
	}
	failed := 0
	for _, d := range chosen {
		err := t.stopDaemon(ctx, cfg, d.Socket)
		switch {
		case err == nil:
		case !cfg.All || errors.Is(err, errInterrupted):
			return err
		case errors.Is(err, errGone):
		default:
			fmt.Fprintf(out, "hostrunner: %v\n", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d daemons not stopped", failed, len(chosen))
	}
	return nil
}

// choose returns the daemons cfg chooses: the one of cfg.Dir, every one
// under cfg.Root, the one the human chooses from them, or the one whose
// host workspace holds cfg.Cwd. None found, or several holding cfg.Cwd,
// is an error; the daemons found are printed then.
func (t *term) choose(ctx context.Context, cfg Config) ([]daemonlist.Daemon, error) {
	if cfg.Dir != "" {
		socket := launch.ApprovalSocket(cfg.Dir)
		info, err := daemonlist.Probe(ctx, socket)
		if err != nil {
			// A daemon from before approvals has its socket only.
			c, cerr := dial(ctx, launch.Socket(cfg.Dir))
			if cerr != nil {
				return nil, fmt.Errorf("%w on %s: %v", errGone, hostterm.Display(socket), err)
			}
			c.Close()
		}
		return []daemonlist.Daemon{{Socket: socket, Info: info}}, nil
	}
	found := daemonlist.List(ctx, cfg.Root)
	switch {
	case cfg.All:
		return found, nil
	case len(found) == 0:
		return nil, fmt.Errorf("no daemons run under %s", hostterm.Display(cfg.Root))
	case cfg.Choose:
		l := hostterm.List{Daemons: found, Cwd: cfg.Cwd, AnyVersion: true, HideServed: true}
		prompt := l.Print(t.out)
		for {
			text, err := t.ask(ctx, prompt, nil)
			if err != nil || hostterm.IsQuit(text) {
				return nil, errors.New("nothing stopped")
			}
			if i, ok := l.Pick(text); ok {
				return found[i : i+1], nil
			}
		}
	}
	var holding []daemonlist.Daemon
	for _, d := range found {
		if daemonlist.Holds(d.Info.Workspace, cfg.Cwd) {
			holding = append(holding, d)
		}
	}
	if len(holding) == 1 {
		return holding, nil
	}
	fmt.Fprintln(t.out, "daemons:")
	for _, d := range found {
		fmt.Fprintf(t.out, "  %s   %s\n", hostterm.Display(launch.RuntimeDir(d.Socket)), hostterm.Display(d.Info.Workspace))
	}
	if len(holding) == 0 {
		return nil, fmt.Errorf("no daemon serves %s: choose one with --dir, --all or --choose", hostterm.Display(cfg.Cwd))
	}
	return nil, fmt.Errorf("%d daemons serve %s: choose one with --dir or --choose", len(holding), hostterm.Display(cfg.Cwd))
}

// stopDaemon stops the daemon on the approval socket socket, holding its
// runtime directory's lock meanwhile (also while the human answers and
// the daemon drains, so an `up` starts no daemon beside it), and returns
// once it has closed its sockets. A daemon running commands is stopped as
// cfg.Answer, or the human, says; one that stops by itself meanwhile
// counts as stopped.
func (t *term) stopDaemon(ctx context.Context, cfg Config, socket string) error {
	dir := launch.RuntimeDir(socket)
	name := hostterm.Display(dir)
	unlock, err := t.lock(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	conn, err := dial(ctx, socket)
	if err != nil {
		// A daemon from before approvals has no approval socket: it is
		// reached on its socket, once chosen by its directory.
		if chosenByDir := cfg.Dir != ""; chosenByDir {
			if c, cerr := dial(ctx, launch.Socket(dir)); cerr == nil {
				defer c.Close()
				e, err := t.ending(ctx, cfg, dir, nil)
				if err != nil {
					return err
				}
				return t.stopOlder(ctx, cfg, dir, c, e)
			}
		}
		return fmt.Errorf("%w on %s: %v", errGone, hostterm.Display(socket), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(exchangeTimeout))
	info, err := daemonlist.Hello(conn)
	if err != nil {
		return t.stoppedAnyway(ctx, cfg, dir, err)
	}
	e, err := t.ending(ctx, cfg, dir, &info)
	if err != nil {
		return err
	}
	if !info.Stops {
		return t.stopOlder(ctx, cfg, dir, conn, e)
	}
	list, err := running(conn)
	if err != nil {
		return t.stoppedAnyway(ctx, cfg, dir, err)
	}
	_ = conn.SetDeadline(time.Time{})
	c := watchConn(conn)
	defer c.release()
	if list.Standing > 0 || list.Session > 0 {
		fmt.Fprintf(t.out, "%s: %d standing and %d session approvals end with the daemon\n", name, list.Standing, list.Session)
	}
	drain := false
	if len(list.Commands) > 0 {
		fmt.Fprintf(t.out, "%s runs:\n", name)
		for _, cmd := range list.Commands {
			fmt.Fprintf(t.out, "  %s   in %s\n", hostterm.ArgvLine(cmd.Argv), hostterm.Display(cmd.Cwd))
		}
		drain, err = t.whatOfCommands(ctx, cfg, name, c.closed)
		if errors.Is(err, errAbandoned) {
			return t.stoppedAnyway(ctx, cfg, dir, errClosed)
		}
		if err != nil {
			return err
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(exchangeTimeout))
	if err := protocol.WriteJSON(conn, protocol.FrameStop, protocol.Stop{Drain: drain}); err != nil {
		return t.stoppedAnyway(ctx, cfg, dir, err)
	}
	f, err := c.next(exchangeTimeout)
	if err != nil {
		return t.stoppedAnyway(ctx, cfg, dir, err)
	}
	if f.Type != protocol.FrameStopping {
		return fmt.Errorf("%s: %w", name, daemonlist.ErrorOf(f))
	}
	if drain {
		if err := t.waitDrained(ctx, cfg, c, name); err != nil {
			return err
		}
	} else if err := c.waitClosed(cfg.StopTimeout); err != nil {
		return fmt.Errorf("%s: the daemon did not stop within %v", name, cfg.StopTimeout)
	}
	return t.stopped(ctx, cfg, dir, e)
}

// ending returns what follows the daemon of dir, which said info of itself
// (nil for a daemon from before approvals, which says nothing), stopping:
// for a restart, a new daemon, prepared now so that invalid rules leave
// the old one running, unless the daemon saw its armed container stop and
// is about to exit (for a daemon of an older hostrunner, which does not
// say: unless no container of its workspace runs). A daemon still waiting
// for its container (after an arm, during a rebuild) is replaced by one
// that waits for it the same way. Not knowing what to start, or whether
// the container runs, is an error.
func (t *term) ending(ctx context.Context, cfg Config, dir string, info *protocol.DaemonInfo) (ending, error) {
	if cfg.up != nil {
		return ending{kind: startNew, up: cfg.up}, nil
	}
	r := cfg.Restart
	if r == nil {
		return ending{kind: justStop}, nil
	}
	name := hostterm.Display(dir)
	if info == nil {
		return ending{}, fmt.Errorf("%s: a daemon from before approvals cannot be restarted, as it does not say what it serves: stop it, and the next hostrunner up starts a new one", name)
	}
	p, err := launch.Prepare(launch.Config{
		Dir:                dir,
		Workspace:          info.Workspace,
		ContainerWorkspace: info.ContainerWorkspace,
		Rules:              info.RulesFile,
		Daemon:             r.Daemon,
		Client:             r.Client,
		ReadyTimeout:       r.ReadyTimeout,
		NoReread:           info.NoReread,
	})
	if err != nil {
		return ending{}, fmt.Errorf("%s: %w; the daemon keeps running", name, err)
	}
	stopped := info.ContainerStopped
	if !info.Stops {
		running, err := containerRuns(ctx, r.Runtimes, info.Workspace)
		if err != nil {
			return ending{}, fmt.Errorf("%s: %w; the daemon keeps running", name, err)
		}
		stopped = !running
	}
	if stopped {
		return ending{kind: notRunning}, nil
	}
	return ending{kind: startNew, up: p}, nil
}

// containerRuns asks runtimes whether a container of the host workspace
// workspace is running; no runtime answering is an error.
func containerRuns(ctx context.Context, runtimes []watch.Runtime, workspace string) (bool, error) {
	if len(runtimes) == 0 {
		return false, errors.New("whether its container runs needs docker or podman in PATH")
	}
	var errs []error
	answered := false
	for _, rt := range runtimes {
		qctx, cancel := context.WithTimeout(ctx, runtimeTimeout)
		found, err := rt.Containers(qctx, workspace)
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		answered = true
		if slices.ContainsFunc(found, func(c watch.Container) bool { return c.Running }) {
			return true, nil
		}
	}
	if !answered {
		return false, fmt.Errorf("no container runtime said whether its container runs: %w", errors.Join(errs...))
	}
	return false, nil
}

// lock takes the lock of the runtime directory dir (see launch.LockDir)
// and returns its unlock; while another `up` or stop holds it, it says so
// and waits, until Ctrl+C or ctx ends the wait.
func (t *term) lock(ctx context.Context, dir string) (func(), error) {
	tick := time.NewTicker(lockPoll)
	defer tick.Stop()
	for said := false; ; said = true {
		unlock, busy, err := launch.TryLockDir(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", hostterm.Display(dir), err)
		}
		if !busy {
			return unlock, nil
		}
		if !said {
			fmt.Fprintf(t.out, "waiting for another hostrunner up or stop on %s\n", hostterm.Display(dir))
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w while waiting for %s", errInterrupted, hostterm.Display(dir))
		case <-t.interrupts:
			return nil, fmt.Errorf("%w while waiting for %s", errInterrupted, hostterm.Display(dir))
		case <-tick.C:
		}
	}
}

// dial connects to socket, for exchangeTimeout at most.
func dial(ctx context.Context, socket string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	return transport.Unix{Path: socket}.Dial(ctx)
}

// running asks the daemon on conn what it runs.
func running(conn io.ReadWriter) (protocol.RunningList, error) {
	var list protocol.RunningList
	if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.FrameRunning}); err != nil {
		return list, err
	}
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		return list, err
	}
	if f.Type != protocol.FrameRunningList {
		return list, daemonlist.ErrorOf(f)
	}
	return list, protocol.DecodeJSON(f, &list)
}

// daemonConn is the connection to a daemon being stopped, read by one
// goroutine from the moment it has said what it runs, so that the daemon
// closing it, by stopping or by itself, is seen at once, also while the
// human is being asked.
type daemonConn struct {
	conn   net.Conn
	frames chan protocol.Frame // the frames read, each handed on before the next is read
	// closed is closed once reading fails: the daemon closed the
	// connection, or it broke. Whether the daemon stopped is told by its
	// sockets (see stopped), not by how the reading failed.
	closed chan struct{}
	done   chan struct{} // closed by release
}

// watchConn starts reading conn.
func watchConn(conn net.Conn) *daemonConn {
	c := &daemonConn{conn: conn, frames: make(chan protocol.Frame), closed: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.closed)
		for {
			f, err := protocol.ReadFrame(conn)
			if err != nil {
				return
			}
			select {
			case c.frames <- f:
			case <-c.done:
				return
			}
		}
	}()
	return c
}

// next returns the next frame the daemon sends within timeout: errClosed
// once the connection has closed first, os.ErrDeadlineExceeded once the
// timeout has passed.
func (c *daemonConn) next(timeout time.Duration) (protocol.Frame, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f := <-c.frames:
		return f, nil
	case <-c.closed:
		return protocol.Frame{}, errClosed
	case <-timer.C:
		return protocol.Frame{}, os.ErrDeadlineExceeded
	}
}

// waitClosed waits up to timeout for the connection to close, dropping
// whatever the daemon still sends; it fails with os.ErrDeadlineExceeded.
func (c *daemonConn) waitClosed(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-c.frames:
		case <-c.closed:
			return nil
		case <-timer.C:
			return os.ErrDeadlineExceeded
		}
	}
}

// release lets the reading goroutine end once the connection is closed.
func (c *daemonConn) release() { close(c.done) }

// whatOfCommands returns whether the daemon named name, which runs
// commands, is to drain rather than kill them: as cfg.Answer says, or the
// human. Leaving the daemon running, or no human to ask, is an error; the
// connection closing first (gone closed) drops the question, errAbandoned.
func (t *term) whatOfCommands(ctx context.Context, cfg Config, name string, gone <-chan struct{}) (drain bool, err error) {
	switch cfg.Answer {
	case Kill:
		return false, nil
	case Wait:
		return true, nil
	}
	if !cfg.Terminal {
		return false, fmt.Errorf("%s runs commands: give --kill or --wait to stop it", name)
	}
	text, err := t.pick(ctx, "k kills them, w waits for them to finish, q leaves the daemon running [k/w/q] ? ", "kwq", gone)
	switch {
	case errors.Is(err, errAbandoned):
		return false, err
	case errors.Is(err, errInterrupted):
		return false, fmt.Errorf("%w: %s left running", err, name)
	case err != nil || text == "q":
		return false, fmt.Errorf("%s left running", name)
	}
	return text == "w", nil
}

// waitDrained waits for the daemon named name on c, which drains, to
// close the connection once its commands have finished. On a terminal,
// Ctrl+C asks whether to kill what still runs, a question the daemon
// stopping meanwhile ends; ending the stop otherwise leaves the drain to
// finish on its own.
func (t *term) waitDrained(ctx context.Context, cfg Config, c *daemonConn, name string) error {
	hint := ""
	if cfg.Terminal {
		hint = " (Ctrl+C asks again)"
	}
	for {
		fmt.Fprintf(t.out, "%s: waiting for the running commands to finish%s\n", name, hint)
	wait:
		for {
			select {
			case <-c.frames:
			case <-c.closed:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("%w: %s drains on its own", errInterrupted, name)
			case <-t.interrupts:
				break wait
			}
		}
		if !cfg.Terminal {
			return fmt.Errorf("%w: %s drains on its own", errInterrupted, name)
		}
		text, err := t.pick(ctx, "k kills what still runs, w keeps waiting [k/w] ? ", "kw", c.closed)
		switch {
		case errors.Is(err, errAbandoned):
		case err != nil:
			return fmt.Errorf("%w: %s drains on its own", errInterrupted, name)
		case text == "k":
			// A daemon that has stopped meanwhile has closed the
			// connection, which the wait above sees.
			_ = protocol.WriteJSON(c.conn, protocol.FrameStop, protocol.Stop{})
		}
	}
}

// stoppedAnyway answers err, the daemon of dir failing before it
// confirmed a stop: the daemon stopped by itself if its approval socket
// stops accepting within goneTimeout; a restart starts no new one then,
// `up --restart` does.
func (t *term) stoppedAnyway(ctx context.Context, cfg Config, dir string, err error) error {
	if launch.WaitStopped(ctx, launch.ApprovalSocket(dir), goneTimeout) != nil {
		return fmt.Errorf("%s: %w", hostterm.Display(dir), err)
	}
	e := ending{kind: justStop}
	if cfg.up != nil {
		e = ending{kind: startNew, up: cfg.up}
	}
	return t.stopped(ctx, cfg, dir, e)
}

// stopped waits until nothing accepts on the sockets of the daemon of
// dir, starts the new daemon e holds, if any, and says what became of it.
func (t *term) stopped(ctx context.Context, cfg Config, dir string, e ending) error {
	for _, socket := range []string{launch.ApprovalSocket(dir), launch.Socket(dir)} {
		if err := launch.WaitStopped(ctx, socket, cfg.StopTimeout); err != nil {
			return err
		}
	}
	name := hostterm.Display(dir)
	switch e.kind {
	case startNew:
		if err := e.up.UpLocked(ctx); err != nil {
			return fmt.Errorf("%s: stopped, but the new daemon did not start: %w", name, err)
		}
		fmt.Fprintf(t.out, "hostrunner: restarted %s\n", name)
	case notRunning:
		fmt.Fprintf(t.out, "hostrunner: stopped %s; its container is not running\n", name)
	default:
		fmt.Fprintf(t.out, "hostrunner: stopped %s\n", name)
	}
	return nil
}

// term is the human's side of a stop: where it prints, and the lines
// typed, read only once a question needs them.
type term struct {
	in         io.Reader
	out        io.Writer
	interrupts <-chan os.Signal
	input      *hostterm.Input // nil until the first question
}

// ask prints prompt and returns the line typed after it, trimmed. It
// fails with errInterrupted on Ctrl+C or when ctx ends, errInputEnded
// when in ends, and errAbandoned once abandon (nil for never) is closed.
func (t *term) ask(ctx context.Context, prompt string, abandon <-chan struct{}) (string, error) {
	if t.input == nil {
		t.input = hostterm.NewInput(t.in)
	}
	t.input.Prompt(t.out, prompt)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(t.out)
			return "", errInterrupted
		case <-t.interrupts:
			fmt.Fprintln(t.out)
			return "", errInterrupted
		case <-abandon:
			fmt.Fprintln(t.out)
			return "", errAbandoned
		case l, more := <-t.input.Lines():
			if !more {
				fmt.Fprintln(t.out)
				return "", errInputEnded
			}
			if text, fresh := t.input.Take(l); fresh {
				return text, nil
			}
		}
	}
}

// pick asks prompt until the human types one of the one-letter answers
// keys, which it returns, "q" for any way of quitting when keys holds q;
// it fails as ask does.
func (t *term) pick(ctx context.Context, prompt, keys string, abandon <-chan struct{}) (string, error) {
	for {
		text, err := t.ask(ctx, prompt, abandon)
		if err != nil {
			return "", err
		}
		if hostterm.IsQuit(text) {
			text = "q"
		}
		if len(text) == 1 && strings.Contains(keys, text) {
			return text, nil
		}
	}
}

// close stops reading the human's lines, if a question started it.
func (t *term) close() {
	if t.input != nil {
		t.input.Close()
	}
}
