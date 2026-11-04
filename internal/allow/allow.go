// Package allow owns the host-side allow store: the record of which
// content of a workspace's rules file, together with which daemon
// settings, a human on the host has trusted. It keeps one file per
// workspace and only the latest allow of a workspace: a new allow
// replaces the previous one, so older content is no longer allowed.
// The store lies outside every workspace and the runtime directory, so
// the container can neither read nor forge it.
//
// It also keeps the latest request of each workspace, which completes the
// cycle of an allow: an `up` refused for want of an allow records what it
// asked for (Request), and a human on the host allows exactly that
// (Pending, then Allow, which removes the request) without repeating the
// settings flags devcontainer.json gave `up`.
package allow

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

	"github.com/kravlab/hostrunner/internal/settings"
)

// Open returns the allow store of the host, in the directory dir returns,
// with getenv reading the environment. With neither XDG_DATA_HOME nor HOME
// set there is no place to keep an allow and Open fails.
func Open(getenv func(string) string) (Store, error) {
	d, err := dir(getenv)
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: d}, nil
}

// dir returns the allow store's directory: $XDG_DATA_HOME/hostrunner/allow,
// or $HOME/.local/share/hostrunner/allow when XDG_DATA_HOME is empty,
// with getenv reading the environment. No setting names the location, so
// a workspace cannot point the store at a place it controls. With both
// variables empty there is no place to keep an allow and dir fails.
func dir(getenv func(string) string) (string, error) {
	if data := getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "hostrunner", "allow"), nil
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "share", "hostrunner", "allow"), nil
	}
	return "", errors.New("allow store: neither XDG_DATA_HOME nor HOME is set")
}

// Store is the allow store in Dir, which must be an absolute path: every
// method of a Store whose Dir is empty or relative fails, so a zero Store
// never reads or writes the current directory. The directory is created
// by the first Allow; until then the store is empty.
type Store struct{ Dir string }

// check returns an error unless the store's Dir is an absolute path.
func (s Store) check() error {
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("allow store: the directory %q is not an absolute path", s.Dir)
	}
	return nil
}

// Entry is one allow: what a human trusted for a workspace.
type Entry struct {
	// Workspace is the key of the workspace, see Store.Allow.
	Workspace string `json:"workspace"`
	// RulesFile is the rules file when allowed, as path made absolute with
	// symbolic links followed (see canonical), or empty when none was
	// named; it is kept for the human to read and is not part of the
	// comparison. The rules_file setting in Settings is held the same way.
	RulesFile string `json:"rules_file"`
	// Digest is the rules digest as package rules computes it, "missing"
	// for a missing file.
	Digest string `json:"digest"`
	// Settings are the daemon settings by key name, as
	// settings.Settings.Daemon() returns them.
	Settings map[string]string `json:"settings"`
}

