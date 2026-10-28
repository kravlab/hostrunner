package daemon

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// WithContainerStopped lets the Server report in its hello whether it saw
// its armed container stopped (protocol.DaemonInfo.ContainerStopped), as f
// tells (e.g. the container watcher). f is called from connection
// goroutines, so it must be safe for concurrent use. Without it, the
// Server reports it not stopped.
func WithContainerStopped(f func() bool) Option {
	return func(s *Server) { s.containerStopped = f }
}

// drainingMessage refuses a command that would start while the daemon
// drains; waiting approval requests get stoppingMessage instead, with
// 126.
const drainingMessage = "the hostrunner daemon is stopping"

// errDraining is why a command is refused while the daemon drains.
var errDraining = errors.New("draining: no new commands start")

// track records req's command as running and returns its key for
// untrack; it refuses (ok false) once the daemon drains.
func (s *Server) track(req protocol.Request) (key uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isDrainingLocked() {
		return 0, false
	}
	s.lastRunning++
	s.running[s.lastRunning] = protocol.RunningCommand{Argv: req.Argv, Cwd: req.Cwd}
	return s.lastRunning, true
}

// untrack forgets the command track returned key for; the last command of
// a draining daemon stops it, unless the drain is not confirmed yet.
func (s *Server) untrack(key uint64) {
	s.mu.Lock()
	delete(s.running, key)
	stopServe := s.drainedLocked()
	s.mu.Unlock()
	if stopServe != nil {
		stopServe()
	}
}

// drainedLocked returns, with s.mu held, what stops Serve when a confirmed
// drain has nothing left to wait for, or nil.
func (s *Server) drainedLocked() context.CancelFunc {
	if !s.isDrainingLocked() || s.confirming || len(s.running) > 0 {
		return nil
	}
	return s.stopServe
}

// runningList answers FrameRunning: the running commands, oldest first,
// and the approvals that end with the daemon.
func (s *Server) runningList() protocol.RunningList {
	s.mu.Lock()
	keys := slices.Sorted(maps.Keys(s.running))
	commands := make([]protocol.RunningCommand, 0, len(keys))
	for _, k := range keys {
		commands = append(commands, s.running[k])
	}
	s.mu.Unlock()
	standing, session := s.approvals.counts()
	return protocol.RunningList{Commands: commands, Standing: standing, Session: session}
}

// stop answers st from the stop on out (pid its process). The first stop
// is confirmed with FrameStopping once it is in effect and before Serve
// stops: a drain has closed both sockets by then. Serve stopping closes
// the connection as it begins, not once it has returned, so the stop
// learns the daemon is gone from its sockets. A stop without Drain on a
// draining daemon kills what still runs, unconfirmed; another drain
// changes nothing.
func (s *Server) stop(out *frameWriter, pid int, st protocol.Stop) {
	s.mu.Lock()
	first := !s.stopping
	s.stopping = true
	stopServe := s.stopServe
	s.mu.Unlock()
	switch {
	case !st.Drain:
		if first {
			s.log.Info("stopping on request", "pid", pid)
			_ = out.write(protocol.Frame{Type: protocol.FrameStopping})
		} else {
			s.log.Info("stopping on request: killing what still runs", "pid", pid)
		}
		if stopServe != nil {
			stopServe()
		}
	case first:
		s.log.Info("draining on request", "pid", pid)
		s.drain()
		_ = out.write(protocol.Frame{Type: protocol.FrameStopping})
		s.confirmDrain()
	}
}

// drain makes the daemon take no new requests: it closes both sockets and
// ends the waiting approval requests. Serve keeps running until
// confirmDrain, and then until no command runs.
func (s *Server) drain() {
	s.mu.Lock()
	close(s.draining)
	s.confirming = true
	closeListeners := s.closeListeners
	s.mu.Unlock()
	if closeListeners != nil {
		closeListeners()
	}
}

// confirmDrain lets the drain stop Serve, at once when no command runs.
func (s *Server) confirmDrain() {
	s.mu.Lock()
	s.confirming = false
	stopServe := s.drainedLocked()
	s.mu.Unlock()
	if stopServe != nil {
		stopServe()
	}
}

// isDraining reports whether a stop drains the daemon.
func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isDrainingLocked()
}

// isDrainingLocked is isDraining with s.mu held.
func (s *Server) isDrainingLocked() bool {
	select {
	case <-s.draining:
		return true
	default:
		return false
	}
}
