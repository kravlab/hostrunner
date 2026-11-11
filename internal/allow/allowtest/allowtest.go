// Package allowtest helps the tests of other packages record allows in an
// allow store.
package allowtest

import (
	"testing"

	"github.com/kravlab/hostrunner/internal/allow"
)

// Allow allows tg for the Place p in s, as `hostrunner allow` allows a
// single pending request (see allow.Store.AllowAll), and returns the Entry
// recorded. It fails the test on any error, a request answered that could
// not be removed included.
func Allow(t testing.TB, s allow.Store, p allow.Place, tg allow.Target) allow.Entry {
	t.Helper()
	allowed, err := s.AllowAll(p.Workspace, []allow.Entry{{Place: p, Target: tg}})
	if err != nil {
		t.Fatalf("allow %s of %s: %v", p.RuntimeDir, p.Workspace, err)
	}
	return allowed[0]
}
