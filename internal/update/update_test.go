package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// release is what the fake GitHub serves: the latest tag and the assets
// of that release, by file name.
type release struct {
	tag    string
	assets map[string][]byte
	status int            // answers the latest-release request when non-zero
	raw    string         // the latest-release body instead of JSON with tag, when set
	codes  map[string]int // the status an asset is served with, when not 200
}

// requests records the paths a fake GitHub was asked for, to prove that a
// refusal downloads nothing.
type requests struct {
	mu    sync.Mutex
	paths []string
}

// record notes r's path; it is safe for concurrent handlers.
func (q *requests) record(r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paths = append(q.paths, r.URL.Path)
}

// downloads returns the recorded paths of release assets.
func (q *requests) downloads() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var got []string
	for _, p := range q.paths {
		if strings.HasPrefix(p, "/download/") {
			got = append(got, p)
		}
	}
	return got
}

// all returns every recorded path.
func (q *requests) all() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.paths)
}

// serve starts a fake GitHub for rel and returns a Config pointing at it,
// with a layout in a temporary root whose current version, current,
// holds the old binaries, and the record of the requests it answers.
func serve(t *testing.T, rel release, current string) (Config, *requests) {
	t.Helper()
	q := &requests{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		q.record(r)
		if rel.status != 0 {
			w.WriteHeader(rel.status)
			return
		}
		if rel.raw != "" {
			fmt.Fprint(w, rel.raw)
			return
		}
		fmt.Fprintf(w, `{"tag_name": %q}`, rel.tag)
	})
	mux.HandleFunc("GET /download/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		q.record(r)
		body, ok := rel.assets[r.PathValue("name")]
		if r.PathValue("tag") != rel.tag || !ok {
			http.NotFound(w, r)
			return
		}
		if code := rel.codes[r.PathValue("name")]; code != 0 {
			w.WriteHeader(code)
		}
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	root := t.TempDir()
	dir := filepath.Join(root, current)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range binaries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old "+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(current, filepath.Join(root, currentLink)); err != nil {
		t.Fatal(err)
	}
	return Config{
		APIURL:      srv.URL + "/api",
		DownloadURL: srv.URL + "/download",
		Current:     current,
		Dir:         dir,
		GOOS:        "linux",
		GOARCH:      "amd64",
		Client:      srv.Client(),
	}, q
}

// tarball is a gzipped tar of files, as GoReleaser packs a release.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newRelease is a release of tag for linux/amd64 holding archive, with a
// checksums.txt that lists sum for it (the archive's real sum when empty).
func newRelease(t *testing.T, tag string, archive []byte, sum string) release {
	t.Helper()
	name := "hostrunner_" + strings.TrimPrefix(tag, "v") + "_linux_amd64.tar.gz"
	if sum == "" {
		s := sha256.Sum256(archive)
		sum = hex.EncodeToString(s[:])
	}
	checksums := fmt.Sprintf("%s  hostrunner_0.0.1_linux_arm64.tar.gz\n%s  %s\n", strings.Repeat("0", 64), sum, name)
	return release{tag: tag, assets: map[string][]byte{name: archive, "checksums.txt": []byte(checksums)}}
}

// fullArchive holds both binaries, marked with tag, and the extra files
// a release archive carries.
func fullArchive(t *testing.T, tag string) []byte {
	t.Helper()
	return tarball(t, map[string]string{
		"hostrunner": "new hostrunner " + tag,
		"hostrun":    "new hostrun " + tag,
		"LICENSE":    "license",
		"README.md":  "readme",
	})
}

// assertFile fails unless dir/name holds want.
func assertFile(t *testing.T, dir, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s: got %q, want %q", name, got, want)
	}
}

// assertEntries fails unless dir holds exactly names, e.g. no leftover
// temporary directory or symlink.
func assertEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := slices.Sorted(slices.Values(names))
	if !slices.Equal(got, want) {
		t.Fatalf("%s holds %v, want %v", dir, got, want)
	}
}

