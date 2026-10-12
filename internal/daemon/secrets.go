package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// minSecretLength is the shortest value a secret may have: a shorter one
// could not be masked without mangling the command's output, and no real
// token is that short, so it is most likely a broken source.
const minSecretLength = 8

// obtainSecrets obtains the value of every secret, fresh for this request,
// with trailing CR and LF trimmed, into the map the injection's templates
// are rendered from (injectFiles adds to it). A command source runs, and a
// relative file source is read, in the working directory dir, whose real
// path is hostPath. A secret that yields no usable value refuses the
// request with ExitHostrunError and a message naming the secret only; the
// reason, never the value, goes to the error for the log.
func obtainSecrets(secrets []rules.Secret, dir *os.File, hostPath string) (map[string]string, *refusal) {
	values := make(map[string]string, len(secrets))
	for _, sec := range secrets {
		value, err := obtain(sec, dir, hostPath)
		if err != nil {
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is not available", sec.Name), fmt.Errorf("secret %q: %w", sec.Name, err)}
		}
		value = strings.TrimRight(value, "\r\n")
		switch n := utf8.RuneCountInString(value); {
		case n == 0:
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is not available", sec.Name), fmt.Errorf("secret %q: the value is empty", sec.Name)}
		case n < minSecretLength:
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is too short to mask", sec.Name), fmt.Errorf("secret %q: the value has %d characters, fewer than %d", sec.Name, n, minSecretLength)}
		}
		values[sec.Name] = value
	}
	return values, nil
}

// obtain reads the raw value of sec from its source.
func obtain(sec rules.Secret, dir *os.File, hostPath string) (string, error) {
	switch sec.Kind {
	case rules.SourceCommand:
		return runSource(sec, dir, hostPath)
	case rules.SourceFile:
		// The rules author decides where the file lies, the workspace
		// included, so symlinks are followed.
		path := sec.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(fdPath(dir), path)
		}
		data, err := os.ReadFile(path)
		return string(data), err
	default:
		value, ok := os.LookupEnv(sec.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", sec.Env)
		}
		return value, nil
	}
}

// runSource runs the command source of sec as the command itself would
// run (working directory, environment, own process group), with an empty
// stdin, which no source waiting for a password can get past, and its
// timeout, after which the whole group is killed. It returns the stdout.
// The stderr of a failed source is part of the error, for the log; a
// successful source's is discarded, as it may carry the value.
func runSource(sec rules.Secret, dir *os.File, hostPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sec.Command[0], sec.Command[1:]...)
	cmd.Dir = fdPath(dir)
	cmd.Env = commandEnv(hostPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return "", fmt.Errorf("command %q timed out after %s; stderr: %q", sec.Command, sec.Timeout, stderr.String())
	case err != nil:
		return "", fmt.Errorf("command %q: %w; stderr: %q", sec.Command, err, stderr.String())
	}
	return stdout.String(), nil
}

// injectFiles creates the injected files, their content rendered from
// values, and appends them to files, the descriptors the command already
// inherits (from path: open), so they get the next numbers. It adds each
// file's descriptor path to values under rules.FileRef, for the argv and
// environment templates. On error the returned files still hold every
// file created, for the caller to close.
func injectFiles(injected []rules.File, values map[string]string, files []*os.File) ([]*os.File, error) {
	for _, inj := range injected {
		f, err := injectedFile(inj.Name, inj.Content.Render(values))
		if err != nil {
			return files, fmt.Errorf("injected file %q: %w", inj.Name, err)
		}
		// ExtraFiles[i] becomes descriptor 3+i in the command.
		values[rules.FileRef(inj.Name)] = fmt.Sprintf("/proc/self/fd/%d", 3+len(files))
		files = append(files, f)
	}
	return files, nil
}

// secretValues returns the values of secrets in values, to mask.
func secretValues(secrets []rules.Secret, values map[string]string) []string {
	masked := make([]string, len(secrets))
	for i, sec := range secrets {
		masked[i] = values[sec.Name]
	}
	return masked
}

// secretNames returns the names of secrets, for the log.
func secretNames(secrets []rules.Secret) []string {
	names := make([]string, len(secrets))
	for i, sec := range secrets {
		names[i] = sec.Name
	}
	return names
}
