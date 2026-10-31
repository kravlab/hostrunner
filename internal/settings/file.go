package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"
)

// readHostFile reads the host settings file hostFile names: its common
// part and its section for workspace (see workspaceSection), nil when it
// has none. A missing default file sets nothing, a missing named one is
// an error.
func readHostFile(named, workspace string, getenv func(string) string) (common, section layer, err error) {
	path, required := hostFile(named, getenv)
	if path == "" {
		return nil, nil, nil
	}
	common, sections, err := readNamed(path, required, true)
	if err != nil {
		return nil, nil, err
	}
	section, err = workspaceSection(sections, path, workspace, getenv)
	if err != nil {
		return nil, nil, err
	}
	return common, section, nil
}

// readNamed reads the settings file at path as readFile does, except
// that a missing file sets nothing unless it is required.
func readNamed(path string, required, host bool) (layer, map[string]layer, error) {
	l, sections, err := readFile(path, host)
	if !required && errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	return l, sections, err
}

// hostFile is the host settings file: named (the --settings flag) when
// given, else $HOSTRUNNER_SETTINGS, both of which must exist (required),
// else $XDG_CONFIG_HOME/hostrunner/settings.yaml, or
// $HOME/.config/hostrunner/settings.yaml when XDG_CONFIG_HOME is empty,
// which may be missing. With neither variable set there is no default
// file: path is empty.
func hostFile(named string, getenv func(string) string) (path string, required bool) {
	if named != "" {
		return named, true
	}
	if p := getenv("HOSTRUNNER_SETTINGS"); p != "" {
		return p, true
	}
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return "", false
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "hostrunner", "settings.yaml"), false
}

// readFile reads the settings file at path: a YAML mapping of snake_case
// keys, durations as strings with a unit (`grace: 1m30s`), bools as YAML
// bools. The host settings file (host) may also hold `workspaces`, a
// mapping from a workspace path to a mapping of settings, returned as
// sections by path as written. An empty file sets nothing. A file that
// cannot be read keeps its error (fs.ErrNotExist for a missing one); an
// unknown key, a key given twice or an invalid value is an error naming
// the file, the section if any, and the key: `settings file
// /p/settings.yaml: workspaces./home/u/app: grase: unknown key`.
func readFile(path string, host bool) (layer, map[string]layer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("settings file: %w", err)
	}
	where := "settings file " + path + ": "
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("%s%v", where, err)
	}
	if len(doc.Content) == 0 { // empty, or comments only
		return layer{}, nil, nil
	}
	return readSettings(doc.Content[0], where, host)
}

// readSettings reads m, a mapping of settings, into a layer; where
// begins its errors (`settings file P: ` or, in a section, `settings
// file P: workspaces./home/u/app: `). The common part of the host
// settings file (host) may hold `workspaces`, read into sections; any
// other mapping refuses the keys of commonOnly.
func readSettings(m *yaml.Node, where string, host bool) (layer, map[string]layer, error) {
	if m.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%snot a mapping of settings (line %d)", where, m.Line)
	}
	fail := func(name, problem string) error { return fmt.Errorf("%s%s: %s", where, name, problem) }
	l := layer{}
	var sections map[string]layer
	for i := 0; i+1 < len(m.Content); i += 2 {
		name, value := m.Content[i].Value, m.Content[i+1]
		if host && name == "workspaces" {
			if sections != nil { // readSections never returns nil
				return nil, nil, fail(name, "given twice")
			}
			s, err := readSections(value, where)
			if err != nil {
				return nil, nil, err
			}
			sections = s
			continue
		}
		if !host && slices.Contains(commonOnly, name) {
			return nil, nil, fail(name, "only allowed in the common part of the host settings file")
		}
		k, ok := lookup(name)
		if !ok {
			return nil, nil, fail(name, "unknown key")
		}
		if _, dup := l[name]; dup {
			return nil, nil, fail(name, "given twice")
		}
		v, err := nodeValue(k.kind, value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s%s: %w", where, name, err)
		}
		l[name] = v
	}
	return l, sections, nil
}

// readSections reads m, the `workspaces` of the host settings file whose
// errors begin with where, into a layer per workspace path as written;
// every section is read, whichever workspace it names.
func readSections(m *yaml.Node, where string) (map[string]layer, error) {
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%sworkspaces: not a mapping of workspaces (line %d)", where, m.Line)
	}
	sections := map[string]layer{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		path := m.Content[i].Value
		if _, dup := sections[path]; dup {
			return nil, fmt.Errorf("%sworkspaces.%s: given twice", where, path)
		}
		l, _, err := readSettings(m.Content[i+1], where+"workspaces."+path+": ", false)
		if err != nil {
			return nil, err
		}
		sections[path] = l
	}
	return sections, nil
}

// nodeValue reads n as a value of kind k. YAML would read a bare number
// into a time.Duration as nanoseconds and `yes` into a bool, so the node's
// tag must match: a string for a duration, a bool for a bool.
func nodeValue(k kind, n *yaml.Node) (any, error) {
	want := "!!str"
	if k == boolean {
		want = "!!bool"
	}
	switch {
	case n.Kind != yaml.ScalarNode:
		return nil, fmt.Errorf("invalid %s: not a single value (line %d)", k, n.Line)
	case n.Tag == want:
		return k.parse(n.Value)
	case n.Tag == "!!null":
		return nil, fmt.Errorf("invalid %s: no value (line %d)", k, n.Line)
	case k == duration && (n.Tag == "!!int" || n.Tag == "!!float"):
		return nil, fmt.Errorf("invalid duration %s: give a unit, e.g. 90s", n.Value)
	case n.Tag == "!!str":
		return nil, fmt.Errorf("invalid %s %q", k, n.Value)
	default:
		return nil, fmt.Errorf("invalid %s %s", k, n.Value)
	}
}
