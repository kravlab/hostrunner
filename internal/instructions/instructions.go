// Package instructions implements `hostrunner instructions`: the text that
// tells an agent in a devcontainer how to run host commands through
// hostrun, and adding it to a project's instructions file such as
// AGENTS.md (docs/specs/instructions.md).
//
// The text is a block between two marker lines, so a later run finds the
// copy it added earlier. A marker counts only as a whole line, optionally
// ending in "\r".
package instructions

import (
	"bytes"
	_ "embed"
	"errors"
)

// The marker lines that open and close the block.
const (
	BeginMarker = "<!-- hostrunner instructions begin -->"
	EndMarker   = "<!-- hostrunner instructions end -->"
)

// ErrNoEndMarker is returned for content holding a begin marker with no end
// marker after it.
var ErrNoEndMarker = errors.New("hostrunner instructions begin marker has no end marker")

//go:embed instructions.md
var block string

// Block is the instructions block: the begin marker, the text and the end
// marker, ending in one newline.
func Block() string { return block }

// Has reports whether content holds a block. It fails with ErrNoEndMarker
// when a begin marker is not followed by an end marker.
func Has(content []byte) (bool, error) {
	spans, err := blocks(content)
	return len(spans) > 0, err
}

// Append returns content with the block added at its end, after a blank
// line when content is not empty and does not already end in one. Content
// not ending in a newline gets one first.
func Append(content []byte) []byte {
	if len(content) == 0 {
		return []byte(block)
	}
	out := bytes.Clone(content)
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	if blankLineBefore(out, len(out)) < 0 {
		out = append(out, '\n')
	}
	return append(out, block...)
}

// Replace returns content with its first block replaced by the current one
// and every later block removed, together with one blank line before it, if
// any. Content without a block is returned unchanged, as a copy. It fails
// with ErrNoEndMarker as Has does.
func Replace(content []byte) ([]byte, error) {
	spans, err := blocks(content)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return bytes.Clone(content), nil
	}
	out := append(bytes.Clone(content[:spans[0].start]), block...)
	cursor := spans[0].end
	for _, s := range spans[1:] {
		gapEnd := s.start
		if blank := blankLineBefore(content, s.start); blank >= cursor {
			gapEnd = blank
		}
		out = append(out, content[cursor:gapEnd]...)
		cursor = s.end
	}
	return append(out, content[cursor:]...), nil
}

// span is the byte range of one block in a file's content, from the start
// of its begin marker line to the end of its end marker line.
type span struct{ start, end int }

// blocks finds the blocks of content in order. A begin marker inside a
// block is part of that block's text. It fails with ErrNoEndMarker when
// the last begin marker has no end marker after it.
func blocks(content []byte) ([]span, error) {
	var spans []span
	start, off := -1, 0
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		switch {
		case start < 0 && isMarker(line, BeginMarker):
			start = off
		case start >= 0 && isMarker(line, EndMarker):
			spans = append(spans, span{start, off + len(line)})
			start = -1
		}
		off += len(line)
	}
	if start >= 0 {
		return nil, ErrNoEndMarker
	}
	return spans, nil
}

// isMarker reports whether line, with its line ending, is marker.
func isMarker(line []byte, marker string) bool {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return string(line) == marker
}

// blankLineBefore returns where the line before the line starting at
// lineStart begins when that line is blank ("\n" or "\r\n"), and -1
// otherwise.
func blankLineBefore(content []byte, lineStart int) int {
	prev := content[:lineStart]
	for _, blank := range []string{"\n", "\r\n"} {
		rest, ok := bytes.CutSuffix(prev, []byte(blank))
		if ok && (len(rest) == 0 || rest[len(rest)-1] == '\n') {
			return len(rest)
		}
	}
	return -1
}
