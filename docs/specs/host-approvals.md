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
session: refuse, this request only, for this session (until the approval
session ends), standing (until the daemon stops), or add a rule. The
container cannot reach the channel the human answers on.

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
- A command the rules, a session approval or a standing approval allow
  runs at once, printing nothing of hostrun's own.
- While the request waits: one stderr line, `hostrun: waiting for approval
  on the host: <reasons joined by "; ">`, the first naming the rule that
  refused it as a refusal does (`denied by rule "git push": …`), with
  ` (guarded)` after `host` for a guarded refusal.
- Approved: `hostrun: approved on the host: this request only` (`session
  (until the approval session ends)`, `standing (until the daemon
  stops)`, `rule added`), then the command's output and exit code as for
  an allowed command.
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
  `FrameApproved`, JSON `{scope}` with `once`, `session`, `standing` or
  `rule` (the answer given, not a request let through by a session or
  standing approval, which gets no approval frames); then the ordinary
  run stream (stdin credit once the command has started, stdout, stderr,
  exit). Every refusal is `FrameError` with 126.
- `Allowed` gains optional `approval`, `standing` or `session`, so a dry
  run can report that a standing or session approval lets the command
  through.
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
- `hostrunner serve` refuses to start when the approval socket would lie
  inside the host workspace (symlinks followed), which the container
  mounts and so could reach to answer its own requests.
- Only the daemon's own uid may connect (`SO_PEERCRED`).
- A write to a session is bounded by 2 seconds: a session that does not
  take a request or a withdrawal in time (suspended, its terminal
  stopped) is dropped (see Unanswered requests), so it holds up neither
  requests nor other sessions.
- Session frames (answer, revoke a standing approval, reread the rules,
  take over) are accepted only there and refused on the container's
  socket; the approval socket accepts no container frames.
- Hello: the daemon answers with its approval protocol version, the host
  and container workspaces, the rules file path, and whether a session
  serves it. A session does not serve a daemon of another approval
  protocol version; there is no backward compatibility.
- Each approval request has an id (a counter in the daemon), sent with its
  content. The session answers by id; only the serving session is heard;
  an answer for an unknown, answered, withdrawn or expired request, or of
  a scope other than `once`, `session`, `standing` or `rule`, is refused,
  and the session says so. The daemon tells the serving session
  of a request withdrawn or expired (`Withdrawn{id, reason}`), so it
  drops it; an answer that crosses it is refused with that reason
  (`Answered{id, error, ended}`), so the session reports the end
  (`request N expired`) whichever frame comes first.
- Take-overs are handled one at a time; the last wins. Waiting requests,
  with the rest of their limit, and the standing approvals pass to the new
  session; the session approvals of the session taken over end (see
  Session approvals).

## Unanswered requests

- No session serves the daemon: `--ask` is refused at once.
- A session serves it: the request waits up to its limit, then is refused
  with "nobody answered" and leaves the session.
- A session that leaves without a take-over refuses every request still
  waiting (126 `refused on the host`, logged `session left`); the session
  says how many (`refused 2 waiting requests`).
- A session that does not take a request within 2 seconds is dropped:
  that request gets 126 `not approved: no approval session serves this
  devcontainer` (logged `no session`), and those already waiting end as
  for a session that left.
- A daemon that stops or steps aside answers every waiting request with
  126 before closing, also when its session goes with it.

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

