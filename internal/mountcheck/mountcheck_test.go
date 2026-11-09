package mountcheck_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/kravlab/hostrunner/internal/mountcheck"
	"github.com/kravlab/hostrunner/internal/watch"
)

// tree is a host layout under a resolved temp dir: a workspace with a
// .devcontainer directory holding a subdirectory and a rules file, and a
// rules file outside the workspace.
type tree struct {
	base, ws, dc, dcSub, dcRules, rules string
}

func newTree(t *testing.T) tree {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := tree{
		base:  base,
		ws:    filepath.Join(base, "ws"),
		dc:    filepath.Join(base, "ws", ".devcontainer"),
		dcSub: filepath.Join(base, "ws", ".devcontainer", "sub"),
		rules: filepath.Join(base, "rules.yaml"),
	}
	tr.dcRules = filepath.Join(tr.dc, "hostrunner-rules.yaml")
	if err := os.MkdirAll(tr.dcSub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{tr.dcRules, tr.rules} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func rw(source, dest string) watch.Mount { return watch.Mount{Source: source, Destination: dest} }

func ro(source, dest string) watch.Mount {
	return watch.Mount{Source: source, Destination: dest, ReadOnly: true}
}

func container(id string, mounts ...watch.Mount) watch.Container {
	return watch.Container{ID: id, Running: true, Mounts: mounts}
}

// wantWritable fails the test unless err is a *WritableError equal to want
// whose message names the container, the host path and the container path.
func wantWritable(t *testing.T, err error, want mountcheck.WritableError) {
	t.Helper()
	got, ok := errors.AsType[*mountcheck.WritableError](err)
	if !ok {
		t.Fatalf("Check = %v, want a *WritableError", err)
	}
	if *got != want {
		t.Fatalf("Check = %+v, want %+v", *got, want)
	}
	for _, s := range []string{want.Container, want.Path, want.ContainerPath} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("message %q does not name %q", err.Error(), s)
		}
	}
}

func TestCheckPassesWhenProtectedPathsAreShadowedReadOnly(t *testing.T) {
	tr := newTree(t)
	tests := []struct {
		name   string
		mounts []watch.Mount
	}{
		{"read-only .devcontainer over writable workspace", []watch.Mount{
			rw(tr.ws, "/workspaces/ws"),
			ro(tr.dc, "/workspaces/ws/.devcontainer"),
		}},
		{"order does not matter", []watch.Mount{
			ro(tr.dc, "/workspaces/ws/.devcontainer"),
			rw(tr.ws, "/workspaces/ws"),
		}},
		{"destinations are cleaned", []watch.Mount{
			rw(tr.ws, "/workspaces//ws/"),
			ro(tr.dc, "/workspaces/ws/./.devcontainer/"),
		}},
		{"shadowed by a writable mount of something else", []watch.Mount{
			rw(tr.ws, "/workspaces/ws"),
			rw(filepath.Join(tr.base, "elsewhere"), "/workspaces/ws/.devcontainer"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.Check([]watch.Container{container("c1", tt.mounts...)}, []string{tr.dc, tr.dcRules})
			if err != nil {
				t.Fatalf("Check = %v, want nil", err)
			}
		})
	}
}

func TestCheckFailsWritableWorkspace(t *testing.T) {
	tr := newTree(t)
	err := mountcheck.Check([]watch.Container{container("c1", rw(tr.ws, "/workspaces/ws"))}, []string{tr.dc})
	wantWritable(t, err, mountcheck.WritableError{
		Container: "c1", Path: tr.dc, Source: tr.ws, ContainerPath: "/workspaces/ws/.devcontainer",
	})
}

func TestCheckFailsWritableAncestor(t *testing.T) {
	tr := newTree(t)
	tests := []struct {
		name          string
		mount         watch.Mount
		containerPath string
	}{
		{"root", rw("/", "/host"), filepath.Join("/host", tr.dc)},
		{"parent of the workspace", rw(tr.base, "/mnt"), "/mnt/ws/.devcontainer"},
		{"the protected path itself", rw(tr.dc, "/dc"), "/dc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.Check([]watch.Container{container("c1", tt.mount)}, []string{tr.dc})
			wantWritable(t, err, mountcheck.WritableError{
				Container: "c1", Path: tr.dc, Source: tt.mount.Source, ContainerPath: tt.containerPath,
			})
		})
	}
}

