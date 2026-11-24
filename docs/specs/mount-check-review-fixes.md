# Spec: fixes from the review of the per-request mount check

Tickets: [#185](https://github.com/kravlab/hostrunner/issues/185) and its
sub-issues #189, #190, #191. Vocabulary in [CONTEXT.md](../../CONTEXT.md)
(Mount check, Mount requirement, Live mount table, Notice).

## Goal

A review of the branch that adds the per-request mount check and mount
requirements found defects in behaviour, in documentation and in the
structure of the code. This spec fixes what each one becomes. Behaviour
not named here does not change.

## Behaviour

### 1. `mounted` through any mount that reaches the path

A `mounted` requirement for path `rel` holds in a container when, for
every counted mount of that container that reaches the resolved host path
`root/rel` (its resolved source is that path or one of its ancestors: the
workspace root, a parent of it, or a directory between the root and the
path), the container path the host path is reached at
(`dest` joined with the host path relative to the source) is a mount
point of its own in the live mount table. A mount that a deeper mount of
the same container covers at that container path does not reach it. A
container with no mount reaching the path passes, as today.

So a container that mounts a parent of the workspace read-write without a
mount of `.git` onto itself fails `.git: mounted`, with
`*NotMountedError` naming the container path under the parent mount.

Tests (`internal/mountcheck`): a parent of the workspace mounted, no `.git`
mount: fails, container path under the parent mount; the same with `.git`
mounted at that path: passes; a mount of `.git` itself: passes; a parent
mount whose workspace path a deeper mount covers: not judged through it.
Cmd level: `hostrun git status` refused 126 for a container mounting the
workspace's parent without a `.git` mount.

### 2. The reason logged for an approved run that does not start

An approved run refused by the check right before its start is logged as
`approval not run` with reason `mount check failed` only when the check
failed or is pending. When the daemon stops while the check runs
(`drainingRefusal`), the reason is `daemon draining` (`endedDraining`).

Tests (`internal/daemon` or client level): an approved run whose final
check is cut short by a stop logs reason `daemon draining`, not
`mount check failed`; a failed check still logs `mount check failed`.

### 3. Mount requirements of every rule whose command starts the argv

The mount requirements of a request are those of every rule whose
command is a prefix of its argv, allowing or denying, not only those of
the rule selected: a `git` rule's `mounts:` also bind `git commit amend`
when a longer `git commit amend` rule selects it. A path two rules name
with different modes must meet both. `rules.Allowed.Mounts` (and the
`Approved` of a `Denial`) hold this union, sorted by path then mode; each
requirement names the rule that declares it (`MountRequirement.Rule`).

A failed requirement refuses the commands of the rule that declares it,
and its refusal, log line and notice name that rule. `hostrunner test` and
the dry run report the union, each requirement with its rule when that is
not the selecting rule.

The README (Rules, `mounts:`) and `docs/specs/rules-engine.md` say that a
longer rule inherits the requirements of shorter ones. `CONTEXT.md`
**Mount requirement** says a longer rule's requests are held to it too.
ADR 0011
(`docs/adr/0011-mount-requirements-on-the-live-mount-table.md`) says that
a rule's requirements are checked for every request whose command starts
with that rule's command, not for that rule's command only.

Tests (`internal/rules`): union of a shorter and a longer rule; same path
two modes; a deny rule's requirements; the order. Cmd level:
`hostrun git commit amend` refused 126 when only the `git` rule names
`.git: mounted` and it is not met; `hostrunner test` output names both
rules' requirements.

### 4. A running container without a process id

A container the runtime reports running with process id 0 (restarting) is
not taken as stopped: the per-request check inspects it again once, as
for a gone process, and when it still has no process id its `Live.Err`
says `container runs but reports no process id`, so every command is
refused (126, mounts unreadable) until a later inspection finds its
process.

Tests (`internal/watch`, fake CLI and `watchtest`): running with pid 0,
then pid set: the request after the fresh inspection runs; still pid 0:
`Live.Err` set, request refused. `watchtest` gains `Runtime.Restarting`
(a running container with no process id), and its `MountTable` answers
only for a container that runs with that process id, for these tests.
`docs/specs/watch-armed-container.md` states the behaviour.

### 5. The contract of the requirements stage

The doc of `daemon.MountCheck` states that when its error is nil the
requirements stage it returns is never nil. No nil guard is added: a nil
stage is a programming error.

### 6. A dry run checks the live mounts when it arrives

A dry run makes the same arrival check as a run (the live check with the
requirements of the rule the rules select for its argv, before the rules'
checks), then the last check right before it answers, as today. A dry run
the rules refuse is refused by the arrival check first when the mounts
fail.

The README (Mount check) says so; `TestRequestMountCheckOfADryRunIsItsLastCheck`
becomes a test of both checks.

### 7. This repository's own rules

The integration step (PR description, or the commit message of a direct
push) states that this repository has no rules file of its own, so #191's
"This repository's rules drop -u/--set-upstream" is met by the README and
`docs/specs/rules-engine.md` examples.

### 8. Behaviour beyond the issues

The integration step lists what the branch adds beyond #189 and #190:
YAML aliases for `mounts:`; `.` and duplicate paths rejected; exit 125
when the daemon stops during a check; armed containers that do not run
inspected again at most once per `StoppedInspectionMaxAge` (item 22).
Editing the text of #189 and #190 is asked for separately at that step.

### 9. Unrelated commits

Commits `c5d458d` (fix(launch): say why a shared add timed out) and
`fcd910f` (test(watch): set mounts before starting containers) move to a
new branch `fix/launch-and-watch-flakes` from `main`, and are removed from
`feat/mount-check-per-request` by a rebase. Updating the pushed
`feat/mount-check-per-request` needs a force push, asked for separately.

## Documentation

10. `CONTEXT.md`: `_Avoid_: read-only check, writable warning` returns
    under **Mount check**; **Live mount table** keeps only its own.
11. `README.md`, exit code table, 126: adds "the commands of a rule whose
    mount requirements the container does not meet".
12. Comments wrapped mid-line are reflowed: the doc of
    `mountcheck.Check`, `watch.Config.Attached`,
    `protocol.MountCheckFailed`, `mountState.recheck`.
13. `cmd/hostrunner/main.go`: the paragraph "Without Reread they take
    effect…" goes back to its text on `main`.

## Structure

No behaviour changes; the existing tests stay green.

14. One reading of a container's mounts per request: `mountcheck` gains a
    step that checks `MountsErr` and `Live.Err`, counts the live mounts and
    resolves their host sources once per container; `Check` and
    `CheckRequirements` both judge its result, and the live check in
    `cmd/hostrunner` reads it once for both stages. Errors and their order
    stay as today.
15. `FrameMountCheckFailed` is built and written by one function, used by
    `tellMountCheckFailed`, `mountCheckFailed` and the session catch-up in
    `approvals.go`.
16. `readAttached` and `readRequested` share one loop over the armed
    containers; what differs (the stale branch) is its parameter.
17. `mountFailureKey` names its stage in its own field (protected paths
    or requirements), not by `rule == ""`.
18. `Server.start` takes its request data in one struct (`startArgs`)
    instead of eight parameters.
19. `wireMountCheck` no longer takes `**daemon.Server` and
    `**watch.Watcher`: it returns its options and a `bind` function the
    caller calls once both exist.
20. Names: the local variables and parameters named `ins` in `watch.go`
    become `inspected`, and the receivers of `inspection`'s methods
    become `in` (golang-naming: receivers of one or two letters);
    `watch.Mount.Point()` becomes `PointPath()`, matching
    `MountPoint.Path`.
21. `watch.go`: reading live mount tables (`readTable`, `readAttached`,
    `readRequested`, `Live`) moves to `mounttable.go`.
22. `watch.Config` gains `StoppedInspectionMaxAge`, how old an inspection
    that found an armed container stopped may be before the per-request
    check inspects it again; zero means `PollInterval`. `PollInterval`'s
    comment keeps its first meaning only.
    `docs/specs/watch-armed-container.md` states it. The setting
    `stopped_inspection_max_age` (`HOSTRUNNER_STOPPED_INSPECTION_MAX_AGE`,
    `--stopped-inspection-max-age`, a duration of the daemon, default 0
    meaning `poll_interval`) sets it, listed in the README settings table.

## Out of scope

The findings rejected in the review assessment (the e2e workspace helper
flag, the two `MountRequirement` types, `rejecter`/`mountRefused`, the
claim that a container without a live table is not checked).