// assertCurrentLink fails unless root's current link points to version.
func assertCurrentLink(t *testing.T, root, version string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(root, currentLink))
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("current -> %q, want %q", got, version)
	}
}

// assertUntouched fails unless cfg's layout is exactly as serve made it.
func assertUntouched(t *testing.T, cfg Config) {
	t.Helper()
	root := filepath.Dir(cfg.Dir)
	assertEntries(t, root, currentLink, cfg.Current)
	assertCurrentLink(t, root, cfg.Current)
	assertEntries(t, cfg.Dir, binaries...)
	assertFile(t, cfg.Dir, "hostrunner", "old hostrunner")
	assertFile(t, cfg.Dir, "hostrun", "old hostrun")
}

// layoutMessage is Run's refusal of cfg as an install outside the layout
// whose current link points to target ("" for no link).
func layoutMessage(cfg Config, target string) string {
	link := filepath.Join(filepath.Dir(cfg.Dir), currentLink)
	found := "readlink " + link + ": no such file or directory"
	if target != "" {
		found = link + " -> " + target
	}
	return "hostrunner is not installed in the versioned layout (" + found + "; running " + cfg.Current + " from " + cfg.Dir + "); reinstall it as the README describes"
}

// holdLock takes the lock Run takes on root, exclusively, until the test
// ends.
func holdLock(t *testing.T, root string) {
	t.Helper()
	flockRoot(t, root, unix.LOCK_EX)
}

// holdSharedLock takes a shared flock on root until the test ends, which
// must keep Run out as well as an exclusive one.
func holdSharedLock(t *testing.T, root string) {
	t.Helper()
	flockRoot(t, root, unix.LOCK_SH)
}