func TestCheckFailsWritableMountInsideProtectedPath(t *testing.T) {
	tr := newTree(t)
	tests := []struct {
		name   string
		mounts []watch.Mount
		source string
		dest   string
	}{
		{"subdirectory elsewhere", []watch.Mount{
			rw(tr.ws, "/workspaces/ws"),
			ro(tr.dc, "/workspaces/ws/.devcontainer"),
			rw(tr.dcSub, "/sub"),
		}, tr.dcSub, "/sub"},
		{"subdirectory over its read-only parent", []watch.Mount{
			ro(tr.dc, "/workspaces/ws/.devcontainer"),
			rw(tr.dcSub, "/workspaces/ws/.devcontainer/sub"),
		}, tr.dcSub, "/workspaces/ws/.devcontainer/sub"},
		{"protected directory under a read-only workspace", []watch.Mount{
			ro(tr.ws, "/workspaces/ws"),
			rw(tr.dc, "/workspaces/ws/.devcontainer"),
		}, tr.dc, "/workspaces/ws/.devcontainer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.Check([]watch.Container{container("c1", tt.mounts...)}, []string{tr.dc})
			wantWritable(t, err, mountcheck.WritableError{
				Container: "c1", Path: tr.dc, Source: tt.source, ContainerPath: tt.dest,
			})
		})
	}
}

func TestCheckRulesFileOutsideDevcontainer(t *testing.T) {
	tr := newTree(t)
	paths := []string{tr.dc, tr.rules}

	err := mountcheck.Check([]watch.Container{container("c1",
		ro(tr.ws, "/workspaces/ws"),
		rw(tr.rules, "/etc/hostrunner/rules.yaml"),
	)}, paths)
	wantWritable(t, err, mountcheck.WritableError{
		Container: "c1", Path: tr.rules, Source: tr.rules, ContainerPath: "/etc/hostrunner/rules.yaml",
	})

	err = mountcheck.Check([]watch.Container{container("c1",
		rw(tr.ws, "/workspaces/ws"),
		ro(tr.dc, "/workspaces/ws/.devcontainer"),
		ro(tr.base, "/hostbase"),
		ro(tr.rules, "/etc/hostrunner/rules.yaml"),
	)}, paths)
	if err != nil {
		t.Fatalf("rules file only read-only: Check = %v, want nil", err)
	}
}

func TestCheckPrefixesAreComponentWise(t *testing.T) {
	tr := newTree(t)
	mounts := []watch.Mount{
		rw(tr.ws+"2", "/workspaces/ws2"),
		rw(tr.dc+"2", "/workspaces/dc2"),
		rw(tr.rules+".bak", "/rules.bak"),
	}
	if err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tr.dc, tr.rules}); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
}

func TestCheckFollowsSymlinks(t *testing.T) {
	tr := newTree(t)
	link := filepath.Join(tr.base, "link")
	if err := os.Symlink(tr.ws, link); err != nil {
		t.Fatal(err)
	}

	t.Run("mount source", func(t *testing.T) {
		err := mountcheck.Check([]watch.Container{container("c1", rw(link, "/workspaces/ws"))}, []string{tr.dc})
		wantWritable(t, err, mountcheck.WritableError{
			Container: "c1", Path: tr.dc, Source: tr.ws, ContainerPath: "/workspaces/ws/.devcontainer",
		})
	})
	t.Run("protected path", func(t *testing.T) {
		err := mountcheck.Check([]watch.Container{container("c1", rw(tr.ws, "/workspaces/ws"))},
			[]string{filepath.Join(link, ".devcontainer")})
		wantWritable(t, err, mountcheck.WritableError{
			Container: "c1", Path: tr.dc, Source: tr.ws, ContainerPath: "/workspaces/ws/.devcontainer",
		})
	})
	t.Run("protected path read-only through its link target", func(t *testing.T) {
		err := mountcheck.Check([]watch.Container{container("c1",
			rw(tr.ws, "/workspaces/ws"),
			ro(tr.dc, "/workspaces/ws/.devcontainer"),
		)}, []string{filepath.Join(link, ".devcontainer")})
		if err != nil {
			t.Fatalf("Check = %v, want nil", err)
		}
	})
}

