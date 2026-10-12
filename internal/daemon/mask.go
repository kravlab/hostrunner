package daemon

import (
	"bytes"
	"io"
	"slices"
)

// maskText replaces a secret's value in a command's output.
const maskText = "***"

// maskWriter writes a command's output to w with every exact occurrence of
// a secret's value replaced by maskText. A value split across two writes
// is still masked: the bytes that could start one are held back until the
// next write shows whether they do, or until Flush at the end of the
// command. Encoded forms of a value are not caught: masking is a safeguard
// for a program or server echoing a secret, not a boundary.
//
// A maskWriter serves one stream and is not safe for concurrent use.
type maskWriter struct {
	w       io.Writer
	secrets [][]byte // longest first, so a longer value wins over its prefix
	pending []byte   // output not yet written: the tail a value may start in
	err     error    // the first error of w; later writes fail with it
}

// newMaskWriter returns w masking the values of secrets, or w itself when
// there are none, so that output without secrets passes through as is.
func newMaskWriter(w io.Writer, values []string) io.Writer {
	if len(values) == 0 {
		return w
	}
	m := &maskWriter{w: w}
	for _, v := range values {
		m.secrets = append(m.secrets, []byte(v))
	}
	slices.SortFunc(m.secrets, func(a, b []byte) int { return len(b) - len(a) })
	return m
}

// Write masks p together with the bytes held back from earlier writes and
// writes what can no longer be the start of a value.
func (m *maskWriter) Write(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	m.pending = append(m.pending, p...)
	out, rest := m.scan(m.pending, false)
	m.pending = append(m.pending[:0], rest...)
	if len(out) > 0 {
		if _, m.err = m.w.Write(out); m.err != nil {
			return 0, m.err
		}
	}
	return len(p), nil
}

// Flush masks and writes the bytes held back, once the command has ended
// and no write can complete a value.
func (m *maskWriter) Flush() error {
	if m.err != nil || len(m.pending) == 0 {
		return m.err
	}
	out, _ := m.scan(m.pending, true)
	m.pending = m.pending[:0]
	_, m.err = m.w.Write(out)
	return m.err
}

// scan returns buf with the values replaced, up to the first byte where a
// value could still begin with bytes not seen yet, and the rest of buf
// from there. With final set, nothing more is coming, so nothing is held.
func (m *maskWriter) scan(buf []byte, final bool) (out, rest []byte) {
	for i := 0; i < len(buf); {
		if s := m.matchAt(buf[i:]); s != nil {
			out = append(out, maskText...)
			i += len(s)
			continue
		}
		if !final && m.mayStartAt(buf[i:]) {
			return out, buf[i:]
		}
		out = append(out, buf[i])
		i++
	}
	return out, nil
}

// matchAt returns the value b starts with, if any.
func (m *maskWriter) matchAt(b []byte) []byte {
	for _, s := range m.secrets {
		if bytes.HasPrefix(b, s) {
			return s
		}
	}
	return nil
}

// mayStartAt reports whether b, the end of the output so far, is the
// beginning of a value, which the next write could complete.
func (m *maskWriter) mayStartAt(b []byte) bool {
	for _, s := range m.secrets {
		if len(b) < len(s) && bytes.HasPrefix(s, b) {
			return true
		}
	}
	return false
}
