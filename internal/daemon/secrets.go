package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// minSecretLength is the shortest value a secret may have: a shorter one
// could not be masked without mangling the command's output, and no real
// token is that short, so it is most likely a broken source.
const minSecretLength = 8

// errSourceTimeout is the cause of a command source's context ending
// because the source ran out of time, as opposed to the context it runs
// under ending, with a deadline of its own or not.
var errSourceTimeout = errors.New("the secret source timed out")

// secretSources is where the secrets of one request are obtained from: a
// command source runs, and a relative file source is read, in the working
// directory dir, whose real path is hostPath. An env source is read from
// env, which also gives a command source its environment and looks its
// program up.
type secretSources struct {
	env      *Environment
	dir      *os.File
	hostPath string
}

// obtain obtains the value of every secret, fresh for this request, with
// trailing CR and LF trimmed, into a new map, which the caller owns, that
// the injection's templates are rendered from (injectFiles adds to it, and
// so does prepareAllowed). A secret that yields no usable value refuses
// the request with ExitHostrunError and a message naming the secret only;
// the reason, never the value, goes to the error for the log. Ending ctx
// kills a command source that runs.
func (src secretSources) obtain(ctx context.Context, secrets []rules.Secret) (map[rules.Ref]string, *refusal) {
	values := make(map[rules.Ref]string, len(secrets))
	for _, sec := range secrets {
		value, err := src.read(ctx, sec)
		if err != nil {
			return nil, unavailable(sec, err)
		}
		value = strings.TrimRight(value, "\r\n")
		switch n := utf8.RuneCountInString(value); {
		case n == 0:
			return nil, unavailable(sec, errors.New("the value is empty"))
		case n < minSecretLength:
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is too short to mask", sec.Name), fmt.Errorf("secret %q: the value has %d characters, fewer than %d", sec.Name, n, minSecretLength)}
		}
		values[rules.SecretRef(sec.Name)] = value
	}
	return values, nil
}

// unavailable is the refusal for a secret without a usable value: the
// message names the secret only, and err, the reason, goes to the log.
func unavailable(sec rules.Secret, err error) *refusal {
	return &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is not available", sec.Name), fmt.Errorf("secret %q: %w", sec.Name, err)}
}

// check checks, for a dry run, what obtain would need without obtaining
// anything: a command source's program is found and executable in the
// working directory, as starting it would look it up, but it is not run; an
// env source's variable is set; a file source opens for reading. A failed
// check refuses as obtaining the value would.
func (src secretSources) check(secrets []rules.Secret) *refusal {
	for _, sec := range secrets {
		var err error
		switch sec.Kind {
		case rules.SourceCommand:
			err = lookupProgram(src.env, sec.Command[0], src.dir)
		case rules.SourceFile:
			var f *os.File
			if f, err = os.Open(src.filePath(sec)); err == nil {
				f.Close()
			}
		default:
			if _, ok := src.env.lookup(sec.Env); !ok {
				err = fmt.Errorf("environment variable %s is not set", sec.Env)
			}
		}
		if err != nil {
			return unavailable(sec, err)
		}
	}
	return nil
}

// filePath is the path a file source is read from: as written when
// absolute, from the working directory otherwise. The rules author decides
// where the file lies, the workspace included, so symlinks are followed.
func (src secretSources) filePath(sec rules.Secret) string {
	if filepath.IsAbs(sec.File) {
		return sec.File
	}
	return filepath.Join(fdPath(src.dir), sec.File)
}

// read reads the raw value of sec from its source.
func (src secretSources) read(ctx context.Context, sec rules.Secret) (string, error) {
	switch sec.Kind {
	case rules.SourceCommand:
		return src.run(ctx, sec)
	case rules.SourceFile:
		data, err := os.ReadFile(src.filePath(sec))
		return string(data), err
	default:
		value, ok := src.env.lookup(sec.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", sec.Env)
		}
		return value, nil
	}
}

// run runs the command source of sec as the command itself would run
// (working directory, environment, own process group), with an empty
// stdin, which no source waiting for a password can get past, and its
// timeout, after which the whole group is killed, as it is when ctx ends.
// It returns the stdout.
// The stderr of a failed source is part of the error, for the log; a
// successful source's is discarded, as it may carry the value.
func (src secretSources) run(ctx context.Context, sec rules.Secret) (string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, sec.Timeout, errSourceTimeout)
	defer cancel()
	cmd := inDirectory(src.env.command(ctx, sec.Command), src.dir, src.hostPath, src.env.with())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	switch {
	case errors.Is(context.Cause(ctx), errSourceTimeout):
		return "", fmt.Errorf("command %q timed out after %s; stderr: %q", sec.Command, sec.Timeout, stderr.String())
	case ctx.Err() != nil:
		return "", fmt.Errorf("command %q cancelled; stderr: %q", sec.Command, stderr.String())
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
func injectFiles(injected []rules.File, values map[rules.Ref]string, files []*os.File) ([]*os.File, error) {
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
func secretValues(secrets []rules.Secret, values map[rules.Ref]string) []string {
	masked := make([]string, len(secrets))
	for i, sec := range secrets {
		masked[i] = values[rules.SecretRef(sec.Name)]
	}
	return masked
}
