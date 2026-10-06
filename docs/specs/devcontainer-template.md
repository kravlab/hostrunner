# Spec: take the devcontainers from the template

## Goal

The devcontainer setup that was built in this repository now lives in a
Copier template, [kravlab/devcontainer-template], so other projects can
use it. This repository becomes one of its users: `.devcontainer/` and
the mise config for the containers come from the template and are updated
with Copier (`mise run template-update`), and only what is specific to
hostrunner stays here.

[kravlab/devcontainer-template]: https://github.com/kravlab/devcontainer-template

Behaviour does not change: the same two containers, with the same names,
volumes, mounts and host scripts.

## What comes from the template

Applied with `uvx copier copy --overwrite gh:kravlab/devcontainer-template .`
at version `v0.1.0`, and updated to `v0.2.0` since (see "Version `v0.2.0`");
`.copier-answers.yml` records the version in use:

- `.devcontainer/Dockerfile`, both `devcontainer.json` files and the five
  host scripts. They differ from what was here only in comments and in
  the project's name: `"name"` and the volume names use
  `${localWorkspaceFolderBasename}` in place of the literal `hostrunner`.
  In a folder named `hostrunner` the names are what they were, so the
  bash history and Claude's login survive.
- `.config/mise/conf.d/devcontainer.toml`, new: a mise config that mise
  reads by itself next to `mise.toml`. It has the tasks `dc`, `dc-task`,
  `dev`, `agent`, `dev-recreate` and `agent-recreate`, which were in
  `mise.toml` (`agent` now joins `devcontainer exec … claude` to its
  `devcontainer up` with `&&`), and what `v0.2.0` added: the task
  `template-update`, a cooldown for new releases and the oldest mise it
  works with.

The template owns these files: they are not edited here. A change to
them is made in the template and taken with `mise run template-update`.

## Version `v0.2.0`

Taken with the command the template's release notes give for a project at
`v0.1.0`, which has no `template-update` yet:
`mise exec uv@0.12.17 -- uvx --exclude-newer "5 days" copier@9.18.2 update`.

- The tasks move from `.devcontainer/mise-tasks.toml`, which the update
  removes, to `.config/mise/conf.d/devcontainer.toml`. `mise.toml` loses
  the `[task_config] includes` line that named the old file; mise would
  not complain about it, but it hides mise's default task directories.
- `mise run template-update` takes the template's later versions. The
  host needs only mise for it: mise brings uv, which runs Copier.
- `minimum_release_age = "5d"`: mise does not install a release that has
  been out for less than five days, on the host, in the containers and in
  CI. Every tool in `mise.toml` is pinned exactly, and mise installs an
  exact pin whatever its age, so what is installed does not change. One
  reservation: for an `npm:` tool mise passes the cooldown on to npm,
  which applies it to the tool's own unpinned dependencies.
  `npm:@devcontainers/cli` 0.89.0 has none.
- `min_version = "2026.5.0"`: an older mise refuses the project instead of
  installing without the cooldown. The containers have 2026.9.11, pinned
  in the `Dockerfile`; CI takes the mise that `jdx/mise-action` installs.
- Nothing the containers are built or configured from changes (under
  `.devcontainer/` only the old tasks file goes), so they need not be
  recreated.
- The tasks file was under `.devcontainer/`, which the Claude Code
  container mounts read-only; the new one is in the writable workspace.
  That gives the agent no new way to the host: `mise.toml`, where the same
  tasks could be replaced, was writable there already.

## What stays here

- `.devcontainer/hostrun.yaml`: the template does not replace an existing
  one. It keeps the rules for `gh issue` and `gh pr`.
- `mise.toml`: the toolchain and `setup-dev` and `setup-agent`, which the
  containers run when they are created. It loses the six tasks, and no
  setting in it names the template's config: mise finds that by itself.
- `examples/devcontainer`, the e2e suite and everything about hostrunner
  itself: untouched.

## What leaves

- `internal/devcontainer/scripts_test.go`: the tests of the host scripts.
  The template has them, rewritten in Python, and tests the scripts where
  they are maintained. `mise run test` and CI no longer test the copies
  here; a copy changes only through `mise run template-update`.
- `docs/specs/claude-devcontainer.md`, `devcontainer-host-config.md`,
  `devcontainer-git-excludes.md` and
  `devcontainer-claude-skills-hooks.md`: moved to the template's
  `docs/specs/`.
- The devcontainer tasks in `docs/specs/mise-tasks.md`: the template's
  `docs/specs/mise-tasks.md` has them. What remains there is `fmt`,
  `lint`, `check` and `pre-commit`.

## Docs

README, Development: the devcontainers come from the template; its mise
config brings the tasks, the cooldown and the oldest mise;
`mise run template-update`, run on the host with nothing uncommitted,
takes a new version; the details of the setup are in the template's
specs.

## Edge cases

- The tasks are host tasks. The mise config is shared with the
  containers, so `mise tasks` lists them there too, where the six
  container tasks cannot work: a container has no container engine.
  (`setup-dev` does install the devcontainer CLI in the everyday
  container; the image itself has neither.) It was so before.
  `template-update` cannot work in the Claude Code container, which
  mounts `.devcontainer/` read-only.
- A running container keeps its configuration until it is recreated
  (`mise run dev-recreate`, `mise run agent-recreate`). Nothing in the
  effective configuration changes here, so there is no need to.
- A clone in a folder not named `hostrunner` gets containers and volumes
  named after that folder, as `--name` and `--hostname` already were.
- `template-update` fails in a tree with uncommitted changes (Copier
  refuses it), and runs on the host.
- A host with mise older than 2026.5.0: mise refuses the repository's
  config, naming the version it needs (checked with `mise tasks ls`).

## Verification

No Go code changes. Checked on the host:

- `mise run check` and `mise run test-scripts` pass; `mise run e2e`
  passes.
- `mise tasks` lists the seven tasks with
  `.config/mise/conf.d/devcontainer.toml` as their source, and
  `mise settings get minimum_release_age` gives `5d`.
- `mise run template-update` keeps the version in use when it is the
  latest.
- In a copy of the two mise files with an empty `HOME`, `mise install`
  installs all seven tools at their pinned versions, cooldown in force.
- `devcontainer read-configuration` gives both configurations the names
  and volumes they had.
- `mise run dev-recreate`, then `mise run dc -- go version`: the everyday
  container is built from the template's files and runs.
- The running Claude Code container is left alone; it takes the files at
  its next `mise run agent-recreate`.