func TestCheckFailsMissingFileUnderWritableDirectory(t *testing.T) {
	tr := newTree(t)
	settings := filepath.Join(tr.ws, ".vscode", "settings.json")
	tests := []struct {
		name string
		path string
	}{
		{"as given", settings},
		{"through a symlinked ancestor", filepath.Join(tr.base, "link", ".vscode", "settings.json")},
	}
	if err := os.Symlink(tr.ws, filepath.Join(tr.base, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mountcheck.Check([]watch.Container{container("c1",
				rw(tr.ws, "/workspaces/ws"),
				ro(tr.dc, "/workspaces/ws/.devcontainer"),
			)}, []string{tr.dc, tt.path})
			wantWritable(t, err, mountcheck.WritableError{
				Container: "c1", Path: settings, Source: tr.ws, ContainerPath: "/workspaces/ws/.vscode/settings.json",
			})
		})
	}
}

func TestCheckIgnoresMountsWithoutHostSource(t *testing.T) {
	tr := newTree(t)
	mounts := []watch.Mount{
		{Type: "tmpfs", Destination: "/workspaces/ws/.devcontainer"},
		rw("", "/workspaces/ws/.devcontainer"),
		rw("relative/ws", "/ws"),
	}
	if err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tr.dc}); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
}

// volume is a named volume as a runtime reports it, a local one without
// options, as the runtime makes it by default: its source is an absolute
// path under the runtime's storage.
func volume(source, dest string) watch.Mount {
	return watch.Mount{Type: "volume", Name: "v", Source: source, Destination: dest, Volume: &watch.Volume{Driver: "local"}}
}

// bindVolume is a writable mount at dest of a volume of driver whose
// options name the device device, as a local volume created with type=none,
// o=bind and that device binds it; its source lies in the runtime's
// storage.
func bindVolume(driver, device, dest string) watch.Mount {
	return watch.Mount{
		Type: "volume", Name: "v", Source: "/var/lib/example/volumes/v/_data", Destination: dest,
		Volume: &watch.Volume{Driver: driver, Options: map[string]string{"type": "none", "o": "bind", "device": device}},
	}
}

// A local volume that names a host directory as its device is checked as
// a bind mount of that directory, resolved, with the mount's own
// read-only flag: one writable over a protected path fails the check.
func TestCheckFailsAWritableVolumeOfAProtectedDevice(t *testing.T) {
	tr := newTree(t)
	link := filepath.Join(tr.base, "link")
	symlink(t, tr.dc, link)
	tests := []struct {
		name   string
		mounts []watch.Mount
		want   mountcheck.WritableError
	}{
		{
			name:   "the .devcontainer directory over a read-only workspace",
			mounts: []watch.Mount{ro(tr.ws, "/workspaces/ws"), bindVolume("local", tr.dc, "/workspaces/ws/.devcontainer")},
			want:   mountcheck.WritableError{Container: "c1", Path: tr.dc, Source: tr.dc, ContainerPath: "/workspaces/ws/.devcontainer"},
		},
		{
			name:   "the .devcontainer directory through a link",
			mounts: []watch.Mount{bindVolume("local", link, "/dc")},
			want:   mountcheck.WritableError{Container: "c1", Path: tr.dc, Source: tr.dc, ContainerPath: "/dc"},
		},
		{
			name:   "the workspace",
			mounts: []watch.Mount{bindVolume("local", tr.ws, "/workspaces/ws")},
			want:   mountcheck.WritableError{Container: "c1", Path: tr.dc, Source: tr.ws, ContainerPath: "/workspaces/ws/.devcontainer"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantWritable(t, mountcheck.Check([]watch.Container{container("c1", tt.mounts...)}, []string{tr.dc}), tt.want)
		})
	}
}

// The same volume mounted read-only, or shadowed by a deeper read-only
// mount, passes, as a bind mount would.
func TestCheckPassesAVolumeOfAProtectedDeviceMountedReadOnly(t *testing.T) {
	tr := newTree(t)
	readOnly := bindVolume("local", tr.dc, "/workspaces/ws/.devcontainer")
	readOnly.ReadOnly = true
	for name, mounts := range map[string][]watch.Mount{
		"read-only": {readOnly},
		"shadowed":  {bindVolume("local", tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")},
	} {
		if err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tr.dc}); err != nil {
			t.Errorf("%s: Check = %v, want nil", name, err)
		}
	}
}

