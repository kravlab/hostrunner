# Spec: Rules engine

Ticket: [Rules engine](https://github.com/kravlab/hostrunner/issues/5).
Rule model: [Rule model and config format](https://github.com/kravlab/hostrunner/issues/3)
(resolution comment). Items marked ★ refine points that ticket left implicit.

## Goal

The daemon runs a command only if `.devcontainer/hostrun.yaml` allows it.
Denied calls exit 126 with a message naming the rule and the reason.

## Config loading

- `hostrunner serve --config <path>`; default
  `<workspace>/.devcontainer/hostrun.yaml` (read on the host). `up` does
  not pass it.
- Read once at startup. Invalid file → the daemon does not start (so
  `hostrunner up`, and with it `devcontainer up`, fails with the error).
- Missing file → the daemon starts with no rules; every call is denied
  with `no .devcontainer/hostrun.yaml: every command is denied`.
- YAML via `go.yaml.in/yaml/v3`, strict (`KnownFields`): unknown keys are
  errors with line numbers.

## Schema

```yaml
rules:
  - command: git push          # argv prefix, split on whitespace; required
    args: any | none           # exclusive with flags/positional
    flags:
      allow: [-u, --tags]      # or deny: [...], never both
      values:                  # flags that take a value; value filter:
        -o: { allow: [ci.skip] }   # allow or deny list of globs
        --only: {}                 # any value
    positional:
      deny: ["+*"]             # or allow: [...], never both
```

Validation errors: empty or duplicate `command`; `args` other than
`any`/`none`; `args` together with `flags`/`positional`; `allow` together
with `deny` (in `flags`, `positional`, or a value filter); a flag name in
`allow`/`deny`/`values` that does not start with `-`; empty rule
(`command` only) ★ — it must say `args: any` explicitly.

## Matching

- The rule with the longest `command` that is a token-wise prefix of argv
  applies; no merging. No match → `no rule allows "<argv[0..1]>"`.
- `argv[0]` is compared literally: `/usr/bin/git push` does not match
  `git push` (denied).

## Argument parsing (per matched rule) ★

Tokens after the command prefix, getopt-style (unless the rule sets
`flag_style: go`, see [rules-flag-style.md](rules-flag-style.md)), using
the rule's `flags.values` to know which flags take a value:

- `--` ends flags; everything after is positional. `-` alone is positional.
- `--name=value` is flag `--name` with an inline value; `--name` with a
  value flag takes the next token (missing → denied: `flag --name needs a
  value`).
- `-abc` is a cluster: `-a`, `-b`, `-c`; if one is a value flag, the rest
  of the token (or the next token) is its value (`-ovalue`, `-o value`).
- Everything else is positional.

Checks:

- `args: none` → any token denies. `args: any` → allowed.
- `flags.allow` (strict): each flag must be in `allow` or `values`; a
  boolean flag given an inline value (`--tags=x`) is denied.
- `flags.deny` (best effort): a flag is denied if it is in `deny`, or if it
  is a long flag whose name is a prefix of a denied long flag (`--forc`).
  A long flag that is a prefix of exactly one declared value flag is treated
  as that value flag (`--proj=prod` → `--project`), so its value is still
  filtered; ambiguous prefixes are denied.
- Values of value flags: checked against their `allow`/`deny` globs; `{}`
  accepts any value. Repeated flags check every value.
- `positional.allow`: every positional must match; `positional.deny`: none
  may match.
- Omitted `flags` or `positional` section: that part is unrestricted.

## Globs ★

`*` matches any sequence **including `/`**, `?` one character, everything
else is literal. (With `path.Match`, `deny: ["+*"]` would miss
`+refs/heads/main`.) No character classes.

## Denial message ★

`denied by rule "<command>": <reason>`, e.g.
`denied by rule "git push": flag --force is not allowed`,
`denied by rule "git push": argument "+main" is denied`.
Sent as `Error{Code: 126}`; the client prints `hostrun: <message>`.

## Daemon wiring

- `internal/rules`: `Load(path) (*Policy, error)`, `Parse([]byte)`,
  `(*Policy).Check(argv []string) error` (a `*Denial` with rule and reason).
- `internal/daemon`: `WithPolicy(Policy)` option, `Policy` interface
  (`Check([]string) error`) defined in the daemon. Checked after the
  request is decoded, before the cwd is mapped; denial → 126. Without the
  option the daemon allows everything (tests only; `serve` always sets it).

## cwd TOCTOU (carried over) ★

`workspace.Mapper.Open(containerCwd)` replaces `HostPath`: it opens the
directory through `os.Root` rooted at the host workspace (escapes via `..`
or symlinks fail with `ErrOutsideWorkspace`) and returns the open
directory plus its real host path. The command starts with
`Dir = /proc/self/fd/<fd>` (the child inherits the fd until exec) and
`PWD=<real host path>` in its environment, so a symlink swapped in after
the check cannot redirect it. A directory deleted after the check no
longer yields 127. `os.Root` refuses every absolute symlink, even one
pointing inside the workspace; absolute symlinks written in the container
name container paths anyway, so only relative symlinks are followed.

## Example config and docs

- `examples/devcontainer/.devcontainer/hostrun.yaml`:

  ```yaml
  rules:
    - command: git status
      args: any
    - command: git fetch
      args: any
    - command: git rev-parse
      args: any
    - command: git pull
      flags:
        deny: [--force, -f]
    - command: git push
      flags:
        deny: [--force, -f, --force-with-lease, --force-if-includes, --mirror, --delete, -d, --prune]
      positional:
        deny: ["+*", ":*"]   # force refspecs and deletions
  ```

- README: config reference; that `deny` is best effort; and that rules
  restrict argv only — tools that read config or hooks from the workspace
  (git, fakehost, …) can be steered by the container, so allowing them
  against a hostile agent is a conscious risk of host code execution.

## Tests

- `internal/rules`: table-driven — parse/validation errors, prefix
  matching, each rule kind, getopt forms, deny normalization, globs,
  denial messages, missing file.
- `internal/workspace`: `Open` confinement; the returned directory stays
  the original after the path is swapped for a symlink.
- End-to-end (client ↔ daemon): allowed call runs; denied call → 126 with
  the message; no config → everything denied; the command runs in the
  opened directory.
- `cmd/hostrunner`: invalid config → `serve` fails; `--config` default.
- `e2e`: the workspace is a git repo using the example config (plus test
  rules for `pwd`/`sh`); `hostrun git rev-parse --show-toplevel` prints the
  host path; `hostrun git push --force` and an unlisted command → 126.
  Podman runs only with `HOSTRUNNER_E2E_PODMAN=1`: devcontainer CLI 0.89
  sometimes never sees the container's start event from `podman events`
  and hangs, also without hostrunner (1 of 4 runs). Cleanup is registered
  before `up` and removes containers, daemons and runtime dirs of the test
  workspace even when `up` fails.

## Review follow-ups (supersede the sections above where they differ)

- **Deny list before abbreviations:** a denied flag stays denied even if it
  abbreviates a declared value flag (`--force` vs `--force-with-lease`). An
  abbreviated value flag must carry its value inline (`--proj=dev`);
  `--proj dev` is denied.
- **Stricter config:** an empty or null section or list (`flags: {}`,
  `positional: {}`, `allow:`) is an error (`allow: []` explicitly allows
  nothing); flag names must be `-x` or `--name` without `=`; the program
  must be a name or an absolute path; the file holds a single document.
- **Rules reload:** `hostrunner up` validates the rules file (invalid →
  `up` fails) and arms the daemon with its SHA-256 (`Arm.config_digest`).
  A daemon that loaded other rules answers `Armed{restart: true}` and
  stops; `up` waits for the socket to free up and starts a new daemon.
  `serve` takes `--config`; `up` passes it.
- **Daemon:** the policy is a required argument of `daemon.New`
  (`WithPolicy` is gone); a panic while handling a connection is answered
  with `Error` 125 and does not stop the daemon.
- **cwd:** the directory is opened with `O_DIRECTORY` (a FIFO fails at once
  instead of blocking); errno errors (ENOENT, ENOTDIR, EACCES, ELOOP) are
  "not available on the host", only `os.Root` escapes are "outside the
  workspace".
- **Missing config message:** `no rules file: every command is denied`
  (no path, which could be a host path); the daemon logs the path and the
  rules digest.
- **Example:** `fetch`/`pull`/`push` use flag allow lists (closing
  `--upload-pack`, `--receive-pack`, `--exec`) and deny URL, scp-style and
  path remotes; a test keeps it valid and checks those denials.

## Out of scope

Raw-command input, mount-check profiles, hot reload of the config.
