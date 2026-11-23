package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Environment is the environment a daemon runs its commands and secret
// sources with, as NAME=value entries in os.Environ's form. A daemon given
// one (see WithEnvironment) reads no other: not the process's, which it may
// share with other daemons.
type Environment struct {
	vars    []string
	process bool // the process's, read whenever it is used; vars is unused
}

// processEnvironment is what a Server runs with unless WithEnvironment
// gives it another: the environment of its process, as it is when a command
// starts, which is the daemon's own where it is alone in its process.
var processEnvironment = &Environment{process: true}

// NewEnvironment returns an Environment of vars, which it copies.
func NewEnvironment(vars []string) *Environment {
	return &Environment{vars: slices.Clone(vars)}
}

// Entries returns the NAME=value entries of e, in a slice of its own that
// the caller may change.
func (e *Environment) Entries() []string {
	if e.process {
		return os.Environ()
	}
	return slices.Clone(e.vars)
}

// with returns the entries of e with the NAME=value entries of extra over
// them: an entry of e that extra names again is replaced by it.
func (e *Environment) with(extra ...string) []string {
	return withEntries(e.Entries(), extra...)
}

// lookup returns the value of the variable name and whether e sets it. As
// in a command's environment, the last entry of a name wins.
func (e *Environment) lookup(name string) (string, bool) {
	for _, kv := range slices.Backward(e.Entries()) {
		if value, ok := strings.CutPrefix(kv, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

// Getenv returns the value of the variable name in e, empty when e does
// not set it, as os.Getenv does in the process's environment: the getenv
// a daemon resolves its settings with.
func (e *Environment) Getenv(name string) string {
	value, _ := e.lookup(name)
	return value
}

// LookPath returns the path of the executable file the program name stands
// for, as exec.LookPath does but in the PATH of e, not of the process: the
// first executable file of that name in the directories of PATH. Like
// exec.LookPath, it fails with exec.ErrNotFound if there is none, and with
// exec.ErrDot for one found in a directory PATH names relatively, which
// would be the daemon's working directory, not the command's.
func (e *Environment) LookPath(name string) (string, error) {
	path, _ := e.lookup("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if checkExecutable(candidate) != nil {
			continue
		}
		if !filepath.IsAbs(candidate) {
			return "", &exec.Error{Name: name, Err: exec.ErrDot}
		}
		return candidate, nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// command returns the command that runs argv under ctx, with a program
// that is a name, not a path, looked up in the PATH of e, which os/exec
// would look up in the process's PATH instead. The program is still the
// name to the command, and a failed lookup is what starting it fails with.
// The caller sets its directory, environment and files.
func (e *Environment) command(ctx context.Context, argv []string) *exec.Cmd {
	program, lookupErr := argv[0], error(nil)
	if !strings.Contains(program, "/") {
		program, lookupErr = e.LookPath(program)
		if lookupErr != nil {
			program = argv[0]
		}
	}
	cmd := exec.CommandContext(ctx, program, argv[1:]...)
	cmd.Args[0] = argv[0]
	if lookupErr != nil {
		cmd.Err = lookupErr
	}
	return cmd
}

// checkExecutable returns the error that starting path would fail with, as
// far as it can be told without starting it: path must be a file, not a
// directory (execve refuses it with EACCES), that its process may execute.
func checkExecutable(path string) error {
	if err := syscall.Access(path, xOK); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err != nil {
		return err
	} else if fi.IsDir() {
		return syscall.EACCES
	}
	return nil
}

// withEntries returns a copy of env without the entries that set the names
// of set, followed by set.
func withEntries(env []string, set ...string) []string {
	kept := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return slices.ContainsFunc(set, func(e string) bool {
			name, _, _ := strings.Cut(e, "=")
			return strings.HasPrefix(kv, name+"=")
		})
	})
	return append(kept, set...)
}
