# Spec: Dry run and Rules test

Ticket: [Dry run and Rules test for hostrun commands](https://github.com/kravlab/hostrunner/issues/37),
split into [#38](https://github.com/kravlab/hostrunner/issues/38),
[#39](https://github.com/kravlab/hostrunner/issues/39),
[#40](https://github.com/kravlab/hostrunner/issues/40) and
[#41](https://github.com/kravlab/hostrunner/issues/41).
Vocabulary in [CONTEXT.md](../../CONTEXT.md).

## Goal

Test a command without running it. In the container, an agent learns
whether `hostrun` would let a command through without side effects on the
host. On the host, a rule author learns whether a rules file allows a
command without restarting the container.

## Dry run

`hostrun --dry-run <command> [args...]`, in the container.

- `--dry-run` is hostrun's only when it is the first argument; the rest is
  the command's argv. Anywhere else it is the command's own
  (`hostrun git push --dry-run`).
- No command after it: `hostrun: usage: hostrun [--dry-run] <command>
  [args...]`, exit 125.
- The daemon puts the command through every check a run meets before the
  command starts, in the same order: the rules, the working directory,
  the path checks (`path: open` files are opened and closed again), then
  the program, looked up as starting it would (a name in the daemon's
  `PATH`, a path from the working directory; a directory is refused as
  execve refuses it).
- Allowed: `hostrun: dry run: allowed by rule "<command>"` on stderr,
  exit 0. `<command>` is the rule's command, the longest matching one.
- Refused: the message and exit code a run of the same command would get
  (126 refused, 127 not on the host, 125 hostrun failed), without host
  paths.
- The command never starts, and stdin is not read.
- The daemon logs every dry run with `dry_run=true`: allowed ones as
  `dry run allowed` (argv, rule, host directory), refused ones as
  `request rejected`, including malformed requests.

## Protocol

- New frame types: `FrameDryRun` (client → daemon, JSON `Request`) and
  `FrameAllowed` (daemon → client, JSON `{"rule": "<command>"}`). A
  refusal is `FrameError`, as for a run. `Version` stays 1.
- A frame type rather than a field in `Request`: a daemon older than dry
  runs rejects the unknown frame type (exit 125), where it would ignore an
  unknown field and run the command.

## Rules test

`hostrunner test [--config <path>] [--] <command> [args...]`, on the host.

- Reads `.devcontainer/hostrun.yaml` under the current directory, or the
  `--config` file.
- Its flags end at the first argument that is not one of them, or at
  `--`; the rest is the command.
- Tests the command against the rules alone: not the working directory,
  path checks or the host's programs.
- Allowed: `hostrunner: allowed by rule "<command>"` on stderr, exit 0.
- Denied: the daemon's reason (`hostrunner: denied by rule …`, `hostrunner:
  no rule allows …`), exit 126.
- Exit 1, with the error: no command (usage), a missing file (unlike the
  daemon, which denies everything without one), or an invalid file
  (`rules file: <path>: …`, as `hostrunner up` reports it).

## Docs

README (both features, the exit codes table), the hostrun section of
AGENTS.md (the Dry run and its exit codes), `hostrunner`'s and `hostrun`'s
package docs and usage text.

## Tests

- Dry run, at `client.Run` against an in-process daemon: allowed, with the
  rule and the command not run; every refusal equal to the run's (rules,
  no rule, working directory, path checks, program missing, not
  executable, a directory) and free of host paths; a `path: open` file
  checked and the script it names not run; no command; `--dry-run` after
  the program; stdin not read; an old daemon's refusal; the log lines,
  for a malformed request too.
- Rules test, at `run` of `cmd/hostrunner`: allowed, denied, no rule,
  default file, `--config`, missing file, invalid file, no command, `--`.

## Out of scope

A dry run of `hostrunner up` or of arming; working-directory, path and
program checks in the Rules test; explaining every rule considered;
machine-readable output; any change to real runs.
