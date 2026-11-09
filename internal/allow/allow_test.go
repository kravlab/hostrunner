package allow_test

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/allow"
)

// daemonSettings returns a fresh map of daemon settings, which a test may
// change without affecting another.
func daemonSettings() map[string]string {
	return map[string]string{"reread": "true", "ask_timeout": "5m"}
}

// target returns the Target of a rules file, digest and daemon settings.
func target(rulesFile, digest string, settings map[string]string) allow.Target {
	return allow.NewTarget(rulesFile, digest, settings)
}

// TestNewTarget checks that NewTarget puts each argument in its field.
func TestNewTarget(t *testing.T) {
	settings := daemonSettings()
	got := allow.NewTarget("/w/rules.yaml", "d1", settings)
	want := allow.Target{RulesFile: "/w/rules.yaml", Scope: allow.Scope{Digest: "d1", Settings: settings}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewTarget = %+v, want %+v", got, want)
	}
}

// fakeGetenv returns a getenv over env; every other variable is empty.
func fakeGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// newStore returns a store in a directory of the test that does not exist
// yet, so the store is empty.
func newStore(t *testing.T) allow.Store {
	t.Helper()
	return allow.Store{Dir: filepath.Join(t.TempDir(), "store", "allow")}
}

// mustAllowed reports whether s allows ws with digest and settings,
// failing the test on an error.
func mustAllowed(t *testing.T, s allow.Store, ws, digest string, settings map[string]string) bool {
	t.Helper()
	ok, err := s.Allowed(ws, allow.Scope{Digest: digest, Settings: settings})
	if err != nil {
		t.Fatalf("Allowed(%q): %v", ws, err)
	}
	return ok
}