- Granted by the answer `d`, standing.
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
  approvals: [{n, argv, cwd, guarded, session}]}`, session approvals
  included and marked; `Revoke{n}` answered by `Revoked{n, error}`, for
  either kind. The guarded confirmation for `d` reads `Type "yes" to
  approve it as standing (until the daemon stops): `. A standing approval
  granted, the session says `approved as standing (until the daemon
  stops)`.

## Session approvals

- Granted by the answer `s`, for this session.
- A standing approval in every other respect (the same argv and
  directory, with or without `--ask`, the second confirmation once, how
  it runs and is taken out by a new path failure, the log's
  `approval_id` on the run the answer started), but held only while the
  session that granted it serves the daemon. They end, every one that
  session granted, when it stops serving: it leaves (`quit`, Ctrl+C, end
  of input, `daemons`, its connection failing), is dropped (a write not
  taken within 2 seconds) or is taken over; and with the daemon. A
  command one let through keeps running.
- Session approvals and standing approvals share one numbering and one
  list: `standing` shows both, a session approval marked `session`, and
  `revoke <n>` revokes either (`no approval N` for a number it does not
  hold; `no approvals` for an empty list). Granting either kind for an
  argv and directory the daemon holds an approval for keeps its number
  and takes the new kind and terms.
- The answer `s` is given, but the session stops serving before the
  approved command has passed its checks: the command runs as approved
  (the agent was told `session (until the approval session ends)`
  already), and no session approval is granted; the log says `session
  approval not granted` (`approval_id`, `reason` `session left`).
- The log marks the runs `approved=session`.
- The guarded confirmation for `s` reads `Type "yes" to approve it for
  this session: `. A session approval granted, the session says
  `approved for this session (until it ends)`.

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
  which must be the one the daemon serves (`DaemonInfo.rules_digest`).
  For a file edited by hand since, the session asks first (`the rules file
  changed since the daemon loaded it; reread it, making that change live
  now? [r]eread [c]ancel`): `r` has the daemon reread it, so the hand
  edit goes live and the human sees the diff against the rules the daemon
  then serves; `c` keeps the hand edit out until the next container start
  and gives no rule (`cannot add a rule: … takes effect at the next
  container start`); one the daemon cannot reread gives no rule either
  (`cannot add a rule: … cannot reread it: …`), the old rules staying. A
  rule holding
  YAML anchors or aliases is not widened, as its lists may be other
  rules' too. A missing file is created with `rules:` and the rule; without
  its directory no rule is proposed. Only the touched
  rule's lines change, or the rule is appended. The result, also after the
  editor, must parse as `hostrunner up` parses it and allow the approved
  argv; an invalid result is never written, and the human edits again or
  cancels. The write goes to a temporary file in the same directory,
  renamed over the original. That directory is opened when the proposal
  reads the file, the path's symbolic links resolved then, and the check
  and the write go through it; when the links lead off the configured
  path, the proposal shows `writes <file>` under the diff.
- Then the session tells the daemon to reread the file. The daemon checks
  it and serves new requests with the new rules and their digest, so the
  next arm does not step aside and standing approvals stay; running
  commands are untouched; the approved request runs under the new rule. A
  failed reread keeps the old rules and runs the request once, and the
  session says so. A manual edit takes effect at the next container start,
  or before, when the human agrees to its reread to add a rule.
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
- Session frames: `Reread{rule, edited, digest, id}`, `digest` that of
  the file written, `id` the request the rule is for (no `rule`: a file
  changed by hand, reread before a proposal, `digest` that of the file
  the session read), answered by
  `RereadResult{digest, error, runs_once}`; the daemon keeps its rules
  when the file it rereads has another digest, and `runs_once` says why
  its checks still refuse request `id` under the new rules. The answer
  then has the scope `rule`, or `once` after a failed reread or with
  `runs_once`.

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
  cmd   git push --force origin main
  why   denied by rule "git push": flag --force is not allowed (deny list)
  ⚠ guarded: explicitly denied: flag --force is not allowed
  (1 more waiting)
answer [r]efuse [o]nce [s]ession stan[d]ing [c]md [h]elp ? o
⚠ explicitly denied: flag --force is not allowed
Type "yes" to run it once: yes
approved once

── request 9 ── 10:00 left ──────────────────────────────
  dir   /workspaces/app
  cmd   git rebase -i main
  why   no rule allows "git rebase"
answer [r]efuse [o]nce [s]ession stan[d]ing [a]dd rule [c]md [h]elp ? a
--- .devcontainer/hostrun.yaml
+++ .devcontainer/hostrun.yaml
+  - command: git rebase
+    flags:
+      allow: [-i]
+    positional:
+      allow: [main]
[w]rite [e]dit in $EDITOR [c]ancel ? w
rule added; the daemon reread the rules · request 9 runs under it

── request 10 ── 10:00 left ─────────────────────────────
  dir   /workspaces/app
  cmd   curl https://other.example/
  why   denied by rule "curl": argument "https://other.example/" is not allowed
  ⚠ guarded: injects secrets: token
answer [r]efuse [o]nce [s]ession stan[d]ing [c]md [h]elp ? r
note for the agent (Enter for none): only api.example.com is allowed
refused

> standing
  1  git push --force origin main   in /workspaces/app/svc   guarded
  2  make test   in /workspaces/app   session
> revoke 1
revoked 1
```

- Answers are a letter and Enter: `r` refuse (then an optional note for
  the agent), `o` this request only, `s` for this session (until it
  ends), `d` standing (until the daemon stops), `a` add a rule, `c` the
  whole command, `h` help. Input typed before the prompt is discarded; an
  invalid line asks again.
- `h` prints what each answer offered for the request does, one per
  line, then asks again:

  ```
    r  refuse it; then an optional note for the agent
    o  run this request only
    s  session: run this command from this directory now and
       whenever asked again, until this session ends
    d  standing: run this command from this directory now and
       whenever asked again, until the daemon stops
    a  add a rule to the rules file that allows it, then run it
    c  show every argument of the command whole
    h  this help
  ```

  For a guarded request the `a` line is left out, and the `o`, `s` and
  `d` lines end `; after a warning, asks you to type yes`, wrapped to
  fit (`o  run this request only; after a warning, asks you to type
  yes`).
