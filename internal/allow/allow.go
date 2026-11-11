// Package allow owns the host-side allow store: the record of which
// content of a workspace's rules file, together with which daemon
// settings, a human on the host has trusted for one runtime directory of
// the workspace, which is one per devcontainer config. It keeps one
// directory per workspace and in it one file per runtime directory, and
// only the latest allow of a runtime directory: a new allow replaces the
// previous one, so older content is no longer allowed there. Two
// devcontainer configs of a workspace whose `up`s get different settings
// flags so keep an allow each. The store lies outside every workspace and
// runtime directory, so the container can neither read nor forge it.
//
// It also keeps the latest request of each runtime directory, which
// completes the cycle of an allow: an `up` refused for want of an allow
// records what it asked for (Request), and a human on the host allows
// exactly that, for every runtime directory of the workspace at once
// (PendingAll, then AllowAll, which removes the requests), without
// repeating the settings flags devcontainer.json gave `up`.
//
// A rule added from the approval session moves the allows that covered
// the content it was written over (AllowAddedRule).
package allow

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"

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
// by the first allow or request; until then the store is empty.
type Store struct{ Dir string }

// check returns an error unless the store's Dir is an absolute path.
func (s Store) check() error {
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("allow store: the directory %q is not an absolute path", s.Dir)
	}
	return nil
}

// Scope is what an allow covers: the digest of the rules file's content
// together with the daemon settings.
type Scope struct {
	// Digest is the rules digest as package rules computes it, "missing"
	// for a missing file.
	Digest string `json:"digest"`
	// Settings are the daemon settings by key name, as
	// settings.Settings.Daemon() returns them.
	Settings map[string]string `json:"settings"`
}

// Target is what an allow, or the request for one, is recorded for: the
// Scope with the rules file it is of.
type Target struct {
	// RulesFile is the rules file when allowed, as path made absolute with
	// symbolic links followed (see canonical), or empty when none was
	// named; it is kept for the human to read and is not part of the
	// comparison. The rules_file setting in Settings is held the same way.
	RulesFile string `json:"rules_file"`
	Scope
}

// NewTarget returns the Target of rulesFile with the Scope of digest, the
// digest of its content, and settings, the daemon settings (see Scope).
func NewTarget(rulesFile, digest string, settings map[string]string) Target {
	return Target{RulesFile: rulesFile, Scope: Scope{Digest: digest, Settings: settings}}
}

// Place is one runtime directory of a workspace, which is one per
// devcontainer config: where an allow, and the pending request for one,
// belong. The store keys both paths (see key), so a path named through a
// symbolic link or with a trailing slash is the same Place.
type Place struct {
	// Workspace is the workspace folder on the host.
	Workspace string `json:"workspace"`
	// RuntimeDir is the runtime directory of the workspace's daemon, the
	// one `hostrunner up` gives it.
	RuntimeDir string `json:"runtime_dir"`
}

// Entry is one allow, or one pending request: what a human trusted, or is
// asked to trust, for a Place. Its fields are stored flat: workspace,
// runtime_dir, rules_file, digest and settings. An Entry the store returns
// holds its Place by its keys (see key).
type Entry struct {
	Place
	Target
}

// key returns the identity of a workspace or a runtime directory: its path
// made absolute with symbolic links followed, so a link and its target are
// the same directory, and a trailing slash makes no difference. A path
// that cannot be resolved, because it does not exist, is identified by its
// cleaned absolute path, so that an allow can still be looked up and
// removed.
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