// flockRoot takes a flock of mode how on root until the test ends.
func flockRoot(t *testing.T, root string, how int) {
	t.Helper()
	f, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

// roundTripFunc is an http.RoundTripper, for tests that act around a
// request of Run's.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closeHook runs onClose when its body is closed.
type closeHook struct {
	io.ReadCloser
	onClose func()
}

// Close closes the body, then runs onClose.
func (c closeHook) Close() error {
	err := c.ReadCloser.Close()
	c.onClose()
	return err
}

// stallCtx is a context that reaches its deadline only when expire is
// called, so that a test stalls one request and no other one can run out
// of time on a slow machine.
type stallCtx struct {
	context.Context // Background: no values
	done            chan struct{}
	once            sync.Once
}

// newStallCtx returns a stallCtx that has not expired.
func newStallCtx() *stallCtx {
	return &stallCtx{Context: context.Background(), done: make(chan struct{})}
}

// Done is closed by expire.
func (c *stallCtx) Done() <-chan struct{} { return c.done }

// Err is context.DeadlineExceeded once expire was called.
func (c *stallCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// expire makes c reach its deadline; later calls do nothing.
func (c *stallCtx) expire() { c.once.Do(func() { close(c.done) }) }

// assertSwitched fails unless cfg's layout holds only latest's binaries,
// with current pointing to them, and the other entries in extra.
func assertSwitched(t *testing.T, cfg Config, latest string, extra ...string) {
	t.Helper()
	root := filepath.Dir(cfg.Dir)
	assertEntries(t, root, append([]string{currentLink, latest}, extra...)...)
	assertCurrentLink(t, root, latest)
	dir := filepath.Join(root, latest)
	assertEntries(t, dir, binaries...)
	for _, name := range binaries {
		assertFile(t, dir, name, "new "+name+" "+latest)
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: mode %v, want 0755", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("%s: mode %v, want 0755", dir, info.Mode().Perm())
	}
}

// TestRunSwitchesToANewerRelease checks the update itself: a new version
// directory, the current link switched to it, every other version
// directory removed (also one an earlier run could not remove).
func TestRunSwitchesToANewerRelease(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	for _, stale := range []string{"v0.3.0", "dev"} {
		if err := os.Mkdir(filepath.Join(filepath.Dir(cfg.Dir), stale), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: Updated}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertSwitched(t, cfg, "v0.5.0")
}

// TestRunKeepsEntriesThatAreNotVersionDirectories checks that the
// removal of the other version directories spares what is not one: a
// directory named v… that is not a version, a file and a symlink named
// like versions, and their contents.
func TestRunKeepsEntriesThatAreNotVersionDirectories(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	root := filepath.Dir(cfg.Dir)
	for _, dir := range []string{"vim", "videos", "v0.3.0"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "file"), []byte(dir), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "v1.0.0"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("vim", filepath.Join(root, "v0.2.0")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	assertSwitched(t, cfg, "v0.5.0", "vim", "videos", "v1.0.0", "v0.2.0")
	assertFile(t, filepath.Join(root, "vim"), "file", "vim")
	assertFile(t, filepath.Join(root, "videos"), "file", "videos")
	assertFile(t, root, "v1.0.0", "file")
}

// TestRunInstallsAPreReleaseTag checks that a canonical tag with a
// pre-release is installed under its own name.
func TestRunInstallsAPreReleaseTag(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0-rc.1", fullArchive(t, "v0.5.0-rc.1"), ""), "v0.4.0")
	got, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Result{Current: "v0.4.0", Latest: "v0.5.0-rc.1", Outcome: Updated}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertSwitched(t, cfg, "v0.5.0-rc.1")
}

// TestRunAcceptsAnUpperCaseChecksum checks that checksums.txt may list
// the SHA-256 in upper-case hex.
func TestRunAcceptsAnUpperCaseChecksum(t *testing.T) {
	archive := fullArchive(t, "v0.5.0")
	sum := sha256.Sum256(archive)
	cfg, _ := serve(t, newRelease(t, "v0.5.0", archive, strings.ToUpper(hex.EncodeToString(sum[:]))), "v0.4.0")
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	assertSwitched(t, cfg, "v0.5.0")
}

// TestRunSetsTheModeWhateverTheUmask checks that the binaries and their
// directory are 0755 under a umask that would narrow them.
func TestRunSetsTheModeWhateverTheUmask(t *testing.T) {
	old := unix.Umask(0o077)
	t.Cleanup(func() { unix.Umask(old) })
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	assertSwitched(t, cfg, "v0.5.0")
}

// TestRunLeavesTheLayoutWhenNotNewer checks that a release not newer than
// the running build changes nothing: Run never downgrades.
func TestRunLeavesTheLayoutWhenNotNewer(t *testing.T) {
	for _, tt := range []struct{ name, current, latest string }{
		{"same release", "v0.5.0", "v0.5.0"},
		{"older release", "v0.6.0", "v0.5.0"},
		{"pseudo-version after the release", "v0.5.1-0.20261001120000-abcdef123456+dirty", "v0.5.0"},
		{"same release built dirty", "v0.5.0+dirty", "v0.5.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, q := serve(t, newRelease(t, tt.latest, fullArchive(t, tt.latest), ""), tt.current)
			got, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			want := Result{Current: tt.current, Latest: tt.latest, Outcome: UpToDate}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			assertUntouched(t, cfg)
			if got := q.downloads(); len(got) != 0 {
				t.Fatalf("downloaded %v", got)
			}
		})
	}
}

// TestRunUpdatesAPseudoVersionToItsRelease checks that a build between
// tags, with or without uncommitted changes, sorts below the release it
// precedes.
func TestRunUpdatesAPseudoVersionToItsRelease(t *testing.T) {
	for _, current := range []string{
		"v0.5.0-0.20261001120000-abcdef123456",
		"v0.5.0-0.20261001120000-abcdef123456+dirty",
	} {
		t.Run(current, func(t *testing.T) {
			cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), current)
			got, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != Updated {
				t.Fatalf("got %+v, want an update", got)
			}
			assertSwitched(t, cfg, "v0.5.0")
		})
	}
}

// TestRunCheckOnlyReportsAndWritesNothing checks that --check reports a
// newer release without downloading or writing anything.
func TestRunCheckOnlyReportsAndWritesNothing(t *testing.T) {
	cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	cfg.CheckOnly = true
	got, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: Available}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertUntouched(t, cfg)
	if got := q.downloads(); len(got) != 0 {
		t.Fatalf("downloaded %v", got)
	}
}

