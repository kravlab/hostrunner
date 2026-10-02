# Spec: host global git excludes in the devcontainers

## Goal

In both devcontainers (`.devcontainer/devcontainer.json` and
`.devcontainer/claude/devcontainer.json`), files matched by the host
user's global git excludes are ignored as on the host:

- by the container's own git (`git status`, `git add`, `git clean`), so it
  agrees with `hostrun git status`, which runs host git with the host's
  excludes;
- by ripgrep, which Claude Code's Grep and Glob use: it reads neither the
  system git config nor includes, only `core.excludesFile` from
  `~/.gitconfig` or `~/.config/git/config`, else
  `${XDG_CONFIG_HOME:-~/.config}/git/ignore`.

A host without global excludes is not an error. This extends
`devcontainer-host-config.md`, whose Goal 1 lets only `user.name` and
`user.email` through: the excludes file's content becomes the third thing
that reaches the container. No other host git setting does.

## Excludes: `.devcontainer/git-excludes.sh <dir>`

A POSIX `sh` script shared by both configurations, run on the host from
`initializeCommand` (so on every `devcontainer up`), writing
`<dir>/gitignore`. `<dir>` is the per-container directory of
`devcontainer-host-config.md`, already mounted read-only at
`/run/host-config` in both containers; the script creates it with
`mkdir -p`.

- Finds the host's excludes file as host git does:
  - `git -C <workspace> config --global --includes --type=path --get core.excludesFile`,
    `<workspace>` one level above the script. `--global` and `--includes`
    for the reasons given for the identity in `devcontainer-host-config.md`;
    `--type=path` expands `~/`.
  - Set to an empty value (`excludesFile =`): no excludes, as host git
    treats it; `<dir>/gitignore` is written empty.
  - Exit status 1 (not set): git's default,
    `$XDG_CONFIG_HOME/git/ignore` when `XDG_CONFIG_HOME` is set and not
    empty, else `$HOME/.config/git/ignore`.
  - Any other failure (e.g. a malformed config) fails the script and
    `devcontainer up`.
  - A relative path is resolved from `<workspace>`, as host git run at
    the repository's root resolves it.
- The file is missing: `<dir>/gitignore` is written empty and the script
  exits 0. Host git skips a missing excludes file too.
- The file exists: its content is copied unchanged. A copy that fails
  (e.g. the path is a directory or unreadable) fails the script; the old
  `<dir>/gitignore` stays.
