---
status: accepted
---

# The workspace's host path in inject templates

A rule could not name the workspace's path on the host, which differs per
clone, so it could not pin a tool to the workspace's own files, e.g. git
to the workspace's `.git` through `GIT_COMMON_DIR` (#180). Every
`inject` template now accepts `${host.workspace}`: the workspace's real
path on the host. hostrunner still neutralises no tool on its own (#7);
it only hands the rules author a path it already knows.

## Considered Options

- **`${workspace}`, reserved**: a secret may be named `workspace` today,
  so a rules file with one would stop loading. Rejected for a `host.`
  namespace, which no secret name can enter (a name has no `.`), as
  `file.` already is for injected files.
- **`${workspace}`, a secret of that name winning**: the same string
  would mean different things depending on `secrets:`. Rejected.
- **The path as given to `--workspace`**: with a symlink in it, it can
  name a tree other than the one the mirrored directory lies in.
  Rejected for the real path the mirrored directory is computed from,
  resolved once when the daemon starts.
- **Templates in secret sources (`command`, `file`)**: those fields are
  plain text today, so a `$` in them would change meaning. Rejected;
  only `inject` strings are templates.
- **Treating the path as a secret** (strict lists, a guarded refusal,
  masking): the path gives the container nothing it could leak through
  the program. Rejected; it is plain text, as `inject` without secrets is.

## Consequences

- `${host.workspace}` is allowed in `program_args`, `command_args`,
  `trailing_args`, `env` values and `files` contents. An unknown
  `${host.…}` name is a parse error, so a hostrunner without this change
  refuses a rules file that uses it.
- It lands only after the per-request mount check (#181): without that
  check's `.git` mount, the container can rename `.git` and put its own
  in its place, at the very path the injected variable names.
- Dry runs and the rules test do not name it; they name secrets only.
