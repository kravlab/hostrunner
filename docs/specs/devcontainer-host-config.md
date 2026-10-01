# Spec: host git identity in the devcontainers, global instructions for the agent

## Goal

1. In both devcontainers (`.devcontainer/devcontainer.json` and
   `.devcontainer/claude/devcontainer.json`), `git commit` run by the
   container's own git uses the host user's `user.name` and `user.email`.
   Only these two values reach the container, not the rest of the host's
   git config (credential helpers, signing, `includeIf` with host paths).
   A host without them is not an error.
2. In the Claude Code devcontainer only, Claude Code reads the host user's
   global instructions from the file named by `HOSTRUNNER_AGENTS_MD` on
   the host (e.g. a global `AGENTS.md`). Edits to that file reach the
   running container without recreating it or running `devcontainer up`.
   Without the variable the container starts with no global instructions:
   no host has to have the file.

Both scripts below run on the host from `initializeCommand`, so on every
`devcontainer up`, and write into one per-container directory,
`<dir>` = `${localEnv:XDG_RUNTIME_DIR}/hostrunner-host-config/${devcontainerId}`
(`${devcontainerId}` differs per configuration). Each creates `<dir>` with
`mkdir -p`. The files are world-readable (`gitconfig` is `chmod 0644`ed,
since `mktemp` creates it 0600), so they are readable under rootless
Podman's user namespace too.

## Git identity: `.devcontainer/git-identity.sh <dir>`

A POSIX `sh` script shared by both configurations, writing
`<dir>/gitconfig`.

- Reads `user.name` and `user.email` with
  `git -C <workspace> config --global --includes --get <key>`, where
  `<workspace>` is one level above the script:
  - `--global`: the repository's `.git/config` is not consulted. In the
    Claude container the agent can redirect host git to a config it writes
    (see `claude-devcontainer.md`), so it must not choose the identity.
  - `--includes` (off by default with `--global`) and running in the
    workspace keep identities set through `includeIf "gitdir:…"` in the
    global config.
- A missing value is not an error: it is left out of `<dir>/gitconfig`
  (empty when both are missing), so the container starts on any host, and
  the container's git asks for the identity on `git commit` as it would
  without the include. Only `git config`'s "not set" exit status 1 means
  missing; any other failure (e.g. a malformed host config, status 128)
  fails the script and `devcontainer up`, rather than silently dropping
  an identity the user set.
- Writes with `git config --file` (git escapes the values) to a temporary
  file in `<dir>`, then `mv` over the old one: a reader never sees a
  half-written file, and an identity removed on the host disappears from
  it. The identity is in every commit; it is not secret.

The Claude container mounts `.devcontainer/` read-only, so the agent cannot
change what runs on the host. The everyday container does not: there,
whatever runs in the container could already edit `devcontainer.json` and
its `initializeCommand`, so the script adds no new way to reach the host.

## Global instructions: `.devcontainer/claude/agents-md.sh <dir> <agents-md>`

A POSIX `sh` script, Claude configuration only. `<agents-md>` is
`${localEnv:HOSTRUNNER_AGENTS_MD}`, empty when the variable is not set. The
script makes `<dir>/CLAUDE.md` a symlink (`ln -sf`), always, so the mount
source below exists on every host:

- `<agents-md>` set: it must be an absolute path to a regular file (after
  following symlinks); the symlink points to it. Otherwise the script
  prints `agents-md: HOSTRUNNER_AGENTS_MD (<path>) is not an absolute path
  to a file` to stderr and exits 1: the variable was set on purpose, so a
  typo must not silently drop the instructions.
- `<agents-md>` empty: the symlink points to `<dir>/empty.md`, an empty
  file the script creates.

The original is not copied: Docker and Podman resolve the symlink when
the container starts and bind-mount the original file itself.

## Image: `.devcontainer/Dockerfile`

- Stage `base` (so both configurations get it):
  `git config --system include.path /run/host-config/gitconfig`. git
  ignores an include whose file does not exist, so the image works without
  the mount. The system level leaves `~/.gitconfig` to the container: a
  value set there with `git config --global` overrides the host's.

## `.devcontainer/devcontainer.json` (everyday)

- `initializeCommand`:
  `["${localWorkspaceFolder}/.devcontainer/git-identity.sh", "<dir>"]`
  (array form: no shell, paths with spaces survive).
- Mount, read-only: `source=<dir>,target=/run/host-config,type=bind,readonly`.
  A directory, not a single file: the `mv` replaces the file's inode, and
  a single-file bind mount would keep showing the old one.

## `.devcontainer/claude/devcontainer.json`

- `initializeCommand`, object form; its commands run in parallel and a
  failure of any fails `devcontainer up`:
  - `hostrunner`: the `hostrunner up …` array, unchanged.
  - `git-identity`:
    `["${localWorkspaceFolder}/.devcontainer/git-identity.sh", "<dir>"]`.
  - `agents-md`:
    `["${localWorkspaceFolder}/.devcontainer/claude/agents-md.sh", "<dir>", "${localEnv:HOSTRUNNER_AGENTS_MD}"]`.
