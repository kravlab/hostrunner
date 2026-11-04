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
  and container workspaces, the rules file path, whether a session
  serves it, its build version (`build_version`, optional; see
  Approval session), and whether it reports approved runs (`runs`,
  optional; see Approved runs). A session does not serve a daemon of another approval
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
  session, which learns of the approved runs going (see Approved runs);
  the session approvals of the session taken over end (see Session
  approvals).

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
  session says so. The session also sends the digest of the file it wrote
  over: when the daemon checks allows (ADR 0009) and that file was
  allowed, it records an allow of the content written as it rereads the
  file, once the file parses and has that content's digest; when it was
  not, or the allow cannot be recorded, the old rules stay, the change
  waits for an allow and the request runs once. A manual edit takes
  effect at the next container start,
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
hostrunner approve v1.2.0
daemons:
  1 * /home/u/app        v1.2.0      (container /workspaces/app)       ← current directory
  2   /home/u/tools      v1.1.0 ⚠    (container /workspaces/tools)     served by another session
  3   /home/u/legacy     unknown ⚠   another version: restart its container
choose [1-3, Enter = 1, q quit] ? 1
serving /home/u/app (container /workspaces/app) · daemon v1.2.0
waiting for requests · commands: standing, revoke <n>, out [n], runs, daemons, quit
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
approved once · run 1

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
approved under the new rule · run 2
── run 1 · git push --force origin main · exit 0 · 2.4s · 2 lines ──
! To example.com:app.git
!  + 1a2b3c4...5d6e7f8 main -> main (forced update)

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

- Versions: the session says its own build version as it starts,
  `hostrunner approve <version>`, as `hostrunner version` prints it
  (`dev` without build information). The daemon reports its build version
  in `DaemonInfo` (`build_version`), apart from the approval protocol
  version (`version`, which stays 1); a daemon older than the field
  reports none, which reads as unknown, and an older session ignores it.
  The serving line ends `· daemon <version>`, or `· daemon version
  unknown`. A daemon of another or an unknown version is served all the
  same, after one warning line naming both versions: `⚠ daemon v1.1.0,
  this session v1.2.0: run hostrunner restart to update` (`⚠ daemon
  version unknown, this session v1.2.0: …`). The list of daemons, when
  choosing and after `daemons`, shows every daemon's version, `unknown`
  for none, followed by `⚠` when it is not the session's; the note for
  another approval protocol version stays. A daemon whose version changes
  shows the list again. `hostrunner stop --choose` and `hostrunner
  restart --choose` show the same column and marks, against their own
  version.
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
  is printed in the middle of a prompt (see Notices). A current request
  that is withdrawn or expires is announced (`request N withdrawn`, `…
  expired`), typed input is discarded, and the next is shown; queued ones
  that expire drop out silently.
- The idle prompt `>` takes `standing` (the standing and session
  approvals), `revoke <n>`, `out [n]` and `runs` (see Approved runs),
  `daemons`, `quit`. The serving line's `commands: …` list names them;
  the idle prompt has no other help. A command that takes no argument
  (`standing`, `runs`, `daemons`, `quit`) given one prints
  `usage: <command>` and does nothing.
