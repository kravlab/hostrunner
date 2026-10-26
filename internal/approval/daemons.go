package approval

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/transport"
)

// pollInterval is how often the session looks for daemons while it lists
// them or waits for one.
const pollInterval = 2 * time.Second

// probeTimeout bounds asking one daemon who it is; a socket that does not
// answer within it is skipped.
const probeTimeout = time.Second

// listed is a daemon found on its approval socket, as it says it is.
type listed struct {
	socket string
	info   protocol.DaemonInfo
}

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
	var shown []listed
	show := func(force bool) {
		found := listDaemons(ctx, cfg.Root)
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
		case l, more := <-t.lines:
			if !more {
				return "", false, false
			}
			text, fresh := t.take(l)
			if !fresh {
				continue
			}
			if isQuit(text) {
				return "", false, false
			}
			i, chosen := pick(text, shown, cfg.Cwd)
			switch {
			case !chosen:
				t.prompt(t.asking)
			case shown[i].info.Version != protocol.ApprovalVersion:
				fmt.Fprintf(t.out, "daemon %d speaks another approval protocol version: restart its container\n", i+1)
				show(true)
			case shown[i].info.Served:
				yes, more := t.askTakeOver(ctx, takeOverPrompt)
				if !more {
					return "", false, false
				}
				if yes {
					return shown[i].socket, true, true
				}
				show(true)
			default:
				return shown[i].socket, false, true
			}
		}
	}
}

// pick returns the index of the daemon text chooses in shown: its number,
// or, for an empty line, the default one (see defaultDaemon).
func pick(text string, shown []listed, cwd string) (int, bool) {
	if text == "" {
		i := defaultDaemon(shown, cwd)
		return i, i >= 0
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > len(shown) {
		return 0, false
	}
	return n - 1, true
}

// defaultDaemon returns the index of the daemon chosen by default: the
// first, of the approval protocol version of the session, whose host
// workspace holds cwd; or -1.
func defaultDaemon(shown []listed, cwd string) int {
	return slices.IndexFunc(shown, func(d listed) bool {
		return d.info.Version == protocol.ApprovalVersion && holds(d.info.Workspace, cwd)
	})
}

// holds reports whether dir is workspace or lies inside it.
func holds(workspace, dir string) bool {
	if workspace == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(workspace, dir)
	return err == nil && filepath.IsLocal(rel)
}

// printList prints the daemons and asks to choose one, or, when there is
// none, says that it waits for one.
func (t *term) printList(shown []listed, cwd string) {
	if len(shown) == 0 {
		t.prompt("no daemons yet: waiting for one (q quits)\n")
		return
	}
	width, cwidth := 0, 0
	for _, d := range shown {
		width = max(width, len([]rune(display(d.info.Workspace))))
		cwidth = max(cwidth, len([]rune(display(d.info.ContainerWorkspace))))
	}
	def := defaultDaemon(shown, cwd)
	fmt.Fprintln(t.out, "daemons:")
	for i, d := range shown {
		mark := " "
		if i == def {
			mark = "*"
		}
		ws := display(d.info.Workspace)
		ws += strings.Repeat(" ", width-len([]rune(ws)))
		var note string
		switch {
		case d.info.Version != protocol.ApprovalVersion:
			note = "another version: restart its container"
		default:
			c := display(d.info.ContainerWorkspace)
			note = "(container " + c + ")" + strings.Repeat(" ", cwidth-len([]rune(c)))
			var marks []string
			if i == def {
				marks = append(marks, "← current directory")
			}
			if d.info.Served {
				marks = append(marks, "served by another session")
			}
			if len(marks) > 0 {
				note += "   " + strings.Join(marks, ", ")
			}
		}
		fmt.Fprintf(t.out, "  %d %s %s   %s\n", i+1, mark, ws, strings.TrimRight(note, " "))
	}
	choices := fmt.Sprintf("1-%d", len(shown))
	if def >= 0 {
		choices += fmt.Sprintf(", Enter = %d", def+1)
	}
	t.prompt("choose [" + choices + ", q quit] ? ")
}

// listDaemons returns the daemons that answer on the approval sockets
// under root, ordered by socket; a socket nobody answers on is skipped
// (only daemons remove theirs). The sockets are asked at once, so one that
// hangs holds up the list by probeTimeout at most.
func listDaemons(ctx context.Context, root string) []listed {
	sockets, _ := filepath.Glob(filepath.Join(root, "*"+protocol.ApprovalSuffix))
	slices.Sort(sockets)
	answers := make([]*protocol.DaemonInfo, len(sockets))
	var wg sync.WaitGroup
	for i, socket := range sockets {
		wg.Go(func() {
			if info, err := probe(ctx, socket); err == nil {
				answers[i] = &info
			}
		})
	}
	wg.Wait()
	var found []listed
	for i, info := range answers {
		if info != nil {
			found = append(found, listed{socket: sockets[i], info: *info})
		}
	}
	return found
}

// sameListing reports whether a and b show alike in the list: a change of
// what the list does not show (a reread's digest) does not show it again.
func sameListing(a, b listed) bool {
	return a.socket == b.socket && a.info.Version == b.info.Version && a.info.Served == b.info.Served &&
		a.info.Workspace == b.info.Workspace && a.info.ContainerWorkspace == b.info.ContainerWorkspace
}

// probe asks the daemon on socket who it is, without serving it.
func probe(ctx context.Context, socket string) (protocol.DaemonInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := transport.Unix{Path: socket}.Dial(ctx)
	if err != nil {
		return protocol.DaemonInfo{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	return hello(conn)
}

// take returns the text of l, trimmed, and whether it was typed after the
// current prompt, which it answers; readLines may then read on.
func (t *term) take(l line) (string, bool) {
	t.handled(l)
	return strings.TrimSpace(l.text), l.n >= t.fresh
}

// isQuit reports whether text asks to quit.
func isQuit(text string) bool { return text == "q" || text == "quit" }

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
		case l, ok := <-t.lines:
			if !ok {
				return false, false
			}
			if text, fresh := t.take(l); fresh {
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
			if _, err := probe(ctx, socket); err == nil {
				return nextServe
			}
		case l, more := <-t.lines:
			if !more {
				return nextQuit
			}
			text, fresh := t.take(l)
			switch {
			case !fresh:
			case isQuit(text):
				return nextQuit
			case text == "daemons" && canList:
				return nextList
			default:
				t.prompt(hint + "\n")
			}
		}
	}
}
