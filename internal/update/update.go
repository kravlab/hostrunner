// Package update implements `hostrunner update`: it switches an installed
// hostrunner and hostrun to the latest GitHub release
// (docs/specs/update.md).
//
// The binaries live in a directory per version under a layout root, and
// the root's current link names the version in use (see currentLink).
// One rename of that symlink replaces both binaries, so the pair is never
// mixed, even after a crash: the new version directory and the switch are
// synced to disk before the old version is removed. A flock on the root
// keeps two updates of one layout apart.
//
// The release is trusted as far as its checksums.txt: the archive must
// match the SHA-256 listed there. Build provenance is not checked.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"
	"golang.org/x/sys/unix"
)

// Where releases come from on github.com; Config takes them so that tests
// can point at a fake server.
const (
	APIURL      = "https://api.github.com/repos/kravlab/hostrunner"
	DownloadURL = "https://github.com/kravlab/hostrunner/releases/download"
)

// currentLink is the symlink in the layout root that points, by a
// relative name, to the version directory in use; <prefix>/bin's
// hostrunner and hostrun point through it.
const currentLink = "current"

// tmpPrefix starts the name of every temporary entry an update and `mise
// run install` create in the layout root: .tmp.<version>-* directories and
// .tmp.current-* symlinks. Both take the lock (see lock) before creating
// any, so under the lock no other run is using one. A killed install can
// leave the current link pointing to its .tmp.<version>-* copy; layoutRoot
// and checkCurrentLink refuse that layout, so an update that removes
// temporary entries never removes the one in use.
const tmpPrefix = ".tmp."

// binaries are the files a release consists of; both must be in its
// archive.
var binaries = []string{"hostrun", "hostrunner"}

// supported lists the platforms releases are built for (.goreleaser.yaml).
var supported = map[string]bool{"linux/amd64": true, "linux/arm64": true}

// ErrDevBuild refuses a build without a version to compare, which could
// be newer or older than any release.
var ErrDevBuild = errors.New("cannot compare a dev build with releases; install a release")

// Outcome is what Run did.
type Outcome int

const (
	// UpToDate means the latest release is not newer; nothing was written.
	UpToDate Outcome = iota
	// Available means a newer release exists and was not installed: Run
	// had CheckOnly, or it failed before switching to the release.
	Available
	// Updated means the current link points to the latest release.
	Updated
)

// Result reports the versions Run compared and what it did. With an error,
// Outcome is meaningful only as Updated (the switch was done); otherwise
// it says nothing.
type Result struct {
	Current string  // the running build's version
	Latest  string  // the latest release's tag
	Outcome Outcome // what Run did
}

// Config is what Run needs.
type Config struct {
	APIURL      string       // the repository in GitHub's REST API (see the APIURL constant)
	DownloadURL string       // base of release asset URLs (see the DownloadURL constant)
	Current     string       // the running build's version: a tag, a pseudo-version or "dev"
	Dir         string       // the running hostrunner's version directory, symlinks resolved
	GOOS        string       // operating system of the archive to download
	GOARCH      string       // architecture of the archive to download
	CheckOnly   bool         // only compare; download and write nothing
	Client      *http.Client // nil: http.DefaultClient
}

