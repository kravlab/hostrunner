// Package rulesfile names the default rules file and chooses it between
// the current name and the older one. It has no dependencies: the
// settings and the container's hostrun both need it.
package rulesfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir is the directory of a workspace that holds the default rules file,
// and Name and FallbackName its names: Name is the default, FallbackName
// the older name, used while Name is absent.
const (
	Dir          = ".devcontainer"
	Name         = "hostrunner-rules.yaml"
	FallbackName = "hostrun.yaml"
)

// Choose returns the default rules file in dir, a Dir: path is dir's Name,
// or its FallbackName when Name is absent and FallbackName is present.
// When both are present path is Name, and ignored is the FallbackName
// path, else "". Present means the entry exists, a dangling link
// included; an error other than not existing leaves Name chosen, so that
// a file that cannot be looked at is denied rather than replaced.
func Choose(dir string) (path, ignored string) {
	path, old := filepath.Join(dir, Name), filepath.Join(dir, FallbackName)
	_, err := os.Lstat(path)
	switch {
	case err == nil && present(old):
		return path, old
	case errors.Is(err, fs.ErrNotExist) && present(old):
		return old, ""
	}
	return path, ""
}

// present says whether path exists as a directory entry, a dangling link
// included; any error, not only not existing, counts as absent.
func present(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
