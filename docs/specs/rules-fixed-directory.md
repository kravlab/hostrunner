# Spec: fixed directory

Ticket: [Spec: fixed directory for a rule (dir)](https://github.com/kravlab/hostrunner/issues/43)
(from [#36](https://github.com/kravlab/hostrunner/issues/36)), split into
[#44](https://github.com/kravlab/hostrunner/issues/44),
[#45](https://github.com/kravlab/hostrunner/issues/45),
[#46](https://github.com/kravlab/hostrunner/issues/46) and
[#47](https://github.com/kravlab/hostrunner/issues/47).
Decision record: [ADR 0004](../adr/0004-fixed-directory.md).
Extends [rules-engine.md](rules-engine.md),
[rules-path-check.md](rules-path-check.md) and [dry-run.md](dry-run.md);
vocabulary in [CONTEXT.md](../../CONTEXT.md).

## Goal

A command ran in the mirrored directory, so a tool that reads
configuration from its working directory was steered by the container
whatever the rule said about argv: `tix` probes it with host git and reads
`--repo owner/name` as a path when such a directory exists there, and a
mise shim reads the `mise.toml` above it. A rule can name a fixed
directory for its command to run in instead.

## Schema

```yaml
rules:
  - command: tix
    dir: /                     # an absolute host path
    args: any
  - command: tix api
    dir: inherit               # the dir of the nearest shorter rule
    args: any
```

- `dir` sits on the rule and combines with `args`, `flags`, `positional`
  and `flag_style`.
- An absolute path is cleaned (`/srv/../srv//tix/` is `/srv/tix`); that is
  the directory opened and the one every message names.
- Without `dir` the command runs in the mirrored directory, whatever a
  shorter rule says.
- `inherit` takes the fixed directory of the nearest shorter rule whose
  command is an argv prefix of this one's, after that rule's own
  `inherit` is resolved; file order does not matter.

## Behavior

The daemon's pre-start step, in order:

1. The rules. A denied command is reported as today, whatever its
   directories.
2. The mirrored directory, opened; it must still be inside the workspace.
3. The path checks, resolved from the mirrored directory.
4. For a rule with a fixed directory, the fixed directory, opened on every
   request with `O_DIRECTORY`. It is refused (126) when it does not exist,
   is not a directory, or its real path, symlinks followed, is the
   workspace or below it. It then replaces the mirrored directory as the
   working directory.

The command runs in the opened working directory, and `PWD` is its real
path. Everything opened is closed on a refusal. The daemon log records the
working directory.

`path: open` arguments are handed over as `/proc/self/fd/N`, so the fixed
directory changes nothing for them. `path: check` arguments would be
resolved by the program from the fixed directory, not from the mirrored
directory they were checked in, so a rule with a fixed directory, written
or inherited, cannot use `path: check`.

## Validation errors

- `dir` empty, null, not a string, or relative:
  `dir must be an absolute path or inherit`.
- `dir: inherit` with no shorter prefix rule:
  `dir: inherit needs a shorter rule whose command is a prefix of this one`.
- `dir: inherit` whose nearest shorter rule has no fixed directory:
  `dir: inherit, but the nearest shorter rule "<rule>" has no dir`.
- A fixed directory with `path: check` on positional arguments or on any
  flag value: `dir cannot be combined with path: check (use path: open)`.

Each names the rule.

## Refusal messages

`fixed directory <dir> does not exist`, `… is not a directory`, `… is
inside the workspace`, and `… cannot be opened on the host` for anything
else (e.g. EACCES). `<dir>` is the cleaned path from the rules file, which
the container can read; where it leads on the host goes to the daemon log
only.

## Dry run and Rules test

- The dry run answer (`FrameAllowed`) carries the fixed directory in an
  optional `dir` field, omitted when there is none; `Version` stays 1.
- `hostrun --dry-run` prints `hostrun: dry run: allowed by rule "<rule>",
  runs in <dir>`, and today's line without a fixed directory. A bad fixed
  directory is refused as the run is.
- `hostrunner test` prints `hostrunner: allowed by rule "<rule>", runs in
  <dir>` likewise, with `inherit` resolved. It does not check the
  directory: it tests the rules file alone.

## README

`dir` and `inherit` in the rules reference and the example block, the
`path: check` restriction, the absence of default inheritance, the exit
code table, the dry run and Rules test output. AGENTS.md's hostrun section
says where a command runs and the dry run's `runs in`.

## Tests

- `internal/client`, the client against an in-process daemon: runs in the
  fixed directory (`pwd -P`, `PWD`); a longer rule without `dir` runs in
  the mirrored directory; `inherit` runs in the inherited directory;
  refusals for a missing directory, a file, a directory inside the
  workspace and a symlink into it, without the workspace's host path;
  rules and the mirrored directory checked first; `path: open` resolved
  from the mirrored directory; dry run output and refusals.
- `internal/rules`, parse and check: every validation error above,
  cleaning, `inherit` chains in any file order, no inheritance without
  `inherit`, the fixed directory in the check result.
- `cmd/hostrunner`, the Rules test: written and inherited directories
  reported, a missing one not refused.

## Out of scope

Ignoring the container's directory for a rule with `dir`; a fixed
directory inside the workspace; `path: check` with `dir`; inheriting any
other key, or `dir` by default; checking the directory when the rules
load; the rest of the environment (`HOME`, mise's other config files);
configuration a tool reads from elsewhere on the host.
