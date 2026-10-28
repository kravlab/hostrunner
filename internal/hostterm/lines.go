package hostterm

import (
	"bytes"
	"io"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Line is a line the human typed, its number, counting from 1, and
// whether it is the last of what one read returned.
type Line struct {
	N    uint64
	Text string
	Last bool
}

// StopReading stops ReadLines reading in, closing done: a read it waits
// in would otherwise take the next line typed after the reader is done.
// An in that takes a read deadline (a file the process opened, a pipe
// from os.Pipe) has the read cut short, and once ReadLines has ended,
// lines closed, the deadline is lifted again, a deadline set before
// ReadLines too. Any other (an inherited stdin, terminal or pipe, which
// Go cannot interrupt; the process leaves soon after) is left to its
// read.
func StopReading(in io.Reader, done chan struct{}, lines <-chan Line) {
	close(done)
	d, ok := in.(interface{ SetReadDeadline(time.Time) error })
	if !ok || d.SetReadDeadline(time.Now()) != nil {
		return
	}
	for range lines {
	}
	_ = d.SetReadDeadline(time.Time{})
}

// ReadLines reads lines from in into the returned channel, counting each
// with read as soon as it is read, and closes the channel when in ends or
// done is closed. It reads in directly, without a buffered reader, and
// counts every line of a read before handing any on: a line is counted
// when it is typed, not when the reader gets to it, so a prompt can tell
// the lines typed before it. Only a read returning between the prompt's
// count and its own counting escapes that.
//
// It reads only once want lets it, after the lines of the last read are
// handled: an editor the reader runs meanwhile has the terminal to
// itself.
func ReadLines(in io.Reader, read *atomic.Uint64, want <-chan struct{}, done <-chan struct{}) <-chan Line {
	ch := make(chan Line)
	go func() {
		defer close(ch)
		var partial []byte
		buf := make([]byte, 4096)
		for {
			select {
			case <-want:
			case <-done:
				return
			}
			var lines []Line
			var err error
			for len(lines) == 0 && err == nil {
				var n int
				n, err = in.Read(buf)
				partial = append(partial, buf[:n]...)
				for {
					i := bytes.IndexByte(partial, '\n')
					if i < 0 {
						break
					}
					lines = append(lines, Line{N: read.Add(1), Text: string(partial[:i])})
					partial = partial[i+1:]
				}
			}
			if len(lines) > 0 {
				lines[len(lines)-1].Last = true
			}
			for _, l := range lines {
				select {
				case ch <- l:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// DiscardTypedAhead drops the input a terminal holds that nobody has read
// yet. Input that is not a terminal (a pipe, a file) is left alone. The
// descriptor is reached through SyscallConn, not Fd, which would put a
// pipe into blocking mode, where a read deadline no longer cuts a read
// short (see StopReading).
func DiscardTypedAhead(in io.Reader) {
	f, ok := in.(*os.File)
	if !ok {
		return
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		if _, err := unix.IoctlGetTermios(int(fd), unix.TCGETS); err == nil {
			_ = unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH)
		}
	})
}