- Written to a temporary file in `<dir>`, `chmod 0644` (readable under
  rootless Podman's user namespace), then `mv` over the old one: a reader
  never sees a half-written file, and patterns removed on the host
  disappear from it.

Copied rather than mounted: the copy needs no new mount and no source that
exists on every host, and a single-file mount would miss saves that
rename a new file over the original anyway. The cost: edits on the host
reach the container on the next `devcontainer up`, not at once.

The path comes from the host's global config only. The Claude container
cannot change it: `hostrun git config` is not allowed by
`.devcontainer/hostrun.yaml`, and `.devcontainer/` is read-only there.

## Image: `.devcontainer/Dockerfile`

Stage `base` (so both configurations get it):
`/home/vscode/.config/git/ignore`, a symlink to
`/run/host-config/gitignore`. `~/.config` and `~/.config/git` are owned by
`vscode`, so other tools can still write there. It is git's and ripgrep's
default path, so neither needs `core.excludesFile`: it is not set in the
image. Without the mount the link dangles, which git and ripgrep treat as
no excludes, so the image works on its own.

## `initializeCommand`

- `.devcontainer/devcontainer.json` (everyday): from the array form to the
  object form (commands run in parallel; any failing fails
  `devcontainer up`):
  - `git-identity`: the current array, unchanged;
  - `git-excludes`:
    `["${localWorkspaceFolder}/.devcontainer/git-excludes.sh", "<dir>"]`.
- `.devcontainer/claude/devcontainer.json`: `git-excludes`, the same
  array, added next to `hostrunner`, `git-identity` and `agents-md`.

No mount changes.

## VS Code: `dev.containers.copyGitConfig`

VS Code's Dev Containers extension copies the host's `~/.gitconfig` into
the container's `~/.gitconfig` by default. A host `core.excludesFile` then
points to a path that does not exist in the container, and git and
ripgrep skip the default path: the excludes are lost. The copy also
breaks Goal 1 of `devcontainer-host-config.md` (credential helpers,
signing and `includeIf` with host paths reach the container).

`dev.containers.copyGitConfig` is a VS Code user setting, not a
`devcontainer.json` property, so the repository cannot set it. The README
tells VS Code users to set `"dev.containers.copyGitConfig": false` in
their user settings. Without it, the excludes work only in containers
started with the devcontainer CLI (`mise run dev`, `mise run agent`).

## mise: `dev-recreate`, `agent-recreate`

The image change above reaches a container only when it is recreated
(`devcontainer up` keeps an existing container). Two tasks, one per
container, next to `dev` and `agent`:

- `dev-recreate`:
  `devcontainer up --workspace-folder . --remove-existing-container </dev/null`.
- `agent-recreate`: the same with
  `--config .devcontainer/claude/devcontainer.json`.

The CLI removes the container and creates a new one from the image,
rebuilt where the Dockerfile changed (the build cache is kept). Named
volumes (bash history, Claude's `~/.claude`) survive; anything else
written in the old container is lost. `initializeCommand` and
`postCreateCommand` run as on a first `up`. Nothing is started in the
new container: `mise run dev` / `mise run agent` attach to it. `raw` is
not needed; `</dev/null` as in `dc`, so `up` does not read the terminal.
The JSON result stays on stdout (no `>/dev/null`): with nothing run
afterwards, it is the task's output.

## Edge cases

- No global excludes on the host: `/run/host-config/gitignore` is empty;
  only the repository's `.gitignore` applies.
- `core.excludesFile` set to a missing file or to an empty value: as
  above, `up` succeeds.
- `core.excludesFile` set to a directory: `up` fails with `cp`'s message.
- Host excludes edited, or `core.excludesFile` changed: picked up on the
  next `devcontainer up`, including by a running container, since
  `/run/host-config` is a directory mount.
- Patterns are copied as written; they match paths relative to the
  repository, so host paths in them do not matter. Negations and
  anchored patterns behave the same.
- `core.excludesFile` set in the container (`git config --global`):
  overrides the image's default path; the host's excludes then no longer
  apply to git there.
- `XDG_CONFIG_HOME` set in the container: git and ripgrep look elsewhere;
  the image does not set it.
- VS Code without `copyGitConfig: false`: see above.

## Verification

Script: automated, in `internal/devcontainer/scripts_test.go`, run like
the existing script tests (only `PATH`, a temporary `HOME` and
`XDG_CONFIG_HOME`, `GIT_CONFIG_NOSYSTEM=1`):

- `core.excludesFile` set to an absolute path: `<dir>/gitignore` has its
  content byte for byte.
- Set to `~/…`: resolved against `HOME`.
- Set through `[include]` in `~/.gitconfig`: found.
- Set to a relative path (an existing file of the repository): resolved
  from the workspace.
- Not set, `$XDG_CONFIG_HOME/git/ignore` exists: copied.
- Not set, `XDG_CONFIG_HOME` not in the environment,
  `$HOME/.config/git/ignore` exists: copied.
- Not set, `XDG_CONFIG_HOME` set but empty, `$HOME/.config/git/ignore`
  exists: copied.
- Not set, no default file; set to a missing file; and set to an empty
  value: exits 0, `gitignore` empty.
- Every `gitignore` written has mode 0644.
- Set to a directory; and a malformed `~/.gitconfig`: exits non-zero, the
  old `gitignore` stays.
- Run twice with a pattern removed in between: the pattern is gone.

Containers, by hand with the devcontainer CLI (each rebuilt once, for the
image change), with a host excludes file containing a pattern the
repository's `.gitignore` lacks and a matching untracked file in the
workspace:

- Both containers: `git status` does not list the file;
  `git check-ignore -v <file>` names `~/.config/git/ignore`;
  `rg --files` does not list it; `/run/host-config/gitignore` is not
  writable.
- Claude container: `hostrun git status` and `git status` agree.
- Host excludes unset: `up` succeeds, the file shows up as untracked.
- VS Code with `"dev.containers.copyGitConfig": false`: the same results
  as with the CLI.
- `mise run dev-recreate` and `mise run agent-recreate`: a new container
  ID (`docker ps`), the bash history kept, the excludes checks above
  passing in the new container.

## Docs

- README, Development: both containers ignore what the host's global git
  excludes ignore (copied on `devcontainer up`); VS Code users set
  `"dev.containers.copyGitConfig": false`. The sentence "nothing else of
  the host's git config is copied" names the excludes as the exception.
- `devcontainer-host-config.md`: Goal 1 points to this spec for the
  excludes; its VS Code edge case points to the `copyGitConfig` section
  here.
- Comments in both `devcontainer.json` files and the Dockerfile name the
  new script and link.
- `mise.toml` header and README, Development: `dev-recreate`,
  `agent-recreate`.
