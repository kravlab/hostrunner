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
// lines each stream got, indexed by stream, and the last of them on both
// (tail).
type outputLines struct {
	streams [2]streamLines
	tail    tail
}

// write takes data, a chunk of stream s: it counts its lines, continues
// the stream's line not yet complete, if any, and starts a line of the
// tail at each line's first byte.
func (o *outputLines) write(s stream, data []byte) {
	got := &o.streams[s]
	for len(data) > 0 {
		if got.open == nil {
			got.open = o.tail.start(s)
		}
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			got.open.Write(data)
			return
		}
		got.open.Write(data[:end])
		got.open.End()
		got.open = nil
		got.lines++
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
// many bytes, and, for each stream, indexed by it, whether the last of
// its bytes dropped is not a newline (cut): its line kept first lost its
// start, so that the rest of that line is not shown (see lines).
type keptOutput struct {
	segments []segment
	size     int // bytes in segments
	dropped  int
	cut      [2]bool
}

// segment is output of one stream that arrived with none of the other's
// in between.
type segment struct {
	stream stream
	data   []byte
}

// write adds data, a chunk of stream s, dropping the oldest bytes beyond
// keptBytes. A segment cut at its start is copied, so the bytes dropped
// are released.
func (o *keptOutput) write(s stream, data []byte) {
	if len(data) == 0 {
		return
	}
	if last := len(o.segments) - 1; last >= 0 && o.segments[last].stream == s {
		o.segments[last].data = append(o.segments[last].data, data...)
	} else {
		o.segments = append(o.segments, segment{stream: s, data: slices.Clone(data)})
	}
	o.size += len(data)
	for over := o.size - keptBytes; over > 0; over = o.size - keptBytes {
		first := &o.segments[0]
		n := min(over, len(first.data))
		o.cut[first.stream] = first.data[n-1] != '\n'
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
		if cut[seg.stream] {
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				dropped += len(data)
				continue
			}
			dropped += end + 1
			cut[seg.stream] = false
			data = data[end+1:]
		}
		all.write(seg.stream, data)
	}
	return all.tail.lines, dropped
}