// TestRunRefusesADevBuild checks that a build without a version is
// refused before any request.
func TestRunRefusesADevBuild(t *testing.T) {
	cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "dev")
	_, err := Run(context.Background(), cfg)
	if !errors.Is(err, ErrDevBuild) {
		t.Fatalf("got %v, want ErrDevBuild", err)
	}
	if want := "cannot compare a dev build with releases; install a release"; err.Error() != want {
		t.Fatalf("got %q, want %q", err, want)
	}
	assertUntouched(t, cfg)
	if got := q.all(); len(got) != 0 {
		t.Fatalf("requested %v", got)
	}
}

// TestRunRefusesAndChangesNothing checks the refusals after the version
// lookup, each with its exact message: each leaves the layout as it was.
func TestRunRefusesAndChangesNothing(t *testing.T) {
	archive := fullArchive(t, "v0.5.0")
	const name = "hostrunner_0.5.0_linux_amd64.tar.gz"
	// asset is the URL of a release asset of v0.5.0 on cfg's server.
	asset := func(cfg Config, file string) string { return cfg.DownloadURL + "/v0.5.0/" + file }
	// linked is an archive holding hostrunner and, as a symlink, hostrun.
	linked := func(t *testing.T) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "hostrunner", Mode: 0o755, Size: 3, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("new")); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "hostrun", Linkname: "hostrunner", Typeflag: tar.TypeSymlink}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	for _, tt := range []struct {
		name string
		rel  func(t *testing.T) release
		want func(cfg Config) string
	}{
		{"checksum mismatch", func(t *testing.T) release {
			return newRelease(t, "v0.5.0", archive, strings.Repeat("a", 64))
		}, func(Config) string { return name + ": checksum mismatch" }},
		{"archive not in checksums.txt", func(t *testing.T) release {
			rel := newRelease(t, "v0.5.0", archive, "")
			rel.assets["checksums.txt"] = []byte("")
			return rel
		}, func(Config) string { return name + " is not listed in checksums.txt" }},
		{"archive on a checksums.txt line with more than two fields", func(t *testing.T) release {
			rel := newRelease(t, "v0.5.0", archive, "")
			sum := sha256.Sum256(archive)
			rel.assets["checksums.txt"] = []byte(hex.EncodeToString(sum[:]) + "  " + name + "  extra\n")
			return rel
		}, func(Config) string { return name + " is not listed in checksums.txt" }},
		{"archive holding a binary as a symlink", func(t *testing.T) release {
			return newRelease(t, "v0.5.0", linked(t), "")
		}, func(Config) string { return "release archive has no hostrun" }},
		{"listed archive missing on the server", func(t *testing.T) release {
			rel := newRelease(t, "v0.5.0", archive, "")
			delete(rel.assets, name)
			return rel
		}, func(cfg Config) string { return "GET " + asset(cfg, name) + ": 404 Not Found" }},
		{"checksums.txt missing", func(t *testing.T) release {
			rel := newRelease(t, "v0.5.0", archive, "")
			delete(rel.assets, "checksums.txt")
			return rel
		}, func(cfg Config) string { return "GET " + asset(cfg, "checksums.txt") + ": 404 Not Found" }},
		{"archive served with a 2xx status other than 200", func(t *testing.T) release {
			rel := newRelease(t, "v0.5.0", archive, "")
			rel.codes = map[string]int{name: http.StatusNonAuthoritativeInfo}
			return rel
		}, func(cfg Config) string {
			return "GET " + asset(cfg, name) + ": 203 Non-Authoritative Information"
		}},
		{"latest release unavailable", func(t *testing.T) release {
			return release{status: http.StatusInternalServerError}
		}, func(cfg Config) string {
			return "GET " + cfg.APIURL + "/releases/latest: 500 Internal Server Error"
		}},
		{"latest release not JSON", func(t *testing.T) release {
			return release{raw: "{"}
		}, func(Config) string { return "latest release: unexpected end of JSON input" }},
		{"latest tag not a version", func(t *testing.T) release {
			return release{tag: "nightly"}
		}, func(Config) string { return `latest release "nightly" is not a version` }},
		{"latest tag a shorthand version", func(t *testing.T) release {
			return release{tag: "v1.2"}
		}, func(Config) string { return `latest release "v1.2" is not a version` }},
		{"latest tag with build metadata", func(t *testing.T) release {
			return release{tag: "v0.5.0+meta"}
		}, func(Config) string { return `latest release "v0.5.0+meta" is not a version` }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := serve(t, tt.rel(t), "v0.4.0")
			_, err := Run(context.Background(), cfg)
			if want := tt.want(cfg); err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
			assertUntouched(t, cfg)
		})
	}
}

