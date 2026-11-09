package client

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kravlab/hostrunner/internal/rulesfile"
)

// wOK is access(2)'s W_OK, which package syscall does not export.
const wOK = 0x2

// WarnWritableRules warns on stderr when the container can change the rules
// that govern it: when the nearest .devcontainer at or above cwd, or the
// default rules file in it, is writable; that file is the one the daemon
// uses, hostrunner-rules.yaml or, while that is absent, hostrun.yaml (see
// rulesfile.Choose). A writable directory is enough, since the
// file can then be replaced or, when missing, created. The daemon serves
// such a change only once a human on the host allows it (hostrunner
// allow): within a poll when it rereads by itself, otherwise at the next
// up or when a human adding a rule has the file reread.
//
// It is best effort: it checks the default location only (not a file
// rules_file names), and finds nothing when cwd is outside the
// workspace. A read-only mount fails the check even for root (EROFS).
func WarnWritableRules(cwd string, stderr io.Writer) {
	dir, ok := findRulesDir(cwd)
	if !ok {
		return
	}
	file, _ := rulesfile.Choose(dir)
	for _, p := range []string{dir, file} {
		if syscall.Access(p, wOK) == nil {
			fmt.Fprintf(stderr, "hostrun: warning: %s is writable inside the container, so it can change its own rules; mount %s read-only\n", p, dir)
			return
		}
	}
}

// findRulesDir returns the nearest .devcontainer directory at or above cwd.
func findRulesDir(cwd string) (string, bool) {
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, rulesfile.Dir)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, true
		}
		if dir == filepath.Dir(dir) {
			return "", false
		}
	}
}
