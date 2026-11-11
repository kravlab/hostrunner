package allow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The store's layout: a directory per workspace, named by the hex SHA-256
// of the workspace's key, and in it the files of its runtime directories,
// each named by the hex SHA-256 of the runtime directory's key followed by
// one of these suffixes. Names made so are valid whatever the paths hold.
// The temporary files the store writes start with a dot and end with
// neither suffix. Files of the earlier flat shape, one per workspace in
// the store directory itself, lie outside every workspace directory and so
// are never read.
const (
	allowSuffix   fileKind = ".json"
	requestSuffix fileKind = ".request.json"
)

// fileKind is the kind of a file of the store, an allow or a pending
// request, given as the suffix its name ends with (see allowSuffix).
type fileKind string

// fileOps are the file operations of a Store that a test of the package
// replaces to make a step fail; a nil one is that of package os.
type fileOps struct {
	rename func(oldpath, newpath string) error
	remove func(path string) error
}

// rename renames oldpath to newpath with the rename of s.ops, os.Rename
// when there is none.
func (s Store) rename(oldpath, newpath string) error {
	if s.ops != nil && s.ops.rename != nil {
		return s.ops.rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

// remove removes the file at path with the remove of s.ops, os.Remove
// when there is none.
func (s Store) remove(path string) error {
	if s.ops != nil && s.ops.remove != nil {
		return s.ops.remove(path)
	}
	return os.Remove(path)
}

// workspaceDir returns the directory of the store that holds the files of
// the runtime directories of the workspace key wsKey.
func (s Store) workspaceDir(wsKey string) string {
	return filepath.Join(s.Dir, name(wsKey))
}

// file returns the path of the file of kind of the keyed Place p: its
// allow with allowSuffix, its pending request with requestSuffix.
func (s Store) file(p Place, kind fileKind) string {
	return filepath.Join(s.workspaceDir(p.Workspace), name(p.RuntimeDir)+string(kind))
}

// name returns the hex SHA-256 of a key.
func name(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

// suffixOf returns the kind of the file named n, requestSuffix or
// allowSuffix, or "" for a file that is neither, such as a temporary one.
func suffixOf(n string) fileKind {
	switch {
	case strings.HasPrefix(n, "."):
		return ""
	case strings.HasSuffix(n, string(requestSuffix)):
		return requestSuffix
	case strings.HasSuffix(n, string(allowSuffix)):
		return allowSuffix
	}
	return ""
}

// stage writes the file of e to a temporary file in the directory of its
// workspace, creating the directories readable by their owner only, and
// returns its path, for the caller to rename into place: the first step
// of every write of the store, so that a reader sees the whole old file
// or the whole new one, never a part.
func (s Store) stage(e Entry) (tmp string, err error) {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", fmt.Errorf("allow store: %w", err)
	}
	dir := s.workspaceDir(e.Workspace)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("allow store: %w", err)
	}
	return writeTemp(dir, append(data, '\n'))
}

// write records e as the file of kind of its Place, replacing the
// earlier one: it stages the file (see stage) and renames it into place.
func (s Store) write(e Entry, kind fileKind) error {
	tmp, err := s.stage(e)
	if err != nil {
		return err
	}
	if err := s.rename(tmp, s.file(e.Place, kind)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("allow store: %w", err)
	}
	return nil
}

// earlierFile is the content of a file of the store before writeAll
// replaced it, ok false when there was none.
type earlierFile struct {
	data []byte
	ok   bool
}

// writeAll records each of entries as the file of kind of its Place,
// all or none: it stages every file (see stage) and keeps the earlier
// content of each before it renames any into place. A failure to stage or
// to read an earlier file records none; a rename that fails puts back the
// earlier content of the files renamed before it, removing those that had
// none, so that none is recorded either. The temporary files left are
// removed. Only a failure to put an earlier file back leaves the new one,
// joined to the error.
func (s Store) writeAll(entries []Entry, kind fileKind) error {
	temps := make([]string, 0, len(entries))
	defer func() {
		for _, tmp := range temps {
			if tmp != "" {
				_ = os.Remove(tmp)
			}
		}
	}()
	for _, e := range entries {
		tmp, err := s.stage(e)
		if err != nil {
			return err
		}
		temps = append(temps, tmp)
	}
	earlier := make([]earlierFile, len(entries))
	for i, e := range entries {
		data, err := os.ReadFile(s.file(e.Place, kind))
		switch {
		case err == nil:
			earlier[i] = earlierFile{data: data, ok: true}
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("allow store: %w", err)
		}
	}
	for i, e := range entries {
		if err := s.rename(temps[i], s.file(e.Place, kind)); err != nil {
			return errors.Join(fmt.Errorf("allow store: %w", err), s.restore(entries[:i], earlier[:i], kind))
		}
		temps[i] = ""
	}
	return nil
}

// restore puts back earlier[i] as the file of kind of entries[i], in
// reverse order, removing the file where there was none, and joins the
// errors of those it cannot put back.
func (s Store) restore(entries []Entry, earlier []earlierFile, kind fileKind) error {
	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		path := s.file(entries[i].Place, kind)
		if !earlier[i].ok {
			if err := s.remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("allow store: %w", err))
			}
			continue
		}
		tmp, err := writeTemp(filepath.Dir(path), earlier[i].data)
		if err == nil {
			if err = s.rename(tmp, path); err != nil {
				_ = os.Remove(tmp)
				err = fmt.Errorf("allow store: %w", err)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writeTemp writes data to a new temporary file in dir, readable by its
// owner only, and returns its path, for the caller to rename into place.
// It removes the file when any step fails.
func writeTemp(dir string, data []byte) (path string, err error) {
	tmp, err := os.CreateTemp(dir, ".allow-*")
	if err != nil {
		return "", fmt.Errorf("allow store: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return "", fmt.Errorf("allow store: write %s: %w", tmp.Name(), err)
	}
	if err = tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("allow store: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("allow store: close %s: %w", tmp.Name(), err)
	}
	return tmp.Name(), nil
}

// scan returns the entries of the files of kind, the allows or the
// pending requests, of the runtime directories of the workspace key
// wsKey, sorted by runtime directory, and apart the files of kind that
// cannot be read or are not valid (see decode), sorted by path; none when
// the workspace has no directory in the store. A file removed while scan
// reads the directory is skipped. Only a directory that cannot be read is
// an error.
func (s Store) scan(wsKey string, kind fileKind) (entries []Entry, damaged []DamagedFile, err error) {
	dir := s.workspaceDir(wsKey)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("allow store: %w", err)
	}
	for _, d := range ents {
		if suffixOf(d.Name()) != kind {
			continue
		}
		path := filepath.Join(dir, d.Name())
		e, ok, err := decode(path, wsKey, kind)
		if err != nil {
			damaged = append(damaged, DamagedFile{Path: path, Err: err})
			continue
		}
		if ok {
			entries = append(entries, e)
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.RuntimeDir, b.RuntimeDir) })
	return entries, damaged, nil
}

// list returns the entries of the files of kind of the runtime
// directories of the workspace key wsKey as scan does; a file that cannot
// be read or is not valid is an error naming it (see read).
func (s Store) list(wsKey string, kind fileKind) ([]Entry, error) {
	entries, damaged, err := s.scan(wsKey, kind)
	if err != nil {
		return nil, err
	}
	if len(damaged) > 0 {
		return nil, damagedErr(damaged[0])
	}
	return entries, nil
}

// read returns the Entry in the file at path, the file of kind of a
// runtime directory of the workspace key wsKey, as decode does, with an
// error that names the file.
func read(path, wsKey string, kind fileKind) (e Entry, ok bool, err error) {
	e, ok, err = decode(path, wsKey, kind)
	if err != nil {
		return Entry{}, false, damagedErr(DamagedFile{Path: path, Err: err})
	}
	return e, ok, nil
}

// damagedErr returns the error of the damaged file f, naming it.
func damagedErr(f DamagedFile) error {
	return fmt.Errorf("allow store: %s: %w", f.Path, f.Err)
}

// decode returns the Entry in the file at path, the file of kind of a
// runtime directory of the workspace key wsKey; ok is false, with a nil
// error, when the file does not exist. A file that cannot be read, is not
// valid, or holds another workspace or a runtime directory other than the
// one it is the file of, or none, is an error saying why, without naming
// the file.
func decode(path, wsKey string, kind fileKind) (e Entry, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return Entry{}, false, err
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false, err
	}
	if e.Workspace != wsKey || e.RuntimeDir == "" || filepath.Base(path) != name(e.RuntimeDir)+string(kind) {
		return Entry{}, false, fmt.Errorf("holds the workspace %q and the runtime directory %q, not those of its name", e.Workspace, e.RuntimeDir)
	}
	return e, true, nil
}
