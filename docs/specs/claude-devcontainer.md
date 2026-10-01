# Spec: Claude Code devcontainer

## Goal

The repository gets two devcontainer configurations:

- `.devcontainer/devcontainer.json` — everyday development. No
  hostrunner: it does not need `hostrunner` on the host and comes up with
  a plain `devcontainer up` (or "Reopen in Container").
- `.devcontainer/claude/devcontainer.json` — for running Claude Code in
  the container. Its image adds Claude Code to the everyday one, and the
  container is wired to hostrunner so the agent can run the host commands
  allowed in `.devcontainer/hostrun.yaml`. Brought up with
  `devcontainer up --config .devcontainer/claude/devcontainer.json` or by
  picking the configuration in VS Code.

Both keep bash history across container rebuilds.

## Image: `.devcontainer/Dockerfile`

One multi-stage Dockerfile, so the Claude image is built on the everyday
one without publishing it anywhere:

- Both stages run `RUN` with `bash -o pipefail`, so a failed
  `curl … | sh` download fails the build.
- Stage `base`: `FROM mcr.microsoft.com/devcontainers/base:trixie` (the
  image the configuration uses today), plus:
  - mise `ARG MISE_VERSION=v2026.9.11` (the version the mise feature
    pinned), installed with `https://mise.run` to `/usr/local/bin/mise`,
    and `ENV PATH` with its shims. It replaces the devcontainer feature,
    so both configurations share one pinned version and need no feature
    lock file.
  - `/home/vscode/.commandhistory` owned by `vscode`. A new, empty named
    volume mounted there takes this ownership from the image; otherwise
    it would be root-owned and not writable by `vscode`. It is under the
    home folder because the devcontainer CLI's `updateRemoteUserUID`
    chowns only the home folder when it changes `vscode`'s UID to the
    host's; elsewhere the directory would keep UID 1000.
  - `~/.bashrc` of `vscode` sets `HISTFILE=~/.commandhistory/.bash_history`
    and prepends `history -a` to `PROMPT_COMMAND`, so every command is
    written at once: a shell killed by a container stop never saves its
    history on exit.
- Stage `claude`: `FROM base`, plus:
  - `ARG CLAUDE_CODE_VERSION=2.1.280` (the `stable` channel at the time of
    writing). Pinned rather than `stable`, because a channel name makes the
    layer's content depend on when the cache was filled; bumping the ARG
    is the upgrade and shows up in the diff.
  - Claude Code installed as `vscode` with the native installer
    (`curl -fsSL https://claude.ai/install.sh | bash -s "$CLAUDE_CODE_VERSION"`),
    which puts `claude` in `/home/vscode/.local/bin`.
  - `/home/vscode/.claude` owned by `vscode` (same reason as
    `~/.commandhistory`).
  - `ENV CLAUDE_CONFIG_DIR=/home/vscode/.claude` (keeps `.claude.json`
    inside the volume too), `ENV DISABLE_AUTOUPDATER=1` (the image, not
    the auto-updater, decides the version) and `/home/vscode/.local/bin`
    in `ENV PATH`, so `claude` is found by any process, not only through
    `remoteEnv`.

## `.devcontainer/devcontainer.json` (everyday)

- `build`: `Dockerfile`, target `base`.
- `runArgs`: `--name` and `--hostname` `${localWorkspaceFolderBasename}-dev`,
  so the container is recognizable in `docker ps` and the shell prompt.
- Toolchain: `postCreateCommand` `mise trust && mise run setup-dev` (the
  full development toolchain with gh; the container has no `hostrun`).
- Mount: volume `hostrunner-bashhistory-${devcontainerId}` at
  `/home/vscode/.commandhistory`.
- No `initializeCommand`, no `/run/hostrunner` mount, no `.devcontainer`
  read-only mount, no `remoteEnv` (`PATH` comes from the image).

## `.devcontainer/claude/devcontainer.json`

- `build`: `../Dockerfile` with context `..`, target `claude`.
- `runArgs`: `--name` and `--hostname` `${localWorkspaceFolderBasename}-agent`.
- Toolchain: `postCreateCommand` `mise trust && mise run setup-agent`, a
  mise task that installs go only: e2e (node, devcontainer CLI) runs on
  the host, and gh is the host's, through `hostrun`.
