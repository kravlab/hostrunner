package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/shared"
	"github.com/kravlab/hostrunner/internal/transport"
)

// sharedFiles are where the shared process of a build lives in one
// runtime root, as shared.Files names them: what addLocked and
// startShared need to reach that process, or to start it.
type sharedFiles struct {
	root    string // the runtime root, which the process runs in
	control string // its control socket
	log     string // its log, the process log
}

// stopTimeout bounds the stop of a daemon up added to a shared process
// and gave up waiting for, from reaching its approval socket until it
// stopped (see stopAdded).
const stopTimeout = 5 * time.Second

// addShared is UpLocked's start of the daemon with process: shared. It
// asks the shared process of cfg.Daemon, in the runtime root (the
// directory of cfg.Dir), to add the daemon serveArgs describes, with the
// environment of this up, starting a new process when none listens or the
// one it reached is ending (see addLocked); then, the lock of the process
// released so that concurrent ups do not wait for each other's daemon, it
// waits until the daemon follows the arm a, sent to its approval socket,
// stopping the daemon when the wait fails (see waitReady). One deadline,
// cfg.ReadyTimeout after addShared began, bounds it all: the wait for the
// lock of the process, its start, every add, and the wait for the arm,
// which gets what remains, each arm included; a failure because it passed
// says what was waited for and names cfg.ReadyTimeout. A failed add,
// refused by the process or past the deadline, carries what the daemon
// wrote to its log meanwhile, as waitReady quotes it, and no process log,
// even of a process this up started: that process did start. When this
// up started the process, the wait for the arm fails at once if the
// process exits, and every failure of that wait quotes the process log
// too (see spawned.host); one that exits before it listens is
// startShared's error, its log quoted. A cancelled ctx is returned as it
// is.
func addShared(ctx context.Context, cfg Config, socket string, a protocol.Arm) error {
	d := newReadyDeadline(cfg.ReadyTimeout)
	root := filepath.Dir(cfg.Dir)
	control, processLog, err := shared.Files(root, cfg.Daemon)
	if err != nil {
		return err
	}
	files := sharedFiles{root: root, control: control, log: processLog}
	logPath := filepath.Join(cfg.Dir, LogName)
	var logStart int64
	if info, err := os.Lstat(logPath); err == nil {
		logStart = info.Size()
	}
	// The daemon runs in the shared process, which ends with it alone no
	// more: up watches no end of the daemon, only that of a process it
	// started (host), and stops the daemon rather than kill the process.
	s := spawned{what: "daemon", logPath: logPath, kill: func() { stopAdded(cfg.Dir) }, logStart: logStart}
	req := protocol.AddDaemon{Args: serveArgs(cfg, socket)[1:], Env: os.Environ()}
	host, err := addLocked(ctx, cfg, d, files, req)
	s.host = host
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return withLogs(err, s.ownLogSinceStart())
	}
	armCtx, cancel := context.WithDeadline(ctx, d.at)
	defer cancel()
	armed := armReady(armCtx, cfg, a)
	return waitReady(ctx, d, s, func() (bool, error) {
		done, err := armed()
		if errors.Is(err, os.ErrDeadlineExceeded) && d.passed() {
			return false, nil // cut short by d: waitReady reports the timeout
		}
		return done, err
	})
}

