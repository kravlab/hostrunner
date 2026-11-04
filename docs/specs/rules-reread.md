# Spec: reread the rules file when it changes

Ticket: [Reread the rules file when it changes](https://github.com/kravlab/hostrunner/issues/107).
Vocabulary in [CONTEXT.md](../../CONTEXT.md) (Lifecycle: reread, restart,
step aside).

## Goal

A daemon serves the rules it loaded at start. A rules file edited by hand
on the host (an editor, `git checkout`, `git pull`) used to take effect
only at the next `hostrunner up`, which replaced the daemon, or after
`hostrunner restart`; both end every standing and session approval. Now
the daemon rereads the file by itself once it changes, without stopping.

## Behavior

- Every 2 s (the container watcher's interval) the daemon looks at the
  rules file by its path, links followed (stat: device, inode, size,
  modification and change time). When that changed, it reads the file;
  content equal to the rules it serves changes nothing. The first look
  reads the file, so an edit made between the load and the start of
  `serve` is not missed.
- A reread replaces the rules the daemon serves, as `hostrunner up` loads
  them: requests checked afterwards (runs, `--ask`, dry runs) see the new
  rules; running commands, the approval session, both sockets and
  standing and session approvals stay. The digest arms compare is the
  new one, so the next `up` arms the daemon instead of replacing it.
- Rereads are one at a time: one the session asks for after adding a
  rule and one of the daemon's own never interleave, so a slower one
  never replaces newer rules.
- Logged as `rules reread` with the file and the digest.
- A file that does not load (invalid, unreadable): the old rules stay,
  logged as `rules reread failed: the old rules stay` with the error,
  once for that content; touching the file without changing it reports
  nothing more. The next content that loads is reread.
- A missing file loads as the rules that deny every command, as at start.
- A file that changes again while it is reread is left to the next look.
- Only content allowed is reread (issue #139): before a reread the daemon
  checks the content's digest in the allow store, for its workspace and
  its daemon settings, as `hostrunner up` checks it. A missing file is
  content like any other, of digest `missing`: until it is allowed the
  old rules stay. Content not allowed keeps the rules served, logged as
  ``rules changed but not allowed: the old rules stay until `hostrunner
  allow` `` with the file and the digest, and the session gets the error
  ``its content (digest <digest>) is not allowed; run `hostrunner allow`
  on the host``. The daemon also records an allow request, as a refused
  `up` does, with its own rules file, the digest and its daemon
  settings, so `hostrunner allow` without flags allows exactly what the
  daemon checks, settings flags `up` was given included; a request that
  cannot be recorded is logged as `allow request not recorded` and
  changes nothing else. An allow store that cannot be read allows nothing:
  logged as `rules change not checked: the old rules stay` with the
  error, the session getting it too.
- Each refusal is reported once each time the file changes to that
  content: the daemon remembers the one refusal pending, its content and
  its reason, for its own looks and the session's `a` alike. The same
  content refused for the other reason (not allowed, or not checked) is
  reported again; so is content refused before, once another content
  was refused in between (B, C, B reports B twice) or the file went back
  to the rules served.
- While a refusal is pending the daemon checks the allow again at every
  look, the file changed or not, so after `hostrunner allow` it rereads
  the file at the next look (`rules reread`, approvals kept). New rules
  served, by the daemon or a session's reread, end the refusal. The file
  going back to the rules served ends it too, logged as `rules change
  undone: the file holds the rules served`, without a notice; not when
  a session's reread just served the pending content.
- While a refusal for content not allowed is pending, every `hostrun`
  call of the daemon (issue #140) prints one line on stderr,
  `hostrun: warning: <text>`, naming the digest and saying a human on
  the host runs `hostrunner allow` (when the allow cannot be checked:
  looks at the daemon log). It changes neither output nor exit code and
  ends once the content is allowed or the refusal ends. The daemon sends
  the text as the optional `warning` of the first answer of each
  exchange (the initial credit, waiting, allowed and error frames); an
  older `hostrun` ignores it, an older daemon sends none.
- `up` keeping a daemon (rules or settings not allowed, the daemon
  serving allowed ones) reports what is not allowed in `Arm`
  (`not_allowed`: the digest and the daemon settings `up` found not
  allowed, which the daemon compares with its own to choose the text);
  the daemon warns the same way until the allow store allows it, a later
  arm reports none, or, with the same settings, the rules file goes back
  to the served content. Undoing a settings change alone is seen only at
  the next `up`. Both sources pending: both texts on one line, joined by
  "; ". An older daemon ignores `not_allowed`.
- Content allowed that does not load is a file that does not load, as
  above.

### Approval session

- A session says in `Serve` (`notices`) that it reads `FrameRulesReread`;
  the daemon sends that frame, with the new digest or the error, only to
  the session serving it that said so. An older session, which takes an
  unknown frame for an error, gets none.
- The session prints `the rules file changed; the daemon reread it` or
  `the rules file changed, but the daemon cannot reread it, so the old
  rules stay: <error>`, a notice, printed as the host approvals spec
  says (held while a request is shown, at once at the idle prompt). It
  keeps the new digest for later proposals at once.
- `DaemonInfo` says `rereads` (the daemon rereads by itself) or
  `no_reread` (it was started with `--no-reread`); an older daemon says
  neither.
- Adding a rule (`a`) after a hand edit the daemon has not reread yet: a
  daemon with `rereads` is asked to reread at once and the diff is against
  the rules it then serves, without the `[r]eread [c]ancel` question; for
  any other daemon the question stays. A hand edit not allowed is not
  reread: the session says it cannot add a rule, with the daemon's
  error, and asks for the answer again; the old rules stay. A rule the
  session writes is reread as before (issue #141).

### Commands

```sh
hostrunner up    [--restart [--kill | --wait]] [--no-reread] --dir … --workspace … --container-workspace …
hostrunner serve --socket … --workspace … --container-workspace … [--rules-file …] [--watch] [--no-reread]
```

- `--no-reread`: the daemon does not reread the rules file by itself;
  `up` passes it to the daemon it starts.
- `Arm` carries the `up`'s `--no-reread` (`no_reread`); a daemon whose
  own differs steps aside, as for changed rules, and `up` starts one with
  the `up`'s setting. A daemon older than the field does not compare it.
- `restart` takes the new daemon's `reread` from the settings it
  resolves for the workspace, as `up` does (see
  [daemon-stop-restart.md](daemon-stop-restart.md)), not from
  `DaemonInfo`. `up --restart` takes the setting from its own flags.

## Edge cases

- An editor that saves by rename, a symlink swapped for one to another
  file, the file a symlink names edited in place: the stat of the path,
  links followed, changes, the file is reread.
- A save of invalid content, then of valid content: one warning, then a
  reread.
- A rule the session adds: the session's reread serves it; the daemon's
  next look finds the content it serves and does nothing. A look that
  read the file just before the session's reread waits for it, then
  finds the rules served and does nothing either: no second log entry,
  no notice.
- An edit not allowed, touched without changing, or met again by `a`:
  nothing more is reported.
- A deleted rules file while allows are checked: the old rules stay
  until the missing file is allowed, then every command is denied.
- With `--no-reread` the daemon does not look at the file, so nothing
  changes: allowed new content takes effect at the next `up`.
- `.devcontainer/` is mounted read-only in the container, so only the host
  changes the rules; `hostrun` warns when it is not.

## Tests

- `internal/client`, against a real daemon with a short interval: a hand
  edit allows a refused command and is logged with its digest; a file
  replaced by rename; a symlink swapped; the file a symlink names edited
  in place; an invalid file keeps the old rules, is logged once even
  when touched, and a valid save is reread; a
  deleted file denies everything; a running command survives; standing
  and session approvals survive; a dry run answers under the new rules;
  an arm after a reread gets the new digest; a daemon without rereading
  leaves a hand edit out; `DaemonInfo` says `rereads` or `no_reread`; the
  session prints both lines; a session without `notices` gets no frame;
  `a` after a hand edit rereads without asking. With a real allow store:
  an edit not allowed keeps the old rules, is logged once even when
  touched, and the session prints the notice; once allowed it is served
  and approvals stay; going back to the served content is logged and the
  same edit again is reported again, with no notice of the undo; an
  allow check that fails keeps the old rules, is logged once and is
  retried; the same content refused for the other reason is reported
  again; `a` after a hand edit not allowed adds no rule, and `a` after
  the daemon reported it logs nothing more; a session's reread of the
  pending content is not logged as undone; the allow request is
  recorded once per refusal, and one that fails is logged. The warning
  (`rereadwarning_test.go`, `armwarning_test.go`, `warning_test.go`):
  every call prints it on stderr while a change is pending, from a
  reread and from an arm, without changing output or exit code, and it
  goes once allowed or undone.
- `internal/protocol`: the frame's wire value and the JSON names of
  `DaemonInfo`, `Serve` and `RulesReread`, and of `warning` and
  `not_allowed`.
- `internal/launch`: an `up` with another `--no-reread` replaces the
  daemon, and the next such `up` arms the new one; an `up` keeping a
  daemon reports what is not allowed in the arm.
- `internal/stop`: `restart` takes `reread` from the settings it
  resolves.
- `cmd/hostrunner`: `--no-reread` for `up` and `serve`, in their usages;
  the arm handler steps aside on another setting; `serve`'s hello says
  whether it rereads; `serve` opens the allow store as `up` does (no
  place for it is an error) and rereads a hand edit only once allowed;
  without reread an allowed hand edit waits for the next `up`; a hand
  edit not allowed of a serve given a setting `up` without flags does
  not resolve records the request, and `hostrunner allow` then has it
  served.
- e2e: a hand edit on the host takes effect in a running container
  without a restart.