- hostrunner wiring as today: `initializeCommand` (`hostrunner up …`), the
  read-only runtime directory at `/run/hostrunner`, the read-only
  `.devcontainer` mount (which also covers `.devcontainer/claude/`), and
  `/run/hostrunner` in `PATH`. The rules stay in `.devcontainer/hostrun.yaml`,
  the default path `hostrunner up` uses; no Go change.
- Read-only mounts of the workspace's `.git/config` and `.git/hooks`.
- Volumes: `hostrunner-bashhistory-${devcontainerId}` at
  `/home/vscode/.commandhistory`, `hostrunner-claude-${devcontainerId}` at
  `/home/vscode/.claude` (login and settings survive a rebuild).
- `remoteEnv` appends only `/run/hostrunner` to the image's `PATH`.

`${devcontainerId}` differs per configuration, so each keeps its own
history.

## Read-only `.git/config` and `.git/hooks`: what they do not stop

They stop the agent from editing the repository's config and hooks in
place, which host git would otherwise pick up through `hostrun git …`.
They do not make `hostrun git` safe, as the README already warns:

- `.git/commondir`: git reads this file in any git directory and then
  takes config and hooks from the directory it names. `.git` itself is
  writable, so the agent can create it, and a file that does not exist
  yet cannot be mounted read-only.
- `mv .git .git.old && mkdir .git` (or restoring a repository there):
  Linux allows renaming a directory that contains mount points, and host
  git then reads the new `.git/config` — e.g. `core.fsmonitor=<command>`,
  which runs on `hostrun git status`.
- A nested repository (`git init x && cd x && hostrun git push`): the
  daemon runs git in the mirrored directory, with `x/.git/config` and
  `x/.git/hooks/pre-push`.

Side effects inside the container: git commands that write the config
(`git config`, `git remote add`, `git branch --set-upstream-to`,
`git push -u` run by the container's own git) fail. `.git/config` is a
single-file bind mount, which follows the file's inode; host git writes
the config to `config.lock` and renames it over, so host-side changes
(e.g. the upstream recorded by `hostrun git push -u`) reach the
container only after a restart.

A real fix (hostrunner running git with forced `-c core.hooksPath=…`,
`-c core.fsmonitor=false`, …) is a separate task.

## Edge cases

- `hostrunner` not in the host's `PATH`: the everyday configuration comes
  up; the Claude one fails with the `hostrunner up` error.
- `.git` is a file (a git worktree) or `.git/hooks` is missing: the bind
  mount source does not exist and the Claude configuration fails to start.
  It requires a regular clone.
- Rebuild: both volumes survive; a changed `CLAUDE_CODE_VERSION` installs
  the new version. Removing the volumes (`docker|podman volume rm`) resets
  history and the Claude login.
- A container with the same name already exists (another clone with the
  same folder name): `docker|podman run` fails, and so does
  `devcontainer up`. A rebuild is not affected: the CLI removes the old
  container before creating the new one.
- Host UID other than 1000: `updateRemoteUserUID` chowns the home folder,
  history directory and `~/.claude` included, so both volumes stay
  writable.

## Verification

Configuration only, no Go code, so no unit tests. The e2e suite covers
hostrunner itself on `examples/devcontainer`; building these images there
would add network-bound, slow builds without testing hostrunner further.
Checked with the devcontainer CLI on the host:

- Everyday: `devcontainer up` succeeds with no `hostrunner` process started
  for it; `/run/hostrunner` does not exist in the container.
- Claude: `devcontainer up --config .devcontainer/claude/devcontainer.json`
  succeeds; `claude --version` prints 2.1.280; `hostrun git status` works;
  writing to `.git/config`, `.git/hooks/` and `.devcontainer/` fails.
- History: a command run in each container is in `history` after
  `devcontainer up --remove-existing-container`.
- Claude login survives the same rebuild.

## mise tasks

`mise run dev` and `mise run agent` run `devcontainer up` for the
respective configuration, then `devcontainer exec … bash` (`raw`, so the
shell gets the terminal). They are interactive: while `initializeCommand`
(`hostrunner up`) runs, the devcontainer CLI forwards its stdin to it, so
input piped into `mise run agent` never reaches the shell.

## Docs

README, Development: the two configurations, how to pick the Claude one,
that only it needs `hostrunner` on the host, and the limits of the
read-only `.git` mounts.
