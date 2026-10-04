# Spec: host global skills and hooks in the Claude Code devcontainer

## Goal

In the Claude Code devcontainer only
(`.devcontainer/claude/devcontainer.json`), Claude Code:

1. loads the host user's global skills, the entries of `~/.claude/skills`
   on the host;
2. runs the host user's global hooks, the `hooks` key of
   `~/.claude/settings.json` on the host.

Both are copied on every `devcontainer up`, so host edits reach the
container, a running one included, after the next `up`, without recreating
it. A host with no skills, no `settings.json` or no hooks is not an error.
Nothing else of the host's `~/.claude` reaches the container: not the rest
of `settings.json` (`env`, `permissions`, …), not the login, not the
history.

## Why not the container's own `~/.claude`

The container's `~/.claude` is a named volume and is `CLAUDE_CONFIG_DIR`
(see `claude-devcontainer.md`). Claude Code keeps its own state there:
checked in the running container, `~/.claude/skills/synced` holds the
skills it syncs from the signed-in account, and `~/.claude/settings.json`
is the container's own.

- A read-only mount over `~/.claude/skills` would stop that sync from
  writing.
- Merging the host's hooks into the volume's `settings.json` would change
  state the container owns, and the agent could rewrite or remove them.
- `claude --settings <file>` applies only to sessions started with the
  flag, so only through `mise run agent`.

So both go to Claude Code's system directory, which on Linux is
`/etc/claude-code/`:

