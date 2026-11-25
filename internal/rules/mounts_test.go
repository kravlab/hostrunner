package rules

import (
	"slices"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// The report names the rule of a requirement only when it is not the rule
// that allows the command, in the order Mounts has them.
func TestMountRequirementsReportNamesTheRulesOfOtherRules(t *testing.T) {
	p := mustParse(t, "rules:\n"+
		"  - command: git\n    args: any\n    mounts:\n      .git: mounted\n"+
		"  - command: git commit\n    args: any\n    mounts:\n      .git/hooks: read-only\n")
	allowed, err := p.Check([]string{"git", "commit"})
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.MountRequirement{
		{Path: ".git", Mode: "mounted", Rule: "git"},
		{Path: ".git/hooks", Mode: "read-only"},
	}
	if got := allowed.MountRequirementsReport(); !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