// TestRunRefusesAnUnsupportedPlatformFirst checks that a platform without
// release builds is refused, with --check too, before the layout check
// and the lock, so it is the reason given whatever else is wrong, and
// that nothing is downloaded.
func TestRunRefusesAnUnsupportedPlatformFirst(t *testing.T) {
	for _, spoil := range []struct {
		name string
		do   func(t *testing.T, root string)
	}{
		{"layout intact", func(*testing.T, string) {}},
		{"flat install", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, currentLink)); err != nil {
				t.Fatal(err)
			}
		}},
		{"held lock", holdLock},
	} {
		for _, platform := range [][2]string{{"linux", "386"}, {"darwin", "amd64"}} {
			for _, checkOnly := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s %s/%s check=%v", spoil.name, platform[0], platform[1], checkOnly), func(t *testing.T) {
					cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
					spoil.do(t, filepath.Dir(cfg.Dir))
					cfg.GOOS, cfg.GOARCH, cfg.CheckOnly = platform[0], platform[1], checkOnly
					want := "no release builds for " + platform[0] + "/" + platform[1]
					if _, err := Run(context.Background(), cfg); err == nil || err.Error() != want {
						t.Fatalf("got %v, want %q", err, want)
					}
					if got := q.downloads(); len(got) != 0 {
						t.Fatalf("downloaded %v", got)
					}
					if spoil.name == "layout intact" {
						assertUntouched(t, cfg)
					}
				})
			}
		}
	}
}

// TestRunRefusesAnInstallOutsideTheLayout checks that an install one
// rename cannot update is refused, by --check too, before anything is
// downloaded.
func TestRunRefusesAnInstallOutsideTheLayout(t *testing.T) {
	for _, tt := range []struct {
		name      string
		current   string // where root's current link points; "" for no link
		checkOnly bool
	}{
		{"no current link", "", false},
		{"current points to another version", "v0.3.0", false},
		{"check, no current link", "", true},
		{"check, current points to another version", "v0.3.0", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
			cfg.CheckOnly = tt.checkOnly
			root := filepath.Dir(cfg.Dir)
			link := filepath.Join(root, currentLink)
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			want := []string{cfg.Current}
			if tt.current != "" {
				if err := os.Symlink(tt.current, link); err != nil {
					t.Fatal(err)
				}
				want = append(want, currentLink)
			}
			_, err := Run(context.Background(), cfg)
			if want := layoutMessage(cfg, tt.current); err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
			assertEntries(t, root, want...)
			if tt.current != "" {
				assertCurrentLink(t, root, tt.current)
			}
			assertEntries(t, cfg.Dir, binaries...)
			if got := q.downloads(); len(got) != 0 {
				t.Fatalf("downloaded %v", got)
			}
		})
	}
}

