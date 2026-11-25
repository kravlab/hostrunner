# Spec: the daemon follows its armed container

Ticket: [Spec: the daemon follows its armed container by start time](https://github.com/kravlab/hostrunner/issues/50)
(from [#12](https://github.com/kravlab/hostrunner/issues/12)).
Decision record: [ADR 0005](../adr/0005-armed-container-by-start-time.md).
Changes the watcher of [devcontainer-integration.md](devcontainer-integration.md)
and supersedes the workaround in [e2e-rebuild-attach.md](e2e-rebuild-attach.md);
vocabulary in [CONTEXT.md](../../CONTEXT.md) (Lifecycle).

## Goal

After an arm the watcher ignored what it saw for a grace, so a container
that came up and stopped within that window, or within one poll after it,
was never attached to, and the daemon stayed until its startup timeout. The
daemon now follows its armed container, recognised by start time.

## Runtime

- `Runtime.Containers(ctx, workspace) ([]Container, error)`: the
  workspace's containers in every state; `Container{ID, Running, Pid,
  StartedAt}`. An empty list is an answer (absent); an error means the
  runtime did not answer (unknown).
- The CLI runtime runs `<rt> ps -aq --filter
  label=devcontainer.local_folder=<workspace>` and, only when that lists
  something, `<rt> inspect --type container --format '{{.Id}}
  {{.State.Running}} {{.State.Pid}} {{json .State.StartedAt}} {{json
  .Mounts}}' <ids…>`. `json` renders docker's string and podman's
  `time.Time` alike, in RFC 3339. `Pid` is the host process ID of the
  container's init process, 0 when it does not run.
- `Runtime.MountTable(ctx, pid) ([]MountPoint, error)`: the live mount
  table of process `pid`, read from `/proc/<pid>/mountinfo` on the host
  (docker and podman alike), in file order; `MountPoint{Path, ReadOnly}`,
  read-only by the per-mount options. A process that is gone fails with
  `ErrProcessGone`.
- A container removed between the listing and the inspection fails the
  inspection with "no such container" on stderr while the others are still
  printed: it is absent. Any other failure is the runtime's.
- Containers of all answering runtimes are merged.
- For the mount check, the volumes the containers' mounts name are
  inspected once, all together: `<rt> volume inspect --format '{{json
  .}}' <names…>`, one JSON object per line whose `Name`, `Driver` and
  `Options` docker and podman name alike. A volume it does not report
  (one removed meanwhile, which makes the inspection fail with `no such
  volume`), and a volume mount that names no volume, carries an error on
  its mount instead; the mount check fails on it.

## Watcher

- `Arm()` takes the arm's time when it is called, i.e. before the daemon
  answers `FrameArm` and so before devcontainer starts or removes a
  container. Creating the watcher counts as an arm and is logged as one.
- Waiting → attached when a poll finds the armed container:
  - a container whose `StartedAt` is after the arm, running or stopped;
  - otherwise, a grace or more after the arm, a running container (`up`
    on a running container, which nothing restarts).
- Attached: the watcher keeps the IDs it found, adding any container
  that starts after the arm later (a recreated Compose container). Present
  while one of them runs; another container of the workspace does not
  count. The daemon exits after absence observed for a grace, counted from
  the attach at the earliest, so also a grace after a first poll that saw
  the armed container already stopped. Unknown resets the observed
  absence.
- On each attach or join, before `Attached`, every armed container that
  runs (`Running`, `Pid` > 0) gets `Live`, its table read through the
  runtime that reported it. A process gone meanwhile: one fresh query of
  every runtime, then the fresh data, read once more if it runs.
  One that runs without a process id gets no `Live` there, as one that
  does not run; the first `Live` call judges it.
- `Live(ctx)`: copies of the latest attach's armed containers, each as
  the newest inspection of it reported (the attach's, a later poll's or a
  `Live` call's); `ErrNotAttached` from an arm until its attach. Each that
  runs has its table read now. A gone process is inspected again at once.
  One that runs without a process id (`Running`, `Pid` 0: restarting) is
  not taken as stopped: it is inspected again once, whatever the age of its
  inspection, and when it still has no process id its `Live.Err` is
  `ErrNoProcessID` (`container runs but reports no process id`), so the
  check refuses until a later inspection finds its process. One that did
  not run is left out: it is inspected again only when its inspection is
  at least `StoppedInspectionMaxAge` old (zero: `PollInterval`; reported
  as it runs now, or left out if it does not; one no runtime could
  inspect carries the error), otherwise left out without a query. The
  fresh data is kept for the next call, unless an entry holds a newer
  inspection.
- `Run`'s polls that change no armed set refresh the cache (`takePoll`):
  each armed container of the latest attach takes what the poll reported,
  so `Live` follows the runtimes without inspecting a container that
  stopped more than once per `PollInterval`. One the poll no longer
  reports keeps its data, inspected at the poll's time. An entry with a
  newer inspection, a `Live` call's that started after the poll, is kept
  as it is; a poll for an earlier arm changes nothing. Safe alongside
  `Run`.
- Unchanged: the startup timeout from the last arm while waiting, the
  runtime-unavailable exit while attached, the flags and their defaults.
- Logs: `armed: waiting for the armed container`, `attached to the armed
  container`, `armed container stopped`.

## Tests

- `internal/watch`, fake runtime with real start times: a container that
  starts after an arm and stops before any poll; one that stops within a
  grace of the arm; `up` on a stopped container (same ID started again,
  then stopped); `up` on a running container; a rebuild; a restart shorter
  than a grace; a container that starts and stops between two polls after
  a long build (the grace still applies); another running container that
  does not keep the daemon; no container; failing, hanging and several
  answering runtimes.
- `internal/watch`, fake CLI binary: listing in every state, no inspection
  without containers, start times with an offset, a container gone before
  the inspection, another inspection failure, a failing binary.
- `cmd/hostrunner`: `serve --watch` with an injected runtime.
- Both use `internal/watch/watchtest.Runtime`, a fake runtime whose
  containers a test starts, stops and removes, with real start times.
- e2e (docker; podman opt-in): the rebuilt container and the restarted
  container are each stopped right after `up`, without waiting for the
  daemon to attach, and the daemon exits.

## Out of scope

`docker events`/`podman events`; a container run with `--rm` that stops
before a poll sees it (documented in the README); containers started
outside the devcontainer tooling; non-Linux hosts.
