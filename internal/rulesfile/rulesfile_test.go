package rulesfile

import (
	"os"
	"path/filepath"
	"testing"
)

// touch creates the file name in dir.
func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestChoose covers each row of the table in docs/specs/rules-file-name.md.
func TestChoose(t *testing.T) {
	tests := []struct {
		name         string
		newOn, oldOn bool
		wantOld      bool // the fallback is used
		wantIgnored  bool // the fallback is reported as ignored
	}{
		{"new only", true, false, false, false},
		{"old only", false, true, true, false},
		{"both", true, true, false, true},
		{"neither", false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.newOn {
				touch(t, dir, Name)
			}
			if tt.oldOn {
				touch(t, dir, FallbackName)
			}
			path, ignored := Choose(dir)
			want, wantIgnored := filepath.Join(dir, Name), ""
			if tt.wantOld {
				want = filepath.Join(dir, FallbackName)
			}
			if tt.wantIgnored {
				wantIgnored = filepath.Join(dir, FallbackName)
			}
			if path != want || ignored != wantIgnored {
				t.Errorf("Choose = (%q, %q), want (%q, %q)", path, ignored, want, wantIgnored)
			}
		})
	}
}

// TestChooseDanglingLink: a dangling link under the new name is present, so
// it is chosen, with or without the older file.
func TestChooseDanglingLink(t *testing.T) {
	for _, withOld := range []bool{false, true} {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, Name)); err != nil {
			t.Fatal(err)
		}
		if withOld {
			touch(t, dir, FallbackName)
		}
		path, ignored := Choose(dir)
		wantIgnored := ""
		if withOld {
			wantIgnored = filepath.Join(dir, FallbackName)
		}
		if path != filepath.Join(dir, Name) || ignored != wantIgnored {
			t.Errorf("old file %t: Choose = (%q, %q), want the new name and ignored %q", withOld, path, ignored, wantIgnored)
		}
	}
}

// TestChooseOtherError: an error other than "does not exist" while looking
// at the new name keeps it chosen and reports nothing. A directory below a
// regular file gives ENOTDIR without relying on permission bits.
func TestChooseOtherError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path, ignored := Choose(file)
	if want := filepath.Join(file, Name); path != want || ignored != "" {
		t.Errorf("Choose = (%q, %q), want (%q, \"\")", path, ignored, want)
	}
}