// TestRunReportsAFlatInstallUpToDate checks that the layout matters only
// when there is something to update.
func TestRunReportsAFlatInstallUpToDate(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.5.0")
	root := filepath.Dir(cfg.Dir)
	if err := os.Remove(filepath.Join(root, currentLink)); err != nil {
		t.Fatal(err)
	}
	got, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Result{Current: "v0.5.0", Latest: "v0.5.0", Outcome: UpToDate}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertEntries(t, root, "v0.5.0")
}

// TestRunRefusesAVersionDirectoryNotNamedAfterTheRunningVersion checks
// the case where removing a leftover <root>/<latest> would remove the
// running version directory, the current link's target: v0.4.0 runs
// from v0.5.0/. --check refuses it too.
func TestRunRefusesAVersionDirectoryNotNamedAfterTheRunningVersion(t *testing.T) {
	for _, checkOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("check=%v", checkOnly), func(t *testing.T) {
			cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.5.0")
			cfg.Current, cfg.CheckOnly = "v0.4.0", checkOnly
			root := filepath.Dir(cfg.Dir)
			_, err := Run(context.Background(), cfg)
			if want := layoutMessage(cfg, "v0.5.0"); err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
			assertEntries(t, root, currentLink, "v0.5.0")
			assertCurrentLink(t, root, "v0.5.0")
			assertFile(t, cfg.Dir, "hostrunner", "old hostrunner")
			assertFile(t, cfg.Dir, "hostrun", "old hostrun")
			if got := q.downloads(); len(got) != 0 {
				t.Fatalf("downloaded %v", got)
			}
		})
	}
}

// TestRunRefusesWhileAnotherUpdateHoldsTheLock checks that two updates
// (or an update and an install) of one layout never run at once, whether
// the other holds the lock exclusively or shared.
func TestRunRefusesWhileAnotherUpdateHoldsTheLock(t *testing.T) {
	for _, hold := range []struct {
		name string
		do   func(t *testing.T, root string)
	}{
		{"exclusive", holdLock},
		{"shared", holdSharedLock},
	} {
		t.Run(hold.name, func(t *testing.T) {
			cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
			root := filepath.Dir(cfg.Dir)
			hold.do(t, root)
			_, err := Run(context.Background(), cfg)
			if want := "another update or install is running in " + root; err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
			assertUntouched(t, cfg)
			if got := q.downloads(); len(got) != 0 {
				t.Fatalf("downloaded %v", got)
			}
		})
	}
}

// TestRunRefusesAnArchiveMissingABinary checks that a release without
// either binary is refused with a message naming it.
func TestRunRefusesAnArchiveMissingABinary(t *testing.T) {
	for _, missing := range binaries {
		t.Run(missing, func(t *testing.T) {
			files := map[string]string{"hostrunner": "new", "hostrun": "new"}
			delete(files, missing)
			cfg, _ := serve(t, newRelease(t, "v0.5.0", tarball(t, files), ""), "v0.4.0")
			_, err := Run(context.Background(), cfg)
			if want := "release archive has no " + missing; err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
			assertUntouched(t, cfg)
		})
	}
}

// TestCheckCurrentLinkRefusesASwitchedLink checks the check under the lock:
// another update or install switched the current link, or removed it,
// after Run's first look.
func TestCheckCurrentLinkRefusesASwitchedLink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, currentLink)
	if err := checkCurrentLink(root, "v0.4.0"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no link: got %v, want a not-exist error", err)
	}
	if err := os.Symlink("v0.5.0", link); err != nil {
		t.Fatal(err)
	}
	want := "another update or install switched " + link + " to v0.5.0"
	if err := checkCurrentLink(root, "v0.4.0"); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if err := checkCurrentLink(root, "v0.5.0"); err != nil {
		t.Fatalf("unchanged link: got %v", err)
	}
}

// TestRunRemovesALeftoverVersionDirectoryOnAnError checks that a
// <root>/<latest> an interrupted update left is removed by an update
// that fails before the switch, like everything it creates.
func TestRunRemovesALeftoverVersionDirectoryOnAnError(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), strings.Repeat("a", 64)), "v0.4.0")
	if err := os.Mkdir(filepath.Join(filepath.Dir(cfg.Dir), "v0.5.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg); err == nil || !strings.HasSuffix(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v, want a checksum mismatch", err)
	}
	assertUntouched(t, cfg)
}

