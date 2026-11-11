package allow_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kravlab/hostrunner/internal/allow"
	"github.com/kravlab/hostrunner/internal/allow/allowtest"
)

// The runtime directories of two devcontainer configs of a workspace. They
// do not exist, so the store keys them by their cleaned absolute paths.
const (
	runA = "/run/hostrunner/a"
	runB = "/run/hostrunner/b"
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

// mustAllowed reports whether s allows runtime directory rd of ws with
// digest and settings, failing the test on an error.
func mustAllowed(t *testing.T, s allow.Store, ws, rd, digest string, settings map[string]string) bool {
	t.Helper()
	ok, err := s.Allowed(allow.Place{Workspace: ws, RuntimeDir: rd}, allow.Scope{Digest: digest, Settings: settings})
	if err != nil {
		t.Fatalf("Allowed(%q, %q): %v", ws, rd, err)
	}
	return ok
}

// mustAllow allows tg for runtime directory rd of ws, failing the test on
// an error.
func mustAllow(t *testing.T, s allow.Store, ws, rd string, tg allow.Target) allow.Entry {
	t.Helper()
	return allowtest.Allow(t, s, allow.Place{Workspace: ws, RuntimeDir: rd}, tg)
}

// mustRequest records the request of tg for runtime directory rd of ws,
// failing the test on an error.
func mustRequest(t *testing.T, s allow.Store, ws, rd string, tg allow.Target) {
	t.Helper()
	if err := s.Request(allow.Place{Workspace: ws, RuntimeDir: rd}, tg); err != nil {
		t.Fatalf("Request(%q, %q): %v", ws, rd, err)
	}
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
	e := mustAllow(t, s, ws, runA, target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", daemonSettings()))
	want := allow.Entry{
		Workspace:  evalDir(t, ws),
		RuntimeDir: runA,
		Target:     target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", map[string]string{"reread": "true", "ask_timeout": "5m"}),
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Entry = %+v, want %+v", e, want)
	}
	if !mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
		t.Fatal("workspace is not allowed after Allow")
	}
}

func TestDifferentDigestIsNotAllowed(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	if mustAllowed(t, s, ws, runA, "def456", daemonSettings()) {
		t.Fatal("a different digest is allowed")
	}
}

func TestChangedSettingsAreNotAllowed(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	changed := daemonSettings()
	changed["reread"] = "false"
	added := daemonSettings()
	added["extra"] = "1"
	removed := map[string]string{"reread": "true"}
	for name, st := range map[string]map[string]string{"changed value": changed, "added key": added, "removed key": removed, "nil": nil} {
		if mustAllowed(t, s, ws, runA, "abc123", st) {
			t.Errorf("settings with %s are allowed", name)
		}
	}
}

func TestWorkspaceNeverAllowedIsNotAllowedWithoutStoreDir(t *testing.T) {
	s := newStore(t)
	if mustAllowed(t, s, t.TempDir(), runA, "abc123", daemonSettings()) {
		t.Fatal("a workspace never allowed is allowed")
	}
}

func TestAllowReplacesTheEarlierAllowOfTheRuntimeDir(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "contentA", daemonSettings()))
	mustAllow(t, s, ws, runA, target("rules.yaml", "contentB", daemonSettings()))
	if !mustAllowed(t, s, ws, runA, "contentB", daemonSettings()) {
		t.Error("the latest content is not allowed")
	}
	if mustAllowed(t, s, ws, runA, "contentA", daemonSettings()) {
		t.Error("the earlier content is still allowed")
	}
}

func TestAllowOfOneWorkspaceDoesNotAllowAnother(t *testing.T) {
	s := newStore(t)
	x, y := t.TempDir(), t.TempDir()
	mustAllow(t, s, x, runA, target("rules.yaml", "abc123", daemonSettings()))
	if mustAllowed(t, s, y, runA, "abc123", daemonSettings()) {
		t.Fatal("workspace Y is allowed by the allow of X")
	}
}

// Two devcontainer configs of one workspace, each with its runtime
// directory, keep an allow each: allowing one neither allows the other
// nor replaces its allow.
func TestAllowsOfTwoRuntimeDirsAreKeptApart(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	if mustAllowed(t, s, ws, runB, "abc123", daemonSettings()) {
		t.Fatal("runtime directory B is allowed by the allow of A")
	}
	grace := map[string]string{"grace": "1m0s"}
	mustAllow(t, s, ws, runB, target("rules.yaml", "abc123", grace))
	if !mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
		t.Error("the allow of B replaced that of A")
	}
	if !mustAllowed(t, s, ws, runB, "abc123", grace) {
		t.Error("B is not allowed with its settings")
	}
	if mustAllowed(t, s, ws, runA, "abc123", grace) {
		t.Error("A is allowed with the settings of B")
	}
}

