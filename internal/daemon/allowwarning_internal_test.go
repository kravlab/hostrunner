package daemon

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
)

// allowCheckOf is the one conversion of an error to an allowCheck: for the
// result of a check (nil, or why it failed) as for the cause of a refusal
// of refuseNotAllowed.
func TestAllowCheckOf(t *testing.T) {
	failure := errors.New("the allow store is broken")
	for _, tt := range []struct {
		name string
		err  error
		want allowCheck
	}{
		{"no error", nil, checkNotAllowed},
		{"not allowed", errNotAllowed, checkNotAllowed},
		{"refusal, not allowed", &notAllowedError{digest: "d", err: errNotAllowed}, checkNotAllowed},
		{"wrapped, not allowed", fmt.Errorf("check: %w", errNotAllowed), checkNotAllowed},
		{"check failed", failure, checkNotChecked},
		{"refusal, check failed", &notAllowedError{digest: "d", err: failure}, checkNotChecked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowCheckOf(tt.err); got != tt.want {
				t.Errorf("allowCheckOf(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// An arm with the daemon's own settings is not named when its digest is
// that of the refusal pending, but only then: one with an empty digest
// while no refusal is pending has no refusal to be taken for.
func TestArmChangeSuppressesOnlyThePendingRefusal(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pending string
		digest  string
		wantOK  bool
	}{
		{"the refusal pending", "d", "d", false},
		{"another digest", "d", "e", true},
		{"no refusal pending", "", "e", true},
		{"empty digest, no refusal pending", "", "", true},
		{"empty digest, a refusal pending", "d", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			n, ok := s.armChange(protocol.ArmNotAllowed{Digest: tt.digest}, tt.pending)
			if ok != tt.wantOK {
				t.Fatalf("armChange(%q) with %q pending: ok = %v, want %v", tt.digest, tt.pending, ok, tt.wantOK)
			}
			if want := changeNotice(tt.digest, rulesChanged, checkNotAllowed); ok && n != want {
				t.Errorf("armChange(%q) = %+v, want %+v", tt.digest, n, want)
			}
		})
	}
}

// pendingChanges names the refusal pending, then the arm's change, merging
// an arm's change of the rules file into the refusal of the same digest.
func TestPendingChanges(t *testing.T) {
	other := map[string]string{"reread": "false"}
	for _, tt := range []struct {
		name    string
		pending notAllowedReport
		arm     *protocol.ArmNotAllowed
		want    []protocol.RulesNotAllowed
	}{
		{"none", notAllowedReport{}, nil, nil},
		{"a refusal not allowed", notAllowedReport{digest: "d", check: checkNotAllowed}, nil,
			[]protocol.RulesNotAllowed{{Digest: "d"}}},
		{"a refusal not checked", notAllowedReport{digest: "d", check: checkNotChecked}, nil,
			[]protocol.RulesNotAllowed{{Digest: "d", NotChecked: true}}},
		{"an arm of other settings", notAllowedReport{}, &protocol.ArmNotAllowed{Digest: "e", Settings: other},
			[]protocol.RulesNotAllowed{{Digest: "e", Settings: true}}},
		{"an arm of the refusal pending", notAllowedReport{digest: "d", check: checkNotAllowed}, &protocol.ArmNotAllowed{Digest: "d"},
			[]protocol.RulesNotAllowed{{Digest: "d"}}},
		{"a refusal and an arm of other settings", notAllowedReport{digest: "d", check: checkNotAllowed}, &protocol.ArmNotAllowed{Digest: "d", Settings: other},
			[]protocol.RulesNotAllowed{{Digest: "d"}, {Digest: "d", Settings: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{notAllowed: tt.pending, armNotAllowed: tt.arm}
			if got := s.pendingChanges(); !slices.Equal(got, tt.want) {
				t.Errorf("pendingChanges() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
