package settings

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// readWorkspaceFile reads the workspace settings file workspaceFile
// names, which holds the keys at its top level and no sections. A missing
// default file sets nothing, a missing named one is an error.
func readWorkspaceFile(in scopes, named, workspace string, getenv func(string) string) (sourced, error) {
	path, required := workspaceFile(named, workspace, getenv)
	l, _, err := readNamed(in, path, required, false)
	return sourced{l, inFile(path, "")}, err
}

// workspaceFile is the workspace settings file of the workspace at the
// host path workspace: named (the --workspace-settings flag, relative to
// the current directory) when given, else $HOSTRUNNER_WORKSPACE_SETTINGS,
// relative to the workspace, both of which must exist (required), else
// .devcontainer/hostrunner.yaml in the workspace, which may be missing.
func workspaceFile(named, workspace string, getenv func(string) string) (path string, required bool) {
	if named != "" {
		return named, true
	}
	if p := getenv("HOSTRUNNER_WORKSPACE_SETTINGS"); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(workspace, p)
		}
		return p, true
	}
	return filepath.Join(workspace, ".devcontainer", "hostrunner.yaml"), false
}

// workspaceSection returns the path of the section of sections, read
// from the host settings file at file, for the workspace at the host path
// workspace: the one whose path is the workspace's after ~ expansion
// (with $HOME getenv reads) and symlink resolution, empty when none is. A path that
// does not exist, like a workspace that does not, matches nothing; a path
// that is not absolute after ~ expansion, or two paths of the workspace,
// is an error naming the file and the paths.
func workspaceSection(sections map[string]layer, file, workspace string, getenv func(string) string) (string, error) {
	if len(sections) == 0 {
		return "", nil
	}
	ws := realPath(workspace)
	match := ""
	for _, p := range slices.Sorted(maps.Keys(sections)) {
		expanded, ok := expandHome(p, getenv)
		if !ok {
			return "", fmt.Errorf("settings file %s: workspaces.%s: ~ needs HOME, which is not set", file, p)
		}
		if !filepath.IsAbs(expanded) {
			return "", fmt.Errorf("settings file %s: workspaces.%s: not an absolute path", file, p)
		}
		if ws == "" || realPath(expanded) != ws {
			continue
		}
		if match != "" {
			return "", fmt.Errorf("settings file %s: workspaces: %s and %s both name the workspace %s", file, match, p, workspace)
		}
		match = p
	}
	return match, nil
}

// expandHome replaces a leading ~ of p, alone or before a /, with $HOME
// as getenv reads it; ~user stays as it is. With HOME empty such a ~
// cannot be expanded: ok is false.
func expandHome(p string, getenv func(string) string) (expanded string, ok bool) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, true
	}
	home := getenv("HOME")
	if home == "" {
		return p, false
	}
	return home + p[1:], true
}

// realPath is p made absolute with every symlink resolved, or empty when
// p cannot be resolved (it does not exist).
func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	return resolved
}
