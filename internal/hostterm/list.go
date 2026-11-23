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
	// BuildVersion is this hostrunner's build version: every daemon's is
	// shown, one of another or an unknown version marked (see
	// OtherVersion).
	BuildVersion string
}

// DevVersion is the build version of a hostrunner built without one, as
// `hostrunner version` prints it; it also stands for a build version left
// unset.
const DevVersion = "dev"

// OtherVersion reports whether info is of a daemon whose build version
// is not version: another one, or none reported (a daemon older than
// protocol.DaemonInfo.BuildVersion), which reads as unknown.
func OtherVersion(info protocol.DaemonInfo, version string) bool {
	return info.BuildVersion == "" || info.BuildVersion != version
}

// unknownText is what the list shows for a build version or a kind of
// process a daemon reports none of.
const unknownText = "unknown"

// ShownVersion returns info's build version as Display shows it, or
// unknown when the daemon reports none (see OtherVersion).
func ShownVersion(info protocol.DaemonInfo, unknown string) string {
	if info.BuildVersion == "" {
		return unknown
	}
	return Display(info.BuildVersion)
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
	wsWidth, versionWidth, processWidth, containerWidth := 0, 0, 0, 0
	for _, d := range l.Daemons {
		wsWidth = max(wsWidth, len([]rune(Display(d.Info.Workspace))))
		versionWidth = max(versionWidth, len([]rune(l.version(d))))
		processWidth = max(processWidth, len([]rune(l.processKind(d))))
		containerWidth = max(containerWidth, len([]rune(Display(d.Info.ContainerWorkspace))))
	}
	def := l.Default()
	fmt.Fprintln(w, "daemons:")
	for i, d := range l.Daemons {
		mark := " "
		if i == def {
			mark = "*"
		}
		ws := padRight(Display(d.Info.Workspace), wsWidth)
		v := padRight(l.version(d), versionWidth)
		p := padRight(l.processKind(d), processWidth)
		var note string
		switch {
		case !l.choosable(d):
			note = "another version: restart its container"
		default:
			c := Display(d.Info.ContainerWorkspace)
			note = "(container " + c + ")" + strings.Repeat(" ", containerWidth-len([]rune(c)))
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
		fmt.Fprintf(w, "  %d %s %s   %s   %s   %s\n", i+1, mark, ws, v, p, strings.TrimRight(note, " "))
	}
	choices := fmt.Sprintf("1-%d", len(l.Daemons))
	if def >= 0 {
		choices += fmt.Sprintf(", Enter = %d", def+1)
	}
	return "choose [" + choices + ", q quit] ? "
}

// padRight returns s followed by spaces up to width runes.
func padRight(s string, width int) string {
	return s + strings.Repeat(" ", width-len([]rune(s)))
}

// version returns d's build version as the list shows it, unknownText
// when it reports none, followed by " ⚠" when it is not l.BuildVersion.
func (l List) version(d daemonlist.Daemon) string {
	v := ShownVersion(d.Info, unknownText)
	if OtherVersion(d.Info, l.BuildVersion) {
		v += " ⚠"
	}
	return v
}

// processKind returns the kind of process d runs in as the list shows it,
// own or shared (protocol.DaemonInfo.Process), or unknownText when it
// reports none, as a daemon older than the field does.
func (l List) processKind(d daemonlist.Daemon) string {
	if d.Info.Process == "" {
		return unknownText
	}
	return Display(d.Info.Process)
}

// choosable reports whether d can be chosen from the list.
func (l List) choosable(d daemonlist.Daemon) bool {
	return l.AnyVersion || d.Info.Version == protocol.ApprovalVersion
}
