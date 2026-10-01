# Spec: host git identity and global instructions in the Claude devcontainer

## Goal

In the Claude Code devcontainer (`.devcontainer/claude/devcontainer.json`):

1. `git commit` run by the container's own git (commits do not go through
   `hostrun`) uses the host user's `user.name` and `user.email`. Only these
   two values reach the container, not the rest of the host's git config
   (credential helpers, signing, `includeIf` with host paths).
2. Claude Code in the container reads the host user's global instructions
   (`~/.claude/CLAUDE.md` on the host, which may be a symlink to a global
   `AGENTS.md`).

The everyday devcontainer (`.devcontainer/devcontainer.json`) does not
change.

## Git identity

### Host side: `.devcontainer/claude/git-identity.sh <dir>`

A POSIX `sh` script run on the host from `initializeCommand`, so on every
`devcontainer up`. It lives in `.devcontainer/`, which the container
mounts read-only: the container cannot change what runs on the host.

- Reads `user.name` and `user.email` with
  `git -C <workspace> config --global --includes --get <key>`, where
  `<workspace>` is two levels above the script:
  - `--global`: the repository's `.git/config` is not consulted. The
    container can redirect host git to a config it writes (see
    `claude-devcontainer.md`), so it must not choose the identity.
  - `--includes` (off by default with `--global`) and running in the
    workspace keep identities set through `includeIf "gitdir:…"` in the
    global config.
- Either value missing: prints
  `git-identity: <key> is not set in the host's global git config` to
  stderr and exits 1, so `devcontainer up` fails before the container is
  created rather than leaving commits without an author.
- Writes `<dir>/gitconfig` with `git config --file` (git escapes the
  values) to a temporary file in `<dir>`, then `mv` over the old one: a
  reader never sees a half-written file. `<dir>` is created with
  `mkdir -p`. Default modes (umask): the identity is in every commit, it
  is not secret, and a readable file works under rootless Podman's user
  namespace too.

### `.devcontainer/claude/devcontainer.json`

- `initializeCommand` becomes the object form; its commands run in
  parallel and a failure of either fails `devcontainer up`:
  - `hostrunner`: the current `hostrunner up …` array, unchanged.
  - `git-identity`:
    `["${localWorkspaceFolder}/.devcontainer/claude/git-identity.sh", "${localEnv:XDG_RUNTIME_DIR}/hostrunner-git/${devcontainerId}"]`
    (array form: no shell, paths with spaces survive).
- Mount, read-only:
  `source=${localEnv:XDG_RUNTIME_DIR}/hostrunner-git/${devcontainerId},target=/run/git-identity,type=bind,readonly`.
  A directory, not a single file: the `mv` replaces the file's inode, and
  a single-file bind mount would keep showing the old one.

### Image: `.devcontainer/Dockerfile`, stage `claude`

- `git config --system include.path /run/git-identity/gitconfig`. git
  ignores an include whose file does not exist, so the image works without
  the mount. The system level leaves `~/.gitconfig` to the container: a
  value set there with `git config --global` overrides the host's.

## Global instructions

`.devcontainer/claude/devcontainer.json`, mount, read-only:
`source=${localEnv:HOME}/.claude/CLAUDE.md,target=/home/vscode/.claude/CLAUDE.md,type=bind,readonly`.

- The target is inside the `~/.claude` volume, which is
  `CLAUDE_CONFIG_DIR`; Claude Code reads its user instructions from
  `$CLAUDE_CONFIG_DIR/CLAUDE.md`.
- Docker and Podman resolve a symlinked source on the host, so a
  `CLAUDE.md` that links to a global `AGENTS.md` works, and no personal
  path is in the repository.
- Read-only: the agent cannot rewrite its own instructions.

## Edge cases

- No `~/.claude/CLAUDE.md` on the host (or a dangling symlink): the bind
  mount source does not exist and the container fails to start.
- Host Claude Code uses a `CLAUDE_CONFIG_DIR` other than `~/.claude`: the
  mount still takes `~/.claude/CLAUDE.md`.
- `CLAUDE.md` edited on the host: a single-file bind mount follows the
  inode, so an editor that saves by writing a new file and renaming it is
  seen only after a container restart; an in-place write is seen at once.
- A `CLAUDE.md` already in the `~/.claude` volume (written in the
  container earlier) is hidden by the mount; it reappears if the mount is
  removed.
- Host identity changed: picked up on the next `devcontainer up`,
  including by a running container, since `/run/git-identity` is a
  directory mount.
- `user.name`/`user.email` missing in the host's global config:
  `devcontainer up` fails with the script's message.
- `user.name` set only in the repository's `.git/config`: ignored (see
  `--global` above); the script fails if the global config lacks it.
- `XDG_RUNTIME_DIR` is cleared at logout; the next `devcontainer up`
  writes the file again.
- The container sets its own identity (`git config --global user.name …`
  in `~/.gitconfig`): it wins over the host's for the container's commits.

## Verification

Configuration and a short host-side script, as in
`claude-devcontainer.md`: no unit tests. The e2e suite covers hostrunner
on `examples/devcontainer`; building the Claude image there would add a
network-bound, slow build without testing hostrunner further. Checked
with the devcontainer CLI on the host:

- `devcontainer up --config .devcontainer/claude/devcontainer.json`
  succeeds; `hostrun git status` still works.
- In the container: `git config user.name` and `git config user.email`
  print the host's values; `git config --show-origin user.name` shows
  `/run/git-identity/gitconfig`; a commit in a scratch repository has that
  author and committer.
- `/run/git-identity` and `~/.claude/CLAUDE.md` are not writable; the
  file's content matches the host's (`diff` of `sha256sum`).
- Script, run on the host with `HOME` pointing to a directory whose
  `.gitconfig` lacks `user.email`: exits 1 with the message, writes no
  file.
- Container rebuilt (`--remove-existing-container`): identity and
  instructions are still there.

## Docs

README, Development: the Claude devcontainer takes `user.name` and
`user.email` from the host's global git config and the global
instructions from `~/.claude/CLAUDE.md`, and both are required.
