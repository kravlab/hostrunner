package daemon_test

import (
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// A dry run reports the mounts the allowing rule requires, in the order the
// rules give them, so the line the client prints names them.
func TestDryRunReportsTheMountsTheRuleRequires(t *testing.T) {
	l := startServer(t, policyOf{rules.Allowed{Rule: "true", Mounts: []rules.MountRequirement{
		{Path: ".git", Mode: rules.MountMounted},
		{Path: ".git/config", Mode: rules.MountReadOnly},
	}}})

	got := ask(t, l, protocol.FrameDryRun, "true")

	if got.end.Type != protocol.FrameAllowed {
		t.Fatalf("the dry run ended with frame %d %s, want Allowed", got.end.Type, got.end.Payload)
	}
	var a protocol.Allowed
	if err := protocol.DecodeJSON(got.end, &a); err != nil {
		t.Fatal(err)
	}
	want := `allowed by rule "true", requires mounts: .git mounted, .git/config read-only`
	if a.String() != want {
		t.Errorf("got %q, want %q", a.String(), want)
	}
}
