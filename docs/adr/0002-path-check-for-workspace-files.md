# Path check for workspace files

A list sees only the text of an argument, so a rule could not require that
an argument names a file inside the workspace
([#25](https://github.com/kravlab/hostrunner/issues/25)): the container
writes the workspace and can plant a symlink that leads a lexically clean
path out of it. A positional or flag-value list can now carry a path check,
`path: open` or `path: check`, next to its patterns; an argument has to pass
both.

## Considered Options

- **`check` only**: resolve the path on the host when the rules are
  checked and pass the argument as given. The program opens the path
  itself afterwards, so a symlink swapped in between still redirects it.
- **`open` only**: the daemon opens the file and the program gets
  `/proc/self/fd/N`, as the working directory is already opened before
  the command starts. Nothing swapped in later redirects it, but the
  program never sees the original name. Both modes are offered and the
  rules file picks one per list.
- **Absolute container paths**: they would have to be rewritten to host
  paths, so `check` could no longer pass the argument as given. Refused.
- **Paths that do not exist yet** (output files): `open` has nothing to
  open. Refused in both modes.
- **Directories**: the program would open their entries itself, and those
  are not checked. Only regular files are accepted.

## Consequences

- An argument passes a path check only if it is a relative path, resolved
  against the working directory, to an existing regular file whose real
  path on the host is inside the workspace.
- `check` leaves the window between the check and the program opening the
  path open; it is documented as such.
- With `open` the program sees `/proc/self/fd/N`, not the name it was
  given, so it cannot rely on that name or its extension.
- A path check on `positional` applies to every positional argument of the
  rule.
- The list's patterns are matched against the argument as given, before
  any rewrite.
- Amends [ADR 0001](0001-regex-lists-next-to-globs.md), which says a list
  is exactly one of `allow`, `deny`, `allow_regex`, `deny_regex`: a list
  is now at most one of those, plus an optional `path`, and not empty.
- `check` starts from the working directory's real path as the daemon
  opened it, not from the open directory: `os.Root` cannot start below its
  root and still allow `..` up to it. A parent renamed in between moves
  what `check` looks at, inside the workspace; `open` is unaffected.
- The file's type is checked before it is opened, so a device node swapped
  in between is opened, then refused. `O_PATH` would avoid the open, but
  `os.Root` adds `O_NOFOLLOW`, and with `O_PATH` that returns a symlink
  itself instead of following it.