- skills: `.claude/skills/<skill-name>/SKILL.md` in that directory (the
  "Enterprise" location in
  <https://code.claude.com/docs/en/skills.md>);
- hooks: a drop-in file in `managed-settings.d/`
  (<https://code.claude.com/docs/en/managed-settings.md>). Hook entries
  merge across settings levels rather than replacing each other
  (<https://code.claude.com/docs/en/hooks.md>), so the container's own
  hooks still run, and a file-based policy is reloaded when the file
  changes.

The volume is not touched, and the agent can neither edit nor disable
what the host passes: `disableAllHooks` in user, project or local settings
does not disable managed hooks.

## Layout

Both scripts below run on the host from `initializeCommand` and write
into `<dir>/claude/`, where `<dir>` is the per-container directory of
`devcontainer-host-config.md`,
`${localEnv:XDG_RUNTIME_DIR}/hostrunner-host-config/${devcontainerId}`.
Each creates `<dir>/claude` with `mkdir -p`. What they write is
world-readable (as in `devcontainer-host-config.md`), so it is readable
under rootless Podman's user namespace too.

| Host                                   | `<dir>`              | Container                                            |
| :------------------------------------- | :------------------- | :--------------------------------------------------- |
| `~/.claude/skills/*` (not `synced`)    | `claude/skills/`     | `/etc/claude-code/.claude/skills`                    |
| `hooks` of `~/.claude/settings.json`   | `claude/hooks.json`  | `/etc/claude-code/managed-settings.d/host-hooks.json` |
| —                                      | `claude/`            | `${localEnv:HOME}/.claude` (the host's path)         |

## Skills: `.devcontainer/claude/skills.sh <dir>`

A POSIX `sh` script. The source is `$HOME/.claude/skills`.

- Copies every non-hidden entry of the source except `synced` into a
  temporary directory inside `<dir>/claude` with `cp -RL`, so symlinks
  are replaced by what they point to, at any depth. On the host the
  entries are usually symlinks out of `~/.claude` (relative, e.g.
  `../../.agents/skills/<name>`, or absolute), which a bind mount of the
  directory would leave dangling in the container.
- `synced` is skipped: it is Claude Code's own copy of the account's
  skills, and the container syncs its own into its volume.
- The source missing or empty is not an error: `<dir>/claude/skills`
  ends up an empty directory.
- A copy failure (a dangling symlink, an unreadable file) fails the
  script and `devcontainer up` with `cp`'s message, and the old
  `<dir>/claude/skills` stays: a skill the user installed must not
  silently disappear.
- The temporary directory is made world-readable and owner-writable
  (`chmod -R u+w,a+rX`): `mktemp -d` creates it 0700, and `cp` keeps the
  mode of a read-only skill directory, which the next run could not
  remove. It then replaces `<dir>/claude/skills`: the old directory is
  renamed aside, the new one renamed into place, the old one removed. A
  skill removed on the host disappears. Between the two renames the
  directory is missing for a moment; `rename` cannot replace a non-empty
  directory, and nothing reads the directory during `devcontainer up` but
  a session that happens to list skills right then. A script that fails
  or is interrupted between them renames the old directory back.

## Hooks: `.devcontainer/claude/hooks.sh <dir>`

A POSIX `sh` script. The source is `$HOME/.claude/settings.json`.

- Writes `<dir>/claude/hooks.json`: `{"hooks": <the source's hooks>}`
  when the source has a `hooks` key, `{}` otherwise or when the source
  does not exist. Only that key is passed.
- The key is extracted with `jq`, needed on the host only when the
  source exists. `jq` missing then, or a source that is not exactly one
  JSON object (not JSON, an empty file, several values, an array,
  `null`, a dangling symlink), fails the script and `devcontainer up`
  with the tool's message, and the old `hooks.json` stays: hooks the user
  set must not silently disappear.
- Written to a temporary file in `<dir>/claude`, `chmod 0644`ed, then
  `mv`ed over the old one, as `git-identity.sh` does: a reader never
  sees a half-written file, and hooks removed on the host disappear.

Hook commands are passed unchanged and run in the container, as the
container's user.

## Image: `.devcontainer/Dockerfile`, stage `claude`

As root, like the `~/.config/git/ignore` link of
`devcontainer-git-excludes.md`:

- `/etc/claude-code/.claude/skills` → `/run/host-config/claude/skills`;
- `/etc/claude-code/managed-settings.d/host-hooks.json` →
  `/run/host-config/claude/hooks.json`.

Without the mount both links dangle, which leaves Claude Code with no
enterprise skills and no drop-in file.

## `.devcontainer/claude/devcontainer.json`

- `initializeCommand` gains:
  - `skills`:
    `["${localWorkspaceFolder}/.devcontainer/claude/skills.sh", "<dir>"]`;
  - `hooks`:
    `["${localWorkspaceFolder}/.devcontainer/claude/hooks.sh", "<dir>"]`.
- The existing `/run/host-config` mount of `<dir>` already carries
  `claude/`; it is a directory mount, so the replaced `skills` directory
  and `hooks.json` are seen by a running container.
- New mount, read-only:
  `source=<dir>/claude,target=${localEnv:HOME}/.claude,type=bind,readonly`.
  The copy is also visible at the path the host's `~/.claude` has on the
  host, so a hook command written with an absolute host path into
  `~/.claude/skills` (e.g.
  `sed … /var/home/<user>/.claude/skills/<name>/SKILL.md`) runs unchanged.
  Hook commands are not rewritten: a path in a shell command cannot be
  rewritten reliably. The source is `<dir>/claude` itself, which the
  scripts never replace, so the mount keeps following the replaced
  `skills` directory.

The everyday configuration (`.devcontainer/devcontainer.json`) does not
change: it has no Claude Code.

## Edge cases

- No `~/.claude/skills` on the host: the container has no host skills.
- No `~/.claude/settings.json`, or one without `hooks`: `hooks.json` is
  `{}`, no host hooks; `jq` is not needed for the former.
- A hook command that uses a host path outside `~/.claude/skills`, or a
  tool the image lacks, fails in the container. The image has `sed` and
  `jq` (checked in the running container).
- A skill or hook changed on the host: in the container after the next
  `devcontainer up` (`mise run agent`). The documentation promises
  pick-up within a running session for `~/.claude/skills`, the project's
  and `--add-dir` skills, and says nothing of the enterprise location: a
  running session may need `/reload-skills` or a restart.
- A skill with the same name in the host's copy and in the container
  (project or account skill): Claude Code's own precedence between
  locations applies.
- The account is under an organization's server-managed settings: by
  default (`first-wins`) Claude Code uses the highest-ranked managed
  source and ignores the files in `/etc/claude-code`, so the host's hooks
  do not run.
- The host's `$HOME` is `/home/vscode`: the new mount's target would be
  the container's own `~/.claude` and hide the volume. Not supported.
- `CLAUDE_CONFIG_DIR` set on the host: not followed; the scripts read
  `$HOME/.claude`.
- `XDG_RUNTIME_DIR` is cleared at logout; the next `devcontainer up`
  writes the directory again.
- The existing container must be recreated once (`mise run
  agent-recreate`) to get the image's links and the new mount.

## Verification

Scripts: automated, in `internal/devcontainer/scripts_test.go`, each run
with only `PATH` and a temporary `HOME`, as the other scripts' tests.
The `hooks.sh` tests need `jq` on the machine that runs them.

- `skills.sh`:
  - a skill that is a real directory, one behind a relative symlink and
    one behind an absolute symlink are all copied as directories with
    their files' content; a symlink inside a skill is copied as the file
    it points to;
  - `synced` and hidden entries are not copied;
  - no `~/.claude/skills`, and an empty one: exits 0, `<dir>/claude/skills`
    is an empty directory;
  - a skill removed on the host is gone after the next run;
  - a dangling symlink: exits non-zero with `cp`'s message, the old copy
    stays;
  - a skill whose directory is read-only: a second run still replaces
    the copy;
  - the copy is readable by others.

  The rename back after a failure between the two renames is not tested:
  nothing the script's caller controls makes the second rename fail.
- `hooks.sh`:
  - a `settings.json` with `hooks` and other keys: `hooks.json` has the
    same `hooks` and no other key;
  - no `settings.json`, and one without `hooks`: exits 0, `hooks.json`
    is `{}`;
  - hooks removed on the host are gone after the next run;
  - a `settings.json` that is not JSON, an array, `null`, empty, holds
    several values or is a dangling symlink: exits non-zero with `jq`'s
    message, the old `hooks.json` stays;
  - without `jq` in `PATH`: no `settings.json` exits 0 with `{}`; with
    one, exits non-zero naming `jq`, the old `hooks.json` stays;
  - `hooks.json` is readable by others.

Container, by hand, after `mise run agent-recreate`:

- the documentation does not say whether Claude Code follows a symlinked
  `/etc/claude-code/.claude/skills` or a symlinked drop-in file. Checked
  first: the host's skills are listed by `/skills` and the host's hook by
  `/hooks`. If either link is not followed, this spec is changed (direct
  bind mounts in place of the links) before any further work;
- the host's `UserPromptSubmit` hook, which reads a skill through its
  absolute host path, adds its context to a prompt;
- `~/.claude/skills/synced` in the volume is still written by the
  container;
- `/etc/claude-code` and `${localEnv:HOME}/.claude` are not writable;
- a skill added on the host shows up after `devcontainer up`, without
  recreating the container.

## Docs

README, Development: the Claude container takes the host's global skills
and hooks, copied on every `devcontainer up`; what is not passed; `jq` on
the host. `devcontainer-host-config.md`: a pointer to this spec. Comments
in `.devcontainer/claude/devcontainer.json` and the `Dockerfile`.