- Notices: what the daemon tells the session of its own accord, not as
  the outcome of what the human typed: a run's start line (a repeat's,
  or one that started before the session; see Approved runs), a run's
  summary and tail, and the rules-reread lines
  (`the rules file changed; …`). A request is on screen from its block
  until the daemon has taken its answer (the answer's line printed) or
  it is withdrawn or expires, with every prompt it asks meanwhile (a
  refusal's note, the guarded `yes`, adding a rule, rereading a file
  changed by hand), the answer on its way to the daemon included. While
  one is, notices are held, nothing of them printed; once it is gone
  they are printed in the order they arrived, below its last line (the
  answer's line, or `request N withdrawn`), before the next request is
  shown or the idle prompt. A session that stops serving the daemon
  meanwhile (Ctrl+C, taken over, a write failing, the daemon gone)
  prints them after a blank line, once it has said what it says of that
  (`refused 1 waiting request`, `taken over by another session`, …),
  before it ends or waits for the next daemon (`the daemon ended:
  waiting for the next one` follows them). At the idle prompt a notice
  is printed at once, on a line of its own, and `> ` is shown again,
  keeping what the human has typed so far: a command typed in part
  (`stand`) runs once finished (`standing`), on a terminal too, whose
  input a prompt discards. A command's outcome (`standing`, `revoke`,
  `out`, `runs`) is not a notice, but it is held alike: while a request
  is on screen it is held with the notices, in the order they arrived;
  at the idle prompt it is printed at once, right below the command's
  line, and `> ` is shown again, keeping what the human has typed so
  far.
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
  Without `--dir`, the runtime root (`$XDG_RUNTIME_DIR/hostrunner`
  unless `runtime_root` says) must be known.
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
- Session frames: `Serve{take_over, notices, runs}` (an empty `Serve`
  serves a daemon nobody serves; `notices` and `runs` say the session
  reads `FrameRulesReread` and the frames of approved runs), answered by
  `FrameServing`, the approved runs going (see Approved runs) and the
  requests passed on; the session replaced gets `FrameTakenOver`.

## Approved runs

- An approved run is one run of a command an approval let through (see
  [CONTEXT.md](../../CONTEXT.md)). The session reports the runs a human
  approved when asked: once, for this session, standing, and the run of
  an "add a rule" answer (under the new rule, or once); and the repeats
  a standing or session approval lets through without asking, with or
  without `--ask`, so the human sees the commands that run without
  asking them. Commands the rules allow, later runs of a rule added by
  "add a rule" included (also of a command an approval is held for),
  and dry runs are not approved runs. A run the rules allow but whose
  argument fails its path check is let through by the approval held
  for it, which passes that failure as given: it is a repeat, and is
  shown.
- Asking: the session sends `Serve{runs: true}`; a daemon that reports
  approved runs says so in its hello (`DaemonInfo.runs`) and sends their
  frames to the serving session only when it asked. An older session
  asks nothing and gets none. A session serving a daemon without `runs`
  (older than it) serves it all the same and says so, after the serving
  line and any version warning: `no run output: this daemon does not
  report approved runs`. The approval protocol version stays 1.
- Frames, daemon → session:
  - `RunStarted{run, argv, cwd, approval, request, earlier,
    elapsed_ms}`: `run` is the daemon's number for the run, which its
    later frames carry; `approval` the scope that let it through
    (`once`, `session`, `standing`, `rule`), `request` the id of the
    request answered. Sent once the answer is taken, before the checks
    starting the command runs again. A repeat has no `request` (0) and
    the kind of the approval that let it through (`standing`,
    `session`); it is sent once its checks have passed, from the plain
    run path and the `--ask` path alike. `earlier` marks a run announced
    to a session that starts serving while it goes (see Hand-over), and
    `elapsed_ms` how long the run had gone, counted from its start
    frame, when it was announced.
  - `RunOutput{run, stderr, data}`: a chunk of stdout, or of stderr with
    `stderr`, of at most 4 KiB, exactly as the container gets it, after
    the mask of the rule's secrets (`data` base64 in the JSON).
  - `RunEnded{run, code, error, cancelled, duration_ms, dropped}`: the
    exit code; or `error`, why a run never started after its approval
    (the daemon draining, a check refusing it when checked again, a
    start failure), as its client is told, with the client's exit code
    in `code`; or `cancelled`, `client gone` (its client went away) or
    `daemon stopping` (the daemon stopped or stepped aside), also for a
    run whose request is cancelled as its command starts. `duration_ms`
    counts from the run's start frame; `dropped` the bytes of output the
    session was not sent.
- Delivery: the run frames of each session go through a buffer of their
  own, apart from the container's stream, holding up to 256 KiB of
  output; output beyond it is dropped and counted in `dropped`; start and
  end frames are never dropped. A run frame the session does not take
  within 100 ms is tried again later, other frames going first; one it
  takes part of is finished before any other frame, which then waits
  only for the rest of that frame, a few KiB at most. So the container
  is never slowed and the session never dropped for run output; the
  2-second limit stays for other frames. Run frames go to the session
  serving when each is sent; a session that starts serving while a run
  goes is told of it as it does (see Hand-over).
- Hand-over: a session that starts serving the daemon, by any `Serve` (a
  take over, a daemon nobody served, or the same daemon again after
  `daemons` or once it answers again after a write failed), learns of
  the approved runs already going, whatever approval let them through:
  once, for a session, standing, "add a rule", or a repeat. Runs the
  rules allow are not announced. The daemon keeps a record of each
  approved run while it goes, its start frame as sent (the approval that
  let it through and, for a fresh approval, its request id) and when it
  was sent, and no output. Right after `FrameServing`, before the
  requests passed on, it sends a session that asked for runs that frame
  for each run going, in the order of their numbers, with `earlier` and
  `elapsed_ms`; the run's output from then on and its end follow as for
  any run. The daemon takes the runs going as the session starts
  serving, and sends a run's end and forgets the run, under one lock:
  the session is told the end of every run it is told of, and of no run
  whose end went to the session it replaced. The session numbers each at
  once, on its own counter, and prints a start line naming the approval
  that let it through, marked `started before this session`, the command
  shown as on the `cmd` line, cut alike:

  ```
  run 1 · make test · approved once · started before this session
  run 2 · make serve · by standing approval · started before this session
  ```

  A fresh approval, whose answer was confirmed to another session (or to
  this session's earlier connection to the daemon, whose number for the
  run `runs` then shows with `end not received`), is named `approved
  once`, `approved for a session`, `approved as standing` or `approved
  under a new rule`; a repeat `by standing approval` or `by session
  approval`; a kind the session does not know `by <kind>`, as the daemon
  names it, shown escaped. The start line is a notice (see Notices). The
  run's summary, its line in `runs` and the line `out` shows it under
  say `earlier output not received` after its lines, before any `<N>
  bytes not received`: its lines, its tail and its kept output hold only
  the output since. Its summary gives the duration the daemon counted
  from the run's start; while it goes, `runs` counts it from the run's
  start too: from the start line's arrival, less `elapsed_ms`. The
  session taken over ends as before (`taken over by another session`);
  what it got of the run's output is lost with it.
- A daemon that stops keeps a session that asked for runs, for up to 3
  seconds, until the runs the stop cancels have ended and their end
  frames have reached it.
- Numbers: the session numbers runs 1, 2, 3… across the daemons it
  serves, apart from request numbers. A fresh approval's run gets its
  number when the daemon confirms the answer (`Answered`), whose line
  names it: `approved once · run 3` (`approved for this session (until
  it ends) · run 3`, …). The run's frames come from another goroutine of
  the daemon than `Answered`, in either order: frames that arrive first
  wait for it. A run that started before the session served the daemon
  is numbered as it is announced (see Hand-over).
- Start line: a repeat is numbered as it starts, and the session prints
  one line naming the approval that let it through, the command shown as
  on the `cmd` line, cut alike:

  ```
  run 4 · git push origin main · by standing approval
  run 5 · make test · by session approval
  ```

  It is a notice (see Notices): held while a request is on screen,
  printed at once at the idle prompt. Its summary follows when it ends.
  A fresh approval prints no start line: its answer line names its run;
  a run that started before the session served the daemon prints one
  however it was approved (see Hand-over).
- Summary: once a numbered run ends, one line:

  ```
  ── run 3 · git push origin main · exit 0 · 2.4s · 12 lines ──
  ── run 4 · ./run.sh · not started: command ./run.sh not found on the host · 0.0s · 0 lines ──
  ── run 5 · make test · cancelled: client gone · 3m07s · 1 line ──
  ── run 6 · make logs · exit 0 · 1h02m · 920 lines · 4096 bytes not received ──
  ── run 7 · make serve · exit 0 · 3m07s · 4 lines · earlier output not received ──
  ```

  The command is shown as on the `cmd` line, cut alike; `not started:
  <why>`, `cancelled: client gone` or `cancelled: daemon stopping` stand
  in place of the exit code. The duration is in seconds with one decimal
  under a minute (`12.3s`), minutes and seconds under an hour (`3m07s`),
  hours and minutes above (`1h02m`). The session counts the lines of the
  output it got, the complete lines of stdout and stderr and an
  unfinished last line of each; `<N> bytes not received` follows when
  output was dropped (`1 line`, `1 byte` in the singular).
- Tail: under the summary, the last `approve_tail_lines` lines of the
  run's output (3 unless set), of stdout and stderr in the order the
  lines' first bytes arrived (a line
  that arrives in chunks, the other stream's in between, is shown whole
  where it began), each marked `│ ` (stdout) or `! ` (stderr), then
  `(<K> lines above · out N)` when the run printed more lines than
  shown (`1 line` in the singular), counted as the summary counts them:

  ```
  ── run 3 · make test · exit 2 · 12.3s · 41 lines ──
  │ ok   example/app/core  0.41s
  ! --- FAIL: TestCheckout (0.02s)
  ! FAIL
  (38 lines above · out 3)
  ```

  `approve_tail_lines` sets how many lines the tail shows: from
  `--approve-tail-lines`, then `HOSTRUNNER_APPROVE_TAIL_LINES`, then the
  common part of the host settings file, then 3. The session takes it as
  it starts and keeps it for every daemon it serves. With `0` the summary
  line is printed alone, without `(<K> lines above · out N)`: the summary
  counts the lines already, and `out N` still prints the run's kept
  output, which does not depend on the tail. A negative, non-numeric or
  fractional value, or a quoted number in a settings file, fails
  `hostrunner approve`, naming its source
  (`HOSTRUNNER_APPROVE_TAIL_LINES: must not be negative, got -1`,
  `settings file <path>: approve_tail_lines: invalid integer "many"`,
  `settings file <path>: approve_tail_lines: invalid integer 2.5`,
  `settings file <path>: approve_tail_lines: invalid integer "3"`,
  `--approve-tail-lines: must not be negative; usage: …`); a non-numeric
  or fractional flag value is refused by the flag package, which names
  the flag (`invalid value "many" for flag -approve-tail-lines: parse
  error`). In the workspace settings file or a workspace section the key
  is an error, as `runtime_root` is.

  The output is what the container got, the rule's secrets masked. A
  tail line longer than 200 characters is cut as an argument of the
  `cmd` line is, `…(+<N> bytes)` counting the bytes left out. Each line
  is shown sanitised, so no output can steer the terminal: SGR (colour)
  sequences are removed; a `\r` right before the `\n` ending a line
  belongs to the ending (a CRLF line shows whole), and otherwise the
  text before a line's last `\r` is dropped, as the terminal would
  overwrite it; every other character that is not printable is escaped
  on its own with its Go escape (`\x1b`, `\a`, `\u200b`, `\xff` for a
  byte of invalid UTF-8), with no quotes around the line, while a tab is
  shown as it is. An escape counts as one character of the 200; the
  bytes counted are those of the text, SGR sequences not included (a
  sequence of the SGR form longer than 64 bytes is not taken for one and
  is shown as text). Of each line the session holds only the first 200
  characters of text after its last `\r`, removing SGR sequences as they
  arrive, and counts the bytes of the rest.