// CanonicalSettings returns a clone of the daemon settings whose
// rules_file, when present, is canonical, as the store holds and compares
// it (see Store.Allow); the caller's map is not changed. A nil map stays
// nil. Settings that did not come from the store, such as
// settings.DaemonDefaults, are compared with those of an allow through it.
func CanonicalSettings(in map[string]string) (map[string]string, error) {
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

// canonicalTarget returns t in the form the store holds and compares it
// (see canonical, CanonicalSettings).
func canonicalTarget(t Target) (Target, error) {
	rf, err := canonical(t.RulesFile)
	if err != nil {
		return Target{}, err
	}
	st, err := CanonicalSettings(t.Settings)
	if err != nil {
		return Target{}, err
	}
	return Target{RulesFile: rf, Scope: Scope{Digest: t.Digest, Settings: st}}, nil
}

// keyed checks the store and returns p by its keys (see key). An empty
// runtime directory names none and is an error, so that nothing is ever
// recorded for the current directory.
func (s Store) keyed(p Place) (Place, error) {
	if err := s.check(); err != nil {
		return Place{}, err
	}
	if p.RuntimeDir == "" {
		return Place{}, errors.New("allow store: no runtime directory given")
	}
	ws, err := key(p.Workspace)
	if err != nil {
		return Place{}, err
	}
	rd, err := key(p.RuntimeDir)
	if err != nil {
		return Place{}, err
	}
	return Place{Workspace: ws, RuntimeDir: rd}, nil
}

// canonicalEntry returns the Entry of the keyed Place p and t, t in the
// form the store holds and compares it (see canonicalTarget), with
// settings that are never nil.
func canonicalEntry(p Place, t Target) (Entry, error) {
	t, err := canonicalTarget(t)
	if err != nil {
		return Entry{}, err
	}
	if t.Settings == nil {
		t.Settings = map[string]string{}
	}
	return Entry{Place: p, Target: t}, nil
}

// sameTarget reports whether a and b, both in canonical form, are the same
// Target: the same rules file, digest and daemon settings.
func sameTarget(a, b Target) bool {
	return a.RulesFile == b.RulesFile && a.Digest == b.Digest && maps.Equal(a.Settings, b.Settings)
}

// covers reports whether the allow e covers the Scope sc, whose settings
// are in canonical form: the same digest and the same daemon settings.
func covers(e Entry, sc Scope) bool {
	return e.Digest == sc.Digest && maps.Equal(e.Settings, sc.Settings)
}

// Allow records that the content of the rules file, with the digest and
// the daemon settings of t, is trusted for the Place p, and returns the
// Entry it recorded. It is AllowAll of one request: it replaces any
// earlier allow of p, leaving those of the workspace's other runtime
// directories, and then removes the pending request of p if it is that of
// t. With the allow recorded but the request not removed, it returns the
// Entry together with the error.
func (s Store) Allow(p Place, t Target) (Entry, error) {
	allowed, err := s.AllowAll(p.Workspace, []Entry{{Place: p, Target: t}})
	if len(allowed) == 0 {
		return Entry{}, err
	}
	return allowed[0], err
}

// AllowAll allows each of requests, pending requests of runtime
// directories of workspace as PendingAll returns them, for its Place, and
// returns the Entries it recorded, in the order of requests. It allows all
// or none (see writeAll): a request not of workspace (see key) or without
// a runtime directory, or a failure to write or to move any allow into
// place, records none, the earlier allows staying as they were. Once all
// are recorded it removes the requests they answer: a request file that
// holds another request by then, one recorded since PendingAll read it,
// stays pending for the next allow. A request that cannot be read or
// removed then is a *RequestsNotRemovedError returned together with the
// Entries recorded.
// The rules file of a request, like the rules_file setting, is stored in
// its canonical form (see canonical).
func (s Store) AllowAll(workspace string, requests []Entry) ([]Entry, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	wsKey, err := key(workspace)
	if err != nil {
		return nil, err
	}
	allowed := make([]Entry, 0, len(requests))
	for _, r := range requests {
		p, err := s.keyed(r.Place)
		if err != nil {
			return nil, err
		}
		if p.Workspace != wsKey {
			return nil, fmt.Errorf("allow store: the request for %s is of the workspace %s, not of %s", p.RuntimeDir, p.Workspace, wsKey)
		}
		e, err := canonicalEntry(p, r.Target)
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, e)
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	if err := s.writeAll(allowed, allowSuffix); err != nil {
		return nil, err
	}
	return allowed, s.removeAnswered(allowed)
}

// RequestsNotRemovedError is the error of an allow recorded whose pending
// requests answered could not all be read or removed: the allows are
// recorded, so a caller goes on as allowed, and a request left pending
// asks a human again for what is allowed already.
type RequestsNotRemovedError struct{ Err error }

// Error returns the error of the requests not removed.
func (e *RequestsNotRemovedError) Error() string { return e.Err.Error() }

// Unwrap returns the error of the requests not removed.
func (e *RequestsNotRemovedError) Unwrap() error { return e.Err }

// removeAnswered removes the pending request of the Place of each allow
// of allowed when it is still the request the allow answers (see
// sameTarget); a request recorded since stays. The errors of the requests
// that cannot be read or removed are joined in a *RequestsNotRemovedError.
func (s Store) removeAnswered(allowed []Entry) error {
	var errs []error
	for _, e := range allowed {
		path := s.file(e.Place, requestSuffix)
		req, ok, err := read(path, e.Workspace, requestSuffix)
		if err != nil {
			errs = append(errs, fmt.Errorf("the request answered is not removed: %w", err))
			continue
		}
		if !ok || !sameTarget(req.Target, e.Target) {
			continue
		}
		if err := remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("allow store: the request answered is not removed: %w", err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &RequestsNotRemovedError{Err: errors.Join(errs...)}
}

// Request records the pending request of the Place p: what an `up`
// refused for want of an allow there asked for, t, the rules file, the
// digest of its content and the daemon settings, for a human on the host
// to allow (see PendingAll). Each Place has one request: a new one
// replaces the old, so the request is always that of its latest refused
// `up`, and the requests of the workspace's other runtime directories
// stay. It is written as an allow is, rules file and settings in the
// canonical form included, next to it, and is no allow: Allowed does not
// read it.
func (s Store) Request(p Place, t Target) error {
	k, err := s.keyed(p)
	if err != nil {
		return err
	}
	e, err := canonicalEntry(k, t)
	if err != nil {
		return err
	}
	return s.write(e, requestSuffix)
}

// Deny removes the allows of every runtime directory of workspace and
// returns them, sorted by runtime directory; none, with a nil error, when
// the workspace had none, so denying twice is not a failure. It reads
// every allow first, and an allow file that cannot be read or is not
// valid is an error naming the file, with no allow removed. An allow that
// cannot be removed is an error returned together with the allows removed
// before it. The pending requests of the workspace stay: a later `allow`
// may still answer them.
func (s Store) Deny(workspace string) (removed []Entry, err error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	wsKey, err := key(workspace)
	if err != nil {
		return nil, err
	}
	allows, err := s.list(wsKey, allowSuffix)
	if err != nil {
		return nil, err
	}
	for _, e := range allows {
		if err := remove(s.file(e.Place, allowSuffix)); err != nil {
			return removed, fmt.Errorf("allow store: %w", err)
		}
		removed = append(removed, e)
	}
	return removed, nil
}

// AllowAddedRule records the allow of a rule added from the approval
// session to the rules file served from the Place p: writtenOver is the
// Scope the daemon of p checked the content the session wrote over with,
// and digest is the digest of the content written. The allow of p must
// cover writtenOver (see Allowed); its rules file is the one the rule was
// added to, as the rules_file setting is among what it covers. Every allow
// of p's workspace of that rules file with the digest of writtenOver, p's
// included, is moved to digest, its daemon settings unchanged, so that
// each runtime directory that trusted the content written over trusts what
// the human approved, with its own settings; the other allows stay. It
// returns the Entries it recorded, sorted by runtime directory. It moves
// all or none (see writeAll): an allow of p not covering writtenOver, an
// allow file that cannot be read or is not valid, or a failed write
// records none and is an error, the allows staying as they were. Once all
// are recorded it removes the pending requests they answer (see
// removeAnswered): a request that cannot be read or removed then is a
// *RequestsNotRemovedError returned together with the Entries recorded.
func (s Store) AllowAddedRule(p Place, writtenOver Scope, digest string) ([]Entry, error) {
	own, err := s.keyed(p)
	if err != nil {
		return nil, err
	}
	st, err := CanonicalSettings(writtenOver.Settings)
	if err != nil {
		return nil, err
	}
	allows, err := s.list(own.Workspace, allowSuffix)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(allows, func(e Entry) bool { return e.RuntimeDir == own.RuntimeDir })
	if i < 0 || !covers(allows[i], Scope{Digest: writtenOver.Digest, Settings: st}) {
		return nil, fmt.Errorf("allow store: the allow of %s does not cover the content %s written over with the daemon settings", own.RuntimeDir, writtenOver.Digest)
	}
	rulesFile := allows[i].RulesFile
	var moved []Entry
	for _, e := range allows {
		if e.RulesFile == rulesFile && e.Digest == writtenOver.Digest {
			e.Digest = digest
			moved = append(moved, e)
		}
	}
	if err := s.writeAll(moved, allowSuffix); err != nil {
		return nil, err
	}
	return moved, s.removeAnswered(moved)
}

// Allowed reports whether the Place p has an allow that covers exactly
// the given Scope: the same digest and the same daemon settings, the same
// keys with the same values, none added or missing; the rules_file value
// is compared in its canonical form (see canonical). The allows of the
// workspace's other runtime directories do not count. A Place never
// allowed, or a store directory that does not exist, is not allowed and
// not an error. An allow file that cannot be read or is not valid is an
// error naming the file, not a silent denial, so that damage to the store
// is noticed.
func (s Store) Allowed(p Place, sc Scope) (bool, error) {
	k, err := s.keyed(p)
	if err != nil {
		return false, err
	}
	settings, err := CanonicalSettings(sc.Settings)
	if err != nil {
		return false, err
	}
	e, ok, err := read(s.file(k, allowSuffix), k.Workspace, allowSuffix)
	if !ok || err != nil {
		return false, err
	}
	return covers(e, Scope{Digest: sc.Digest, Settings: settings}), nil
}

// PendingAll returns the pending requests of every runtime directory of
// workspace (see Request), sorted by runtime directory, for a human on the
// host to allow with AllowAll; none, with a nil error, when there is none.
// A request file that cannot be read or is not valid is an error naming
// the file.
func (s Store) PendingAll(workspace string) ([]Entry, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	wsKey, err := key(workspace)
	if err != nil {
		return nil, err
	}
	return s.list(wsKey, requestSuffix)
}
