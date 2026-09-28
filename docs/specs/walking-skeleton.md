# Spec: Walking skeleton (client ↔ daemon)

Ticket: [Walking skeleton: client to daemon](https://github.com/kravlab/hostrunner/issues/4)

## Goal

`hostrun <cmd> [args…]` inside a devcontainer makes the host daemon run
`<cmd> [args…]` on the host, allowing everything (no rules yet). Stdio is
streamed over a Unix socket and the exit code is propagated.

## Binaries

Module `github.com/kravlab/hostrunner`, Go 1.27, stdlib only.

- `cmd/hostrun` — client. Socket path defaults to
  `/run/hostrunner/hostrunner.sock`, overridable with `HOSTRUN_SOCKET`.
- `cmd/hostrunner serve --socket <path> --workspace <host-path>
  --container-workspace <path>` — daemon. How these values are supplied is
  decided by the "Devcontainer integration" ticket; `up` is not part of
  this spec.

## Packages

- `internal/transport` — `Transport` interface (`Listen`, `Dial`) and a
  Unix socket implementation. A stale socket file is removed before
  listening, but a socket a live daemon still answers on is refused
  (`ErrAlreadyListening`); the new socket is created under umask 0177
  (mode 0600, no looser window).
- `internal/protocol` — protocol version and frames `[type:1][len:4][payload]`:
  `Request` (JSON: version, argv, cwd), `Stdin`, `StdinClose`, `Stdout`,
  `Stderr`, `Exit` (code), `Error` (code + message), `StdinCredit` (bytes).
  Stdin is flow-controlled: the daemon grants a 256 KiB window when the
  command starts and re-grants what the command consumed; exceeding the
  credit is a protocol violation. This keeps the daemon's connection reader
  from ever blocking, so a disconnect is always noticed. The version is
  checked before the rest of the request is decoded.
- `internal/workspace` — maps the container cwd to a host path. The cwd
  must equal the container workspace or lie inside it (`..` cannot escape).
  On the host the result is resolved with `EvalSymlinks` and checked again,
  so a symlink cannot lead outside the host workspace.
- `internal/daemon` — accepts connections concurrently (temporary accept
  errors such as EMFILE are retried with backoff; a permanent one stops the
  daemon and kills running commands), gives a connection 10 s to send its
  request, resolves the
  command with `exec.LookPath` against the host `PATH`, runs it with the
  host environment, and kills the child's process group if the client
  disconnects. Output still held open by a background grandchild is
  forwarded for at most 2 s after the command exits.
- `internal/client` — forwards stdin, splits stdout/stderr to its own
  streams and returns the received exit code.

## Client exit codes

| Situation | Code |
|---|---|
| Command finished | its exit code |
| Command killed by a signal | 128 + signal number |
| Command not found | 127 |
| Rejected (cwd outside workspace; later: rule denial) | 126 |
| Transport / protocol error, or `hostrun` without a command | 125 |
| Interrupted (Ctrl+C) | 130 |

Error messages go to stderr prefixed with `hostrun:`. Messages sent to the
container name container paths only; host details go to the daemon log.
A client protocol violation is answered with an `Error` (125) before the
command is killed.

## Known gaps (deferred to "Rules engine")

- The cwd is checked, then entered by path at `Start`: a container racing a
  symlink swap can escape the workspace. Irrelevant while all commands are
  allowed; close it (e.g. `os.Root` + `/proc/self/fd`) before rules gate.
- A cwd removed between the check and `Start` is reported as 127.

## Edge cases (covered by tests)

Empty argv; cwd outside the workspace; symlink escaping the workspace;
unknown command; non-executable file; large stdout; stdin piped through
`cat`; non-zero exit; termination by signal; protocol version mismatch;
client disconnect kills the command and its children, also with flooded
stdin; daemon going away mid-command; background child holding stdout;
stdin beyond credit; unexpected client frame; idle connection; temporary
and permanent accept errors; socket already served; no host paths in
errors; Ctrl+C → 130.

## Test seams

- `internal/protocol` — frame round-trip and malformed input.
- `internal/workspace` — cwd mapping.
- `internal/transport` — socket lifecycle.
- `daemon.Server.Serve` — accept loop and request timeout (fake listener).
- End-to-end: client ↔ daemon over a temporary socket.

Stdlib `testing` only.

## Checks

`gofmt`, `go vet`, `go test -race`.

## Out of scope

Rules, `hostrunner up` / lifecycle, signal forwarding, TTY.