// stopAdded stops the daemon of the runtime directory dir that up added
// to a shared process and gave up waiting for, as `hostrunner stop --kill`
// stops a daemon: it kills what the daemon runs and returns once the
// daemon stopped. Added means started, not listening yet, so it tries
// again until its approval socket answers, for stopTimeout at most; a
// daemon it cannot stop is left to end by itself, as one never armed does
// once its startup timeout passes.
func stopAdded(dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for requestStop(ctx, ApprovalSocket(dir)) != nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// requestStop asks the daemon on the approval socket socket to stop,
// killing what it runs, with FrameStop after the hello as `hostrunner
// stop` does, and waits until the daemon closes the connection as it
// stops, until ctx ends at most.
func requestStop(ctx context.Context, socket string) error {
	c, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if _, err := daemonlist.Hello(c); err != nil {
		return err
	}
	if err := protocol.WriteJSON(c, protocol.FrameStop, protocol.Stop{}); err != nil {
		return err
	}
	f, err := protocol.ReadFrame(c)
	if err != nil {
		return err
	}
	if f.Type != protocol.FrameStopping {
		return daemonlist.ErrorOf(f)
	}
	_, err = io.Copy(io.Discard, c)
	return err
}

// endingRetry is how long addLocked waits before it adds again after the
// shared process it reached answered that it is ending.
const endingRetry = 50 * time.Millisecond

// addLocked asks the shared process whose files are files, on its control
// socket, to add the daemon req describes, holding the lock of that
// process (see shared.LockFile) from its first add until the add succeeded
// or failed for good, so that concurrent ups of one build, for any runtime
// directory of the runtime root of files, start one process and add their
// daemons to it. It adds in a loop: when nothing listens, it starts a new
// process (see startShared) and adds again; when the process reached is
// ending (shared.ErrEnding), it adds again after endingRetry, as that
// process soon stops listening and a later add starts a new one; any
// other error, or a failed start, ends the loop with that error. The
// deadline d, of addShared, and ctx bound it all: the wait for the lock
// (see lockShared), each start, and each add, whose exchange d cuts short
// when it comes before the add's own timeout (see shared.Add). Once d
// passed, an add that would be retried, or one d cut short, fails saying
// the daemon was not added within the ready timeout (see notAdded),
// naming first the error of the last add that reached an ending
// process, the likely cause, then that of the add that failed; a start
// of a new process forgets that error. host is the process this up
// started and found listening, the last one when it started several,
// nil when it started none; a start that fails returns nil, its error
// quoting the log of the process.
func addLocked(ctx context.Context, cfg Config, d readyDeadline, files sharedFiles, req protocol.AddDaemon) (host *spawned, err error) {
	unlock, err := lockShared(ctx, d, files.control)
	if err != nil {
		return nil, err
	}
	defer unlock()
	addCtx, cancel := context.WithDeadline(ctx, d.at)
	defer cancel()
	var ending error // the last error of an add that reached an ending process
	for {
		err := shared.Add(addCtx, files.control, req)
		if errors.Is(err, shared.ErrEnding) {
			ending = err
		}
		retry := errors.Is(err, shared.ErrNotListening) || errors.Is(err, shared.ErrEnding)
		switch {
		case !retry && errors.Is(err, os.ErrDeadlineExceeded) && d.passed():
			return host, notAdded(d, ending, err)
		case !retry:
			return host, err
		case d.passed():
			return host, notAdded(d, ending, err)
		}
		if errors.Is(err, shared.ErrNotListening) {
			started, err := startShared(ctx, cfg, d, files)
			if err != nil {
				return nil, err
			}
			host = &started
			ending = nil
			continue
		}
		select {
		case <-ctx.Done():
			return host, ctx.Err()
		case <-time.After(endingRetry):
		}
		// Checked after the wait too, so that the error is that of this
		// add, not that of a next one that d cuts short as it connects.
		if d.passed() {
			return host, notAdded(d, ending, err)
		}
	}
}

// notAdded is the error of an add that failed with err, past the ready
// deadline d or cut short by it: it says the daemon was not added within
// the ready timeout, naming first ending, the error of the last add that
// reached an ending process, when there was one and err is not that
// error again, then err as the error of the next add.
func notAdded(d readyDeadline, ending, err error) error {
	if ending != nil && !errors.Is(err, shared.ErrEnding) {
		return fmt.Errorf("the daemon was not added to a shared process within the ready timeout of %v: %w; the next add failed: %w", d.timeout, ending, err)
	}
	return fmt.Errorf("the daemon was not added to a shared process within the ready timeout of %v: %w", d.timeout, err)
}

// lockPoll is how often lockShared tries again to take the lock of a
// shared process another holds.
const lockPoll = 50 * time.Millisecond

// lockShared takes the lock of the shared process whose control socket is
// control (see shared.LockFile) and returns the function that releases
// it. A flock that waits cannot be given up, so it tries the lock without
// waiting every lockPoll until it takes it, d passes, failing with an
// error saying it waited for that lock, or ctx ends.
func lockShared(ctx context.Context, d readyDeadline, control string) (unlock func(), err error) {
	ticker := time.NewTicker(lockPoll)
	defer ticker.Stop()
	for {
		unlock, busy, err := lockFile(shared.LockFile(control), "shared process", syscall.LOCK_EX|syscall.LOCK_NB)
		if !busy {
			return unlock, err
		}
		if d.passed() {
			return nil, fmt.Errorf("waited for the lock of the shared process on %s, which another up holds, past the ready timeout of %v", hostterm.Display(control), d.timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// minFirstAdd is the least time startShared gives a shared process for
// its first add, for a deadline that passed, or all but did, as it starts
// the process.
const minFirstAdd = time.Millisecond

// startShared spawns `hostrunner shared` of cfg.Daemon (see spawn) with
// the files files: in their runtime root, listening on their control
// socket and logging to their process log. It then waits, until the
// deadline d at most, until the control socket accepts connections (see
// waitReady), returning the process it started, as waitReady watched it,
// for the caller to watch on;
// a process that exits first, or does not listen in time, is waitReady's
// error, quoting the process log. The process is given what remains of d
// for its first add (--ready-timeout), minFirstAdd at least so that the
// flag stays positive: one that does not come in that time ends it (see
// shared.Serve), so a process no daemon is added to ends about when this
// up gives up, not cfg.ReadyTimeout after its start. The caller holds the lock of the process (see addLocked),
// so no other up starts one beside it.
func startShared(ctx context.Context, cfg Config, d readyDeadline, files sharedFiles) (spawned, error) {
	firstAdd := max(time.Until(d.at), minFirstAdd)
	args := []string{"shared", "--socket", files.control, "--ready-timeout", firstAdd.String()}
	s, err := spawn("shared process", cfg.Daemon, args, files.root, files.log)
	if err != nil {
		return spawned{}, err
	}
	return s, waitReady(ctx, d, s, func() (bool, error) {
		return listening(ctx, files.control), nil
	})
}

// withLogs is err followed by logs, the tail of one log or more as up's
// errors quote them ("<err>: <logs>"); err alone when logs is empty.
func withLogs(err error, logs string) error {
	if logs == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, logs)
}
