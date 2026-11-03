package client_test

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kravlab/hostrunner/internal/approval"
	"github.com/kravlab/hostrunner/internal/daemon"
	"github.com/kravlab/hostrunner/internal/fakedaemon"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// startVersionedDaemon starts a daemon with the echoOnly rules, its
// runtime directory at base, reporting the build version version.
func startVersionedDaemon(t *testing.T, base, version string) harness {
	t.Helper()
	return startHarnessAt(t, slog.New(slog.DiscardHandler), base, func(string) (daemon.Policy, []daemon.Option) {
		return policy(t, echoOnly), []daemon.Option{daemon.WithBuildVersion(version)}
	})
}

// versionWarning is the line a session prints when its daemon's build
// version, daemon, differs from its own, session.
func versionWarning(daemon, session string) string {
	return "⚠ " + daemon + ", this session " + session + ": run hostrunner restart to update\n"
}

// listLine returns the line of list that names workspace.
func listLine(t *testing.T, list, workspace string) string {
	t.Helper()
	for line := range strings.Lines(list) {
		if strings.Contains(line, workspace+" ") {
			return line
		}
	}
	t.Fatalf("the list %q does not name %s", list, workspace)
	return ""
}

// versionedDaemons are three daemons under one runtime root: same of
// build version v1.2.0, older of v1.1.0, and a fake one serving the host
// workspace unknown that reports none.
type versionedDaemons struct {
	root, unknown string
	same, older   harness
}

// startVersionedDaemons starts the three versionedDaemons under a new
// runtime root.
func startVersionedDaemons(t *testing.T) versionedDaemons {
	t.Helper()
	same, root := newBase(t)
	older, _ := newBase(t)
	unknown, _ := newBase(t)
	fakeDaemonFor(t, unknown, "/home/u/unknown")
	return versionedDaemons{
		root:    root,
		same:    startVersionedDaemon(t, same, "v1.2.0"),
		older:   startVersionedDaemon(t, older, "v1.1.0"),
		unknown: "/home/u/unknown",
	}
}

// checkListed fails the test unless list, shown by a hostrunner of
// version v1.2.0, shows every daemon of d's version, marking those of
// v1.1.0 and of none.
func (d versionedDaemons) checkListed(t *testing.T, list string) {
	t.Helper()
	if line := listLine(t, list, d.same.hostRoot); !strings.Contains(line, " v1.2.0 ") || strings.Contains(line, "⚠") {
		t.Errorf("the line %q, want version v1.2.0 unmarked", line)
	}
	if line := listLine(t, list, d.older.hostRoot); !strings.Contains(line, " v1.1.0 ⚠ ") {
		t.Errorf("the line %q, want version v1.1.0 marked", line)
	}
	if line := listLine(t, list, d.unknown); !strings.Contains(line, " unknown ⚠ ") {
		t.Errorf("the line %q, want an unknown version marked", line)
	}
}

// A session says which version it is and which its daemon is, and warns
// of nothing when they are the same.
func TestSessionShowsItsAndTheDaemonsVersion(t *testing.T) {
	base, _ := newBase(t)
	h := startVersionedDaemon(t, base, "v1.2.0")
	s := startSessionWith(t, approval.Config{Socket: h.approvalSocket, BuildVersion: "v1.2.0"})
	out := s.out.String()
	for _, want := range []string{"hostrunner approve v1.2.0\n", "serving " + h.hostRoot + " (container " + containerRoot + ") · daemon v1.2.0\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the session printed %q, lacking %q", out, want)
		}
	}
	if strings.Contains(out, "hostrunner restart") {
		t.Errorf("the session warns of versions that are the same: %q", out)
	}
}

// A daemon of another build version is served, with one warning naming
// both versions.
func TestSessionWarnsOfADaemonOfAnotherVersion(t *testing.T) {
	base, _ := newBase(t)
	h := startVersionedDaemon(t, base, "v1.1.0")
	s := startSessionWith(t, approval.Config{Socket: h.approvalSocket, BuildVersion: "v1.2.0"})
	out := s.out.String()
	if want := "serving " + h.hostRoot + " (container " + containerRoot + ") · daemon v1.1.0\n"; !strings.Contains(out, want) {
		t.Errorf("the session printed %q, lacking %q", out, want)
	}
	if n := strings.Count(out, versionWarning("daemon v1.1.0", "v1.2.0")); n != 1 {
		t.Errorf("the session printed %q, with %d warnings of the versions; want one", out, n)
	}
	ch := h.ask(t, "touch", "marker")
	s.waitFor(t, answerPromptText)
	s.answer(t, "o")
	if got := awaitResult(t, ch); got.code != 0 {
		t.Fatalf("got %+v, want it approved by the session", got)
	}
}

