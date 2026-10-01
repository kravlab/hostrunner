package client

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// rulesDir and rulesName locate the default rules file under a workspace.
const (
	rulesDir  = ".devcontainer"
	rulesName = "hostrun.yaml"
)

// wOK is access(2)'s W_OK, which package syscall does not export.
const wOK = 0x2

// WarnWritableRules warns on stderr when the container can change the rules
// that govern it: when the nearest .devcontainer at or above cwd, or the
// hostrun.yaml in it, is writable. A writable directory is enough, since the
// file can then be replaced or, when missing, created; the daemon picks up
// such a change on the next container start.
//
// It is best effort: it checks the default location only (not a --config
// file), and finds nothing when cwd is outside the workspace. A read-only
// mount fails the check even for root (EROFS).
func WarnWritableRules(cwd string, stderr io.Writer) {
	dir, ok := findRulesDir(cwd)
	if !ok {
		return
	}
	file := filepath.Join(dir, rulesName)
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
		p := filepath.Join(dir, rulesDir)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, true
		}
		if dir == filepath.Dir(dir) {
			return "", false
		}
	}
}
