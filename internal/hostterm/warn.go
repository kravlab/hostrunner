package hostterm

import (
	"fmt"
	"io"
)

// WarnIgnoredRules warns on w that the older rules file ignored is left
// unread because the rules file used exists; it prints nothing when
// ignored is empty (see settings.Resolved.IgnoredRules). The line, with
// both paths shown as Display shows them, is
//
//	hostrunner: warning: <ignored> is ignored: <used> is used; remove it
func WarnIgnoredRules(w io.Writer, ignored, used string) {
	if ignored != "" {
		fmt.Fprintf(w, "hostrunner: warning: %s is ignored: %s is used; remove it\n", Display(ignored), Display(used))
	}
}