// TestRunRemovesALeftoverLinkToTheRunningVersion checks that a leftover
// temporary symlink pointing to the running version directory is removed
// without touching the directory, even when the update then fails.
func TestRunRemovesALeftoverLinkToTheRunningVersion(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), strings.Repeat("a", 64)), "v0.4.0")
	if err := os.Symlink("v0.4.0", filepath.Join(filepath.Dir(cfg.Dir), ".tmp.current-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg); err == nil || !strings.HasSuffix(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v, want a checksum mismatch", err)
	}
	assertUntouched(t, cfg)
}

// TestRunRemovesLeftoverTemporaryEntries checks that the temporary
// entries (.tmp.*) a killed update or install leaves behind, a non-empty
// directory and a symlink, are cleaned up by the next update without
// following the symlink, and that entries that are neither temporary nor
// version directories are kept.
func TestRunRemovesLeftoverTemporaryEntries(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	root := filepath.Dir(cfg.Dir)
	tmp := filepath.Join(root, ".tmp.v0.5.0-123")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "hostrunner"), []byte("partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A leftover symlink is removed, not followed: the directory it
	// points to keeps its contents.
	target := filepath.Join(root, "kept")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "file"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("kept", filepath.Join(root, ".tmp.current-456")); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{".keep", ".x.tmp-keep"} {
		if err := os.WriteFile(filepath.Join(root, keep), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	assertSwitched(t, cfg, "v0.5.0", ".keep", ".x.tmp-keep", "kept")
	assertFile(t, target, "file", "kept")
}

// TestRunReplacesALeftoverVersionDirectory checks that the version
// directory an update interrupted before the switch leaves behind is
// replaced by the next update.
func TestRunReplacesALeftoverVersionDirectory(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	leftover := filepath.Join(filepath.Dir(cfg.Dir), "v0.5.0")
	if err := os.Mkdir(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "hostrunner"), []byte("truncated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	assertSwitched(t, cfg, "v0.5.0")
}

// TestRunRefusesALeftoverItCannotRemove checks that a leftover of a
// killed or interrupted run that cannot be removed refuses the update
// before any download, naming it once.
func TestRunRefusesALeftoverItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from read-only directories")
	}
	for _, leftover := range []string{".tmp.v0.5.0-123", "v0.5.0"} {
		t.Run(leftover, func(t *testing.T) {
			cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
			root := filepath.Dir(cfg.Dir)
			sub := filepath.Join(root, leftover, "sub")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sub, "file"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(sub, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(sub, 0o755) })
			_, err := Run(context.Background(), cfg)
			prefix := "could not remove " + filepath.Join(root, leftover) + ": "
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("got %v, want %q and a permission error", err, prefix+"…")
			}
			if n := strings.Count(err.Error(), "could not remove"); n != 1 {
				t.Fatalf("got %q, want the failure reported once", err)
			}
			assertEntries(t, root, currentLink, cfg.Current, leftover)
			assertCurrentLink(t, root, cfg.Current)
			if got := q.downloads(); len(got) != 0 {
				t.Fatalf("downloaded %v", got)
			}
		})
	}
}

// TestRunReportsATemporaryDirectoryItCannotRemove checks that a failure
// to remove the temporary directory after an error is reported after
// that error. The root becomes unwritable while the archive is
// requested, which then fails.
func TestRunReportsATemporaryDirectoryItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes entries of read-only directories")
	}
	rel := newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), "")
	const name = "hostrunner_0.5.0_linux_amd64.tar.gz"
	delete(rel.assets, name)
	cfg, _ := serve(t, rel, "v0.4.0")
	root := filepath.Dir(cfg.Dir)
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	base := cfg.Client.Transport
	cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if filepath.Base(r.URL.Path) == name {
			if err := os.Chmod(root, 0o555); err != nil {
				return nil, err
			}
		}
		return base.RoundTrip(r)
	})}
	_, err := Run(context.Background(), cfg)
	prefix := "GET " + cfg.DownloadURL + "/v0.5.0/" + name + ": 404 Not Found\ncould not remove " + filepath.Join(root, tmpPrefix+"v0.5.0-")
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("got %v, want %q and a permission error", err, prefix+"…")
	}
	assertCurrentLink(t, root, cfg.Current)
}

