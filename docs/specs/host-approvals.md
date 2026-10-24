# Spec: host approvals

Ticket: [Spec: one-off host approval for commands the rules do not allow](https://github.com/kravlab/hostrunner/issues/88)
(from the map [#71](https://github.com/kravlab/hostrunner/issues/71)), split into
[#89](https://github.com/kravlab/hostrunner/issues/89) to
[#98](https://github.com/kravlab/hostrunner/issues/98).
Decision record: [ADR 0007](../adr/0007-host-approval-outside-the-container.md).
Extends [rules-engine.md](rules-engine.md), [dry-run.md](dry-run.md),
[rules-secrets.md](rules-secrets.md) and
[watch-armed-container.md](watch-armed-container.md); vocabulary in
[CONTEXT.md](../../CONTEXT.md) (Approvals).

## Goal

A command the rules do not let through is refused with 126, and the agent
can only stop and ask in chat; the human then edits the rules file and
restarts the container, or runs the command by hand. Now the agent can
ask, with `hostrun --ask`, and a human answers on the host in an approval
session: refuse, this request only, for the daemon's lifetime, or add a
rule. The container cannot reach the channel the human answers on.

In this spec, a **plain refusal** is what `hostrun` without `--ask` gets
for a command the rules refuse; the **container directory** is the
directory `hostrun` runs in, inside the container; the **approval protocol
version** is the version of what the approval socket speaks, separate
from `protocol.Version` of the container's socket.

## Classes of refusal

- **Approvable**: a refusal by the rules a human may answer with an
  approval:
  - no rule matches;
  - no rules file, while a session serves the daemon;
  - an argument misses an `allow` or `allow_regex` list (flag name, flag
    value, positional);
  - arguments given to an `args: none` rule;
  - every guarded refusal.
- **Guarded refusal**, approvable after a warning and a second
  confirmation:
  - explicitly denied: a flag name, an abbreviation of a denied flag, a
    flag value or a positional argument matches a `deny` or `deny_regex`
    list;
  - a failed path check;
  - an argv the rules cannot read: a flag missing its value, an
    abbreviated flag without `=`, an ambiguous abbreviation, a value given
    to a flag that takes none, and, under `flag_style: go`, tokens the Go
    flag parsers read differently (more than two dashes, a value flag in
    a cluster, a value they disagree on, a path check on such a token);
  - a rule with secrets, which approving would inject.
- **Not approvable**, refused at once with today's code and message and no
  hint: the container directory outside the workspace or unavailable on
  the host; the rule's `dir` missing, not a directory or inside the
  workspace; a secret unavailable or too short to mask; the program not
  found or not executable.
- For an approval request, and for the Rules test, the rules check the
  whole argv and collect every reason, path checks included (an argument
  a list refuses is path-checked too); one guarded reason makes the
  request guarded. A plain refusal reports the first
  reason; its hint's guarded mark is judged over every reason.
- Before asking, the daemon runs every check that cannot be approved, as a
  dry run does (directories, the secrets' sources without obtaining
  values, the program lookup); starting the approved command runs them
  again.

## Client

- `hostrun --ask[=<duration>] <command> [args…]`: `--ask` is hostrun's
  first argument, like `--dry-run`; after the program every argument is
  the command's own. The duration is a Go duration; an unparsable or
  non-positive one fails with 125 before connecting (`--ask limit "x"
  is not a positive duration`). Plain `--ask` waits the daemon's default,
  10 minutes; the daemon refuses more than 1 hour with 125 (`--ask limit
  1h0m1s exceeds 1h0m0s`).
- `--dry-run` and `--ask` together, in either order as the first two
  arguments: 125, `--dry-run and --ask cannot be combined`.
- A command the rules or a standing approval allow runs at once, printing
  nothing of hostrun's own.
- While the request waits: one stderr line, `hostrun: waiting for approval
  on the host: <reasons joined by "; ">`, the first naming the rule that
  refused it as a refusal does (`denied by rule "git push": …`), with
  ` (guarded)` after `host` for a guarded refusal.
- Approved: `hostrun: approved on the host: this request only` (`for the
  daemon's lifetime`, `rule added`), then the command's output and exit
  code as for an allowed command.
- Not approved, all 126:
  - `hostrun: refused on the host`, followed by `: <note>` when the human
    gave one;
  - `hostrun: not approved: nobody answered on the host`;
  - `hostrun: not approved: no approval session serves this devcontainer`;
  - `hostrun: not approved: the hostrunner daemon is stopping`.
- Interrupted: 130 as today; the dropped connection withdraws the request.
- While a session serves the daemon, a plain refusal that can be approved
  ends with `(ask for an approval: hostrun --ask …)`, or `(ask for an
  approval: hostrun --ask …; guarded: needs a second confirmation)`; the
  `…` is literal. It hints only when `--ask` would ask a human: not when
  a check an approval cannot lift (a directory, a secret's source, the
  program) would refuse the command anyway.
- Exact wording may be polished when implemented; the scheme of codes and
  `hostrun:` messages stays.

## Container protocol

- `FrameAsk` (client → daemon), JSON `Ask{version, argv, cwd,
  timeout_ms}`; `timeout_ms` 0 is the daemon's default.
- `FrameWaiting` (daemon → client), JSON `{reasons, guarded}`;
  `FrameApproved`, JSON `{scope}` with `once`, `lifetime` or `rule` (the
  answer given, not a request let through by a standing approval, which
  gets no approval frames); then the ordinary run stream (stdin credit
  once the command has started, stdout, stderr, exit). Every refusal is
  `FrameError` with 126.
- `Allowed` gains optional `standing`, so a dry run can report that a
  standing approval lets the command through.
- `protocol.Version` stays 1. A daemon that predates `FrameAsk` rejects
  the frame type with 125 and runs nothing; an older client never sends
  it. `hostrunner up` installs the client with the daemon, so both
  normally come from one install.
- While a request waits the daemon keeps reading its connection; a client
  that goes away withdraws the request.

## Approval socket

- `<runtime directory>.approval.sock`, beside the runtime directory and
  outside the container's mount; for `hostrunner serve --socket <path>`
  the runtime directory is the socket's directory. Created as the main
  socket is (mode 0600, a stale socket removed, refused when something
  listens) and removed on exit. Not an abstract socket, which a container
  on the host's network would share.
- Only the daemon's own uid may connect (`SO_PEERCRED`).
- Session frames (answer, revoke a standing approval, reread the rules,
  take over) are accepted only there and refused on the container's
  socket; the approval socket accepts no container frames.
- Hello: the daemon answers with its approval protocol version, the host
  and container workspaces, the rules file path, and whether a session
  serves it. A session does not serve a daemon of another approval
  protocol version; there is no backward compatibility.
- Each approval request has an id (a counter in the daemon), sent with its
  content. The session answers by id; only the serving session is heard;
  an answer for an unknown, answered, withdrawn or expired request is
  refused, and the session says so. The daemon tells the serving session
  of a request withdrawn or expired (`Withdrawn{id, reason}`), so it
  drops it; an answer that crosses it is refused with that reason
  (`Answered{id, error, ended}`), so the session reports the end
  (`request N expired`) whichever frame comes first.
- Take-overs are handled one at a time; the last wins. Waiting requests,
  with the rest of their limit, and the standing approvals pass to the new
  session.

## Unanswered requests

- No session serves the daemon: `--ask` is refused at once.
- A session serves it: the request waits up to its limit, then is refused
  with "nobody answered" and leaves the session.
- A session that leaves without a take-over refuses every request still
  waiting (126 `refused on the host`, logged `session left`); the session
  says how many (`refused 2 waiting requests`). A daemon that stops or
  steps aside answers every waiting request with 126 before closing,
  also when its session goes with it.

## Running an approved command

- No rule matches: the mirrored directory, the daemon's environment with
  `PWD`, no path checks, secrets or injection.
- A refusing rule: as the rule's allowed commands run, in its `dir`
  (`inherit` resolved), with its injection and the masking of its secrets'
  values.
- Path-check arguments that pass keep their mode (`open` becomes
  `/proc/self/fd/N`, `check` is passed as given); an argument approved
  after failing its path check is passed as given.
- The same streaming, stdin credit, process group, kill on a dropped
  connection and exit codes as an allowed command; no time limit of its
  own.

## Standing approvals

- Granted by the answer "for the daemon's lifetime".
- The same argv, byte for byte, from the same container directory; let
  through with or without `--ask`, printing nothing of hostrun's own.
- Guarded refusals can get one; the second confirmation comes once, when
  it is granted.
- Held in the daemon's memory only: kept through arms that keep the daemon
  (a rebuild, a restart shorter than the grace), gone with the daemon
  (step aside, exit). They work without a session.
- The session lists them and revokes any one; revoking does not stop a
  running command.
- It runs as the approved request did: under the rule that refused it,
  with the path failures the human saw passed as given, each for the
  reason the human saw; a path check that fails otherwise, or only
  later, takes the command out of the standing approval: it is refused
  with the hint, and `--ask` asks the human again. The log marks the runs
  `approved=standing`. After the daemon rereads its rules (a rule added),
  it runs under the rule that matches it then, with that rule's `dir`,
  secrets and checks.
- Granted once the approved command has passed its checks; granting one
  the daemon holds for the same argv and directory keeps its number.
- A one-off approval likewise passes a path failure as given only for
  the reason the human saw.
- Session frames: `Standing` (no payload) answered by `StandingList{
  approvals: [{n, argv, cwd, guarded}]}`; `Revoke{n}` answered by
  `Revoked{n, error}`. The guarded confirmation for `l` reads `Type "yes"
  to approve it for the daemon's lifetime: `.

## Add a rule

- Not offered for guarded refusals.
- No rule matches: a new rule. `command` is the program and the next token
  unless it is a flag; `flags: allow` holds exactly the flags given and
  `positional: allow` exactly the arguments given, as the rules parse the
  argv. An argument holding `*` or `?` goes into `allow_regex`, anchored
  and quoted.
- A matching rule refused by an allow list or `args: none`: the refused
  flags, values and arguments are added to its allow lists, and
  `args: none` becomes lists allowing exactly the arguments given; its
  `dir`, secrets and the rest stay.
- The session shows the unified diff, then `[w]rite [e]dit in $EDITOR
  [c]ancel`.
- It writes the file the daemon loaded, computed against the file on disk,
  which must be the one the daemon serves (`DaemonInfo.rules_digest`): a
  file edited by hand since is refused, so its change waits for the next
  start rather than going live unreviewed with the rule. A rule holding
  YAML anchors or aliases is not widened, as its lists may be other
  rules' too. A missing file is created with `rules:` and the rule. Only the touched
  rule's lines change, or the rule is appended. The result, also after the
  editor, must parse as `hostrunner up` parses it and allow the approved
  argv; an invalid result is never written, and the human edits again or
  cancels. The write goes to a temporary file in the same directory,
  renamed over the original.
- Then the session tells the daemon to reread the file. The daemon checks
  it and serves new requests with the new rules and their digest, so the
  next arm does not step aside and standing approvals stay; running
  commands are untouched; the approved request runs under the new rule. A
  failed reread keeps the old rules and runs the request once, and the
  session says so. A manual edit still takes effect at the next container
  start.
- A flag given its value in its token (`--file=x`) is declared in a new
  rule's `flags.values`, allowing that value; given apart (`--file x`) it
  is a flag and an argument, as the rules read it. An argument holding `*`
  or `?` turns its whole list into `allow_regex`, since a list takes one
  kind of pattern; a rule's `allow` list cannot take such an argument, and
  the session says it cannot add the rule.
- When no edit lets the request through (the file does not parse, the
  rules allow it already, the rule has no list to widen), the session
  says why and asks for the answer again.
- `e` opens a temporary copy of the edit in `$VISUAL`, or else `$EDITOR`,
  as a shell runs it; with neither set the session says `$EDITOR is not
  set` and asks again. A valid edit is written at once; an invalid one is
  reported and `[e]dit again [c]ancel` asked. The session reads nothing
  from the terminal while the editor runs.
- Session frames: `Reread{rule, edited, digest}`, `digest` that of the
  file written, answered by `RereadResult{digest, error}`; the daemon
  keeps its rules when the file it rereads has another digest. The answer
  then has the scope `rule`, or `once` after a failed reread.

## Approval session

`hostrunner approve [--dir <runtime directory>]`, a line-oriented log
without an alternate screen. The agreed screens:

```
$ hostrunner approve
daemons:
  1 * /home/u/app        (container /workspaces/app)       ← current directory
  2   /home/u/tools      (container /workspaces/tools)     served by another session
  3   /home/u/legacy     another version: restart its container
choose [1-3, Enter = 1, q quit] ? 1
serving /home/u/app (container /workspaces/app)
waiting for requests · commands: standing, revoke <n>, daemons, quit
>

── request 7 ── 10:00 left ──────────────────────────────
  dir   /workspaces/app/svc
  argv  "git" "push" "--force" "origin" "main"
  why   denied by rule "git push": flag --force is not allowed (deny list)
  ⚠ guarded: explicitly denied: flag --force is not allowed
  (1 more waiting)
answer [r]efuse [o]nce [l]ifetime [?]more ? o
⚠ explicitly denied: flag --force is not allowed
Type "yes" to run it once: yes
approved once

── request 9 ── 10:00 left ──────────────────────────────
  dir   /workspaces/app
  argv  "git" "rebase" "-i" "main"
  why   no rule allows "git rebase"
answer [r]efuse [o]nce [l]ifetime [a]dd rule [?]more ? a
--- .devcontainer/hostrun.yaml
+++ .devcontainer/hostrun.yaml
+  - command: git rebase
+    flags:
+      allow: [-i]
+    positional:
+      allow: [main]
[w]rite [e]dit in $EDITOR [c]ancel ? w
rule added; the daemon reloaded the rules · request 9 runs under it

── request 10 ── 10:00 left ─────────────────────────────
  dir   /workspaces/app
  argv  "curl" "https://other.example/"
  why   denied by rule "curl": argument "https://other.example/" is not allowed
  ⚠ guarded: injects secrets: token
answer [r]efuse [o]nce [l]ifetime [?]more ? r
note for the agent (Enter for none): only api.example.com is allowed
refused

> standing
  1  "git" "push" "--force" "origin" "main"   in /workspaces/app/svc   guarded
> revoke 1
revoked 1
```

- Answers are a letter and Enter: `r` refuse (then an optional note for
  the agent), `o` this request only, `l` for the daemon's lifetime, `a`
  add a rule, `?` the whole argv. Input typed before the prompt is
  discarded; an invalid line asks again.
- Guarded: the block lists every guarded reason, `⚠ guarded: <reason>`
  each, sent with the request; after `o` or `l`, a warning repeats them,
  `⚠ <reason>` each, then `yes` to type; anything else returns to the
  answer prompt. The `why` line names the list kind that refused
  (`(deny list)`), which the rules' own message does not.
- What came from the container is shown in Go quotes: control characters
  and terminal escapes appear escaped. Printable Unicode is shown as is,
  and an argument holding non-ASCII gets `⚠ non-ASCII` and its ASCII form.
  An argument over 200 characters is cut with `…(+N bytes)`, an argv over
  20 arguments with `…(+N args)`, and `argv truncated: ? shows all` is
  printed; `?` prints every argument, one numbered per line.
- Requests come one at a time, first in, first out. A new one rings the
  bell (BEL) and counts in `(N more waiting)` at the next block; nothing
  is printed in the middle of a prompt. A current request that is
  withdrawn or expires is announced (`request N withdrawn`, `… expired`),
  typed input is discarded, and the next is shown; queued ones that expire
  drop out silently.
- The idle prompt `>` takes `standing`, `revoke <n>`, `daemons`, `quit`.
- Without `--dir`, the session globs
  `$XDG_RUNTIME_DIR/hostrunner/*.approval.sock` and sends hellos, every 2
  seconds while it lists or waits. The daemon whose host workspace holds
  the session's directory is marked `*` and is the default; a daemon
  served by another session is marked; another approval protocol version
  is marked and cannot be chosen; an empty list waits; dead sockets are
  skipped and only daemons remove them. `--dir X` goes straight to
  `X.approval.sock`; a daemon without one has no approvals, and the
  session says so.
- Choosing a served daemon asks `Take over? [y/N]`; no returns to the
  list. A session that is taken over prints `taken over by another
  session` and exits. Ctrl+C refuses the waiting requests, says how many,
  and exits. A session learns that its daemon ended from the connection
  closing; it says so and waits for the next daemon in the same runtime
  directory.

## Dry run and Rules test

- Dry run: what a run would get. A refused command: 126 and the plain
  refusal's message and hint, guarded marked; not approvable as today; a
  standing approval: 0 and `hostrun: dry run: allowed by a standing
  approval`, with `dir` and secrets' names.
- Rules test: a refused command exits 126; its line lists every reason,
  joined by `; `, and ends with `(approvable)` or `(guarded: approvable
  after a second confirmation)`, judged from the file alone. It still
  does no path checks and knows nothing of sessions or standing
  approvals. Allowed commands, and a missing or invalid file, are
  unchanged.

## Daemon log

- Info: `approval requested` (`approval_id`, `argv`, `dir`, `reasons`,
  `guarded`, `timeout`); `approval answered` (`approval_id`, `scope` or
  `refused`, `note`); `approval not answered` (`approval_id`, `reason`:
  `withdrawn`, `expired`, `no session`, `session left`, `daemon
  stopping`); `standing approval granted` and `revoked`; `rule added`
  (file, rule) and `rules reread` (digest); `approval session serving`,
  `taken over`, `left` (pid).
- Runs an approval let through: `command finished` and `command
  cancelled` gain `approved` (`once`, `lifetime`, `rule` for the answer
  that started them, `standing` for a later run a standing approval let
  through) and, with a request, `approval_id`.
- Warn: a failed reread; a refused approval-socket connection (another
  uid, a frame that does not belong there).
- Kept out: secrets' values; anything from the session's environment.
- The container can read the daemon log: it lies in the runtime directory.

## Edge cases

- No rules file, a session serving: every command is approvable and runs
  as with no matching rule; "add a rule" creates the file.
- A daemon older than `--ask`: 125, nothing runs. A client older than
  `--ask` never sends it and works as before.
- A daemon of another approval protocol version: listed, cannot be served.
- `--ask=0`, `--ask=-1m`, `--ask=soon`: 125 before connecting; `--ask=2h`:
  125 from the daemon.
- `--dry-run --ask` or `--ask --dry-run`: 125; `hostrun git --ask` passes
  `--ask` to git.
- A request withdrawn or expired while the human types: announced, input
  discarded; an answer that arrives too late is refused and reported.
- Two sessions taking over at once: the last wins; the other is told it
  was taken over.
- The daemon steps aside or exits while requests wait: they get 126 before
  the connection closes; standing approvals are gone.
- The rules file changes between the proposal and the write: nothing is
  written, and the session asks for the answer again.
- A failed reread after a write: the request runs once, the old rules
  stay, the session says so.
- An argv holding terminal escapes, look-alike letters, or megabytes of
  arguments: shown escaped, marked and cut; `?` shows all.

## Docs

README: usage of `--ask` and `hostrunner approve`, exit codes, dry run
and Rules test, the daemon log being readable in the container, what an
older daemon does with `--ask`. The agent instructions' "Host commands
(hostrun)": on a refusal hinting at `--ask`, re-run with
`--ask=<duration>` within the tool timeout; on a refusal by the human or
no answer, report and wait instead of asking again.

## Tests

- Tests drive the external interfaces only: what `hostrun` prints and
  exits with, what the session shows and accepts, whether a command ran on
  the host, the rules file's content, the daemon log's lines. No test
  reaches into the daemon's internals or the session's state.
- The client tests' harness (the real client against the real daemon over
  a temporary socket) gains the approval socket and a scripted approval
  session, so one test plays the agent and the human. Every behavior above
  is checked there, by exit codes, output, markers in the workspace, the
  rules file and log lines.
- The Rules test's tests: every reason and the class marks.
- e2e: from inside the container the approval socket is unreachable, and
  `--ask` without a session is refused at once with 126.
- No new seams beyond these; the rules engine, protocol and socket are
  covered through the client harness, apart from where existing unit tests
  of a touched module must stay accurate.

## Out of scope

Desktop notifications
([#83](https://github.com/kravlab/hostrunner/issues/83)); dialogs and
notification actions as the answer channel; backward compatibility with
other approval protocol versions; a switch to turn approvals on or off in
the rules file, a `hostrunner up` flag or the environment, and any opt-out
for a workspace; persisting standing approvals across daemons; rereading
manually edited rules without a container start.
