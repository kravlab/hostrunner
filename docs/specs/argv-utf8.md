# Spec: Arguments that are not valid UTF-8

Ticket: [hostrun: argv bytes that are not UTF-8 reach the host as
U+FFFD](https://github.com/kravlab/hostrunner/issues/101).
Vocabulary in [CONTEXT.md](../../CONTEXT.md).

## Goal

The host runs exactly the argv the container gave, or nothing.

Today a request is JSON, and Go's encoding/json replaces bytes that are
not valid UTF-8 with U+FFFD, both when the client encodes the request
and when the daemon decodes it. An argument holding such bytes (a
Latin-1 file name, binary data) reaches the host changed: the command
runs with arguments the container did not give, the rules check the
changed argv, and an approval session shows and approves it. The same
holds for the container directory the request carries.

hostrun now refuses such a request instead, in the client before it
connects and again in the daemon before it decodes the request. The
protocol does not change: arguments and the current directory stay
text, and the rules, the path checks, standing and session approvals,
and the rules a session adds keep seeing only valid UTF-8.

## User stories

1. As an agent in the container, I want hostrun to refuse an argument
   that is not valid UTF-8, so that a command never runs on the host
   with arguments I did not give.
2. As an agent, I want the refusal to name each such argument by its
   number, so that I know which one to fix.
3. As an agent, I want the refusal to show each such argument with its
   bad bytes escaped, so that I see where they are and my terminal is
   not fed raw bytes.
4. As an agent, I want a long argument (binary data) cut in the
   refusal, so that a large argument does not flood my output.
5. As an agent, I want every bad argument listed at once, so that I fix
   them all in one go.
6. As an agent, I want hostrun to refuse a current directory that is
   not valid UTF-8, so that a command never runs in another host
   directory than the one I am in.
7. As an agent, I want the refusal to exit 125, as other hostrun
   failures do, so that I do not try `--ask` for something no approval
   can let through.
8. As an agent, I want `--dry-run` and `--ask` to refuse such a request
   as a run does, so that a dry run predicts the run and an approval is
   never asked for a changed argv.
9. As an agent, I want the refusal to come before hostrun connects to
   the daemon, so that nothing reaches the host, the daemon's log
   included.
10. As a human answering approval requests, I want an approval request
    never to show an argv other than the one the container gave, so
    that I approve what will run.
11. As a host owner, I want the daemon to refuse a request whose JSON
    holds bytes that are not valid UTF-8, so that a client other than
    hostrun cannot get a changed argv run either.
12. As a host owner, I want the daemon to refuse a request whose JSON
    holds an escaped UTF-16 surrogate that is not part of a pair
    (`\ud800`), which encoding/json also turns into U+FFFD, for the
    same reason.
13. As a host owner, I want an argument that is valid UTF-8 and holds
    U+FFFD itself, or an escaped surrogate pair, to run as before, so
    that the check refuses only what would change.
14. As a host owner, I want the daemon's refusal logged as other
    malformed requests are, so that I can see it was refused.
15. As a rule author, I want the rules to keep matching argv as text,
    so that globs and regexes need no rules for bytes.
16. As a user reading the README, I want it to say that arguments and
    the current directory must be valid UTF-8, and how to pass bytes
    instead, so that I know the limit before I meet it.
17. As an agent reading the hostrun section of AGENTS.md, I want exit
    125 described as covering this refusal, so that I read the exit
    code right.

## Behavior

### Client

- Before it connects, `hostrun` checks every argument of the command
  (the program included) and the current directory it would send. This
  holds for a run, `--dry-run` and `--ask` alike; hostrun's own flags
  are not part of the command.
- When any of them is not valid UTF-8, hostrun connects to nothing,
  prints one line on stderr for each, arguments first in their order,
  the current directory last, and exits 125:

  ```text
  hostrun: argument 3 is not valid UTF-8: "a\xffb"
  hostrun: the current directory is not valid UTF-8: "/workspaces/app/\xe9t\xe9"
  ```

- Arguments are numbered from 1, the program being argument 1, as an
  approval session numbers them.
- A value is shown Go-quoted: an invalid byte as `\xHH`, control
  characters escaped, so no byte reaches the terminal as it is.
- A value longer than 200 characters is cut after its first 200, an
  invalid byte counting as one character, and the cut is marked with
  the number of bytes left out: `"…"…(+N bytes)`.

### Daemon

- On a request frame from the container (`FrameRequest`, `FrameDryRun`,
  `FrameAsk`), after the version check and before it decodes the
  request, the daemon refuses the request when its JSON
  - holds bytes that are not valid UTF-8, or
  - holds a `\uXXXX` escape of a UTF-16 surrogate that is not part of a
    high-low pair.
- The refusal is `FrameError` with code 125 and the message
  `the request is not valid UTF-8: arguments and the current directory
  must be valid UTF-8`. The command does not start; with `--ask`, no
  approval request is made.
- The daemon logs it as `request rejected`, as other malformed
  requests.
- Other frames (`FrameArm`, the approval socket's frames) are not
  checked: hostrunner sends them from the host, and they carry no argv
  from the container.

### Protocol

`Version` and `ApprovalVersion` stay 1. Requests are encoded as before;
the change is which requests are accepted.

## Edge cases

- An argument holding U+FFFD as valid UTF-8 (`$'\xef\xbf\xbd'`) is
  valid: it is sent and runs.
- An escaped surrogate pair (`"\ud83d\ude00"`) in a request from
  another client is valid; a lone high (`\ud800`) or low (`\udc00`)
  surrogate, or a high one followed by anything but a low one, is
  refused.
- `\\ud800` (an escaped backslash followed by text) is not an escape
  and is valid.
- An empty argument is valid.
- Several bad arguments and a bad current directory: one line each,
  exit 125.
- A bad argument after a command no rule allows: still the UTF-8
  refusal, exit 125; the client never asks the daemon.
- `hostrun --dry-run` and `hostrun --ask=<duration>` with a bad
  argument: the same lines and exit 125; `--ask` waits for nothing.
- Stdin, stdout and stderr are bytes and are not affected.

## Docs

- README: the note under the approval session's argument display that
  invalid UTF-8 reaches the session once #101 is fixed becomes that
  hostrun refuses it, so it never does; the exit codes table names it
  under 125; Limitations says arguments and the current directory must
  be valid UTF-8 and that bytes can go through stdin.
- The hostrun section of AGENTS.md (`internal/instructions/instructions.md`
  and the block in AGENTS.md, kept equal as CONTRIBUTING.md describes):
  125 also covers an argument or current directory that is not valid
  UTF-8.
- The approval session's display of invalid UTF-8 stays as it is.

## Tests

At `client.Run` against an in-process daemon, and with raw frames
written to the daemon's socket, as the existing protocol-violation
tests do. Each asserts the exit code or `FrameError` code, the message,
and that the command did not run.

- Client: a bad argument (run, `--dry-run`, `--ask`), the program
  itself, a bad current directory, several bad values (one line each,
  in order), a value over 200 characters (cut, bytes counted), the
  numbering; hostrun never connects to the daemon.
- Client, valid input: U+FFFD as valid UTF-8, non-ASCII text and an
  empty argument run as given.
- Daemon: a payload with an invalid byte in an argument and in the
  current directory, a lone high and a lone low surrogate escape, for
  each of `FrameRequest`, `FrameDryRun` and `FrameAsk`, and a high
  surrogate followed by another escape or a character: `FrameError`
  125 with the message, command not run.
- Daemon: a `FrameAsk` the rules refuse, with a session serving: the
  refusal, and the session shows no request.
- Daemon: the refusal is logged as `request rejected`.
- Daemon, valid input: an escaped surrogate pair, an escaped backslash
  before `ud800`, and U+FFFD as valid UTF-8 pass the check.
- Daemon: a request of another version is still refused for its
  version first.

## Out of scope

Carrying arguments or the current directory as bytes (a new protocol
version); rules matching bytes; checking `FrameArm` or the approval
socket's frames; changing the approval session's display of invalid
UTF-8; an e2e test in a container.
