# Fixed directory

A command ran in the mirrored directory, so a tool that takes configuration
from its working directory was steered by the container whatever the rule
said about argv
([#36](https://github.com/kravlab/hostrunner/issues/36)): `tix` probes it
with host git and reads `--repo owner/name` as a path when such a directory
exists, and a mise shim reads the `mise.toml` above it. A rule can now name
a fixed directory, `dir: /`, for its command to run in.

## Considered Options

- **Only "not the workspace"**, with hostrunner picking `/`: fewer ways to
  get it wrong, but no other directory could ever be named without a new
  key. Rejected for an absolute host path.
- **A fixed directory inside the workspace**: the container writes it, so
  it could plant the `.git` or `mise.toml` the key exists to avoid.
  Refused.
- **Ignoring the container's directory** for such a rule: `hostrun tix`
  would work from anywhere, but a `path: open` argument would have nothing
  to resolve from. Rejected: every request still comes from the workspace.
- **`path: check`** in such a rule: the program would resolve the argument
  from the fixed directory, not from the mirrored directory it was checked
  in. Refused when the rules are parsed; `path: open` is unaffected, as the
  program gets `/proc/self/fd/N`.
- **Rewriting a `path: check` argument to its host path**: contradicts
  [ADR 0002](0002-path-check-for-workspace-files.md), where `check` passes
  the argument as given, and shows the program host paths. Rejected.
- **Checking the directory when the rules load**: it can vanish or turn
  into a symlink afterwards, and the Rules test knows no workspace.
  Rejected for a check on every request.
- **Inheriting `dir` from a shorter rule by default**: forgetting `dir` on
  a longer rule would be safe, but no rule could then go back to the
  mirrored directory. Rejected for an opt-in `dir: inherit`.

## Consequences

- `dir` is an absolute path or `inherit`. `inherit` takes the resolved
  `dir` of the nearest shorter rule whose command is a prefix of this
  one's, through a chain of `inherit`s; it is a parse error when there is
  no such rule or it has no `dir`. Without `dir`, a rule runs its command
  in the mirrored directory, whatever a shorter rule says.
- On every request the daemon opens the fixed directory, as it opens the
  mirrored directory, and refuses the command (126) when it does not exist,
  is not a directory, or its real path is inside the workspace. The
  refusal names the directory as the rules file writes it, cleaned
  (`inherit` resolved). The mirrored directory is
  still opened and must still be inside the workspace.
- `PWD` is the fixed directory's real path.
- A dry run and the Rules test report the directory a command would run
  in. The fixed directory is no secret: it is written in the rules file the
  container reads.
