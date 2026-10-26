package instructions

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Outcome is what AppendFile did to the file.
type Outcome int

// The outcomes of AppendFile.
const (
	Created   Outcome = iota + 1 // the file did not exist and holds the block alone
	Appended                     // the block was added at the end of the file
	Replaced                     // the file's blocks were replaced by one current block
	Unchanged                    // the human declined; nothing was written
)

// Prompt is where AppendFile asks its questions and reads the answers.
type Prompt struct {
	In       io.Reader // the answers, one line each
	Out      io.Writer // the questions
	Terminal bool      // whether In is a terminal; a question is asked only then
}

// AppendFile adds the block to the file at path, as `hostrunner
// instructions --append` does. A missing file is created after a yes, a
// file without a block gets one, and for a file holding a block the human
// chooses to replace it, append another copy or cancel. Without a
// terminal, a question is refused with an error instead. After an answer
// the file is read again, so an edit made while the question was open is
// kept. The file is written in place, never renamed over, so a symlink is
// followed and a bind mount sees the change; a write that fails midway may
// leave the file partly written. The outcome is 0 whenever err is not nil.
func AppendFile(path string, p Prompt) (Outcome, error) {
	answers := bufio.NewReader(p.In)
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createAfterYes(path, p, answers)
	}
	if err != nil {
		return 0, err
	}
	has, err := hasBlock(path, content)
	if err != nil {
		return 0, err
	}
	if !has {
		return update(path, false)
	}
	if !p.Terminal {
		return 0, fmt.Errorf("%s already has hostrunner instructions; run in a terminal to choose", path)
	}
	for {
		answer, err := ask(answers, p.Out, path+" already has hostrunner instructions: [r]eplace, [a]ppend, [c]ancel? ")
		if err != nil {
			return 0, err
		}
		switch answer {
		case "r", "replace":
			return update(path, true)
		case "a", "append":
			return update(path, false)
		case "c", "cancel", "":
			return Unchanged, nil
		}
	}
}

// createAfterYes creates the missing file at path once the human agrees.
// A missing parent directory and a symlink to a missing file are refused
// first, since no answer could make the file creatable.
func createAfterYes(path string, p Prompt, answers *bufio.Reader) (Outcome, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return 0, fmt.Errorf("%s is a symlink to a missing file", path)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return 0, err
	}
	if !p.Terminal {
		return 0, fmt.Errorf("%s does not exist; run in a terminal to create it", path)
	}
	answer, err := ask(answers, p.Out, path+" does not exist; create it? [y/N] ")
	if err != nil {
		return 0, err
	}
	if answer != "y" && answer != "yes" {
		return Unchanged, nil
	}
	if err := create(path); err != nil {
		return 0, err
	}
	return Created, nil
}

// update reads the file at path and writes the block into it: replacing
// its blocks when replace is set and it holds one, appending otherwise.
func update(path string, replace bool) (Outcome, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	has, err := hasBlock(path, content)
	if err != nil {
		return 0, err
	}
	if !replace || !has {
		if err := appendTo(path, content); err != nil {
			return 0, err
		}
		return Appended, nil
	}
	replaced, err := Replace(content)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if err := overwrite(path, replaced); err != nil {
		return 0, err
	}
	return Replaced, nil
}

// hasBlock is Has for the content of the file at path, naming the file in
// its error.
func hasBlock(path string, content []byte) (bool, error) {
	has, err := Has(content)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return has, nil
}

// ask writes question to out and reads one line from answers, trimmed and
// in lower case. End of input reads as an empty line, the default answer,
// so a question is never repeated past it.
func ask(answers *bufio.Reader, out io.Writer, question string) (string, error) {
	if _, err := io.WriteString(out, question); err != nil {
		return "", err
	}
	line, err := answers.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(line)), nil
}

// create makes the file at path holding the block alone; it fails if the
// file appeared since it was found missing.
func create(path string) error {
	return write(path, os.O_CREATE|os.O_EXCL, []byte(block))
}

// appendTo adds the block to the file at path, whose content was read as
// content, writing only the bytes Append adds.
func appendTo(path string, content []byte) error {
	return write(path, os.O_APPEND, Append(content)[len(content):])
}

// overwrite replaces the content of the file at path with content, in
// place.
func overwrite(path string, content []byte) error {
	return write(path, os.O_TRUNC, content)
}

// write opens the file at path for writing with the extra flags, writes
// data and closes it, reporting a failed close too.
func write(path string, flags int, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
