package daemon

import (
	"log/slog"
	"testing"

	"github.com/kravlab/hostrunner/internal/rules"
	"github.com/kravlab/hostrunner/internal/workspace"
)

// A reread asked for the rules already served, as the watcher's that
// lost the race against a session's own reread of the same file, does
// not reread them and says it changed nothing.
func TestRereadOfTheRulesServedChangesNothing(t *testing.T) {
	mapper, err := workspace.NewMapper(t.TempDir(), "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	p, err := rules.Parse([]byte("rules:\n  - command: echo\n    args: any\n"))
	if err != nil {
		t.Fatal(err)
	}
	rereads := 0
	s := New(mapper, p, slog.New(slog.DiscardHandler), WithReread("old", func() (Policy, string, error) {
		rereads++
		return p, "new", nil
	}))
	if digest, changed, err := s.rereadRules("new"); err != nil || !changed || digest != "new" {
		t.Fatalf("the first reread: got %q, changed %v, %v; want new, changed", digest, changed, err)
	}
	if digest, changed, err := s.rereadRules("new"); err != nil || changed || digest != "new" {
		t.Fatalf("the second reread: got %q, changed %v, %v; want new, unchanged", digest, changed, err)
	}
	if rereads != 1 {
		t.Fatalf("the rules were reread %d times, want once", rereads)
	}
}