// key returns the identity of a workspace: its path made absolute with
// symbolic links followed, so a link and its target are the same
// workspace. A path that cannot be resolved, because it does not exist,
// is identified by its cleaned absolute path, so that an allow can still
// be looked up and removed.
func key(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("allow store: %w", err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// SameWorkspace reports whether the paths a and b name one workspace as
// the store keys it (see key): with symbolic links followed, so a link and
// its target are one workspace, and a path that does not exist compared
// by its cleaned absolute path.
func SameWorkspace(a, b string) (bool, error) {
	ka, err := key(a)
	if err != nil {
		return false, err
	}
	kb, err := key(b)
	if err != nil {
		return false, err
	}
	return ka == kb, nil
}

// canonical returns the identity of a rules file, as key returns that of a
// workspace: its path made absolute with symbolic links followed, so a
// link and its target are the same file. A path that cannot be resolved,
// because it does not exist, is identified by its cleaned absolute path.
// An empty path stays empty: no file is named.
func canonical(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return key(path)
}

// canonicalSettings returns a clone of the daemon settings whose
// rules_file, when present, is canonical; the caller's map is not changed.
// A nil map stays nil.
func canonicalSettings(in map[string]string) (map[string]string, error) {
	out := maps.Clone(in)
	if v, ok := out[settings.RulesFileKey]; ok {
		c, err := canonical(v)
		if err != nil {
			return nil, err
		}
		out[settings.RulesFileKey] = c
	}
	return out, nil
}

// canonicalArgs returns the rules file and the daemon settings in the form
// the store holds and compares them (see canonical, canonicalSettings).
func canonicalArgs(rulesFile string, in map[string]string) (string, map[string]string, error) {
	rf, err := canonical(rulesFile)
	if err != nil {
		return "", nil, err
	}
	st, err := canonicalSettings(in)
	if err != nil {
		return "", nil, err
	}
	return rf, st, nil
}

// file returns the path of the allow file of a workspace key: the hex
// SHA-256 of the key, which is a valid file name whatever the path holds.
func (s Store) file(k string) string {
	return filepath.Join(s.Dir, name(k)+".json")
}

// requestFile returns the path of the request file of a workspace key,
// named as its allow file is, with .request.json.
func (s Store) requestFile(k string) string {
	return filepath.Join(s.Dir, name(k)+".request.json")
}

// name returns the hex SHA-256 of a workspace key.
func name(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

// Allow records that the content of the workspace's rules file, with the
// given digest, and the given daemon settings are trusted, and returns
// the Entry it recorded. It replaces any earlier allow of the workspace
// and then removes its request, if any, which the allow answers.
// The file is written to a temporary file and renamed over the target, so
// a reader sees the whole old allow or the whole new one, never a part,
// and is readable by its owner only. rulesFile, like the rules_file
// setting, is stored in its canonical form (see canonical).
func (s Store) Allow(workspace, rulesFile, digest string, settings map[string]string) (Entry, error) {
	if err := s.check(); err != nil {
		return Entry{}, err
	}
	k, err := key(workspace)
	if err != nil {
		return Entry{}, err
	}
	rulesFile, settings, err = canonicalArgs(rulesFile, settings)
	if err != nil {
		return Entry{}, err
	}
	e, err := s.record(s.file(k), k, rulesFile, digest, settings)
	if err != nil {
		return Entry{}, err
	}
	if err := os.Remove(s.requestFile(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Entry{}, fmt.Errorf("allow store: %w", err)
	}
	return e, nil
}

// Request records what an `up` refused for want of an allow asked for:
// the workspace's rules file, the digest of its content and the daemon
// settings, for a human on the host to allow (see Pending). Each
// workspace has one request: a new one replaces the old, so the request
// is always that of the latest refused `up`. It is written as Allow
// writes an allow, rules file and settings in the canonical form included,
// next to it, and is no allow: Allowed does not read it.
func (s Store) Request(workspace, rulesFile, digest string, settings map[string]string) error {
	if err := s.check(); err != nil {
		return err
	}
	k, err := key(workspace)
	if err != nil {
		return err
	}
	rulesFile, settings, err = canonicalArgs(rulesFile, settings)
	if err != nil {
		return err
	}
	_, err = s.record(s.requestFile(k), k, rulesFile, digest, settings)
	return err
}

// record writes the Entry of the workspace key k to path in the store,
// creating the store directory, and returns it.
func (s Store) record(path, k, rulesFile, digest string, settings map[string]string) (Entry, error) {
	e := Entry{Workspace: k, RulesFile: rulesFile, Digest: digest, Settings: maps.Clone(settings)}
	if e.Settings == nil {
		e.Settings = map[string]string{}
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return Entry{}, fmt.Errorf("allow store: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("allow store: %w", err)
	}
	if err := s.write(path, append(data, '\n')); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// write replaces the file at path with data through a temporary file in
// the store, removing the temporary file when any step fails.
func (s Store) write(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(s.Dir, ".allow-*")
	if err != nil {
		return fmt.Errorf("allow store: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("allow store: write %s: %w", tmp.Name(), err)
	}
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("allow store: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("allow store: close %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("allow store: %w", err)
	}
	return nil
}

// Deny removes the allow of the workspace. removed is false, with a nil
// error, when the workspace had none, so denying twice is not a failure.
// The workspace's request, if any, stays: a later `allow` may still
// answer it.
func (s Store) Deny(workspace string) (removed bool, err error) {
	if err := s.check(); err != nil {
		return false, err
	}
	k, err := key(workspace)
	if err != nil {
		return false, err
	}
	err = os.Remove(s.file(k))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("allow store: %w", err)
}

// Allowed reports whether the workspace has an allow that covers exactly
// the given digest and daemon settings: the same keys with the same
// values, none added or missing; the rules_file value is compared in its
// canonical form (see canonical). A workspace never allowed, or a store
// directory that does not exist, is not allowed and not an error. An allow
// file that cannot be read or is not valid is an error naming the file,
// not a silent denial, so that damage to the store is noticed.
func (s Store) Allowed(workspace, digest string, settings map[string]string) (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	k, err := key(workspace)
	if err != nil {
		return false, err
	}
	settings, err = canonicalSettings(settings)
	if err != nil {
		return false, err
	}
	e, ok, err := read(s.file(k))
	if !ok || err != nil {
		return false, err
	}
	return e.Workspace == k && e.Digest == digest && maps.Equal(e.Settings, settings), nil
}

// Pending returns the request of the workspace, which the latest `up`
// refused for want of an allow recorded (see Request), for a human on the
// host to allow. ok is false, with a nil error, when there is none: no
// `up` was refused since the last allow, or the store directory does not
// exist. A request file that cannot be read, is not valid or holds another
// workspace is an error naming the file.
func (s Store) Pending(workspace string) (e Entry, ok bool, err error) {
	if err := s.check(); err != nil {
		return Entry{}, false, err
	}
	k, err := key(workspace)
	if err != nil {
		return Entry{}, false, err
	}
	path := s.requestFile(k)
	e, ok, err = read(path)
	if !ok || err != nil {
		return Entry{}, false, err
	}
	if e.Workspace != k {
		return Entry{}, false, fmt.Errorf("allow store: %s: holds the request of %s, not of %s", path, e.Workspace, k)
	}
	return e, true, nil
}

// read returns the Entry in the file at path; ok is false, with a nil
// error, when the file does not exist. A file that cannot be read or is
// not valid is an error naming it.
func read(path string) (e Entry, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("allow store: %w", err)
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false, fmt.Errorf("allow store: %s: %w", path, err)
	}
	return e, true, nil
}
