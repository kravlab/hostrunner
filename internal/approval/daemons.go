package approval

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/hostterm"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// pollInterval is how often the session looks for daemons while it lists
// them or waits for one.
const pollInterval = 2 * time.Second

// choose lists the daemons under cfg.Root and returns the approval socket
// of the one the human chooses, and whether to take it over from the
// session serving it; ok is false when the human quits or input ends. It
// looks for daemons every pollInterval, showing the list again when it
// changes, and waits while there is none. A daemon of another approval
// protocol version cannot be chosen; one another session serves is taken
// over only once the human confirms.
func (t *term) choose(ctx context.Context, cfg Config) (socket string, takeOver, ok bool) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	var shown []daemonlist.Daemon
	show := func(force bool) {
		found := daemonlist.List(ctx, cfg.Root)
		if !force && slices.EqualFunc(found, shown, sameListing) {
			return
		}
		shown = found
		t.printList(shown, cfg.Cwd)
	}
	show(true)
	for {
		select {
		case <-ctx.Done():
			return "", false, false
		case <-tick.C:
			show(false)
		case l, more := <-t.Lines():
			if !more {
				return "", false, false
			}
			text, fresh := t.Take(l)
			if !fresh {
				continue
			}
			if hostterm.IsQuit(text) {
				return "", false, false
			}
			i, chosen := hostterm.List{Daemons: shown, Cwd: cfg.Cwd}.Pick(text)
			switch {
			case !chosen:
				t.prompt(t.asking)
			case shown[i].Info.Version != protocol.ApprovalVersion:
				fmt.Fprintf(t.out, "daemon %d speaks another approval protocol version: restart its container\n", i+1)
				show(true)
			case shown[i].Info.Served:
				yes, more := t.askTakeOver(ctx, takeOverPrompt)
				if !more {
					return "", false, false
				}
				if yes {
					return shown[i].Socket, true, true
				}
				show(true)
			default:
				return shown[i].Socket, false, true
			}
		}
	}
}

// sameListing reports whether a and b show alike in the list: a change of
// what the list does not show (a reread's digest) does not show it again.
func sameListing(a, b daemonlist.Daemon) bool {
	return a.Socket == b.Socket && a.Info.Version == b.Info.Version && a.Info.Served == b.Info.Served &&
		a.Info.Workspace == b.Info.Workspace && a.Info.ContainerWorkspace == b.Info.ContainerWorkspace
}

// printList prints the daemons and asks to choose one, or, when there is
// none, says that it waits for one.
func (t *term) printList(shown []daemonlist.Daemon, cwd string) {
	if len(shown) == 0 {
		t.prompt("no daemons yet: waiting for one (q quits)\n")
		return
	}
	t.prompt(hostterm.List{Daemons: shown, Cwd: cwd}.Print(t.out))
}

// isYes reports whether text says yes to taking a daemon over.
func isYes(text string) bool { return text == "y" || text == "yes" }

// askTakeOver asks, with prompt, whether to take over a daemon another
// session serves, and reports whether the human said yes, and more,
// whether they answered at all: input or ctx ending is no answer.
func (t *term) askTakeOver(ctx context.Context, prompt string) (yes, more bool) {
	t.prompt(prompt)
	for {
		select {
		case <-ctx.Done():
			return false, false
		case l, ok := <-t.Lines():
			if !ok {
				return false, false
			}
			if text, fresh := t.Take(l); fresh {
				return isYes(text), true
			}
		}
	}
}

// awaitDaemon waits for the next daemon on socket, its runtime directory's,
// asking for it every pollInterval, and returns nextServe once one answers,
// to be served; nextList when the human asks for the list of daemons
// (canList), and nextQuit when they quit or input ends. Anything else typed
// gets a reminder of what it takes. said, what the session said of the
// daemon gone, is the wait's prompt: a line typed once it shows counts.
func (t *term) awaitDaemon(ctx context.Context, socket string, canList bool, said string) next {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	hint := "waiting for the next daemon: q quits"
	if canList {
		hint += ", daemons lists them"
	}
	t.prompt(said)
	for {
		select {
		case <-ctx.Done():
			return nextQuit
		case <-tick.C:
			if _, err := daemonlist.Probe(ctx, socket); err == nil {
				return nextServe
			}
		case l, more := <-t.Lines():
			if !more {
				return nextQuit
			}
			text, fresh := t.Take(l)
			switch {
			case !fresh:
			case hostterm.IsQuit(text):
				return nextQuit
			case text == "daemons" && canList:
				return nextList
			default:
				t.prompt(hint + "\n")
			}
		}
	}
}
