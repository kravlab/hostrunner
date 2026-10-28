# Spec: stop and restart a daemon from the host

Ticket: [Stop and restart a daemon from the host](https://github.com/kravlab/hostrunner/issues/105).
Vocabulary in [CONTEXT.md](../../CONTEXT.md) (Lifecycle: stop, restart,
drain, step aside).

## Goal

A daemon ends only about 15 s after its armed container stops, and
`hostrunner up` arms a running daemon instead of replacing it unless the
rules changed. To run a new version after `hostrunner update`, to pick up
a hand-edited rules file or to drop standing approvals, the human has to
stop the container or find and kill the process. Now the human can stop
or restart a daemon from the host, and `up --restart` replaces it at a
container start.

## Commands

```sh
hostrunner stop    [--dir <runtime-dir> | --all | --choose] [--kill | --wait]
hostrunner restart [--dir <runtime-dir> | --all | --choose] [--kill | --wait]
hostrunner up --restart [--kill | --wait] --dir … --workspace … --container-workspace …
```

`--dir`, `--all` and `--choose` exclude each other, and so do `--kill` and
`--wait`; a combination is a usage error.

## Channel

- Stop and restart reach the daemon over its approval socket, outside
  everything the container mounts ([ADR 0007](../adr/0007-host-approval-outside-the-container.md)).
  The container's socket refuses the stop frames like the session frames.
- Stopping needs no approval session and does not take one over. A session
  serving the daemon sees it end and waits for the next one, as today.

## Choosing the daemon

The daemons are those found as `hostrunner approve` finds them: the
approval sockets under `$XDG_RUNTIME_DIR/hostrunner`, each answering a
hello with its `DaemonInfo`.

- Without a flag: the one daemon whose host workspace holds the current
  directory. None, or more than one (two devcontainer configurations of
  one folder, nested workspaces), is an error listing the daemons found,
  each with its runtime directory and workspace; nothing is stopped.
- `--dir <runtime-dir>`: that directory's daemon. A directory with no
  daemon answering on its sockets is an error.
- `--all`: every daemon found; one failing does not keep the others from
  being stopped, and the exit code is non-zero if any failed.
- `--choose`: the list `hostrunner approve` shows, numbered; Enter picks
  the daemon for the current directory. Daemons of every version can be
  chosen (see Older daemons).
- `XDG_RUNTIME_DIR` unset: an error naming `--dir`, as `approve` does.

## Running commands

Before stopping, the daemon is asked what it runs and how many standing
and session approvals it holds.

- Nothing running: the daemon is stopped at once.
- Commands running: each is printed (argv and container directory), then
  the question:
  - `k`: kill them (SIGKILL to each process group, as today) and stop;
  - `w`: drain: the daemon closes both sockets, so new requests fail as
    with a daemon that is gone (hostrun 125, daemon unreachable), answers
    waiting approval requests with 126 `the hostrunner daemon is
    stopping` (the approval session, still connected, drops them:
    `request N ended: the daemon is stopping`), refuses commands not
    started yet, lets the running commands finish and exits;
  - `q`: do nothing; exit 1.
- While draining, Ctrl+C asks again: `k` kills what still runs, `w` keeps
  waiting. A stop or restart that ends (its terminal closed) leaves the
  drain to finish on its own.
- Ctrl+C at a question, or while waiting for the runtime directory's lock,
  ends the whole stop or restart: with `--all`, the daemons not yet
  stopped are left running.
- `--kill` and `--wait` answer up front, in a terminal too. Without a
  terminal on stdin and without either, commands running is an error
  listing them; the daemon is left alone.
- With `--all`, each busy daemon is asked on its own; `q` skips only that
  daemon. Idle daemons are stopped without a question.
- Approvals lost are reported (`2 standing and 1 session approvals end
  with the daemon`).
- Stop returns once the daemon has exited (nothing accepts on its socket
  any more).

## Restart

- The workspace, container workspace and rules file come from the running
  daemon (`DaemonInfo`), the runtime directory from where it was found.
- The rules file is read and validated before anything stops; invalid
  rules are an error and the daemon keeps running.
- The daemon is stopped as above, then a new one is started and armed as
  `up` does, with the installed `hostrunner` (the one `restart` runs as).
- A daemon that saw its armed container stop (attached, the container
  gone at the last runtime answer: it counts its grace and is about to
  exit) is only stopped: `restart` says so, and the next `up` starts a new
  daemon. A daemon still waiting for its container (just armed by an `up`
  that found the container running, or during a rebuild's image build) is
  replaced like a running one: the new daemon is armed and waits the same
  way, so a container that runs, or is about to, keeps a daemon. The
  daemon says whether it saw its armed container stop in its `DaemonInfo`;
  for an older daemon, which does not, `restart` asks docker or podman
  whether a container of its workspace is running.
- Standing and session approvals end with the old daemon; an approval
  session waiting for the next daemon serves the new one.

## `up --restart`

- No daemon running: as `up`.
- A daemon running: stop it as `restart` does (running commands, `--kill`,
  `--wait`), then start a new one. From `initializeCommand` there is no
  terminal, so a busy daemon without `--kill` or `--wait` fails `up`, and
  with it `devcontainer up`, and is left alone.
- Without `--restart`, `up` arms as today; a changed rules file still makes
  the daemon step aside. `--kill` or `--wait` without `--restart` is a
  usage error.

## Older daemons

A daemon whose `DaemonInfo` does not say it accepts the stop request (any
released before this change) is stopped by signal:

- its PID comes from the peer credentials of a connection to its socket
  (`SO_PEERCRED`);
- `/proc/<pid>/cmdline` must be `hostrunner serve --socket <that socket> …`
  and the process must belong to the user; otherwise it is an error and no
  signal is sent;
- it gets SIGTERM, which kills its running commands, as today.

It cannot list its running commands, so the question is `k` or `q`, and
without a terminal `--kill` is needed (`--wait` is an error for it). A
daemon from before approvals has no approval socket: it is found only by
`--dir`, through the container's socket. It cannot be restarted, as it
does not say what it serves (workspace, container workspace, rules file):
`restart` refuses it before stopping anything; `stop` it, and the next
`up` starts a new daemon.

## Exit codes and output

- 0: every chosen daemon was stopped (and, for restart, started again, or
  only stopped as its container has stopped).
- 1: anything else, usage errors included, as the other subcommands: no
  or several daemons for the current directory, `q`, busy without a
  terminal, invalid rules, a daemon that did not exit, a start that
  failed.
- Messages go to stderr, one line per daemon
  (`hostrunner: stopped <runtime-dir>`, `restarted`, `stopped; its
  container is not running`). A stop waiting for another `up` or stop on
  the runtime directory says so once: `waiting for another hostrunner up
  or stop on <runtime-dir>`.

## Edge cases

- A command starting between the question and the answer: `k` kills it
  too; `w` lets it finish.
- The daemon exits by itself while being asked (its container stopped):
  the question is dropped; stop reports it stopped; restart starts no new
  one.
- Two `stop`s or a `stop` and an `up` at once: the runtime directory lock
  `up` takes is taken by stop and restart too, so they run one after the
  other; the second finds no daemon (stop: error for `--dir`, nothing for
  `--all`) or the new one.
- A stop request from the container's socket: refused like session frames.

## Documentation

- README: the new commands in "Use in a devcontainer" (or a "Stop and
  restart" section), `up --restart` in the `initializeCommand` example's
  notes, and the Limitations item about upgrades rewritten.
- `usage` and per-command `-h` text.

## Tests

- The daemon side (`internal/client/stop_test.go`, against a real
  daemon): the stop query lists running commands and approval counts;
  stop with kill ends running commands and returns; drain refuses new
  connections, answers waiting approval requests with 126 and exits after
  the running commands finish; the container's socket refuses stop
  frames.
- `cmd/hostrunner`: flag parsing (exclusive flags, usage errors,
  `XDG_RUNTIME_DIR`, `-h`).
- The stop itself (`internal/client/stopcmd_test.go`, against real
  daemons): choosing by the current directory (one, none, several); `--dir`
  with no daemon; busy without a terminal and without `--kill`/`--wait`
  errors and leaves the daemon running; the daemon exiting while asked.
- Restart (`internal/stop`): invalid rules leave the daemon running; a
  daemon that saw its container stop is only stopped; one waiting for its
  container is replaced.
- Older daemons (`internal/stop`): the PID check refuses a process whose
  command line names another socket; `--wait` is refused; restart asks
  the container runtime whether the workspace's container runs.
- `DaemonInfo` (`internal/watch`) reports the armed container stopped
  while the daemon is attached and last saw it gone, counting its grace;
  not while waiting for it or while it runs.
- `up --restart` (`internal/stop`): with no daemon starts one; with an
  idle daemon replaces it; with a busy daemon and no flag fails and leaves
  it; with `--kill` replaces it.
- e2e: restart picks up a hand-edited rules file in a running container.