- Guarded: the block lists every guarded reason, `⚠ guarded: <reason>`
  each, sent with the request; after `o`, `s` or `d`, a warning repeats
  them, `⚠ <reason>` each, then `yes` to type; anything else returns to
  the answer prompt. The `why` line names the list kind that refused
  (`(deny list)`), which the rules' own message does not.
- The command from the container is shown on the `cmd` line quoted so
  that bash reads each argument back as it is, its arguments separated
  by spaces, so that none can steer the terminal or hide where it ends:
  - an argument made only of ASCII letters, digits and `_@%+=:,./-` as
    is (`git push origin --delete feat/x`);
  - an empty one as `''`;
  - one holding any other printable character, `'` and combining marks
    (Unicode Mn, Me) excepted, in single quotes (`git commit -m 'fix: a
    b'`);
  - one holding `'`, a control character, a combining mark, a character
    that is not printable or invalid UTF-8 in ANSI-C quotes, `$'…'`
    (`echo $'\x1b[2J'`): `'` and `\` as `\'` and `\\`, newline and tab
    as `\n` and `\t`, any other ASCII control character, DEL and a byte
    of invalid UTF-8 as `\xHH`, a combining mark and any other character
    that is not printable as `\uHHHH`, or `\UHHHHHHHH` above U+FFFF, and
    every other character as it is. Invalid UTF-8 reaches the session
    only once [#101](https://github.com/kravlab/hostrunner/issues/101) is
    fixed;
    until then such bytes arrive as U+FFFD.

  The directory, reasons and guards are shown as they are when every
  character is printable, Go-quoted otherwise. An argument holding
  non-ASCII gets `⚠ non-ASCII` and its ASCII form. An argument over 200
  characters is cut, its first 200 shown as above and followed by
  `…(+N bytes)`, a command over 20 arguments with `…(+N args)`, and
  `cmd truncated: c shows all` is printed; `c` prints every argument
  whole, one numbered per line, shown as above, one holding non-ASCII
  followed by `   ⚠ non-ASCII: <ASCII form>`, whole too, and asks
  again. A non-ASCII argument gets a line of its own, `⚠ non-ASCII:
  argument N <as shown> is <ASCII form>`, the ASCII form in `$'…'` with
  every character outside ASCII escaped (`\uHHHH`, or `\UHHHHHHHH`
  above U+FFFF), both cut alike. `standing` shows a command as the
  block does, cut and marked. A note for the agent holding control
  characters is sent Go-quoted.
- Requests come one at a time, first in, first out. A new one rings the
  bell (BEL) and counts in `(N more waiting)` at the next block; nothing
  is printed in the middle of a prompt. A current request that is
  withdrawn or expires is announced (`request N withdrawn`, `… expired`),
  typed input is discarded, and the next is shown; queued ones that expire
  drop out silently.
- The idle prompt `>` takes `standing` (the standing and session
  approvals), `revoke <n>`, `daemons`, `quit`.
- Without `--dir`, the session globs
  `$XDG_RUNTIME_DIR/hostrunner/*.approval.sock` and sends hellos, every 2
  seconds while it lists or waits. The daemon whose host workspace holds
  the session's directory is marked `*` and is the default; a daemon
  served by another session is marked; another approval protocol version
  is marked and cannot be chosen; an empty list waits; dead sockets are
  skipped and only daemons remove them. `--dir X` goes straight to
  `X.approval.sock`; a daemon without one has no approvals, and the
  session says so and ends; it serves that daemon alone, with no list. A
  daemon another session serves asks `another approval session serves
  this daemon` and `Take over? [y/N]`, as the list does: `y` or `yes`
  takes it over; anything else, input ending or Ctrl+C ends the session
  with that refusal, unless the other session has left meanwhile, when
  the daemon is served. A daemon gone by then is waited for, as one that
  ends while served; one served by another session when it comes asks
  again.
  Without `--dir`, `$XDG_RUNTIME_DIR` must be set.
- Choosing a served daemon asks `Take over? [y/N]`; no returns to the
  list. A session that is taken over prints `taken over by another
  session` and exits. Ctrl+C refuses the waiting requests, says how many,
  and exits. A session learns that its daemon ended from the connection
  closing; it says so (`the daemon ended: waiting for the next one`) and
  waits for the next daemon in the same runtime directory; `q` quits and
  `daemons` lists them meanwhile. A write to the daemon that fails (an
  answer, `standing`, `revoke`, a reread) ends serving it alike: the
  session says `the connection to the daemon failed: …`, that a rule
  written was not reread (`it takes effect at the next container
  start`), how many waiting requests the daemon refused, the one whose
  answer did not reach it among them, and `waiting for the next one`. A
  note longer than 4096 bytes, as sent, is asked for again (`note too
  long`).
- `daemons` at the idle prompt leaves the daemon served (nothing waits at
  that prompt) and shows the list; choosing it again serves it again. One
  session serves one daemon. A session that ends, by `daemons`, `quit` or
  Ctrl+C, waits (up to 1 s) for the daemon to drop it, so a list shown
  right after shows the daemon unserved. Leaving a daemon, by any of
  these, ends the session approvals the session granted it; serving it
  again starts with none.
- Session frames: `Serve{take_over}` (an empty `Serve` serves a daemon
  nobody serves), answered by `FrameServing` and the requests passed on;
  the session replaced gets `FrameTakenOver`.

## Dry run and Rules test

- Dry run: what a run would get. A refused command: 126 and the plain
  refusal's message and hint, guarded marked; not approvable as today; a
  standing approval: 0 and `hostrun: dry run: allowed by a standing
  approval`, with `dir` and secrets' names; a session approval likewise,
  `allowed by a session approval`.
- Rules test: a refused command exits 126; its line lists every reason,
  joined by `; `, and ends with `(approvable)` or `(guarded: approvable
  after a second confirmation)`, judged from the file alone. It still
  does no path checks and knows nothing of sessions, session or standing
  approvals. Allowed commands, and a missing or invalid file, are
  unchanged.

## Daemon log

- Info: `approval requested` (`approval_id`, `argv`, `dir`, `reasons`,
  `guarded`, `timeout`); `approval answered` (`approval_id`, `scope` or
  `refused`, `note`); `approval not answered` (`approval_id`, `reason`:
  `withdrawn`, `expired`, `session left`, `daemon stopping`, or, for a
  request no session received, `no session` (none serves, or the serving
  one did not take it) or `too large`, logged without `approval_id` but
  with `argv` and `dir`); `standing approval granted` and `revoked`,
  `session approval granted`, `revoked` and `not granted`, each with the
  approval's number (`approval`); `session approvals ended` (`count`,
  `reason`: `session left` or `taken over`, `pid` of the session), when
  a session stops serving and had granted some, but not when the daemon
  stops, which every approval goes with;
  `rule added` (file, rule), `hand-edited rules reread` (file) and
  `rules reread` (digest); `approval session serving`, `taken over`,
  `left` (pid).
