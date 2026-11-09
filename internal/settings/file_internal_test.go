package settings

import "testing"

// A key of stop, restart or approve is allowed in the common part of the
// host settings file alone, mount_check in the host settings file alone,
// and every other key in every part.
func TestKeyAllowedIn(t *testing.T) {
	parts := []struct {
		name string
		p    part
	}{{"common part", commonPart}, {"section", sectionPart}, {"workspace settings file", workspacePart}}
	for _, tt := range []struct {
		key  string
		want [3]bool // by part, in the order of parts
	}{
		{"grace", [3]bool{true, true, true}},
		{RulesFileKey, [3]bool{true, true, true}},
		{mountCheckKey, [3]bool{true, true, false}},
		{"runtime_root", [3]bool{true, false, false}},
		{"stop_timeout", [3]bool{true, false, false}},
		{"approve_tail_lines", [3]bool{true, false, false}},
	} {
		k := mustLookup(tt.key)
		for i, p := range parts {
			if got := k.allowedIn(p.p); got != tt.want[i] {
				t.Errorf("%s in the %s: allowed %v, want %v", tt.key, p.name, got, tt.want[i])
			}
		}
	}
}