// A volume whose device is no host path (a tmpfs, an NFS export) or that
// names none, whatever its driver, holds no protected path: it is skipped.
func TestCheckIgnoresVolumesWithoutAHostDevice(t *testing.T) {
	tr := newTree(t)
	for name, v := range map[string]watch.Volume{
		"local, no options":   {Driver: "local"},
		"local tmpfs":         {Driver: "local", Options: map[string]string{"type": "tmpfs", "device": "tmpfs"}},
		"local nfs":           {Driver: "local", Options: map[string]string{"type": "nfs", "o": "addr=192.0.2.1,rw", "device": ":/export"}},
		"local, empty device": {Driver: "local", Options: map[string]string{"device": ""}},
		"another driver":      {Driver: "example", Options: map[string]string{"size": "1G"}},
	} {
		m := watch.Mount{Type: "volume", Name: "v", Source: tr.ws, Destination: "/workspaces/ws", Volume: &v}
		if err := mountcheck.Check([]watch.Container{container("c1", m)}, []string{tr.dc}); err != nil {
			t.Errorf("%s: Check = %v, want nil", name, err)
		}
	}
}

// A volume the runtime could not inspect, one reported without its
// inspection, or one of another driver whose options name a device, which
// the daemon cannot tell the content of, fails the check as unreadable
// mounts do.
func TestCheckFailsVolumesItCannotTell(t *testing.T) {
	tr := newTree(t)
	cause := errors.New("no such volume")
	for _, tt := range []struct {
		name  string
		mount watch.Mount
		text  string // in the error's message
	}{
		{"not inspected", watch.Mount{Type: "volume", Name: "v", Destination: "/v", VolumeErr: cause}, cause.Error()},
		{"no inspection reported", watch.Mount{Type: "volume", Name: "v", Destination: "/v"}, "volume v"},
		{"another driver with a device", bindVolume("example", tr.dc, "/v"), "example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fine := container("c1", ro(tr.ws, "/workspaces/ws"))
			err := mountcheck.Check([]watch.Container{fine, container("c2", tt.mount)}, []string{tr.dc})
			got, ok := errors.AsType[*mountcheck.UnreadableError](err)
			if !ok {
				t.Fatalf("Check = %v, want an *UnreadableError", err)
			}
			if got.Container != "c2" || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("Check = %v, want one of container c2 naming %q", err, tt.text)
			}
			if tt.mount.VolumeErr != nil && !errors.Is(err, cause) {
				t.Errorf("errors.Is(%v, cause) = false, want true", err)
			}
		})
	}
}

// A device that cannot be resolved fails the check as a bind mount source
// does, naming the container.
func TestCheckFailsOnAnUnresolvableDevice(t *testing.T) {
	tr := newTree(t)
	device := filepath.Join(tr.dcRules, "sub")
	err := mountcheck.Check([]watch.Container{container("c1", bindVolume("local", device, "/data"))}, []string{tr.dc})
	wantUnresolvable(t, err, "c1", device, syscall.ENOTDIR)
}

func TestCheckIgnoresVolumeMounts(t *testing.T) {
	tr := newTree(t)
	// The volume's source lies over the workspace, so a bind mount of it
	// would be writable there; the volume is not looked at.
	err := mountcheck.Check([]watch.Container{container("c1", volume(tr.ws, "/workspaces/ws"))}, []string{tr.dc})
	if err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
	err = mountcheck.Check([]watch.Container{container("c1", rw(tr.ws, "/workspaces/ws"))}, []string{tr.dc})
	if _, ok := errors.AsType[*mountcheck.WritableError](err); !ok {
		t.Fatalf("a bind mount of the same source: Check = %v, want a *WritableError", err)
	}
}

func TestCheckIgnoresVolumeWithUnresolvableSource(t *testing.T) {
	tr := newTree(t)
	// A volume source the daemon cannot read, as under a runtime's storage
	// owned by root, must not fail the check.
	source := filepath.Join(tr.dcRules, "_data")
	err := mountcheck.Check([]watch.Container{container("c1", volume(source, "/data"))}, []string{tr.dc})
	if err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
	err = mountcheck.Check([]watch.Container{container("c1", rw(source, "/data"))}, []string{tr.dc})
	wantUnresolvable(t, err, "c1", source, syscall.ENOTDIR)
}

