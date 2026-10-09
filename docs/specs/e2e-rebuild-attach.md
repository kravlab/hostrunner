# Spec: e2e waits for the daemon to attach after a rebuild

Superseded by [watch-armed-container.md](watch-armed-container.md): the
daemon now sees a container that stops before any poll, so the e2e test
stops the rebuilt container without waiting, and `waitAttached`,
`attachedSinceRearm` and `attachTimeout` are gone.

## Problem

`TestDevcontainerIntegration/docker/daemon_exits_after_the_container_stops`
fails intermittently in CI ("daemon still serving … 1m0s after the
container stopped"), and `restart_brings_a_new_daemon` then fails because
`up` reused the lingering daemon.

Causal chain:

1. The daemon exits on an absent container only once its watcher is
   attached (`internal/watch`, `Run`).
2. The watcher attaches on a poll that sees the container (every 2 s),
   once Grace (15 s) has passed since the rearm. The rebuild takes about
   20 s after the rearm, so Grace has expired when the container appears;
   only the poll remains.
3. The rebuilt container lives about 1–2 s: the rebuild subtest ends right
   after `hostrun pwd`, and the next subtest stops it at once.
4. That window can fall between two polls; the watcher never attaches and
   waits for `StartupTimeout` (30 min).
5. Root cause: the test stops the container without waiting until the
   daemon has observed it. Polling cannot see a container that lives
   shorter than the poll interval; the test assumed it could.

## Change: `e2e/devcontainer_test.go` only

- `attachTimeout = 30 * time.Second`, next to `stopTimeout`, bounds
  `waitAttached` (which runs once `up` has returned and `hostrun` works):
  a few 2 s polls, plus what is left of Grace if the container came up
  sooner than that after the rearm.
- `daemonLog` reads the runtime dir's `daemon.log`; `daemonStarts`
  (`msg=listening`) counts in it.
- `attachedSinceRearm(log)`: the last `msg="devcontainer is running"`
  comes after the last `msg="rearmed: waiting for the devcontainer"`
  (both logged by `internal/watch`, `slog.TextHandler` format). The last
  rearm is the latest `up`'s, so this means the watcher attached after
  that `up`. (The watcher follows the workspace label, not a container
  ID; attaching after the rearm is what makes it notice the next stop.)
- `eventually(t, timeout, cond, describe)`: polls `cond` every 500 ms and
  fails with `describe()` after `timeout`. Used by `waitAttached` and by
  `daemon exits after the container stops`, which had the same loop
  inline.
- `waitAttached(t, runtimeDir)`: `eventually` over `attachedSinceRearm`
  with `attachTimeout`; the failure message includes the daemon log.
- `daemon survives a rebuild slower than its grace period` calls
  `waitAttached` after `hostrun` works, so it also checks that the daemon
  attaches to the rebuilt container. Only then does the next subtest stop
  the container.

`internal/watch` does not change.

## Product limitation: tracked separately

A container stopped within about one poll interval of a rebuild is
never observed, and the daemon stays until `StartupTimeout`. A GitHub
issue (`needs-triage`) records it with the repro and the options
(attach to a new container ID at once; `docker events` instead of
polling).

## Edge cases

- A watcher regression that stops attaching after a rebuild fails in
  `waitAttached` with the daemon log, not 60 s later at the stop.
- Ordering, not counting: the log already holds attaches from earlier
  `up`s, and a late attach of a previous daemon to the old container must
  not count.
- A log without any rearm: an attach counts (`LastIndex` of the rearm is
  -1).

## Tests

- Unit, `TestAttachedSinceRearm` (runs with the e2e tag, needs no
  containers): empty log, attach without rearm, rearm without attach,
  rearm after attach, attach after the last rearm, rearm after the last
  attach.
- `go vet -tags e2e ./...`.
- `mise run e2e` on a host with Docker (with approval).
- `e2e` is green on this fix's pull request; afterwards `e2e` is re-run
  on PR #10.
