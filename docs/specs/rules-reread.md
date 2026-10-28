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

### Approval session

- A session says in `Serve` (`notices`) that it reads `FrameRulesReread`;
  the daemon sends that frame, with the new digest or the error, only to
  the session serving it that said so. An older session, which takes an
  unknown frame for an error, gets none.
- The session prints `the rules file changed; the daemon reread it` or
  `the rules file changed, but the daemon cannot reread it, so the old
  rules stay: <error>`, and shows its prompt again unless it awaits the
  daemon. It keeps the new digest for later proposals.
- `DaemonInfo` says `rereads` (the daemon rereads by itself) or
  `no_reread` (it was started with `--no-reread`); an older daemon says
  neither.
- Adding a rule (`a`) after a hand edit the daemon has not reread yet: a
  daemon with `rereads` is asked to reread at once and the diff is against
  the rules it then serves, without the `[r]eread [c]ancel` question; for
  any other daemon the question stays.

### Commands

```sh
hostrunner up    [--restart [--kill | --wait]] [--no-reread] --dir … --workspace … --container-workspace …
hostrunner serve --socket … --workspace … --container-workspace … [--config …] [--watch] [--no-reread]
```

- `--no-reread`: the daemon does not reread the rules file by itself;
  `up` passes it to the daemon it starts.
- `Arm` carries the `up`'s `--no-reread` (`no_reread`); a daemon whose
  own differs steps aside, as for changed rules, and `up` starts one with
  the `up`'s setting. A daemon older than the field does not compare it.
- `restart` starts the new daemon with `--no-reread` when `DaemonInfo`
  says `no_reread`; an older daemon's successor rereads, the default.
  `up --restart` takes the setting from its own flags.

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
  `a` after a hand edit rereads without asking.
- `internal/protocol`: the frame's wire value and the JSON names of
  `DaemonInfo`, `Serve` and `RulesReread`.
- `internal/launch`: an `up` with another `--no-reread` replaces the
  daemon, and the next such `up` arms the new one.
- `internal/stop`: `restart` keeps `--no-reread`; an older daemon's
  successor rereads.
- `cmd/hostrunner`: `--no-reread` for `up` and `serve`, in their usages;
  the arm handler steps aside on another setting; `serve`'s hello says
  whether it rereads.
- e2e: a hand edit on the host takes effect in a running container
  without a restart.
