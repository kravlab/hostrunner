package rules

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// MountMode is how a rule requires a path of the workspace to be mounted
// in the container.
type MountMode int

const (
	// MountMounted (`mounted`): the path is a mount of its own.
	MountMounted MountMode = iota + 1
	// MountReadOnly (`read-only`): the path is a mount the container
	// cannot write to.
	MountReadOnly
)

// mountModeNames are the YAML spellings of the modes, indexed by mode: the
// one source of both String and parseMountMode.
var mountModeNames = [...]string{MountMounted: "mounted", MountReadOnly: "read-only"}

// String returns the YAML spelling of the mode, or "unknown" for a value
// that is not a mode.
func (m MountMode) String() string {
	if m < MountMounted || int(m) >= len(mountModeNames) {
		return "unknown"
	}
	return mountModeNames[m]
}

// parseMountMode returns the mode the YAML spelling name stands for, and
// whether it stands for one.
func parseMountMode(name string) (MountMode, bool) {
	for m := MountMounted; int(m) < len(mountModeNames); m++ {
		if mountModeNames[m] == name {
			return m, true
		}
	}
	return 0, false
}

// MountRequirement is what a rule requires of one path in the container:
// Path is relative to the workspace root, in forward-slash form, and Mode
// says how it has to be mounted.
type MountRequirement struct {
	Path string
	Mode MountMode
}

// MountRequirementsReport returns the allowing rule's mount requirements
// as an Allowed answer reports them, in the order Mounts has them, or nil
// when it has none. The dry run and the Rules test (`hostrunner test`)
// both report them through it, as they report the secrets through
// SecretNames.
func (a Allowed) MountRequirementsReport() []protocol.MountRequirement {
	var report []protocol.MountRequirement
	for _, r := range a.Mounts {
		report = append(report, protocol.MountRequirement{Path: r.Path, Mode: r.Mode.String()})
	}
	return report
}

// newMounts validates the `mounts` value of a rule.
//
// A failure names the line of the entry, as the yaml package counts it.
// `mounts: *name` stands for the anchored map it refers to, so rules can
// share one block; a value that is not a map names the line of the alias.
func newMounts(n yaml.Node) ([]MountRequirement, error) {
	if n.Kind == 0 {
		return nil, nil // no mounts key
	}
	entries := &n
	if n.Kind == yaml.AliasNode {
		entries = n.Alias
	}
	if entries.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("mounts: not a mapping of workspace paths to modes (line %d)", n.Line)
	}
	var reqs []MountRequirement
	for i := 0; i+1 < len(entries.Content); i += 2 {
		key, val := entries.Content[i], entries.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" {
			return nil, fmt.Errorf("mounts: path must be a string (line %d)", key.Line)
		}
		if path.IsAbs(key.Value) {
			return nil, fmt.Errorf("mounts: path %q must be relative to the workspace root (line %d)", key.Value, key.Line)
		}
		if slices.Contains(strings.Split(key.Value, "/"), "..") {
			return nil, fmt.Errorf("mounts: path %q must not contain a .. component (line %d)", key.Value, key.Line)
		}
		switch clean := path.Clean(key.Value); {
		case clean == ".":
			return nil, fmt.Errorf("mounts: path %q is not a path below the workspace root (line %d)", key.Value, key.Line)
		case clean != key.Value:
			return nil, fmt.Errorf("mounts: path %q is not clean, use %q (line %d)", key.Value, clean, key.Line)
		}
		if val.Kind != yaml.ScalarNode || val.ShortTag() != "!!str" {
			return nil, fmt.Errorf("mounts: path %q: mode must be the string mounted or read-only (line %d)", key.Value, key.Line)
		}
		mode, ok := parseMountMode(val.Value)
		if !ok {
			return nil, fmt.Errorf("mounts: path %q: mode must be mounted or read-only, not %q (line %d)", key.Value, val.Value, key.Line)
		}
		if slices.ContainsFunc(reqs, func(r MountRequirement) bool { return r.Path == key.Value }) {
			return nil, fmt.Errorf("mounts: duplicate path %q (line %d)", key.Value, key.Line)
		}
		reqs = append(reqs, MountRequirement{Path: key.Value, Mode: mode})
	}
	slices.SortFunc(reqs, func(a, b MountRequirement) int { return strings.Compare(a.Path, b.Path) })
	return reqs, nil
}
