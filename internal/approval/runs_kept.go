package approval

import (
	"bytes"
	"cmp"
	"math"
	"slices"
)

// Limits of what a session keeps of its runs' output, in its memory
// only, for out (see keptOutput): the last keptBytes of each of the last
// keptRuns runs.
const (
	keptRuns  = 20
	keptBytes = 64 << 10
)

// outputLines puts a run's output together in lines as it arrives: the
// lines each stream got, and the last of them on both (tail).
type outputLines struct {
	stdout, stderr streamLines
	tail           tail
}

// write takes data, a chunk of stderr or of stdout: it counts its lines,
// continues the stream's line not yet complete, if any, and starts a line
// of the tail at each line's first byte.
func (o *outputLines) write(stderr bool, data []byte) {
	stream := &o.stdout
	if stderr {
		stream = &o.stderr
	}
	for len(data) > 0 {
		if stream.open == nil {
			stream.open = o.tail.start(stderr)
		}
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			stream.open.Write(data)
			return
		}
		stream.open.Write(data[:end])
		stream.open.End()
		stream.open = nil
		stream.lines++
		data = data[end+1:]
	}
}

// outputKept reports whether the session keeps r's output for out: from
// the run's start until it is no longer one of the last runs (see
// term.keep).
func (r *sessionRun) outputKept() bool { return r.output != nil }

// dropOutput drops the output kept of r, no longer one of the last runs.
func (r *sessionRun) dropOutput() { r.output = nil }

// keep keeps run r, just numbered, for out and runs, in the order of the
// runs' numbers: a run numbered as its answer was confirmed may start
// after a later one. Of more than keptRuns, the one numbered first is no
// longer kept, its output dropped; out says so of every number up to it
// (droppedThrough).
func (t *term) keep(r *sessionRun) {
	i, _ := t.findKept(r.n)
	t.kept = slices.Insert(t.kept, i, r)
	if len(t.kept) > keptRuns {
		t.droppedThrough = max(t.droppedThrough, t.kept[0].n)
		t.kept[0].dropOutput()
		t.kept = slices.Delete(t.kept, 0, 1)
	}
}

// findKept returns where run n is among the runs kept, and whether it is
// there; where it would be when it is not.
func (t *term) findKept(n uint64) (int, bool) {
	return slices.BinarySearchFunc(t.kept, n, func(k *sessionRun, n uint64) int { return cmp.Compare(k.n, n) })
}

// keptOutput is the last keptBytes of a run's output, as it arrived, in
// segments of one stream each, and what was dropped before them: how
// many bytes, and, for each stream, whether its line kept first lost its
// start (cut), so that the rest of that line is not shown (see lines).
type keptOutput struct {
	segments []segment
	size     int // bytes in segments
	dropped  int
	cut      lineCut
}

// segment is output of one stream, stderr or stdout, that arrived with
// none of the other's in between.
type segment struct {
	stderr bool
	data   []byte
}

// lineCut is, for each stream, whether the last of its bytes dropped is
// not a newline: the line it is part of lost its start.
type lineCut struct{ stdout, stderr bool }

// of returns the stream's flag, stderr's or stdout's.
func (c *lineCut) of(stderr bool) *bool {
	if stderr {
		return &c.stderr
	}
	return &c.stdout
}

// write adds data, a chunk of stderr or of stdout, dropping the oldest
// bytes beyond keptBytes. A segment cut at its start is copied, so the
// bytes dropped are released.
func (o *keptOutput) write(stderr bool, data []byte) {
	if len(data) == 0 {
		return
	}
	if last := len(o.segments) - 1; last >= 0 && o.segments[last].stderr == stderr {
		o.segments[last].data = append(o.segments[last].data, data...)
	} else {
		o.segments = append(o.segments, segment{stderr: stderr, data: slices.Clone(data)})
	}
	o.size += len(data)
	for over := o.size - keptBytes; over > 0; over = o.size - keptBytes {
		first := &o.segments[0]
		n := min(over, len(first.data))
		*o.cut.of(first.stderr) = first.data[n-1] != '\n'
		if n == len(first.data) {
			o.segments[0] = segment{}
			o.segments = o.segments[1:]
		} else {
			first.data = slices.Clone(first.data[n:])
		}
		o.size -= n
		o.dropped += n
	}
}

// lines returns the output kept as whole lines, of both streams in the
// order their first bytes arrived, and the bytes dropped before them: a
// stream's first line, when its start was dropped, is dropped too, up to
// its newline, and counted.
func (o *keptOutput) lines() ([]tailLine, int) {
	all := outputLines{tail: tail{max: math.MaxInt, whole: true}}
	dropped, cut := o.dropped, o.cut
	for _, seg := range o.segments {
		data := seg.data
		if skip := cut.of(seg.stderr); *skip {
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				dropped += len(data)
				continue
			}
			dropped += end + 1
			*skip = false
			data = data[end+1:]
		}
		all.write(seg.stderr, data)
	}
	return all.tail.lines, dropped
}
