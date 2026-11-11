package allow

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustAllow allows tg for the Place p in s, as AllowAll of that single
// request, and returns the Entry recorded, failing the test on an error.
func mustAllow(t *testing.T, s Store, p Place, tg Target) Entry {
	t.Helper()
	allowed, err := s.AllowAll(p.Workspace, []Entry{{Place: p, Target: tg}})
	if err != nil {
		t.Fatalf("allow %s: %v", p.RuntimeDir, err)
	}
	return allowed[0]
}

// opsOf returns the file operations of s, setting them first when s has
// none.
func opsOf(s *Store) *fileOps {
	if s.ops == nil {
		s.ops = &fileOps{}
	}
	return s.ops
}

// failingRename makes the rename of s one whose n-th call fails.
func failingRename(s *Store, n int) {
	calls := 0
	opsOf(s).rename = func(oldpath, newpath string) error {
		if calls++; calls == n {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldpath, newpath)
	}
}

// failingRemove makes the remove of s one whose n-th call fails.
func failingRemove(s *Store, n int) {
	calls := 0
	opsOf(s).remove = func(path string) error {
		if calls++; calls == n {
			return errors.New("injected remove failure")
		}
		return os.Remove(path)
	}
}

// tempFiles returns the temporary files left under dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".allow-") {
			found = append(found, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A rename that fails while AllowAddedRule moves the allows records none:
// each allow keeps the content before the write, and no temporary file
// stays.
func TestAllowAddedRuleMovesNoneWhenARenameFails(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "allow")}
	ws := t.TempDir()
	settings := map[string]string{"reread": "true"}
	dirs := []string{"/run/hostrunner/a", "/run/hostrunner/b"}
	for _, rd := range dirs {
		mustAllow(t, s, Place{Workspace: ws, RuntimeDir: rd}, NewTarget("rules.yaml", "before", settings))
	}
	failingRename(&s, 2)
	if _, err := s.AllowAddedRule(Place{Workspace: ws, RuntimeDir: dirs[0]}, Scope{Digest: "before", Settings: settings}, "after"); err == nil {
		t.Fatal("AllowAddedRule returned no error")
	}
	for _, rd := range dirs {
		if ok, err := s.Allowed(Place{Workspace: ws, RuntimeDir: rd}, Scope{Digest: "before", Settings: settings}); err != nil || !ok {
			t.Errorf("%s: Allowed(before) = %v, %v; want the allow kept", rd, ok, err)
		}
	}
	if left := tempFiles(t, s.Dir); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// A rename that fails while AllowAll moves the allows into place records
// none: the allows renamed before it are put back as they were, an
// earlier allow rewritten and a runtime directory without one left
// without, and no temporary file stays.
func TestAllowAllPutsBackTheEarlierAllowsWhenARenameFails(t *testing.T) {
	ws := t.TempDir()
	withEarlier := Place{Workspace: ws, RuntimeDir: "/run/hostrunner/a"}
	without := Place{Workspace: ws, RuntimeDir: "/run/hostrunner/b"}
	earlier := NewTarget("rules.yaml", "earlier", map[string]string{"grace": "15s"})
	later := NewTarget("rules.yaml", "later", map[string]string{"grace": "15s"})
	for name, order := range map[string][]Place{
		"earlier allow renamed first": {withEarlier, without},
		"no allow renamed first":      {without, withEarlier},
	} {
		t.Run(name, func(t *testing.T) {
			s := Store{Dir: filepath.Join(t.TempDir(), "allow")}
			mustAllow(t, s, withEarlier, earlier)
			failingRename(&s, 2)
			if _, err := s.AllowAll(ws, []Entry{{Place: order[0], Target: later}, {Place: order[1], Target: later}}); err == nil {
				t.Fatal("AllowAll returned no error")
			}
			for _, tt := range []struct {
				p    Place
				t    Target
				want bool
			}{
				{withEarlier, earlier, true},
				{withEarlier, later, false},
				{without, later, false},
			} {
				if got, err := s.Allowed(tt.p, tt.t.Scope); err != nil || got != tt.want {
					t.Errorf("Allowed(%s, %s) = %v, %v; want %v", tt.p.RuntimeDir, tt.t.Digest, got, err, tt.want)
				}
			}
			if left := tempFiles(t, s.Dir); len(left) != 0 {
				t.Errorf("temporary files left: %v", left)
			}
		})
	}
}

// An allow that cannot be removed fails Deny, which returns the allows it
// removed before it.
func TestDenyReturnsTheAllowsRemovedBeforeAFailure(t *testing.T) {
	ws := t.TempDir()
	s := Store{Dir: filepath.Join(t.TempDir(), "allow")}
	a := mustAllow(t, s, Place{Workspace: ws, RuntimeDir: "/run/hostrunner/a"}, NewTarget("rules.yaml", "d", nil))
	mustAllow(t, s, Place{Workspace: ws, RuntimeDir: "/run/hostrunner/b"}, NewTarget("rules.yaml", "d", nil))
	failingRemove(&s, 2)
	removed, err := s.Deny(ws)
	if err == nil {
		t.Fatal("Deny returned no error")
	}
	if len(removed) != 1 || removed[0].RuntimeDir != a.RuntimeDir {
		t.Errorf("Deny removed %+v, want the allow of a only", removed)
	}
}
