# Spec: Path check for workspace files

Ticket: [Spec: path check for workspace files](https://github.com/kravlab/hostrunner/issues/31)
(from [#25](https://github.com/kravlab/hostrunner/issues/25)).
Decision record: [ADR 0002](../adr/0002-path-check-for-workspace-files.md).
Extends [rules-engine.md](rules-engine.md) and
[rules-regex-lists.md](rules-regex-lists.md); vocabulary in
[CONTEXT.md](../../CONTEXT.md).

## Goal

A rule can require that an argument names a workspace file: an existing
regular file inside the workspace, after symlinks are followed on the
host. A list sees only the text of an argument, and the container can
plant a symlink that leads a clean-looking path such as `build/app.pkg`
out of the workspace.

## Schema

A list under `positional`, or under a value flag in `flags.values`, takes
a `path` key next to at most one pattern key, or alone:

```yaml
rules:
  - command: abx install
    flags:
      allow: [-r]
    positional:
      allow: ["*.pkg"]
      path: open
  - command: mytool convert       # a placeholder program
    flags:
      values:
        --input: { path: check }
```

- `path` is `open` or `check`; anything else is a load error naming the
  value.
- A list with only `path` lets every value through its (absent) patterns.
- `path` is unknown directly under `flags`, and a rule with `args` cannot
  have `positional`, so neither can carry it there.

## Behavior

1. The rules are checked on the text first, as before: a denied command is
   reported as denied whatever its working directory. The patterns see the
   argument as given.
2. The daemon opens the working directory (as before), then checks each
   argument a path check applies to, in argv order:
   - empty → refused;
   - absolute → refused;
   - otherwise resolved from the working directory's real host location,
     as the kernel would resolve it (`..` after a symlink climbs from the
     symlink's target), through an `os.Root` at the workspace: `..` or a
     symlink leading out of the workspace, and an absolute symlink, are
     refused; `..` that stays inside is fine;
   - missing → refused; not a regular file (directory, FIFO, socket,
     device) → refused. The type is checked before the open and again on
     the open file; the open is read-only, non-blocking and takes no
     controlling terminal, so a FIFO cannot stall the daemon.
3. `path: check`: the file is closed again and the argument passed as
   given.
4. `path: open`: the files stay open and are passed to the command as
   descriptors 3, 4, … in argv order; each checked value is replaced with
   `/proc/self/fd/N`, keeping the rest of its token (`--file=`, `-f`,
   `-xf`). The daemon closes its copies once the command has started.
5. Every spelling of a flag value is covered: `--file v`, `--file=v`, an
   unambiguous abbreviation `--fi=v`, `-f v`, `-fv`, and a cluster `-xfv`.

## Denial messages

Exit code 126. The message names the value as the container gave it and
never a host path; the daemon log gets the full error:

```
argument "x" is not a workspace file: it is empty
argument "x" is not a workspace file: it is an absolute path
argument "x" is not a workspace file: it does not exist
argument "x" is not a workspace file: it is not a regular file
argument "x" is not a workspace file: it leads outside the workspace
argument "x" is not a workspace file: it cannot be opened on the host
```

## Edge cases and limits

- `check` leaves a window between the check and the program opening the
  path: a symlink swapped in then is followed. Documented in the README.
- The working directory's starting point for the check is its real path
  when the daemon opened it, not the open directory itself (`os.Root`
  cannot start below its root and still allow `..` up to it). A parent
  directory renamed in between makes `check` look at another path than the
  program will; the path is still confined to the workspace, and `open` is
  unaffected because the program gets the file the check opened.
- The type check happens before the open, so a device node swapped in
  between is opened (and then refused). Opening with `O_PATH` would avoid
  that, but `os.Root` adds `O_NOFOLLOW`, and `O_PATH|O_NOFOLLOW` returns a
  symlink itself instead of following it.
- With `open` the program sees `/proc/self/fd/N`, not the name; its own
  children inherit the descriptors.

## README

The Rules section documents `path`, both modes and what each promises;
Limitations repeats the race `check` leaves open; the exit-code table
lists the refusal.

## Tests

- Daemon `Server` (Serve harness, real policy and workspace, real `cat`,
  `echo` and `sh`): `check` passes the argument as given; `open` passes
  `/proc/self/fd/N` in argv order, positional and flag values mixed; every
  refusal above, the FIFO without hanging; symlinks inside followed, out
  of the workspace or absolute refused; `..` inside and out; a symlinked
  working directory; combination with a pattern list; every flag-value
  spelling in both modes; a symlink swapped in after the check redirects
  `check` but not `open`; no host path in any message.
- `rules.Parse`/`Check`: valid `path` placements; invalid values and
  places; the "which keys" message; a pattern denial still wins over a
  path check.

## Out of scope

Absolute and not-yet-existing paths, directories, per-position checks,
paths a program reads from elsewhere, and closing the `check` window.