func TestCheckReportsUnreadableMounts(t *testing.T) {
	tr := newTree(t)
	cause := errors.New("inspect failed")
	fine := container("c1", rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer"))
	unreadable := watch.Container{ID: "c2", Running: true, MountsErr: cause}

	err := mountcheck.Check([]watch.Container{fine, unreadable}, []string{tr.dc})
	got, ok := errors.AsType[*mountcheck.UnreadableError](err)
	if !ok {
		t.Fatalf("Check = %v, want an *UnreadableError", err)
	}
	if got.Container != "c2" || got.Err != cause {
		t.Errorf("Check = %+v, want container c2 and the cause", *got)
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(%v, cause) = false, want true", err)
	}
	if !strings.Contains(err.Error(), "c2") || !strings.Contains(err.Error(), cause.Error()) {
		t.Errorf("message %q does not name the container and the cause", err.Error())
	}
}

func TestCheckReportsFirstFailure(t *testing.T) {
	tr := newTree(t)
	cause := errors.New("inspect failed")
	writable := container("c1", rw(tr.ws, "/workspaces/ws"))
	unreadable := watch.Container{ID: "c2", MountsErr: cause}
	paths := []string{tr.dc, tr.dcRules}

	err := mountcheck.Check([]watch.Container{writable, unreadable}, paths)
	wantWritable(t, err, mountcheck.WritableError{
		Container: "c1", Path: tr.dc, Source: tr.ws, ContainerPath: "/workspaces/ws/.devcontainer",
	})

	err = mountcheck.Check([]watch.Container{unreadable, writable}, paths)
	if _, ok := errors.AsType[*mountcheck.UnreadableError](err); !ok {
		t.Fatalf("unreadable first: Check = %v, want an *UnreadableError", err)
	}

	err = mountcheck.Check([]watch.Container{writable}, []string{tr.dcRules, tr.dc})
	wantWritable(t, err, mountcheck.WritableError{
		Container: "c1", Path: tr.dcRules, Source: tr.ws, ContainerPath: "/workspaces/ws/.devcontainer/hostrunner-rules.yaml",
	})
}

func TestCheckPassesWithNothingToCheck(t *testing.T) {
	tr := newTree(t)
	if err := mountcheck.Check(nil, []string{tr.dc}); err != nil {
		t.Errorf("no containers: Check = %v, want nil", err)
	}
	if err := mountcheck.Check([]watch.Container{container("c1", rw("/", "/host"))}, nil); err != nil {
		t.Errorf("no paths: Check = %v, want nil", err)
	}
}

// symlink creates link pointing at target, which need not exist.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestCheckFollowsDanglingSymlinks(t *testing.T) {
	tr := newTree(t)
	out := filepath.Join(tr.base, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling link straight into out, and one reaching it relative to
	// its own directory through a chain of directory links.
	direct := filepath.Join(tr.dc, "direct.json")
	symlink(t, filepath.Join(out, "direct.json"), direct)
	symlink(t, "d2", filepath.Join(tr.base, "d1"))
	symlink(t, out, filepath.Join(tr.base, "d2"))
	chain := filepath.Join(tr.dc, "chain.json")
	symlink(t, "../../d1/chain.json", chain)

	workspace := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")}
	tests := []struct {
		name, path, target, containerPath string
	}{
		{"direct", direct, filepath.Join(out, "direct.json"), "/out/direct.json"},
		{"through a directory link chain", chain, filepath.Join(out, "chain.json"), "/out/chain.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name+", target writable", func(t *testing.T) {
			mounts := append(workspace[:len(workspace):len(workspace)], rw(out, "/out"))
			err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tt.path})
			wantWritable(t, err, mountcheck.WritableError{
				Container: "c1", Path: tt.target, Source: out, ContainerPath: tt.containerPath,
			})
		})
		t.Run(tt.name+", target read-only", func(t *testing.T) {
			mounts := append(workspace[:len(workspace):len(workspace)], ro(out, "/out"))
			if err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tt.path}); err != nil {
				t.Fatalf("Check = %v, want nil", err)
			}
		})
	}
}

func TestCheckFollowsDanglingMountSource(t *testing.T) {
	tr := newTree(t)
	link := filepath.Join(tr.base, "new-link")
	target := filepath.Join(tr.dc, "new")
	symlink(t, target, link)
	err := mountcheck.Check([]watch.Container{container("c1",
		rw(tr.ws, "/workspaces/ws"),
		ro(tr.dc, "/workspaces/ws/.devcontainer"),
		rw(link, "/new"),
	)}, []string{tr.dc})
	wantWritable(t, err, mountcheck.WritableError{
		Container: "c1", Path: tr.dc, Source: target, ContainerPath: "/new",
	})
}