- The summary of a run that ended before its answer was confirmed
  follows the answer's line, with its tail; any other is a notice (see
  Notices), printed when the run ends, or held while a request is on
  screen.
- Kept output: the session keeps, in its memory only, the last 64 KiB
  of the output of each of its last 20 runs, by number, across the
  daemons it serves; a run's output is kept from its start, before it is
  numbered too, and an older run is dropped with its output. At the idle
  prompt, `out N` prints run N's line as `runs` lists it, then, when
  output was dropped beyond the 64 KiB kept, `(<B> bytes dropped: out
  keeps a run's last 64 KiB)` (`1 byte` in the singular), then the lines
  kept, of a run still going those that have arrived so far. The lines
  are whole: where the 64 KiB cut falls inside a line, of either stream,
  the rest of that line is dropped too and counted in `<B>`. They are
  shown as the tail shows its lines, of stdout and stderr in the order
  their first bytes arrived, marked `│ ` or `! `, sanitised alike, but
  not cut at 200 characters: a line is at most the 64 KiB kept. `out`
  prints the run with the highest number kept, going or ended:

  ```
  > out 3
  ── run 3 · make logs · exit 0 · 12.3s · 920 lines ──
  (1234 bytes dropped: out keeps a run's last 64 KiB)
  │ first line kept
  ! warning: …
  │ done
  ```

  `runs` lists the runs kept, the oldest first, one line each: an ended
  run's summary, and for a run still going `running` in place of the
  exit code, with its duration so far, counted from its start line's
  arrival (less the time it had gone for a run that started before the
  session; see Hand-over), and its lines so far; a run whose end never
  came because the session stopped serving its daemon (it left it for
  the list, the daemon went away, or a write to it failed) shows `end
  not received`, with its duration until then:

  ```
  > runs
  ── run 3 · make logs · exit 0 · 12.3s · 920 lines ──
  ── run 4 · make test · running · 3m07s · 41 lines ──
  ── run 5 · make serve · end not received · 1h02m · 7 lines ──
  ```

  `out` and `runs` before any run print `no runs yet`; `out` with an
  argument that is not a number prints `usage: out [n], n from runs`;
  `out N` for a number no run has prints `run N: no such run`, and for a
  number in the range of runs no longer kept, whether its run started or
  not, `run N: output dropped`.
