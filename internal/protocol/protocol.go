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
//
// A connection opened with FrameAsk runs a command as FrameRequest does
// when the rules allow it. When they refuse it but a human may approve it,
// the daemon first sends FrameWaiting and then, on an approval,
// FrameApproved and the ordinary run stream, or FrameError on a refusal;
// the client sends no stdin until the command has started.
//
// The approval socket speaks its own frames (FrameHello onwards), with its
// own version (ApprovalVersion): a session opens with FrameHello, the
// daemon answers FrameDaemonInfo; FrameServe makes the session the one
// that serves the daemon, which then sends it each FrameApprovalRequest
// and takes its FrameAnswer, confirmed by FrameAnswered, or tells it with
// FrameWithdrawn that a request ended unanswered. FrameStanding and
// FrameRevoke list and revoke the standing approvals; FrameReread makes
// the daemon reread its rules file once the session has added a rule. A
// session that takes the daemon over from another (Serve.TakeOver) gets
// its waiting requests, and the other FrameTakenOver.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
	FrameAsk                              // client → daemon, JSON Ask
	FrameWaiting                          // daemon → client, JSON Waiting
	FrameApproved                         // daemon → client, JSON Approved

	// Frames of the approval socket, between an approval session and the
	// daemon; the container's socket refuses them, and the approval socket
	// refuses every other frame.
	FrameHello           // session → daemon, JSON Hello
	FrameDaemonInfo      // daemon → session, JSON DaemonInfo
	FrameServe           // session → daemon, JSON Serve (or no payload): serve the daemon
	FrameServing         // daemon → session, no payload: the session serves it
	FrameApprovalRequest // daemon → session, JSON ApprovalRequest
	FrameAnswer          // session → daemon, JSON Answer
	FrameAnswered        // daemon → session, JSON Answered
	FrameWithdrawn       // daemon → session, JSON Withdrawn
	FrameStanding        // session → daemon, no payload: list the standing approvals
	FrameStandingList    // daemon → session, JSON StandingList
	FrameRevoke          // session → daemon, JSON Revoke
	FrameRevoked         // daemon → session, JSON Revoked
	FrameReread          // session → daemon, JSON Reread
	FrameRereadResult    // daemon → session, JSON RereadResult
	FrameTakenOver       // daemon → session, no payload: another session took over

	lastFrameType = FrameTakenOver
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

// Armed answers Arm. Restart means the daemon steps aside: it will not
// follow this arm (its rules are stale, or its watch has ended) and is
// stopping, so `up` must start a new one once the socket is free.
type Armed struct {
	Version int  `json:"version"`
	Restart bool `json:"restart"`
}

// Allowed answers a dry run whose command passed every check before its
// start. Rule is the command of the rule that allows it; Dir is the rule's
// fixed directory, the command's working directory, and is omitted when
// the command would run in the mirrored directory. Secrets are the names
// of the secrets the rule would inject, omitted when there are none; a
// client that predates them ignores the field. Standing is set when a
// standing approval lets the command through, which runs it as the rule
// that refused it would (Dir, Secrets).
type Allowed struct {
	Rule     string   `json:"rule"`
	Dir      string   `json:"dir,omitempty"`
	Secrets  []string `json:"secrets,omitempty"`
	Standing bool     `json:"standing,omitempty"`
}

// String describes a, as the dry run and the Rules test report it.
func (a Allowed) String() string {
	s := fmt.Sprintf("allowed by rule %q", a.Rule)
	if a.Standing {
		s = "allowed by a standing approval"
	}
	if a.Dir != "" {
		s += ", runs in " + a.Dir
	}
	if len(a.Secrets) > 0 {
		s += ", injects secrets: " + strings.Join(a.Secrets, ", ")
	}
	return s
}

// Ask asks the daemon to run a command as Request does and, if the rules
// refuse it, to ask an approval session for an approval. TimeoutMs bounds
// how long it waits for an answer, in milliseconds; 0 is the daemon's
// default. It has a frame type of its own for the reason a dry run has: a
// daemon that predates it rejects the frame type instead of running the
// command.
type Ask struct {
	Request
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
}

// Waiting tells the client that its command waits for a human's answer.
// Reasons are every reason it was refused, the first naming the rule that
// refused it, as a refusal does; Guarded is set when approving it needs a
// second confirmation.
type Waiting struct {
	Reasons []string `json:"reasons"`
	Guarded bool     `json:"guarded,omitempty"`
}

// String is the line the client prints while its command waits.
func (w Waiting) String() string {
	s := "waiting for approval on the host"
	if w.Guarded {
		s += " (guarded)"
	}
	return s + ": " + strings.Join(w.Reasons, "; ")
}

// Scope is how far an approval reaches.
type Scope string

// Scopes of an approval.
const (
	ScopeOnce     Scope = "once"     // this request only
	ScopeLifetime Scope = "lifetime" // a standing approval: the daemon's lifetime
	ScopeRule     Scope = "rule"     // a rule added to the rules file
)

// Approved tells the client that a human approved its command, which the
// ordinary run stream (FrameStdinCredit, output, FrameExit) then follows.
type Approved struct {
	Scope Scope `json:"scope"`
}

// String is the line the client prints before the approved command runs.
func (a Approved) String() string {
	reach := string(a.Scope)
	switch a.Scope {
	case ScopeOnce:
		reach = "this request only"
	case ScopeLifetime:
		reach = "for the daemon's lifetime"
	case ScopeRule:
		reach = "rule added"
	}
	return "approved on the host: " + reach
}