// wantUnresolvable fails the test unless err is an *UnresolvableError for path
// in container (empty for a protected path) caused by cause, whose message
// names path and, if set, the container.
func wantUnresolvable(t *testing.T, err error, container, path string, cause error) {
	t.Helper()
	got, ok := errors.AsType[*mountcheck.UnresolvableError](err)
	if !ok {
		t.Fatalf("Check = %v, want an *UnresolvableError", err)
	}
	if got.Path != path {
		t.Errorf("Path = %q, want %q", got.Path, path)
	}
	if got.Container != container {
		t.Errorf("Container = %q, want %q", got.Container, container)
	}
	if named := strings.Contains(err.Error(), "container "); named != (container != "") {
		t.Errorf("message %q names a container: %v, want %v", err.Error(), named, container != "")
	}
	if container != "" && !strings.Contains(err.Error(), "container "+container) {
		t.Errorf("message %q does not name container %q", err.Error(), container)
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(%v, %v) = false, want true", err, cause)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("message %q does not name %q", err.Error(), path)
	}
}

func TestCheckFailsOnSymlinkLoop(t *testing.T) {
	tr := newTree(t)
	a := filepath.Join(tr.dc, "loop-a")
	symlink(t, "loop-b", a)
	symlink(t, "loop-a", filepath.Join(tr.dc, "loop-b"))
	fine := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")}
	fineContainer := container("c1", fine...)

	t.Run("protected path", func(t *testing.T) {
		err := mountcheck.Check([]watch.Container{container("c1", fine...)}, []string{tr.dc, a})
		wantUnresolvable(t, err, "", a, syscall.ELOOP)
	})
	t.Run("protected path through the loop", func(t *testing.T) {
		p := filepath.Join(a, "settings.json")
		err := mountcheck.Check([]watch.Container{container("c1", fine...)}, []string{p})
		wantUnresolvable(t, err, "", p, syscall.ELOOP)
	})
	t.Run("mount source", func(t *testing.T) {
		mounts := append(fine[:len(fine):len(fine)], ro(a, "/loop"))
		err := mountcheck.Check([]watch.Container{fineContainer, container("c2", mounts...)}, []string{tr.dc})
		wantUnresolvable(t, err, "c2", a, syscall.ELOOP)
	})
}

func TestCheckFollowsAtMost40Links(t *testing.T) {
	tr := newTree(t)
	out := filepath.Join(tr.base, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// link0 -> link1 -> ... -> linkN -> out/f.json; resolving linkN-k
	// takes k+1 hops.
	const n = 41
	link := func(i int) string { return filepath.Join(tr.dc, "link"+strconv.Itoa(i)) }
	symlink(t, filepath.Join(out, "f.json"), link(n-1))
	for i := n - 2; i >= 0; i-- {
		symlink(t, link(i+1), link(i))
	}
	mounts := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer"), rw(out, "/out")}

	err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{link(1)})
	wantWritable(t, err, mountcheck.WritableError{
		Container: "c1", Path: filepath.Join(out, "f.json"), Source: out, ContainerPath: "/out/f.json",
	})

	err = mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{link(0)})
	wantUnresolvable(t, err, "", link(0), syscall.ELOOP)
}

func TestCheckFailsWhenAPathComponentCannotBeRead(t *testing.T) {
	tr := newTree(t)
	fine := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")}
	// A component that is a file, not a directory: lstat fails with
	// ENOTDIR, an error other than not-exist, even for root.
	underFile := filepath.Join(tr.dcRules, "link")

	t.Run("protected path", func(t *testing.T) {
		err := mountcheck.Check([]watch.Container{container("c1", fine...)}, []string{underFile})
		wantUnresolvable(t, err, "", underFile, syscall.ENOTDIR)
	})
	t.Run("mount source", func(t *testing.T) {
		mounts := append(fine[:len(fine):len(fine)], rw(underFile, "/x"))
		err := mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tr.dc})
		wantUnresolvable(t, err, "c1", underFile, syscall.ENOTDIR)
	})
}

func TestCheckFailsThroughAnUnsearchableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; TestCheckFailsWhenAPathComponentCannotBeRead covers the same path")
	}
	tr := newTree(t)
	locked := filepath.Join(tr.base, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	// A link in a directory the daemon cannot search may lead to a
	// writable place; its target is unknown.
	link := filepath.Join(locked, "link")
	symlink(t, tr.ws, link)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	fine := []watch.Mount{rw(tr.ws, "/workspaces/ws"), ro(tr.dc, "/workspaces/ws/.devcontainer")}
	err := mountcheck.Check([]watch.Container{container("c1", fine...)}, []string{link})
	wantUnresolvable(t, err, "", link, syscall.EACCES)

	mounts := append(fine[:len(fine):len(fine)], rw(link, "/x"))
	err = mountcheck.Check([]watch.Container{container("c1", mounts...)}, []string{tr.dc})
	wantUnresolvable(t, err, "c1", link, syscall.EACCES)
}
