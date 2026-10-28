package hostterm

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/kravlab/hostrunner/internal/daemonlist"
	"github.com/kravlab/hostrunner/internal/protocol"
)

// List is a numbered list of daemons for the human to choose one from:
// approval sessions and `hostrunner stop --choose` show it.
type List struct {
	Daemons []daemonlist.Daemon
	Cwd     string // the daemon whose host workspace holds it is the default
	// AnyVersion lets a daemon of any approval protocol version be chosen;
	// without it, one of another version than this hostrunner's is shown
	// as such and is never the default.
	AnyVersion bool
	// HideServed leaves out which daemons another approval session
	// serves, for a list where that does not matter.
	HideServed bool
}

// Default returns the index of the daemon chosen by default: the first
// that can be chosen whose host workspace holds Cwd; or -1.
func (l List) Default() int {
	return slices.IndexFunc(l.Daemons, func(d daemonlist.Daemon) bool {
		return l.choosable(d) && daemonlist.Holds(d.Info.Workspace, l.Cwd)
	})
}

// Pick returns the index of the daemon text chooses: its number, or, for
// an empty line, the default one (see Default).
func (l List) Pick(text string) (int, bool) {
	if text == "" {
		i := l.Default()
		return i, i >= 0
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > len(l.Daemons) {
		return 0, false
	}
	return n - 1, true
}

// Print prints the daemons, of which there is at least one, to w and
// returns the prompt that asks to choose one, for the caller to print.
func (l List) Print(w io.Writer) (prompt string) {
	width, cwidth := 0, 0
	for _, d := range l.Daemons {
		width = max(width, len([]rune(Display(d.Info.Workspace))))
		cwidth = max(cwidth, len([]rune(Display(d.Info.ContainerWorkspace))))
	}
	def := l.Default()
	fmt.Fprintln(w, "daemons:")
	for i, d := range l.Daemons {
		mark := " "
		if i == def {
			mark = "*"
		}
		ws := Display(d.Info.Workspace)
		ws += strings.Repeat(" ", width-len([]rune(ws)))
		var note string
		switch {
		case !l.choosable(d):
			note = "another version: restart its container"
		default:
			c := Display(d.Info.ContainerWorkspace)
			note = "(container " + c + ")" + strings.Repeat(" ", cwidth-len([]rune(c)))
			var marks []string
			if i == def {
				marks = append(marks, "← current directory")
			}
			if d.Info.Served && !l.HideServed {
				marks = append(marks, "served by another session")
			}
			if len(marks) > 0 {
				note += "   " + strings.Join(marks, ", ")
			}
		}
		fmt.Fprintf(w, "  %d %s %s   %s\n", i+1, mark, ws, strings.TrimRight(note, " "))
	}
	choices := fmt.Sprintf("1-%d", len(l.Daemons))
	if def >= 0 {
		choices += fmt.Sprintf(", Enter = %d", def+1)
	}
	return "choose [" + choices + ", q quit] ? "
}

// choosable reports whether d can be chosen from the list.
func (l List) choosable(d daemonlist.Daemon) bool {
	return l.AnyVersion || d.Info.Version == protocol.ApprovalVersion
}