func TestOpenUsesXDGDataHomeWhenSet(t *testing.T) {
	got, err := allow.Open(fakeGetenv(map[string]string{"XDG_DATA_HOME": "/xdg/data", "HOME": "/home/u"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (allow.Store{Dir: "/xdg/data/hostrunner/allow"}); got != want {
		t.Fatalf("Open = %+v, want %+v", got, want)
	}
}

func TestOpenFallsBackToHomeLocalShare(t *testing.T) {
	got, err := allow.Open(fakeGetenv(map[string]string{"HOME": "/home/u"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (allow.Store{Dir: "/home/u/.local/share/hostrunner/allow"}); got != want {
		t.Fatalf("Open = %+v, want %+v", got, want)
	}
}

func TestOpenFailsWithoutXDGDataHomeAndHome(t *testing.T) {
	if got, err := allow.Open(fakeGetenv(nil)); err == nil {
		t.Fatalf("Open = %+v, want an error", got)
	}
}

func TestAllowedWorkspaceWithSameDigestAndSettingsIsAllowed(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	e, err := s.Allow(ws, target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", daemonSettings()))
	if err != nil {
		t.Fatal(err)
	}
	want := allow.Entry{
		Workspace: evalDir(t, ws),
		Target:    target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", map[string]string{"reread": "true", "ask_timeout": "5m"}),
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Entry = %+v, want %+v", e, want)
	}
	if !mustAllowed(t, s, ws, "abc123", daemonSettings()) {
		t.Fatal("workspace is not allowed after Allow")
	}
}

func TestDifferentDigestIsNotAllowed(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if mustAllowed(t, s, ws, "def456", daemonSettings()) {
		t.Fatal("a different digest is allowed")
	}
}

func TestChangedSettingsAreNotAllowed(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	changed := daemonSettings()
	changed["reread"] = "false"
	added := daemonSettings()
	added["extra"] = "1"
	removed := map[string]string{"reread": "true"}
	for name, st := range map[string]map[string]string{"changed value": changed, "added key": added, "removed key": removed, "nil": nil} {
		if mustAllowed(t, s, ws, "abc123", st) {
			t.Errorf("settings with %s are allowed", name)
		}
	}
}

func TestWorkspaceNeverAllowedIsNotAllowedWithoutStoreDir(t *testing.T) {
	s := newStore(t)
	ok, err := s.Allowed(t.TempDir(), allow.Scope{Digest: "abc123", Settings: daemonSettings()})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a workspace never allowed is allowed")
	}
}

func TestAllowReplacesTheEarlierAllowOfTheWorkspace(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, err := s.Allow(ws, target("rules.yaml", "contentA", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Allow(ws, target("rules.yaml", "contentB", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if !mustAllowed(t, s, ws, "contentB", daemonSettings()) {
		t.Error("the latest content is not allowed")
	}
	if mustAllowed(t, s, ws, "contentA", daemonSettings()) {
		t.Error("the earlier content is still allowed")
	}
}

func TestAllowOfOneWorkspaceDoesNotAllowAnother(t *testing.T) {
	s := newStore(t)
	x, y := t.TempDir(), t.TempDir()
	if _, err := s.Allow(x, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if mustAllowed(t, s, y, "abc123", daemonSettings()) {
		t.Fatal("workspace Y is allowed by the allow of X")
	}
}

func TestDenyRemovesTheAllow(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Deny(ws)
	if err != nil || !removed {
		t.Fatalf("Deny = %v, %v, want true, nil", removed, err)
	}
	if mustAllowed(t, s, ws, "abc123", daemonSettings()) {
		t.Error("workspace is allowed after Deny")
	}
	removed, err = s.Deny(ws)
	if err != nil || removed {
		t.Fatalf("second Deny = %v, %v, want false, nil", removed, err)
	}
}

func TestDenyWithoutStoreDirRemovesNothing(t *testing.T) {
	removed, err := newStore(t).Deny(t.TempDir())
	if err != nil || removed {
		t.Fatalf("Deny = %v, %v, want false, nil", removed, err)
	}
}

func TestWorkspaceReachedThroughSymlinkIsTheSameWorkspace(t *testing.T) {
	s := newStore(t)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Allow(real, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if !mustAllowed(t, s, link, "abc123", daemonSettings()) {
		t.Fatal("the link path is not the allowed workspace")
	}
	removed, err := s.Deny(link)
	if err != nil || !removed {
		t.Fatalf("Deny via link = %v, %v, want true, nil", removed, err)
	}
	if mustAllowed(t, s, real, "abc123", daemonSettings()) {
		t.Error("the real path is still allowed after Deny via the link")
	}
}

func TestUnresolvableWorkspaceIsKeyedByItsCleanedPath(t *testing.T) {
	s := newStore(t)
	ws := filepath.Join(t.TempDir(), "gone", "..", "missing")
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if !mustAllowed(t, s, filepath.Clean(ws), "abc123", daemonSettings()) {
		t.Fatal("a workspace that does not exist is not allowed by its cleaned path")
	}
}

func TestStoreDirIs0700AndAllowFileIs0600WithNoTempFilesLeft(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	for _, d := range []string{"contentA", "contentB"} {
		if _, err := s.Allow(ws, target("rules.yaml", d, daemonSettings())); err != nil {
			t.Fatal(err)
		}
	}
	di, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("store dir mode = %o, want 700", got)
	}
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("store holds %d files, want 1", len(ents))
	}
	fi, err := ents[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("allow file mode = %o, want 600", got)
	}
	if !strings.HasSuffix(ents[0].Name(), ".json") || len(ents[0].Name()) != 64+len(".json") {
		t.Errorf("allow file name = %q, want 64 hex digits and .json", ents[0].Name())
	}
}

func TestCorruptAllowFileIsAnErrorNamingTheFile(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(s.Dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("ReadDir = %v, %v", ents, err)
	}
	file := filepath.Join(s.Dir, ents[0].Name())
	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Allowed(ws, allow.Scope{Digest: "abc123", Settings: daemonSettings()})
	if err == nil {
		t.Fatal("Allowed returned no error for a corrupt file")
	}
	if ok {
		t.Error("Allowed is true for a corrupt file")
	}
	if !strings.Contains(err.Error(), file) {
		t.Errorf("error %q does not name %q", err, file)
	}
}

// evalDir returns dir with symlinks followed, as the store keys it.
func evalDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// mustPending returns the request of ws, failing the test on an error.
func mustPending(t *testing.T, s allow.Store, ws string) (allow.Entry, bool) {
	t.Helper()
	e, ok, err := s.Pending(ws)
	if err != nil {
		t.Fatalf("Pending(%q): %v", ws, err)
	}
	return e, ok
}

// requestFile returns the path of the one request file in the store.
func requestFile(t *testing.T, s allow.Store) string {
	t.Helper()
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".request.json") {
			found = append(found, filepath.Join(s.Dir, e.Name()))
		}
	}
	if len(found) != 1 {
		t.Fatalf("store holds request files %v, want one", found)
	}
	return found[0]
}

func TestPendingReturnsTheRequestRecorded(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if err := s.Request(ws, target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	e, ok := mustPending(t, s, ws)
	if !ok {
		t.Fatal("no request is pending after Request")
	}
	want := allow.Entry{
		Workspace: evalDir(t, ws),
		Target:    target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", map[string]string{"reread": "true", "ask_timeout": "5m"}),
	}
	if e.Workspace != want.Workspace || e.RulesFile != want.RulesFile || e.Digest != want.Digest || !maps.Equal(e.Settings, want.Settings) {
		t.Fatalf("Pending = %+v, want %+v", e, want)
	}
}

func TestPendingWithoutARequestIsNone(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, ok := mustPending(t, s, ws); ok {
		t.Fatal("a request is pending in an empty store")
	}
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustPending(t, s, ws); ok {
		t.Fatal("an allow is taken for a request")
	}
}

func TestASecondRequestReplacesTheFirst(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if err := s.Request(ws, target("rules.yaml", "contentA", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if err := s.Request(ws, target("other.yaml", "contentB", map[string]string{"grace": "1m0s"})); err != nil {
		t.Fatal(err)
	}
	other, err := filepath.Abs("other.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := mustPending(t, s, ws)
	if !ok || e.RulesFile != other || e.Digest != "contentB" || !maps.Equal(e.Settings, map[string]string{"grace": "1m0s"}) {
		t.Fatalf("Pending = %+v, %v, want the second request", e, ok)
	}
	requestFile(t, s)
}

func TestRequestOfOneWorkspaceIsNotPendingForAnother(t *testing.T) {
	s := newStore(t)
	x, y := t.TempDir(), t.TempDir()
	if err := s.Request(x, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustPending(t, s, y); ok {
		t.Fatal("the request of X is pending for Y")
	}
}

func TestAllowRemovesTheRequest(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if err := s.Request(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustPending(t, s, ws); ok {
		t.Fatal("the request is still pending after Allow")
	}
	if !mustAllowed(t, s, ws, "abc123", daemonSettings()) {
		t.Fatal("the workspace is not allowed")
	}
}

func TestDenyLeavesTheRequest(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if err := s.Request(ws, target("rules.yaml", "def456", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Deny(ws); err != nil || !removed {
		t.Fatalf("Deny = %v, %v, want true, nil", removed, err)
	}
	e, ok := mustPending(t, s, ws)
	if !ok || e.Digest != "def456" {
		t.Fatalf("Pending = %+v, %v, want the request of def456", e, ok)
	}
}

func TestCorruptRequestFileIsAnErrorNamingTheFile(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if err := s.Request(ws, target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	file := requestFile(t, s)
	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.Pending(ws)
	if err == nil {
		t.Fatal("Pending returned no error for a corrupt file")
	}
	if ok {
		t.Error("Pending reports a request for a corrupt file")
	}
	if !strings.Contains(err.Error(), file) {
		t.Errorf("error %q does not name %q", err, file)
	}
}

func TestStoreDirIs0700AndRequestFileIs0600(t *testing.T) {
	s := newStore(t)
	if err := s.Request(t.TempDir(), target("rules.yaml", "abc123", daemonSettings())); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("store dir mode = %o, want 700", got)
	}
	file := requestFile(t, s)
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("request file mode = %o, want 600", got)
	}
	if name := filepath.Base(file); len(name) != 64+len(".request.json") {
		t.Errorf("request file name = %q, want 64 hex digits and .request.json", name)
	}
}

// A store without a directory, or with a relative one, refuses every
// operation, so a zero Store never reads or writes the current directory.
func TestStoreWithoutAnAbsoluteDirFails(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	ws := t.TempDir()
	for _, s := range []allow.Store{{}, {Dir: "allow"}} {
		if _, err := s.Allow(ws, target("rules.yaml", "abc123", daemonSettings())); err == nil {
			t.Errorf("Store%+v.Allow: got no error", s)
		}
		if err := s.Request(ws, target("rules.yaml", "abc123", daemonSettings())); err == nil {
			t.Errorf("Store%+v.Request: got no error", s)
		}
		if _, err := s.Deny(ws); err == nil {
			t.Errorf("Store%+v.Deny: got no error", s)
		}
		if _, err := s.Allowed(ws, allow.Scope{Digest: "abc123", Settings: daemonSettings()}); err == nil {
			t.Errorf("Store%+v.Allowed: got no error", s)
		}
		if _, _, err := s.Pending(ws); err == nil {
			t.Errorf("Store%+v.Pending: got no error", s)
		}
	}
	if ents, err := os.ReadDir(cwd); err != nil || len(ents) != 0 {
		t.Errorf("the current directory holds %v, %v; want nothing", ents, err)
	}
}

// An allow covers settings whose rules_file names the allowed rules file
// through a symbolic link, and the request and the allow both hold the
// rules file with links followed, so a workspace allowed through a link
// and checked through its real path, or the reverse, is the same allow.
func TestAllowedMatchesARulesFileNamedThroughALink(t *testing.T) {
	s := newStore(t)
	base := evalDir(t, t.TempDir())
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realRules := filepath.Join(real, "rules.yaml")
	linkRules := filepath.Join(link, "rules.yaml")
	if err := os.WriteFile(realRules, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Request(link, target(linkRules, "abc123", map[string]string{"rules_file": linkRules, "grace": "15s"})); err != nil {
		t.Fatal(err)
	}
	want := allow.Entry{
		Workspace: real,
		Target:    target(realRules, "abc123", map[string]string{"rules_file": realRules, "grace": "15s"}),
	}
	if e, ok := mustPending(t, s, link); !ok || !reflect.DeepEqual(e, want) {
		t.Fatalf("Pending = %+v, %v, want %+v", e, ok, want)
	}
	e, err := s.Allow(link, target(linkRules, "abc123", map[string]string{"rules_file": linkRules, "grace": "15s"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Allow = %+v, want %+v", e, want)
	}
	if !mustAllowed(t, s, real, "abc123", map[string]string{"rules_file": realRules, "grace": "15s"}) {
		t.Error("the rules file through its real path is not allowed")
	}
	if !mustAllowed(t, s, link, "abc123", map[string]string{"rules_file": linkRules, "grace": "15s"}) {
		t.Error("the rules file through the link is not allowed")
	}
	if mustAllowed(t, s, real, "abc123", map[string]string{"rules_file": filepath.Join(real, "other.yaml"), "grace": "15s"}) {
		t.Error("another rules file is allowed")
	}
}

// A rules file that does not exist is held by its cleaned absolute path.
func TestAllowHoldsAMissingRulesFileByItsCleanedPath(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	missing := evalDir(t, ws) + "/gone/../missing.yaml"
	e, err := s.Allow(ws, target(missing, "missing", map[string]string{"rules_file": missing}))
	if err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(evalDir(t, ws), "missing.yaml")
	want := allow.Entry{Workspace: evalDir(t, ws), Target: target(clean, "missing", map[string]string{"rules_file": clean})}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Allow = %+v, want %+v", e, want)
	}
}

// A workspace and a symbolic link to it are one workspace, as an allow
// keys them; another directory is not.
func TestSameWorkspaceFollowsSymbolicLinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	other := filepath.Join(base, "other")
	for _, d := range []string{real, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{real, link, true},
		{link, real + "/", true},
		{real, other, false},
		{link, other, false},
	} {
		if got, err := allow.SameWorkspace(tt.a, tt.b); err != nil || got != tt.want {
			t.Errorf("SameWorkspace(%q, %q) = %v, %v; want %v", tt.a, tt.b, got, err, tt.want)
		}
	}
}

// A workspace that does not exist is compared by its cleaned absolute
// path, as an allow keys it.
func TestSameWorkspaceComparesAMissingWorkspaceByItsCleanedPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if got, err := allow.SameWorkspace(missing, missing+"/sub/.."); err != nil || !got {
		t.Errorf("SameWorkspace of one missing path = %v, %v; want true", got, err)
	}
	if got, err := allow.SameWorkspace(missing, missing+"2"); err != nil || got {
		t.Errorf("SameWorkspace of two missing paths = %v, %v; want false", got, err)
	}
}
