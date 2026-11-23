package stop

import (
	"slices"
	"testing"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/settings"
)

// changes names a change of the process the daemon reports, and none for
// a daemon that reports none, as one older than the field does.
func TestChangesNamesTheProcess(t *testing.T) {
	const rules = "/w/hostrun.yaml"
	for _, tt := range []struct {
		name    string
		process string
		to      settings.Process
		want    []string
	}{
		{"own to shared", "own", settings.ProcessShared, []string{"process own → shared"}},
		{"shared to own", "shared", settings.ProcessOwn, []string{"process shared → own"}},
		{"unchanged", "shared", settings.ProcessShared, nil},
		{"reported none", "", settings.ProcessShared, nil},
	} {
		info := &protocol.DaemonInfo{RulesFile: rules, Process: tt.process}
		got := changes(info, settings.Settings{RulesFile: rules, Process: tt.to})
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: changes = %q, want %q", tt.name, got, tt.want)
		}
	}
}