- Runs an approval let through: `command finished` and `command
  cancelled` gain `approved` (`once`, `session`, `standing`, `rule` for
  the answer that started them, `session` or `standing` too for a later
  run a session or standing approval let through) and, with a request,
  `approval_id`: the run the answer started has one, a later run none.
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
  the connection closes; standing and session approvals are gone.
- A session that quits, goes to `daemons`, is dropped, loses its
  connection or is taken over: the session approvals it granted end, a
  command one let through keeps running, and the next run of such a
  command is refused with the hint, so `--ask` asks again; standing
  approvals stay.
- The rules file changed by hand before "add a rule": the session asks
  whether the daemon rereads it; `c` keeps it out, `r` makes it live (or,
  when it does not load, keeps the old rules), and a request that ends
  meanwhile drops the question.
- The rules file changes between the proposal and the write: nothing is
  written, and the session asks for the answer again.
- The rules file or its directory swapped for a symlink between the
  proposal and the write: the write goes to the directory the proposal
  read, kept open meanwhile, never through the link. A directory swapped
  so: the daemon rereads another file and keeps its old rules (`rule
  added, but the daemon kept its old rules: … · request 9 runs once`). A
  file swapped for a link out of that directory: `nothing written: the
  rules file was swapped for a link out of its directory`. A file swapped
  for a link to another file in it: that link is replaced by a regular
  file, with the mode of the file it pointed to.
- A failed reread after a write: the request runs once, the old rules
  stay, the session says so.
- A rule edited by hand that the rules allow but a later check refuses
  (a fixed directory missing): the request runs once, the session says
  so and why (`request 9 runs once: fixed directory … does not exist`),
  and the agent and the log get `once`, not `rule`.
- An argv holding terminal escapes, look-alike letters, or long
  arguments: shown escaped, marked and cut; `c` shows all.
- An approval request whose content for the session (argv, directory,
  reasons and guards) exceeds a frame (1 MiB), though its `FrameAsk` fit
  one, is refused at once while a session serves, before anyone is
  asked: 126 `not approved: the request is too large to show`, logged
  `too large`.

## Docs

README: usage of `--ask` and `hostrunner approve`, exit codes, dry run
and Rules test, the daemon log being readable in the container, what an
older daemon does with `--ask`; its approval session part names the
answers `s`, for this session (until it ends), `d`, standing (until the
daemon stops), `c` and `h`, and shows a command as the `cmd` line does.
The agent instructions' "Host commands (hostrun)": on a refusal hinting
at `--ask`, re-run with `--ask=<duration>` within the tool timeout; on a refusal by the human or
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
manually edited rules without a container start, but to add a rule with
the human's consent.