- Mounts, read-only:
  - `source=<dir>,target=/run/host-config,type=bind,readonly`, as in the
    everyday configuration. The `CLAUDE.md` symlink in it shows up in the
    container as a dangling link to a host path; nothing reads it there.
  - `source=<dir>/CLAUDE.md,target=/home/vscode/.claude/CLAUDE.md,type=bind,readonly`:
    the original (or `empty.md`). The target is inside the `~/.claude`
    volume, which is `CLAUDE_CONFIG_DIR`; Claude Code reads its user
    instructions from `$CLAUDE_CONFIG_DIR/CLAUDE.md`. Read-only: the agent
    cannot rewrite its own instructions.
- Removed: the direct mount of `${localEnv:HOME}/.claude/CLAUDE.md`, the
  `/run/git-identity` mount, and `.devcontainer/claude/git-identity.sh`
  (moved to `.devcontainer/git-identity.sh`).

## Edge cases

- `HOSTRUNNER_AGENTS_MD` not set (e.g. on another host): the Claude
  container starts; `~/.claude/CLAUDE.md` is empty, so no global
  instructions.
- Set to a relative path, a missing file, a directory or a dangling
  symlink: `devcontainer up` of the Claude configuration fails with the
  script's message.
- Original edited on the host in place (truncate and write, as VS Code
  does by default): seen in the running container at once; Claude Code
  reads it at the start of its next session.
- Original saved by writing a new file and renaming it over the old one:
  the single-file mount keeps the old inode, so the container keeps the
  old content (the trade-off of mounting the file rather than its
  directory, which would expose the directory's other files).
- `HOSTRUNNER_AGENTS_MD` changed to another file, set or unset: takes
  effect when the container next starts; the mount resolves the symlink
  only then.
- The environment `devcontainer up` sees decides: a variable exported only
  in an interactive shell's rc file is not seen by a VS Code started from
  the desktop.
- A `CLAUDE.md` already in the `~/.claude` volume (written in the
  container earlier) is hidden by the mount.
- Host identity changed: picked up on the next `devcontainer up`,
  including by a running container, since `/run/host-config` is a
  directory mount.
- `user.name`/`user.email` missing in the host's global config: the
  container starts; the missing value is not set there, and `git commit`
  in the container fails with git's own "Please tell me who you are"
  unless the container sets it. `user.name` set only in the repository's
  `.git/config` is ignored (see `--global` above).
- VS Code's Dev Containers extension copies the host's `~/.gitconfig` into
  the container's `~/.gitconfig`; that copy overrides the system include,
  with the same values in the usual case.
- `XDG_RUNTIME_DIR` is cleared at logout; the next `devcontainer up`
  writes the directory again.

## Verification

The containers are checked by hand, as in `claude-devcontainer.md`: the
e2e suite covers hostrunner on `examples/devcontainer`, and building these
images there would add network-bound, slow builds without testing
hostrunner further.

Scripts: automated, in `internal/devcontainer/scripts_test.go` (part of
`go test ./...`, so `mise run test`; `.devcontainer/` itself is not walked
by `./...`). Each test runs a script with only `PATH` and a temporary
`HOME` in its environment, so the host user's git config never leaks in:

- `git-identity.sh` with the real global config: `<dir>/gitconfig` has the
  host's values. With `HOME` pointing to a directory whose `.gitconfig`
  lacks `user.email`: exits 0, `gitconfig` has `user.name` only. Without a
  `.gitconfig`: exits 0, `gitconfig` is empty. A malformed `.gitconfig`:
  exits non-zero with git's message, the old `gitconfig` stays. A value
  with quotes and backslashes reads back unchanged.
- `agents-md.sh` with `<agents-md>` empty: `<dir>/CLAUDE.md` links to an
  empty `<dir>/empty.md`. An existing file, and a symlink to one: links to
  it. Relative path, missing file, directory: exits 1 with the message.

Containers, with the devcontainer CLI (each recreated once to apply the
new mounts):

- Everyday: `devcontainer up` succeeds; `git config --show-origin
  user.name` shows `/run/host-config/gitconfig`; a commit in a scratch
  repository has the host's author and committer; `/run/host-config` is
  not writable.
- Claude: `devcontainer up --config .devcontainer/claude/devcontainer.json`
  succeeds with `HOSTRUNNER_AGENTS_MD` unset (`~/.claude/CLAUDE.md` empty)
  and with it set (content matches the original, `sha256sum`). With it
  set, an in-place edit of the original on the host shows up in the
  running container without `up` or a restart. Identity checks as in the
  everyday container; `hostrun git status` still works;
  `~/.claude/CLAUDE.md` is not writable.

## Docs

README, Development: both devcontainers take `user.name` and
`user.email` from the host's global git config when set; the Claude one
takes global instructions from the file named by `HOSTRUNNER_AGENTS_MD`
(optional), seen live except for saves that replace the file.