// Run compares cfg.Current with the latest release and, unless
// cfg.CheckOnly, installs a newer release next to cfg.Dir, switches the
// layout's current link to it and removes every other version directory.
// It never downgrades. A newer release is refused, with CheckOnly too, on
// a platform without release builds or when cfg.Dir is not the version
// directory the current link names (see layoutRoot), and without
// CheckOnly when another update or install holds the layout's lock or a
// leftover of a killed or interrupted run cannot be removed. On an error
// before the switch, including a cancelled ctx, the current link still
// points to cfg.Dir and nothing Run created is left (a failure to remove
// it is joined after the error); a failure to sync the switch or to
// remove a version directory after it is returned with Updated. Network
// calls end with ctx: the caller bounds them with a deadline.
func Run(ctx context.Context, cfg Config) (res Result, err error) {
	res = Result{Current: cfg.Current}
	if !semver.IsValid(cfg.Current) { // "dev"
		return res, ErrDevBuild
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	latest, err := latestTag(ctx, cfg)
	if err != nil {
		return res, err
	}
	res.Latest = latest
	// Build metadata (+dirty) is ignored, and a pseudo-version sorts below
	// the release it precedes.
	if semver.Compare(latest, cfg.Current) <= 0 {
		return res, nil
	}
	if platform := cfg.GOOS + "/" + cfg.GOARCH; !supported[platform] {
		return res, fmt.Errorf("no release builds for %s", platform)
	}
	res.Outcome = Available
	// The check runs before the lock too, so that an install outside the
	// layout gets no lock taken in a directory that is not a layout root.
	root, err := layoutRoot(cfg.Dir, cfg.Current)
	if err != nil || cfg.CheckOnly {
		return res, err
	}
	unlock, err := lock(root)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := checkCurrentLink(root, cfg.Current); err != nil {
		return res, err
	}
	if err := removeLeftovers(root, latest); err != nil {
		return res, err
	}
	// Created before the download, so that an unwritable root fails fast.
	tmp, err := os.MkdirTemp(root, tmpPrefix+latest+"-*")
	if err != nil {
		return res, err
	}
	final := filepath.Join(root, latest)
	defer func() {
		// Until the switch, the current link points to cfg.Dir, so neither
		// is in use; removeLeftovers removed any earlier final, so this one
		// is installVersion's. Each exists only on one side of its rename,
		// and removing a missing path succeeds.
		if res.Outcome != Updated {
			if rerr := errors.Join(remove(tmp), remove(final)); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err := os.Chmod(tmp, 0o755); err != nil { // MkdirTemp creates 0700
		return res, err
	}
	files, err := download(ctx, cfg, latest)
	if err != nil {
		return res, err
	}
	if err := installVersion(tmp, final, files); err != nil {
		return res, err
	}
	// A cancellation (Ctrl-C) after the last read of the download, during
	// the extraction or the install, must not be followed by the switch;
	// one during a download already failed it.
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := switchCurrentLink(root, latest); err != nil {
		return res, err
	}
	res.Outcome = Updated
	// Removing the old version before the switch is on disk could leave
	// neither after a crash.
	if err := syncDir(root); err != nil {
		return res, fmt.Errorf("updated to %s, but could not sync %s, so %s is kept: %w", latest, root, cfg.Dir, err)
	}
	if err := removeOtherVersions(root, latest); err != nil {
		return res, fmt.Errorf("updated to %s, but %w", latest, err)
	}
	return res, nil
}

// layoutRoot returns the root of the layout dir belongs to: dir's parent.
// dir must be named after version, the running one, and the root's
// current link must point to that name. Anything else is an install
// outside the layout, which one rename cannot update; a version directory
// named otherwise could even be the <root>/<latest> installVersion
// replaces. The refusal names what was found.
func layoutRoot(dir, version string) (string, error) {
	root := filepath.Dir(dir)
	link := filepath.Join(root, currentLink)
	target, err := os.Readlink(link)
	if err == nil && filepath.Base(dir) == version && target == version {
		return root, nil
	}
	found := link + " -> " + target
	if err != nil {
		found = err.Error()
	}
	return "", fmt.Errorf("hostrunner is not installed in the versioned layout (%s; running %s from %s); reinstall it as the README describes",
		found, version, dir)
}

// lock takes an exclusive flock on the layout root directory without
// waiting, the lock `mise run install` takes too (flock -n on the root): a
// holder is refused rather than waited for, so that Run stays within its
// deadline. The returned function releases it.
func lock(root string) (func(), error) {
	f, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another update or install is running in %s", root)
		}
		return nil, fmt.Errorf("lock %s: %w", root, err)
	}
	return func() { f.Close() }, nil
}

// checkCurrentLink checks, under the lock, that root's current link still
// names version, as layoutRoot found it before the lock: another update
// or install that held the lock meanwhile may have switched it.
func checkCurrentLink(root, version string) error {
	link := filepath.Join(root, currentLink)
	target, err := os.Readlink(link)
	if err != nil {
		return err
	}
	if target != version {
		return fmt.Errorf("another update or install switched %s to %s", link, target)
	}
	return nil
}

// removeLeftovers removes what a killed or interrupted run left in root:
// the temporary entries (tmpPrefix) and the version directory of latest,
// which installVersion is about to create. A symlink is removed, not
// followed. It must run under the lock, when no other run is using them,
// and after checkCurrentLink, which makes sure that the current link
// names the running version: neither a temporary entry nor latest, which
// is newer.
func removeLeftovers(root, latest string) error {
	return removeEntries(root, func(e fs.DirEntry) bool {
		return strings.HasPrefix(e.Name(), tmpPrefix) || e.Name() == latest
	})
}

// removeOtherVersions removes every version directory in root but keep,
// the current link's target: the one just switched from and any an
// earlier update or install could not remove. A version directory is a
// directory, not a symlink, named dev or a semver version
// (docs/specs/update.md); every other entry is kept, so that a root
// holding something else loses nothing but versions.
func removeOtherVersions(root, keep string) error {
	return removeEntries(root, func(e fs.DirEntry) bool {
		name := e.Name()
		return e.IsDir() && name != keep && (semver.IsValid(name) || name == "dev")
	})
}

// removeEntries removes the entries of root that match, stopping at the
// first failure; the entries after it are left for a later run.
func removeEntries(root string, match func(fs.DirEntry) bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !match(e) {
			continue
		}
		if err := remove(filepath.Join(root, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// remove removes path and anything under it, without following symlinks;
// a missing path is no error. The error names path.
func remove(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("could not remove %s: %w", path, err)
	}
	return nil
}

// latestTag returns the tag of the latest release, which GitHub picks
// among published, non-pre-release releases. Only a canonical version is
// accepted: a shorthand like v1.2 would name no release archive.
func latestTag(ctx context.Context, cfg Config) (string, error) {
	body, err := get(ctx, cfg.Client, cfg.APIURL+"/releases/latest")
	if err != nil {
		return "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	if semver.Canonical(rel.TagName) != rel.TagName {
		return "", fmt.Errorf("latest release %q is not a version", rel.TagName)
	}
	return rel.TagName, nil
}

// download fetches the archive of tag for cfg's platform, checks it
// against the release's checksums.txt and returns the binaries in it, by
// name.
func download(ctx context.Context, cfg Config, tag string) (map[string][]byte, error) {
	// GoReleaser's archive name drops the tag's v.
	name := fmt.Sprintf("hostrunner_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), cfg.GOOS, cfg.GOARCH)
	base := cfg.DownloadURL + "/" + tag + "/"
	sums, err := get(ctx, cfg.Client, base+"checksums.txt")
	if err != nil {
		return nil, err
	}
	want, err := checksum(sums, name)
	if err != nil {
		return nil, err
	}
	archive, err := get(ctx, cfg.Client, base+name)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s: checksum mismatch", name)
	}
	return extract(archive)
}

// get returns the body of a GET of url, refusing any status but 200.
func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// checksum finds the SHA-256 of name in a checksums.txt (lines of
// "<hex>  <name>", as sha256sum writes them).
func checksum(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			return strings.ToLower(f[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s is not listed in checksums.txt", name)
}

// extract returns the binaries of a release archive, by name; an archive
// missing either is refused.
func extract(archive []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, name := range binaries {
			if h.Name == name && h.Typeflag == tar.TypeReg {
				if files[name], err = io.ReadAll(tr); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, name := range binaries {
		if _, ok := files[name]; !ok {
			return nil, fmt.Errorf("release archive has no %s", name)
		}
	}
	return files, nil
}

// installVersion writes files into the empty temporary directory tmp
// and renames it to final, the version directory, once every file and tmp
// itself are synced, so final is complete or absent; final's parent, the
// root, is synced after the rename. final must not exist: Run removed a
// leftover one (removeLeftovers). Run removes tmp and final on an error.
func installVersion(tmp, final string, files map[string][]byte) error {
	for name, data := range files {
		if err := writeSynced(filepath.Join(tmp, name), data); err != nil {
			return err
		}
	}
	if err := syncDir(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return syncDir(filepath.Dir(final))
}

// writeSynced writes data to the new executable file path and syncs its
// contents to disk; the directory entry is synced by installVersion.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o755) // the umask may have narrowed the mode
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// switchCurrentLink points root's current link to version with one rename of
// a new symlink over it, the step that replaces both binaries at once;
// Run syncs root afterwards. The new symlink gets a random name
// (tmpPrefix), another one on a collision. On an error the current link
// is unchanged and the new symlink is removed; a failure of that removal
// is joined after the error.
func switchCurrentLink(root, version string) error {
	var tmp string
	for {
		tmp = filepath.Join(root, fmt.Sprintf("%s%s-%d", tmpPrefix, currentLink, rand.Uint64()))
		err := os.Symlink(version, tmp)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if err := os.Rename(tmp, filepath.Join(root, currentLink)); err != nil {
		if rerr := remove(tmp); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

// syncDir syncs the directory dir, making the entries created, renamed or
// removed in it durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