// A runtime directory named through a symbolic link or with a trailing
// slash is the same runtime directory, as the store keys it.
func TestRuntimeDirThroughALinkOrWithATrailingSlashSharesTheAllow(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e := mustAllow(t, s, ws, link+"/", target("rules.yaml", "abc123", daemonSettings()))
	if want := evalDir(t, real); e.RuntimeDir != want {
		t.Errorf("Entry.RuntimeDir = %q, want %q", e.RuntimeDir, want)
	}
	for _, rd := range []string{real, real + "/", link} {
		if !mustAllowed(t, s, ws, rd, "abc123", daemonSettings()) {
			t.Errorf("runtime directory %q does not share the allow", rd)
		}
	}
	mustRequest(t, s, ws, real+"/", target("rules.yaml", "def456", daemonSettings()))
	if got, ok := mustPending(t, s, ws, link); !ok || got.Digest != "def456" {
		t.Errorf("Pending through the link = %+v, %v; want the request of def456", got, ok)
	}
}

func TestDenyRemovesTheAllow(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	e := mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	removed, err := s.Deny(ws)
	if err != nil || !reflect.DeepEqual(removed, []allow.Entry{e}) {
		t.Fatalf("Deny = %+v, %v, want [%+v], nil", removed, err, e)
	}
	if mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
		t.Error("workspace is allowed after Deny")
	}
	removed, err = s.Deny(ws)
	if err != nil || len(removed) != 0 {
		t.Fatalf("second Deny = %+v, %v, want none, nil", removed, err)
	}
}

// Deny removes the allows of every runtime directory of the workspace, and
// returns them sorted by runtime directory; another workspace keeps its.
func TestDenyRemovesTheAllowsOfEveryRuntimeDir(t *testing.T) {
	s := newStore(t)
	ws, other := t.TempDir(), t.TempDir()
	grace := map[string]string{"grace": "1m0s"}
	b := mustAllow(t, s, ws, runB, target("rules.yaml", "abc123", grace))
	a := mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	mustAllow(t, s, other, runA, target("rules.yaml", "abc123", daemonSettings()))
	removed, err := s.Deny(ws)
	if err != nil || !reflect.DeepEqual(removed, []allow.Entry{a, b}) {
		t.Fatalf("Deny = %+v, %v, want [%+v %+v], nil", removed, err, a, b)
	}
	if mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) || mustAllowed(t, s, ws, runB, "abc123", grace) {
		t.Error("a runtime directory is still allowed after Deny")
	}
	if !mustAllowed(t, s, other, runA, "abc123", daemonSettings()) {
		t.Error("Deny removed the allow of another workspace")
	}
}

func TestDenyWithoutStoreDirRemovesNothing(t *testing.T) {
	removed, err := newStore(t).Deny(t.TempDir())
	if err != nil || len(removed) != 0 {
		t.Fatalf("Deny = %+v, %v, want none, nil", removed, err)
	}
}