// ApprovalSuffix makes a runtime directory's path into its daemon's
// approval socket: it lies beside the runtime directory, which the
// container mounts, not in it.
const ApprovalSuffix = ".approval.sock"

// ApprovalVersion is the version of the approval socket's protocol, which
// the session and the daemon exchange in Hello and DaemonInfo. A session
// serves only a daemon of its own version.
const ApprovalVersion = 1

// Serve asks the daemon to be served by the session; TakeOver replaces
// the session that serves it, which is told so (FrameTakenOver) and whose
// waiting requests pass to the new one with the rest of their limit.
type Serve struct {
	TakeOver bool `json:"take_over,omitempty"`
}

// ServedRefusal is the message of the error a daemon refuses Serve with
// when another session serves it and Serve does not take it over.
const ServedRefusal = "another approval session serves this daemon"

// Hello opens a session's connection to an approval socket.
type Hello struct {
	Version int `json:"version"`
}

// DaemonInfo answers Hello: the daemon's approval protocol version, its
// workspace on the host and in the container, the rules file it loaded
// and the digest of the rules it serves (empty when it never rereads
// them), and whether a session serves it.
type DaemonInfo struct {
	Version            int    `json:"version"`
	Workspace          string `json:"workspace"`
	ContainerWorkspace string `json:"container_workspace"`
	RulesFile          string `json:"rules_file"`
	RulesDigest        string `json:"rules_digest,omitempty"`
	Served             bool   `json:"served,omitempty"`
}

// ApprovalRequest is a command waiting for the serving session's answer.
// ID names it in the Answer; Cwd is the container directory; Reasons are
// as in Waiting, each also naming the kind of list that gave it where that
// matters; Guards are the guarded reasons, one line each ("explicitly
// denied: …"), empty unless approving it needs a second confirmation;
// TimeoutMs is the time left to answer it, in milliseconds.
type ApprovalRequest struct {
	ID        uint64   `json:"id"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	Reasons   []string `json:"reasons"`
	Guards    []string `json:"guards,omitempty"`
	TimeoutMs int64    `json:"timeout_ms"`
}

// Answer is the human's answer to the approval request ID: an approval of
// Scope, or a refusal when Scope is empty, with Note for the agent.
type Answer struct {
	ID    uint64 `json:"id"`
	Scope Scope  `json:"scope,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Answered tells the session whether the daemon took its Answer to ID;
// Error says why not (the request was answered, withdrawn or expired).
// Ended is the Withdrawn reason of a request that ended as the answer came,
// when the daemon has not finished telling the session of it, so the
// session reports the end whichever frame comes first.
type Answered struct {
	ID    uint64 `json:"id"`
	Error string `json:"error,omitempty"`
	Ended string `json:"ended,omitempty"`
}

// StandingList answers FrameStanding with the daemon's standing approvals,
// oldest first, or Error, why they are not listed.
type StandingList struct {
	Approvals []Standing `json:"approvals"`
	Error     string     `json:"error,omitempty"`
}

// Standing is a standing approval: N, its number, lets Argv through from
// the container directory Cwd; Guarded says it was granted for a guarded
// refusal.
type Standing struct {
	N       uint64   `json:"n"`
	Argv    []string `json:"argv"`
	Cwd     string   `json:"cwd"`
	Guarded bool     `json:"guarded,omitempty"`
}

// Revoke asks the daemon to revoke standing approval N.
type Revoke struct {
	N uint64 `json:"n"`
}

// Revoked answers Revoke; Error says why N was not revoked.
type Revoked struct {
	N     uint64 `json:"n"`
	Error string `json:"error,omitempty"`
}

// Reread asks the daemon to reread its rules file, into which the session
// has written Rule, the command of a rule it added or widened (Edited: by
// the human, in an editor), leaving the file with Digest; ID is the
// request the rule was added for. Without Rule, the file was changed by
// hand, and the human agreed to have it reread before a rule is proposed;
// Digest is that of the file the session read. The daemon keeps its rules
// when the file it rereads has another digest: an edit nobody reviewed
// waits for the next start.
type Reread struct {
	Rule   string `json:"rule,omitempty"`
	Edited bool   `json:"edited,omitempty"`
	Digest string `json:"digest"`
	ID     uint64 `json:"id,omitempty"`
}

// RereadResult answers Reread: Digest identifies the rules the daemon now
// serves, or Error says why it kept its old ones. RunsOnce says why the
// daemon's checks still refuse request ID under the new rules (a rule
// edited by hand that a later check refuses), so it runs approved once.
type RereadResult struct {
	Digest   string `json:"digest,omitempty"`
	Error    string `json:"error,omitempty"`
	RunsOnce string `json:"runs_once,omitempty"`
}

// Withdrawn tells the session that request ID ended unanswered, for
// Reason: WithdrawnByClient or WithdrawnExpired. It takes no answer any
// more.
type Withdrawn struct {
	ID     uint64 `json:"id"`
	Reason string `json:"reason"`
}

// Reasons a request ends unanswered, as Withdrawn and the daemon log give
// them.
const (
	WithdrawnByClient = "withdrawn" // the client went away
	WithdrawnExpired  = "expired"   // its limit ran out
)

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