// A daemon that reports no build version, older than the field, reads
// as of an unknown version, and the session still serves it.
func TestSessionServesADaemonOfUnknownVersion(t *testing.T) {
	socket, served := fakeDaemonWith(t, protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: "/home/u/app", ContainerWorkspace: containerRoot})
	s := startSessionWith(t, approval.Config{Socket: socket, BuildVersion: "v1.2.0"})
	<-served
	s.waitFor(t, answerPromptText)
	out := s.out.String()
	for _, want := range []string{"serving /home/u/app (container " + containerRoot + ") · daemon version unknown\n", versionWarning("daemon version unknown", "v1.2.0")} {
		if !strings.Contains(out, want) {
			t.Errorf("the session printed %q, lacking %q", out, want)
		}
	}
}

// The list shows every daemon's version, marking those other than the
// session's or unknown, when choosing and again after `daemons`.
func TestSessionListShowsTheDaemonsVersions(t *testing.T) {
	d := startVersionedDaemons(t)
	s := startSessionWith(t, approval.Config{Root: d.root, Cwd: d.same.hostRoot, BuildVersion: "v1.2.0"})
	d.checkListed(t, s.waitForListed(t))
	s.answer(t, "")
	s.waitFor(t, "serving "+d.same.hostRoot)
	s.waitFor(t, "> ")
	s.answer(t, "daemons")
	d.checkListed(t, s.waitForListed(t))
}

// A daemon whose build version alone changes while the list is shown is
// shown again with its new version.
func TestSessionListShowsAChangedVersion(t *testing.T) {
	base, root := newBase(t)
	var version atomic.Value
	version.Store("v1.1.0")
	fakedaemon.Serve(t, base, func() protocol.DaemonInfo {
		return protocol.DaemonInfo{Version: protocol.ApprovalVersion, Workspace: "/home/u/app", BuildVersion: version.Load().(string)}
	})
	s := startSessionWith(t, approval.Config{Root: root, BuildVersion: "v1.2.0"})
	if line := listLine(t, s.waitForListed(t), "/home/u/app"); !strings.Contains(line, " v1.1.0 ⚠ ") {
		t.Fatalf("the line %q, want version v1.1.0 marked", line)
	}
	version.Store("v1.2.0")
	if line := listLine(t, s.waitForListed(t), "/home/u/app"); !strings.Contains(line, " v1.2.0 ") || strings.Contains(line, "⚠") {
		t.Fatalf("the line %q, want version v1.2.0 unmarked", line)
	}
}

// Each serving line of a daemon of another version is followed by one
// warning, also when the session serves it again after `daemons`.
func TestSessionWarnsWithEachServingLine(t *testing.T) {
	base, root := newBase(t)
	h := startVersionedDaemon(t, base, "v1.1.0")
	s := startSessionWith(t, approval.Config{Root: root, Cwd: h.hostRoot, BuildVersion: "v1.2.0"})
	for range 2 {
		s.waitForListed(t)
		s.answer(t, "")
		s.waitFor(t, "serving "+h.hostRoot)
		served := s.waitFor(t, "waiting for requests")
		if n := strings.Count(served, versionWarning("daemon v1.1.0", "v1.2.0")); n != 1 {
			t.Fatalf("serving printed %q, with %d warnings of the versions; want one", served, n)
		}
		s.waitFor(t, "> ")
		s.answer(t, "daemons")
	}
}

// A session given no version is of version dev, as `hostrunner version`
// says of a build without one: a daemon of dev is of its version.
func TestSessionWithoutAVersionIsDev(t *testing.T) {
	base, _ := newBase(t)
	h := startVersionedDaemon(t, base, "dev")
	s := startSessionWith(t, approval.Config{Socket: h.approvalSocket})
	out := s.out.String()
	for _, want := range []string{"hostrunner approve dev\n", "· daemon dev\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the session printed %q, lacking %q", out, want)
		}
	}
	if strings.Contains(out, "hostrunner restart") {
		t.Errorf("the session warns of versions that are the same: %q", out)
	}
}

// The list of a session given no version marks a daemon of dev as of
// its version, as the serving line does.
func TestSessionListWithoutAVersionIsDev(t *testing.T) {
	base, root := newBase(t)
	h := startVersionedDaemon(t, base, "dev")
	s := startSessionWith(t, approval.Config{Root: root})
	if line := listLine(t, s.waitForListed(t), h.hostRoot); !strings.Contains(line, " dev ") || strings.Contains(line, "⚠") {
		t.Errorf("the line %q, want version dev unmarked", line)
	}
}