// TestRunDoesNotSwitchAfterACancellation checks that a ctx cancelled
// after the last download, while the release is installed, stops Run
// before the switch, with nothing it created left.
func TestRunDoesNotSwitchAfterACancellation(t *testing.T) {
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := cfg.Client.Transport
	cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, ".tar.gz") {
			// Closed once the body is read: the cancellation comes after
			// the last read, when nothing but the check before the switch
			// looks at ctx.
			resp.Body = closeHook{resp.Body, cancel}
		}
		return resp, err
	})}
	if _, err := Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	assertUntouched(t, cfg)
}

// TestRunFailsAtTheDeadlineOfAStalledServer checks that a stalled
// connection, on the version lookup or on the archive download, ends with
// ctx and leaves the layout as it was. The deadline expires only once the
// stalled request is made (stallCtx), so the requests before it cannot
// run out of time.
func TestRunFailsAtTheDeadlineOfAStalledServer(t *testing.T) {
	for _, stallArchive := range []bool{false, true} {
		name := "version lookup"
		if stallArchive {
			name = "archive download"
		}
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), "")
			cfg, _ := serve(t, rel, "v0.4.0")
			ctx := newStallCtx()
			// stalled answers what it is asked for to stall with a partial
			// body, lets ctx expire and blocks until the client gives up;
			// anything else it serves from rel.
			var stalledOn atomic.Bool
			stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				name := filepath.Base(r.URL.Path)
				if stallArchive && name == "checksums.txt" {
					w.Write(rel.assets[name])
					return
				}
				stalledOn.Store(true)
				w.Write([]byte("partial"))
				w.(http.Flusher).Flush()
				ctx.expire()
				<-r.Context().Done()
			}))
			t.Cleanup(stalled.Close)
			if stallArchive {
				cfg.DownloadURL = stalled.URL
			} else {
				cfg.APIURL = stalled.URL
			}
			cfg.Client = &http.Client{}
			if _, err := Run(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v, want context.DeadlineExceeded", err)
			}
			if !stalledOn.Load() {
				t.Fatal("the stalled request was never made")
			}
			assertUntouched(t, cfg)
		})
	}
}

// TestRunFailsInAnUnwritableRoot checks that a root Run cannot write to
// is refused with a permission error before any download and left as it
// was.
func TestRunFailsInAnUnwritableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	cfg, q := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	root := filepath.Dir(cfg.Dir)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	if _, err := Run(context.Background(), cfg); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("got %v, want a permission error", err)
	}
	assertUntouched(t, cfg)
	if got := q.downloads(); len(got) != 0 {
		t.Fatalf("downloaded %v", got)
	}
}

// TestRunReportsAnOldVersionItCannotRemove checks that a failure to
// remove the old version directory after the switch is reported, with
// the update done.
func TestRunReportsAnOldVersionItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from read-only directories")
	}
	cfg, _ := serve(t, newRelease(t, "v0.5.0", fullArchive(t, "v0.5.0"), ""), "v0.4.0")
	if err := os.Chmod(cfg.Dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfg.Dir, 0o755) })
	got, err := Run(context.Background(), cfg)
	prefix := "updated to v0.5.0, but could not remove " + cfg.Dir + ": "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("got %v, want %q and a permission error", err, prefix+"…")
	}
	if want := (Result{Current: "v0.4.0", Latest: "v0.5.0", Outcome: Updated}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	root := filepath.Dir(cfg.Dir)
	assertEntries(t, root, currentLink, "v0.4.0", "v0.5.0")
	assertCurrentLink(t, root, "v0.5.0")
}
