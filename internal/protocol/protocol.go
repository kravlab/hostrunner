// Package protocol defines the wire format between the hostrun client and
// the hostrunner daemon.
//
// A connection carries a sequence of frames, each encoded as
// [type:1][length:4, big-endian][payload:length]. A connection either arms
// the daemon (FrameArm, answered by FrameArmed or FrameError), dry-runs one
// command (FrameDryRun, answered by FrameAllowed or FrameError, the command
// never starting), or runs one
// command: the client opens with one FrameRequest, then streams FrameStdin/FrameStdinClose; the daemon streams
// FrameStdout/FrameStderr and, when it has a result to report, ends the
// exchange with one FrameExit or FrameError. If the exchange is cancelled
// (client gone, daemon shutting down) the connection just closes.
//
// Stdin is flow-controlled: the client may only send as many stdin bytes as
// the daemon has granted with FrameStdinCredit (StdinWindow up front, then
// more as the command consumes them). This keeps the daemon's reader of the
// connection from ever blocking on a command that does not read stdin, so it
// always notices a disconnect. Structured payloads are JSON.
//
// A dry run has a frame type of its own rather than a field in Request: a
// daemon that predates it rejects the unknown frame type, where it would
// ignore an unknown field and run the command.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version the client sends in Request. The daemon
// rejects requests with any other version.
const Version = 1

// StdinWindow is the stdin credit the daemon grants when the command starts,
// and so the most stdin data it ever buffers per connection.
const StdinWindow = 256 * 1024

// MaxPayload bounds a single frame's payload so a malformed or hostile peer
// cannot make the reader allocate arbitrary memory. Stream data larger than
// this is split across several frames by the sender.
const MaxPayload = 1 << 20

// FrameType identifies what a frame's payload carries.
type FrameType byte

// Frame types. The zero value is deliberately invalid.
const (
	FrameRequest     FrameType = iota + 1 // client → daemon, JSON Request
	FrameStdin                            // client → daemon, raw stdin bytes
	FrameStdinClose                       // client → daemon, stdin reached EOF
	FrameStdout                           // daemon → client, raw stdout bytes
	FrameStderr                           // daemon → client, raw stderr bytes
	FrameExit                             // daemon → client, JSON Exit
	FrameError                            // daemon → client, JSON Error
	FrameStdinCredit                      // daemon → client, JSON Credit
	FrameArm                              // `hostrunner up` → daemon, JSON Arm
	FrameArmed                            // daemon → `hostrunner up`, JSON Armed
	FrameDryRun                           // client → daemon, JSON Request
	FrameAllowed                          // daemon → client, JSON Allowed

	lastFrameType = FrameAllowed
)

// Exit codes for failures of hostrun itself rather than of the command,
// following the Docker/POSIX shell conventions so scripts can tell them apart.
// The daemon sends them in Error; the client exits with them.
const (
	ExitHostrunError = 125 // transport or protocol failure, or bad usage
	ExitRejected     = 126 // call refused (cwd outside workspace) or not executable
	ExitNotFound     = 127 // command not found on the host
)

// Errors returned by ReadFrame and WriteFrame.
var (
	ErrPayloadTooLarge  = errors.New("protocol: frame payload exceeds MaxPayload")
	ErrUnknownFrameType = errors.New("protocol: unknown frame type")
)

// Frame is one unit on the wire.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// Request asks the daemon to run Argv with the working directory Cwd, a path
// inside the container.
type Request struct {
	Version int      `json:"version"`
	Argv    []string `json:"argv"`
	Cwd     string   `json:"cwd"`
}

// Arm asks a running daemon to wait for its devcontainer (again), as
// `hostrunner up` does on every container start. ConfigDigest identifies the
// rules file `up` just validated (rules.Policy.Digest), so a daemon running
// older rules can step aside.
type Arm struct {
	Version      int    `json:"version"`
	ConfigDigest string `json:"config_digest"`
}

// Armed answers Arm. Restart means the daemon is shutting down (its rules
// are stale) and `up` must start a new one once the socket is free.
type Armed struct {
	Version int  `json:"version"`
	Restart bool `json:"restart"`
}

// Allowed answers a dry run whose command passed every check before its
// start. Rule is the command of the rule that allows it.
type Allowed struct {
	Rule string `json:"rule"`
}

// Exit reports the exit code of a command that ran.
type Exit struct {
	Code int `json:"code"`
}

// Error reports that the command could not run. Code is the exit code the
// client must terminate with; Message is shown to the user.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Credit allows the client to send Bytes more bytes of stdin.
type Credit struct {
	Bytes int `json:"bytes"`
}

const headerSize = 5

// WriteFrame encodes f to w with a single Write call, so a frame is never
// split by a failed partial write of a separate header. Concurrent callers
// must still serialize their calls.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return ErrPayloadTooLarge
	}
	buf := make([]byte, headerSize+len(f.Payload))
	buf[0] = byte(f.Type)
	binary.BigEndian.PutUint32(buf[1:headerSize], uint32(len(f.Payload)))
	copy(buf[headerSize:], f.Payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame decodes the next frame from r. It returns io.EOF when r ends
// cleanly between frames and io.ErrUnexpectedEOF when it ends mid-frame.
func ReadFrame(r io.Reader) (Frame, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	t := FrameType(header[0])
	if t < FrameRequest || t > lastFrameType {
		return Frame{}, fmt.Errorf("%w: %d", ErrUnknownFrameType, header[0])
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > MaxPayload {
		return Frame{}, ErrPayloadTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Type: t, Payload: payload}, nil
}

// WriteJSON marshals v and writes it as a frame of type t.
func WriteJSON(w io.Writer, t FrameType, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("protocol: encode %T: %w", v, err)
	}
	return WriteFrame(w, Frame{Type: t, Payload: payload})
}

// DecodeJSON unmarshals f's payload into v.
func DecodeJSON(f Frame, v any) error {
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("protocol: decode %T: %w", v, err)
	}
	return nil
}