- The output lives in the session's memory only; the daemon keeps none,
  and its log gains none.

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
- A daemon of another build version, or one too old to report it: listed
  marked, served after a warning that names both versions and `hostrunner
  restart`.
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
  command one let through keeps running, the next session to serve the
  daemon learning of it (see Hand-over), and the next run of such a
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
  reaches into the daemon's internals or the session's state, but for
  the daemon-internal tests (`internal/daemon/*_internal_test.go`),
  which check frame orders and races the client harness cannot drive.
- The client tests' harness (the real client against the real daemon over
  a temporary socket) gains the approval socket and a scripted approval
  session, so one test plays the agent and the human. Every behavior above
  is checked there, by exit codes, output, markers in the workspace, the
  rules file and log lines. A fake daemon there stands in for one older
  than a session (no build version, no approved runs) and for frame
  orders a real daemon gives only by chance; a session's output that the
  test stops stands in for a terminal stopped while a run goes; a
  pseudo-terminal stands in for the human's terminal where a prompt
  would discard what is typed on it (a command typed in part).
- The Rules test's tests: every reason and the class marks.
- e2e: from inside the container the approval socket is unreachable, and
  `--ask` without a session is refused at once with 126.
- No new seams beyond these; the rules engine, protocol and socket are
  covered through the client harness, apart from where existing unit tests
  of a touched module must stay accurate and from unit tests of pure text
  helpers (cutting and sanitising lines: `internal/textcut`, the output
  lines of `internal/hostterm`), as `internal/textcut/textcut_test.go`
  and `internal/hostterm/output_test.go` have.

## Out of scope

Desktop notifications
([#83](https://github.com/kravlab/hostrunner/issues/83)); dialogs and
notification actions as the answer channel; backward compatibility with
other approval protocol versions; a switch to turn approvals on or off in
the rules file, a `hostrunner up` flag or the environment, and any opt-out
for a workspace; persisting standing approvals across daemons; rereading
manually edited rules without a container start, but to add a rule with
the human's consent.
