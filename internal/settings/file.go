package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// readHostFile reads the host settings file hostFile names; a missing
// default file sets nothing, a missing named one is an error.
func readHostFile(named string, getenv func(string) string) (layer, error) {
	path, required := hostFile(named, getenv)
	if path == "" {
		return nil, nil
	}
	l, err := readFile(path)
	if !required && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return l, err
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
// bools. An empty file sets nothing. A file that cannot be read keeps
// its error (fs.ErrNotExist for a missing one); an unknown key, a key
// given twice or an invalid value is an error naming the file and the
// key: `settings file /p/settings.yaml: grase: unknown key`.
func readFile(path string) (layer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("settings file: %w", err)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("settings file %s: "+format, append([]any{path}, args...)...)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fail("%v", err)
	}
	l := layer{}
	if len(doc.Content) == 0 { // empty, or comments only
		return l, nil
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, fail("not a mapping of settings (line %d)", m.Line)
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		name, value := m.Content[i].Value, m.Content[i+1]
		k, ok := lookup(name)
		if !ok {
			return nil, fail("%s: unknown key", name)
		}
		if _, dup := l[name]; dup {
			return nil, fail("%s: given twice", name)
		}
		v, err := nodeValue(k.kind, value)
		if err != nil {
			return nil, fail("%s: %w", name, err)
		}
		l[name] = v
	}
	return l, nil
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
