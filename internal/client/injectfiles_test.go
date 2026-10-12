package client_test

import (
	"os"
	"path/filepath"
	"testing"
)

// filesRules hands sh a config file with the secret in $TOKEN as $0, next
// to a path: open argument as $1, and a script that records both paths,
// reads the file twice and tries to write it.
const filesRules = `
rules:
  - command: sh
    secrets:
      token: {env: TOKEN}
    inject:
      command_args:
        - -c
        - |
          { printf '%s\n' "$$0" "$$1"; cat "$$0" "$$0"; } > "$$RECORD"
          printf x >> "$$0" 2>/dev/null && echo writable >> "$$RECORD"
          exit 0
        - ${file.cfg}
      files:
        cfg: "header = Bearer ${token}\n"
    flags:
      allow: []
    positional:
      allow: ["*.txt"]
      path: open
`

// The program gets the file as a descriptor after the path: open files,
// can read it as often as it likes, and cannot change it.
func TestRunHandsTheProgramAnInjectedFile(t *testing.T) {
	t.Setenv("TOKEN", testToken)
	h := startDaemon(t, policy(t, filesRules))
	if err := os.WriteFile(filepath.Join(h.hostRoot, "app.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, rec := h.recorded(t, "sh", "app.txt")
	line := "header = Bearer " + testToken + "\n"
	if want := "/proc/self/fd/4\n/proc/self/fd/3\n" + line + line; got != (result{}) || rec != want {
		t.Fatalf("got %+v, the program got %q; want %q", got, rec, want)
	}
}

// A file needs no secret, and its path can reach the program through the
// environment too.
func TestRunHandsAFileWithoutSecretsThroughTheEnvironment(t *testing.T) {
	config := "rules:\n  - command: sh\n    inject:\n      command_args: [-c, 'cat \"$$CFG\"']\n      env: {CFG: \"${file.cfg}\"}\n      files: {cfg: \"fixed content\\n\"}\n    args: none\n"
	h := startDaemon(t, policy(t, config))
	if got := h.run(t, containerRoot, "", "sh"); got != (result{stdout: "fixed content\n"}) {
		t.Fatalf("got %+v, want the file's content", got)
	}
}

// The file behaves as a regular one: it has its size, and a program can
// seek in it rather than read it from the start.
func TestRunHandsAnInjectedFileThatHasASizeAndSeeks(t *testing.T) {
	config := "rules:\n  - command: sh\n    inject:\n      command_args: [-c, 'stat -L -c %s \"$$0\"; dd if=\"$$0\" bs=1 skip=6 2>/dev/null', \"${file.cfg}\"]\n      files: {cfg: \"fixed content\\n\"}\n    args: none\n"
	h := startDaemon(t, policy(t, config))
	if got := h.run(t, containerRoot, "", "sh"); got != (result{stdout: "14\ncontent\n"}) {
		t.Fatalf("got %+v, want the size and the content after the first 6 bytes", got)
	}
}