// Deny removes a damaged allow file together with the valid allows, which
// it returns, and reports the damaged one in a *allow.DamagedFilesError
// that only warns; the pending request stays.
func TestDenyRemovesADamagedAllowFileAndReportsIt(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	files, _ := storeFiles(t, s)
	b := mustAllow(t, s, ws, runB, target("rules.yaml", "abc123", daemonSettings()))
	mustRequest(t, s, ws, runA, target("rules.yaml", "def456", daemonSettings()))
	if err := os.WriteFile(files[0], []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Deny(ws)
	if !reflect.DeepEqual(removed, []allow.Entry{b}) {
		t.Errorf("Deny removed %+v, want [%+v]", removed, b)
	}
	damaged, ok := errors.AsType[*allow.DamagedFilesError](err)
	if !ok || len(damaged.Files) != 1 || damaged.Files[0].Path != files[0] || damaged.Files[0].Err == nil {
		t.Fatalf("Deny error = %v, want a *allow.DamagedFilesError of %s", err, files[0])
	}
	if !allow.OnlyWarns(err) {
		t.Errorf("Deny error %v does not only warn", err)
	}
	after, _ := storeFiles(t, s)
	if len(after) != 1 || !strings.HasSuffix(after[0], ".request.json") {
		t.Errorf("store files after Deny = %v, want the request only", after)
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
	mustAllow(t, s, real, runA, target("rules.yaml", "abc123", daemonSettings()))
	if !mustAllowed(t, s, link, runA, "abc123", daemonSettings()) {
		t.Fatal("the link path is not the allowed workspace")
	}
	removed, err := s.Deny(link)
	if err != nil || len(removed) != 1 {
		t.Fatalf("Deny via link = %+v, %v, want one, nil", removed, err)
	}
	if mustAllowed(t, s, real, runA, "abc123", daemonSettings()) {
		t.Error("the real path is still allowed after Deny via the link")
	}
}

func TestUnresolvableWorkspaceIsKeyedByItsCleanedPath(t *testing.T) {
	s := newStore(t)
	ws := filepath.Join(t.TempDir(), "gone", "..", "missing")
	mustAllow(t, s, ws, runA+"/x/..", target("rules.yaml", "abc123", daemonSettings()))
	if !mustAllowed(t, s, filepath.Clean(ws), runA, "abc123", daemonSettings()) {
		t.Fatal("a workspace that does not exist is not allowed by its cleaned path")
	}
}

// storeFiles returns the regular files under the store's directory, and
// the directories, in lexical order, failing the test on an error.
func storeFiles(t *testing.T, s allow.Store) (files, dirs []string) {
	t.Helper()
	err := filepath.WalkDir(s.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		} else {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, dirs
}

// checkMode fails the test unless the file at path has the permissions
// want.
func checkMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %o, want %o", path, got, want)
	}
}

func TestStoreDirsAre0700AndAllowFileIs0600WithNoTempFilesLeft(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	for _, d := range []string{"contentA", "contentB"} {
		mustAllow(t, s, ws, runA, target("rules.yaml", d, daemonSettings()))
	}
	files, dirs := storeFiles(t, s)
	for _, d := range dirs {
		checkMode(t, d, 0o700)
	}
	if len(files) != 1 {
		t.Fatalf("store holds files %v, want 1", files)
	}
	checkMode(t, files[0], 0o600)
	if name := filepath.Base(files[0]); !strings.HasSuffix(name, ".json") || len(name) != 64+len(".json") {
		t.Errorf("allow file name = %q, want 64 hex digits and .json", name)
	}
}

func TestCorruptAllowFileIsAnErrorNamingTheFile(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	files, _ := storeFiles(t, s)
	if len(files) != 1 {
		t.Fatalf("store holds files %v, want 1", files)
	}
	if err := os.WriteFile(files[0], []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Allowed(allow.Place{Workspace: ws, RuntimeDir: runA}, allow.Scope{Digest: "abc123", Settings: daemonSettings()})
	if err == nil {
		t.Fatal("Allowed returned no error for a corrupt file")
	}
	if ok {
		t.Error("Allowed is true for a corrupt file")
	}
	if !strings.Contains(err.Error(), files[0]) {
		t.Errorf("error %q does not name %q", err, files[0])
	}
}

// rewrite changes the JSON object in the file at path with edit.
func rewrite(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An allow or request file that holds another runtime directory than the
// one it is the file of, or none, is an error naming the file, not an
// allow or a request.
func TestAFileOfAnotherRuntimeDirIsAnErrorNamingTheFile(t *testing.T) {
	for _, rd := range []string{runB, ""} {
		s := newStore(t)
		ws := t.TempDir()
		mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
		mustRequest(t, s, ws, runA, target("rules.yaml", "def456", daemonSettings()))
		files, _ := storeFiles(t, s)
		for _, f := range files {
			rewrite(t, f, func(m map[string]any) { m["runtime_dir"] = rd })
		}
		_, err := s.Allowed(allow.Place{Workspace: ws, RuntimeDir: runA}, allow.Scope{Digest: "abc123", Settings: daemonSettings()})
		if err == nil || !strings.Contains(err.Error(), s.Dir) {
			t.Errorf("runtime_dir %q: Allowed error = %v, want one naming the file", rd, err)
		}
		if _, err := s.PendingAll(ws); err == nil || !strings.Contains(err.Error(), s.Dir) {
			t.Errorf("runtime_dir %q: PendingAll error = %v, want one naming the file", rd, err)
		}
	}
}

// An allow of the earlier shape, one file per workspace without a runtime
// directory, is no allow: up asks for a new one.
func TestAnAllowOfTheEarlierShapeIsNoAllow(t *testing.T) {
	s := newStore(t)
	ws := evalDir(t, t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(ws))
	data, err := json.Marshal(map[string]any{"workspace": ws, "rules_file": "/ws/rules.yaml", "digest": "abc123", "settings": daemonSettings()})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".json", ".request.json"} {
		if err := os.WriteFile(filepath.Join(s.Dir, hex.EncodeToString(sum[:])+suffix), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
		t.Error("an allow of the earlier shape is an allow")
	}
	if got, err := s.PendingAll(ws); err != nil || len(got) != 0 {
		t.Errorf("PendingAll = %+v, %v; want none", got, err)
	}
	if removed, err := s.Deny(ws); err != nil || len(removed) != 0 {
		t.Errorf("Deny = %+v, %v; want none", removed, err)
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

// mustPending returns the pending request of runtime directory rd of ws,
// as PendingAll returns it, failing the test on an error.
func mustPending(t *testing.T, s allow.Store, ws, rd string) (allow.Entry, bool) {
	t.Helper()
	for _, e := range mustPendingAll(t, s, ws) {
		if same, err := allow.SameWorkspace(e.RuntimeDir, rd); err != nil {
			t.Fatal(err)
		} else if same {
			return e, true
		}
	}
	return allow.Entry{}, false
}

// mustPendingAll returns the requests of ws, failing the test on an error.
func mustPendingAll(t *testing.T, s allow.Store, ws string) []allow.Entry {
	t.Helper()
	got, err := s.PendingAll(ws)
	if err != nil {
		t.Fatalf("PendingAll(%q): %v", ws, err)
	}
	return got
}

// requestFile returns the path of the one request file in the store.
func requestFile(t *testing.T, s allow.Store) string {
	t.Helper()
	files, _ := storeFiles(t, s)
	var found []string
	for _, f := range files {
		if strings.HasSuffix(f, ".request.json") {
			found = append(found, f)
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
	mustRequest(t, s, ws, runA, target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", daemonSettings()))
	e, ok := mustPending(t, s, ws, runA)
	if !ok {
		t.Fatal("no request is pending after Request")
	}
	want := allow.Entry{
		Workspace:  evalDir(t, ws),
		RuntimeDir: runA,
		Target:     target("/ws/.devcontainer/hostrunner-rules.yaml", "abc123", map[string]string{"reread": "true", "ask_timeout": "5m"}),
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Pending = %+v, want %+v", e, want)
	}
}

func TestPendingWithoutARequestIsNone(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	if _, ok := mustPending(t, s, ws, runA); ok {
		t.Fatal("a request is pending in an empty store")
	}
	if got := mustPendingAll(t, s, ws); len(got) != 0 {
		t.Fatalf("PendingAll of an empty store = %+v, want none", got)
	}
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	if _, ok := mustPending(t, s, ws, runA); ok {
		t.Fatal("an allow is taken for a request")
	}
	if got := mustPendingAll(t, s, ws); len(got) != 0 {
		t.Fatalf("PendingAll = %+v, want none", got)
	}
}

func TestASecondRequestReplacesTheFirst(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustRequest(t, s, ws, runA, target("rules.yaml", "contentA", daemonSettings()))
	mustRequest(t, s, ws, runA, target("other.yaml", "contentB", map[string]string{"grace": "1m0s"}))
	other, err := filepath.Abs("other.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := mustPending(t, s, ws, runA)
	if !ok || e.RulesFile != other || e.Digest != "contentB" || !maps.Equal(e.Settings, map[string]string{"grace": "1m0s"}) {
		t.Fatalf("Pending = %+v, %v, want the second request", e, ok)
	}
	requestFile(t, s)
}

func TestRequestOfOneWorkspaceIsNotPendingForAnother(t *testing.T) {
	s := newStore(t)
	x, y := t.TempDir(), t.TempDir()
	mustRequest(t, s, x, runA, target("rules.yaml", "abc123", daemonSettings()))
	if _, ok := mustPending(t, s, y, runA); ok {
		t.Fatal("the request of X is pending for Y")
	}
	if got := mustPendingAll(t, s, y); len(got) != 0 {
		t.Fatalf("PendingAll of Y = %+v, want none", got)
	}
}

// The requests of two runtime directories of a workspace are kept apart:
// the refusal of one does not replace the request of the other, and
// PendingAll returns both, sorted by runtime directory.
func TestRequestsOfTwoRuntimeDirsAreKeptApart(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	grace := map[string]string{"grace": "1m0s"}
	mustRequest(t, s, ws, runB, target("rules.yaml", "abc123", grace))
	mustRequest(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	a, ok := mustPending(t, s, ws, runA)
	if !ok || !maps.Equal(a.Settings, daemonSettings()) {
		t.Fatalf("Pending(A) = %+v, %v; want the request of A", a, ok)
	}
	b, ok := mustPending(t, s, ws, runB)
	if !ok || !maps.Equal(b.Settings, grace) {
		t.Fatalf("Pending(B) = %+v, %v; want the request of B", b, ok)
	}
	if got := mustPendingAll(t, s, ws); !reflect.DeepEqual(got, []allow.Entry{a, b}) {
		t.Fatalf("PendingAll = %+v, want [%+v %+v]", got, a, b)
	}
}

func TestAllowRemovesTheRequest(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustRequest(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	mustRequest(t, s, ws, runB, target("rules.yaml", "abc123", daemonSettings()))
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	if _, ok := mustPending(t, s, ws, runA); ok {
		t.Fatal("the request is still pending after Allow")
	}
	if _, ok := mustPending(t, s, ws, runB); !ok {
		t.Fatal("the allow of A removed the request of B")
	}
	if !mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
		t.Fatal("the workspace is not allowed")
	}
}

// AllowAll allows every request given, each for its runtime directory, and
// removes them.
func TestAllowAllAllowsEveryRequestAndRemovesThem(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	grace := map[string]string{"grace": "1m0s"}
	mustRequest(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	mustRequest(t, s, ws, runB, target("rules.yaml", "abc123", grace))
	reqs := mustPendingAll(t, s, ws)
	got, err := s.AllowAll(ws, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, reqs) {
		t.Errorf("AllowAll = %+v, want %+v", got, reqs)
	}
	if !mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) || !mustAllowed(t, s, ws, runB, "abc123", grace) {
		t.Error("a runtime directory is not allowed after AllowAll")
	}
	if left := mustPendingAll(t, s, ws); len(left) != 0 {
		t.Errorf("requests left after AllowAll: %+v", left)
	}
}

// AllowAll allows none when one request is not of the workspace or names
// no runtime directory, and leaves every request and no stray file.
func TestAllowAllWithARequestNotOfTheWorkspaceAllowsNothing(t *testing.T) {
	s := newStore(t)
	ws, other := t.TempDir(), t.TempDir()
	mustRequest(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	mustRequest(t, s, other, runB, target("rules.yaml", "abc123", daemonSettings()))
	own := mustPendingAll(t, s, ws)
	foreign := mustPendingAll(t, s, other)
	noDir := own[0]
	noDir.RuntimeDir = ""
	for name, reqs := range map[string][]allow.Entry{
		"another workspace":  {own[0], foreign[0]},
		"no runtime dir":     {own[0], noDir},
		"no workspace given": {{Place: allow.Place{RuntimeDir: runB}, Target: own[0].Target}},
	} {
		before, _ := storeFiles(t, s)
		if _, err := s.AllowAll(ws, reqs); err == nil {
			t.Errorf("%s: AllowAll returned no error", name)
		}
		if mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
			t.Errorf("%s: runtime directory A is allowed", name)
		}
		if after, _ := storeFiles(t, s); !reflect.DeepEqual(after, before) {
			t.Errorf("%s: store files %v, want %v", name, after, before)
		}
	}
	if got := mustPendingAll(t, s, ws); len(got) != 1 {
		t.Errorf("requests of the workspace = %+v, want its one", got)
	}
}

// A request recorded between PendingAll and AllowAll is not the one
// AllowAll answers: it stays pending for the next allow, while the
// request answered is allowed.
func TestAllowAllLeavesARequestRecordedMeanwhile(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustRequest(t, s, ws, runA, target("rules.yaml", "contentA", daemonSettings()))
	mustRequest(t, s, ws, runB, target("rules.yaml", "contentA", daemonSettings()))
	reqs := mustPendingAll(t, s, ws)
	mustRequest(t, s, ws, runA, target("rules.yaml", "contentB", daemonSettings()))
	if _, err := s.AllowAll(ws, reqs); err != nil {
		t.Fatal(err)
	}
	if !mustAllowed(t, s, ws, runA, "contentA", daemonSettings()) || !mustAllowed(t, s, ws, runB, "contentA", daemonSettings()) {
		t.Error("a request answered is not allowed")
	}
	if e, ok := mustPending(t, s, ws, runA); !ok || e.Digest != "contentB" {
		t.Errorf("pending request of A = %+v, %v; want the one recorded meanwhile", e, ok)
	}
	if _, ok := mustPending(t, s, ws, runB); ok {
		t.Error("the request of B, answered, is still pending")
	}
}

// When a request answered cannot be removed after the allows are
// recorded, AllowAll returns the allows recorded together with the error.
func TestAllowAllReturnsTheAllowsWhenARequestCannotBeRemoved(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustRequest(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	reqs := mustPendingAll(t, s, ws)
	file := requestFile(t, s)
	// A directory that is not empty can be neither read nor removed.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(file, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := s.AllowAll(ws, reqs)
	if err == nil {
		t.Fatal("AllowAll returned no error")
	}
	if !reflect.DeepEqual(got, reqs) {
		t.Errorf("AllowAll = %+v, want the allows recorded %+v", got, reqs)
	}
	if !mustAllowed(t, s, ws, runA, "abc123", daemonSettings()) {
		t.Error("the allow is not recorded")
	}
}

func TestDenyLeavesTheRequest(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	mustRequest(t, s, ws, runA, target("rules.yaml", "def456", daemonSettings()))
	if removed, err := s.Deny(ws); err != nil || len(removed) != 1 {
		t.Fatalf("Deny = %+v, %v, want one, nil", removed, err)
	}
	e, ok := mustPending(t, s, ws, runA)
	if !ok || e.Digest != "def456" {
		t.Fatalf("Pending = %+v, %v, want the request of def456", e, ok)
	}
}

// The runtime directories of the allows the TestAllowAddedRule tests keep
// besides runA and runB: one allowed other content of the rules file, one
// another rules file.
const (
	runStale     = "/run/hostrunner/stale"
	runElsewhere = "/run/hostrunner/elsewhere"
)

// rulesSettings returns daemonSettings with extra and a rules_file setting
// naming rulesFile, as the settings of a daemon serving it name it.
func rulesSettings(rulesFile string, extra map[string]string) map[string]string {
	s := daemonSettings()
	maps.Copy(s, extra)
	s["rules_file"] = rulesFile
	return s
}

// writtenOver is the Scope a daemon with daemonSettings checked the
// content "before" with, the content a rule was written over.
func writtenOver() allow.Scope {
	return allow.Scope{Digest: "before", Settings: daemonSettings()}
}

// AllowAddedRule moves to the content after the write every allow of the
// workspace that covered the rules file of the adding runtime directory's
// allow with the content before it, that runtime directory's and another's,
// each with its own settings, and returns them; it leaves an allow of other
// content, one of another rules file and one of another workspace.
func TestAllowAddedRuleMovesEveryAllowThatCoveredTheContentBefore(t *testing.T) {
	s := newStore(t)
	ws, otherWS := t.TempDir(), t.TempDir()
	own := rulesSettings("rules.yaml", nil)
	grace := rulesSettings("rules.yaml", map[string]string{"grace": "1m0s"})
	elsewhere := rulesSettings("other.yaml", nil)
	mustAllow(t, s, ws, runA, target("rules.yaml", "before", own))
	mustAllow(t, s, ws, runB, target("rules.yaml", "before", grace))
	mustAllow(t, s, ws, runStale, target("rules.yaml", "earlier", own))
	mustAllow(t, s, ws, runElsewhere, target("other.yaml", "before", elsewhere))
	mustAllow(t, s, otherWS, runB, target("rules.yaml", "before", own))
	got, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, allow.Scope{Digest: "before", Settings: own}, "after")
	if err != nil {
		t.Fatal(err)
	}
	rulesFile, err := filepath.Abs("rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []allow.Entry{
		{Place: allow.Place{Workspace: evalDir(t, ws), RuntimeDir: runA}, Target: target(rulesFile, "after", rulesSettings(rulesFile, nil))},
		{Place: allow.Place{Workspace: evalDir(t, ws), RuntimeDir: runB}, Target: target(rulesFile, "after", rulesSettings(rulesFile, map[string]string{"grace": "1m0s"}))},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllowAddedRule = %+v, want %+v", got, want)
	}
	if !mustAllowed(t, s, ws, runA, "after", own) || !mustAllowed(t, s, ws, runB, "after", grace) {
		t.Error("a runtime directory whose allow covered the content before is not allowed the content after with its own settings")
	}
	for _, c := range []struct {
		ws, rd, digest string
		settings       map[string]string
	}{
		{ws, runStale, "earlier", own},
		{ws, runElsewhere, "before", elsewhere},
		{otherWS, runB, "before", own},
	} {
		if !mustAllowed(t, s, c.ws, c.rd, c.digest, c.settings) || mustAllowed(t, s, c.ws, c.rd, "after", c.settings) {
			t.Errorf("the allow of %s of %s changed; want it to keep %s", c.rd, c.ws, c.digest)
		}
	}
}

// AllowAddedRule moves none, and fails, when the adding runtime directory
// has no allow covering the Scope its daemon checked the content before
// the write with: its allow is of other content or other settings, or it
// has none.
func TestAllowAddedRuleWithoutTheAllowOfItsOwnRuntimeDirMovesNone(t *testing.T) {
	earlier := target("rules.yaml", "earlier", daemonSettings())
	otherSettings := target("rules.yaml", "before", map[string]string{"grace": "1m0s"})
	for name, own := range map[string]*allow.Target{"other content": &earlier, "other settings": &otherSettings, "no allow": nil} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			ws := t.TempDir()
			if own != nil {
				mustAllow(t, s, ws, runA, *own)
			}
			mustAllow(t, s, ws, runB, target("rules.yaml", "before", daemonSettings()))
			if got, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, writtenOver(), "after"); err == nil {
				t.Fatalf("AllowAddedRule = %+v, want an error", got)
			}
			if mustAllowed(t, s, ws, runA, "after", daemonSettings()) || !mustAllowed(t, s, ws, runB, "before", daemonSettings()) {
				t.Error("an allow was moved")
			}
		})
	}
}

// AllowAddedRule skips a damaged allow file of another runtime directory:
// it moves the valid allows, returns them, leaves the damaged file as it
// is and reports it in a *allow.DamagedFilesError that only warns.
func TestAllowAddedRuleSkipsADamagedAllowFileOfAnotherRuntimeDir(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runB, target("rules.yaml", "before", daemonSettings()))
	files, _ := storeFiles(t, s)
	mustAllow(t, s, ws, runA, target("rules.yaml", "before", daemonSettings()))
	mustAllow(t, s, ws, runStale, target("rules.yaml", "before", daemonSettings()))
	if err := os.WriteFile(files[0], []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, writtenOver(), "after")
	damaged, ok := errors.AsType[*allow.DamagedFilesError](err)
	if !ok || len(damaged.Files) != 1 || damaged.Files[0].Path != files[0] {
		t.Fatalf("AllowAddedRule error = %v, want a *allow.DamagedFilesError of %s", err, files[0])
	}
	if !allow.OnlyWarns(err) {
		t.Errorf("AllowAddedRule error %v does not only warn", err)
	}
	if len(got) != 2 || got[0].RuntimeDir != runA || got[1].RuntimeDir != runStale {
		t.Errorf("AllowAddedRule = %+v, want the allows of %s and %s", got, runA, runStale)
	}
	if !mustAllowed(t, s, ws, runA, "after", daemonSettings()) || !mustAllowed(t, s, ws, runStale, "after", daemonSettings()) {
		t.Error("a valid allow was not moved")
	}
	if data, err := os.ReadFile(files[0]); err != nil || string(data) != "not json" {
		t.Errorf("the damaged file holds %q, %v; want it left as it is", data, err)
	}
}

// AllowAddedRule moves none, and fails naming the file, when the allow
// file of the adding runtime directory itself is damaged.
func TestAllowAddedRuleWithItsOwnDamagedAllowFileMovesNone(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "before", daemonSettings()))
	files, _ := storeFiles(t, s)
	mustAllow(t, s, ws, runB, target("rules.yaml", "before", daemonSettings()))
	if err := os.WriteFile(files[0], []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, writtenOver(), "after")
	if err == nil || allow.OnlyWarns(err) || !strings.Contains(err.Error(), files[0]) {
		t.Fatalf("AllowAddedRule = %+v, %v; want a failure naming %s", got, err, files[0])
	}
	if len(got) != 0 {
		t.Errorf("AllowAddedRule = %+v, want none recorded", got)
	}
	if !mustAllowed(t, s, ws, runB, "before", daemonSettings()) {
		t.Error("the allow of another runtime directory was moved")
	}
}

// OnlyWarns is true for nil, the warnings of the store and errors joined
// of only warnings, and false for any other error, also one a
// *allow.RequestsNotRemovedError wraps or one joined with a warning.
func TestOnlyWarns(t *testing.T) {
	other := errors.New("disk on fire")
	notRemoved := &allow.RequestsNotRemovedError{Err: other}
	damaged := &allow.DamagedFilesError{Files: []allow.DamagedFile{{Path: "/s/x.json", Err: other}}}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"requests not removed", notRemoved, true},
		{"damaged files", damaged, true},
		{"both joined", errors.Join(notRemoved, damaged), true},
		{"other", other, false},
		{"other joined with a warning", errors.Join(damaged, other), false},
		{"wrapped warning", fmt.Errorf("x: %w", damaged), false},
	} {
		if got := allow.OnlyWarns(tt.err); got != tt.want {
			t.Errorf("%s: OnlyWarns(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// DamagedFilesError names every file with why it is damaged.
func TestDamagedFilesErrorNamesEveryFile(t *testing.T) {
	err := &allow.DamagedFilesError{Files: []allow.DamagedFile{
		{Path: "/s/a.json", Err: errors.New("not json")},
		{Path: "/s/b.json", Err: errors.New("is a directory")},
	}}
	if got, want := err.Error(), "allow store: damaged allow files: /s/a.json: not json; /s/b.json: is a directory"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// AllowAddedRule removes the pending requests its allows answer, that of
// another runtime directory included, and leaves a request of other
// content.
func TestAllowAddedRuleRemovesTheRequestsItAnswers(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "before", daemonSettings()))
	mustAllow(t, s, ws, runB, target("rules.yaml", "before", daemonSettings()))
	mustRequest(t, s, ws, runA, target("rules.yaml", "hand edit", daemonSettings()))
	mustRequest(t, s, ws, runB, target("rules.yaml", "after", daemonSettings()))
	if _, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, writtenOver(), "after"); err != nil {
		t.Fatal(err)
	}
	if e, ok := mustPending(t, s, ws, runA); !ok || e.Digest != "hand edit" {
		t.Errorf("pending request of A = %+v, %v; want the request of other content", e, ok)
	}
	if e, ok := mustPending(t, s, ws, runB); ok {
		t.Errorf("pending request of B = %+v; want the request answered removed", e)
	}
}

// When a request answered cannot be removed once the allows are moved,
// AllowAddedRule returns the allows recorded with a
// *allow.RequestsNotRemovedError, so that the caller knows they are.
func TestAllowAddedRuleReportsARequestNotRemovedApart(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustAllow(t, s, ws, runA, target("rules.yaml", "before", daemonSettings()))
	mustRequest(t, s, ws, runA, target("rules.yaml", "after", daemonSettings()))
	file := requestFile(t, s)
	// A directory that is not empty can be neither read nor removed.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(file, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, writtenOver(), "after")
	if _, ok := errors.AsType[*allow.RequestsNotRemovedError](err); !ok {
		t.Fatalf("AllowAddedRule error = %v, want an *allow.RequestsNotRemovedError", err)
	}
	if len(got) != 1 || !mustAllowed(t, s, ws, runA, "after", daemonSettings()) {
		t.Errorf("AllowAddedRule = %+v; want the allow moved and returned", got)
	}
}

func TestCorruptRequestFileIsAnErrorNamingTheFile(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	mustRequest(t, s, ws, runA, target("rules.yaml", "abc123", daemonSettings()))
	file := requestFile(t, s)
	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingAll(ws)
	if err == nil || !strings.Contains(err.Error(), file) {
		t.Errorf("PendingAll error = %v, want one naming %q", err, file)
	}
	if len(got) != 0 {
		t.Errorf("PendingAll reports %+v for a corrupt file", got)
	}
}

func TestStoreDirsAre0700AndRequestFileIs0600(t *testing.T) {
	s := newStore(t)
	mustRequest(t, s, t.TempDir(), runA, target("rules.yaml", "abc123", daemonSettings()))
	_, dirs := storeFiles(t, s)
	for _, d := range dirs {
		checkMode(t, d, 0o700)
	}
	file := requestFile(t, s)
	checkMode(t, file, 0o600)
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
		if _, err := s.AllowAll(ws, []allow.Entry{{Place: allow.Place{Workspace: ws, RuntimeDir: runA}}}); err == nil {
			t.Errorf("Store%+v.AllowAll: got no error", s)
		}
		if err := s.Request(allow.Place{Workspace: ws, RuntimeDir: runA}, target("rules.yaml", "abc123", daemonSettings())); err == nil {
			t.Errorf("Store%+v.Request: got no error", s)
		}
		if _, err := s.Deny(ws); err == nil {
			t.Errorf("Store%+v.Deny: got no error", s)
		}
		if _, err := s.Allowed(allow.Place{Workspace: ws, RuntimeDir: runA}, allow.Scope{Digest: "abc123", Settings: daemonSettings()}); err == nil {
			t.Errorf("Store%+v.Allowed: got no error", s)
		}
		if _, err := s.PendingAll(ws); err == nil {
			t.Errorf("Store%+v.PendingAll: got no error", s)
		}
		if _, err := s.AllowAddedRule(allow.Place{Workspace: ws, RuntimeDir: runA}, allow.Scope{Digest: "abc123", Settings: daemonSettings()}, "def456"); err == nil {
			t.Errorf("Store%+v.AllowAddedRule: got no error", s)
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
	mustRequest(t, s, link, runA, target(linkRules, "abc123", map[string]string{"rules_file": linkRules, "grace": "15s"}))
	want := allow.Entry{
		Place:  allow.Place{Workspace: real, RuntimeDir: runA},
		Target: target(realRules, "abc123", map[string]string{"rules_file": realRules, "grace": "15s"}),
	}
	if e, ok := mustPending(t, s, link, runA); !ok || !reflect.DeepEqual(e, want) {
		t.Fatalf("Pending = %+v, %v, want %+v", e, ok, want)
	}
	e := mustAllow(t, s, link, runA, target(linkRules, "abc123", map[string]string{"rules_file": linkRules, "grace": "15s"}))
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("Allow = %+v, want %+v", e, want)
	}
	if !mustAllowed(t, s, real, runA, "abc123", map[string]string{"rules_file": realRules, "grace": "15s"}) {
		t.Error("the rules file through its real path is not allowed")
	}
	if !mustAllowed(t, s, link, runA, "abc123", map[string]string{"rules_file": linkRules, "grace": "15s"}) {
		t.Error("the rules file through the link is not allowed")
	}
	if mustAllowed(t, s, real, runA, "abc123", map[string]string{"rules_file": filepath.Join(real, "other.yaml"), "grace": "15s"}) {
		t.Error("another rules file is allowed")
	}
}

// A rules file that does not exist is held by its cleaned absolute path.
func TestAllowHoldsAMissingRulesFileByItsCleanedPath(t *testing.T) {
	s := newStore(t)
	ws := t.TempDir()
	missing := evalDir(t, ws) + "/gone/../missing.yaml"
	e := mustAllow(t, s, ws, runA, target(missing, "missing", map[string]string{"rules_file": missing}))
	clean := filepath.Join(evalDir(t, ws), "missing.yaml")
	want := allow.Entry{Place: allow.Place{Workspace: evalDir(t, ws), RuntimeDir: runA}, Target: target(clean, "missing", map[string]string{"rules_file": clean})}
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
