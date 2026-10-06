# Spec: take the devcontainers from the template

## Goal

The devcontainer setup that was built in this repository now lives in a
Copier template, [kravlab/devcontainer-template], so other projects can
use it. This repository becomes one of its users: `.devcontainer/` comes
from the template and is updated with `copier update`, and only what is
specific to hostrunner stays here.

[kravlab/devcontainer-template]: https://github.com/kravlab/devcontainer-template

Behaviour does not change: the same two containers, with the same names,
volumes, mounts and host scripts.

## What comes from the template

Applied with `uvx copier copy --overwrite gh:kravlab/devcontainer-template .`,
version `v0.1.0`, recorded in `.copier-answers.yml`:

- `.devcontainer/Dockerfile`, both `devcontainer.json` files and the five
  host scripts. They differ from what was here only in comments and in
  the project's name: `"name"` and the volume names use
  `${localWorkspaceFolderBasename}` in place of the literal `hostrunner`.
  In a folder named `hostrunner` the names are what they were, so the
  bash history and Claude's login survive.
- `.devcontainer/mise-tasks.toml`, new: the tasks `dc`, `dc-task`, `dev`,
  `agent`, `dev-recreate` and `agent-recreate`, which were in `mise.toml`.
  `agent` now joins `devcontainer exec … claude` to its `devcontainer up`
  with `&&`.

The template owns these files: they are not edited here. A change to
them is made in the template and taken with `copier update`.

## What stays here

- `.devcontainer/hostrun.yaml`: the template does not replace an existing
  one. It keeps the rules for `gh issue` and `gh pr`.
- `mise.toml`: the toolchain and `setup-dev` and `setup-agent`, which the
  containers run when they are created. It loses the six tasks and gets
  ```toml
  [task_config]
  includes = [".devcontainer/mise-tasks.toml"]
  ```
  which is how mise finds them. Nothing else here uses mise's default
  task directories, which that setting replaces.
- `examples/devcontainer`, the e2e suite and everything about hostrunner
  itself: untouched.

## What leaves

- `internal/devcontainer/scripts_test.go`: the tests of the host scripts.
  The template has them, rewritten in Python, and tests the scripts where
  they are maintained. `mise run test` and CI no longer test the copies
  here; a copy changes only through `copier update`.
- `docs/specs/claude-devcontainer.md`, `devcontainer-host-config.md`,
  `devcontainer-git-excludes.md` and
  `devcontainer-claude-skills-hooks.md`: moved to the template's
  `docs/specs/`.
- The devcontainer tasks in `docs/specs/mise-tasks.md`: the template's
  `docs/specs/mise-tasks.md` has them. What remains there is `fmt`,
  `lint`, `check` and `pre-commit`.

## Docs

README, Development: the devcontainers come from the template; the
`includes` line in `mise.toml` brings its tasks; `uvx copier update`, run
on the host with nothing uncommitted, takes a new version; the details of
the setup are in the template's specs.

## Edge cases

- The tasks are host tasks. `mise.toml` is shared with the containers, so
  `mise tasks` lists them there too, where they cannot work: a container
  has no container engine. (`setup-dev` does install the devcontainer CLI
  in the everyday container; the image itself has neither.) It was so
  before.
- A running container keeps its configuration until it is recreated
  (`mise run dev-recreate`, `mise run agent-recreate`). Nothing in the
  effective configuration changes here, so there is no need to.
- A clone in a folder not named `hostrunner` gets containers and volumes
  named after that folder, as `--name` and `--hostname` already were.
- `copier update` refuses a tree with uncommitted changes, and runs on the
  host: the Claude Code container mounts `.devcontainer/` read-only.

## Verification

No Go code changes. Checked on the host:

- `mise run check` and `mise run test-scripts` pass; `mise run e2e`
  passes.
- `mise tasks` lists the six tasks with `.devcontainer/mise-tasks.toml` as
  their source.
- `devcontainer read-configuration` gives both configurations the names
  and volumes they had.
- `mise run dev-recreate`, then `mise run dc -- go version`: the everyday
  container is built from the template's files and runs.
- The running Claude Code container is left alone; it takes the files at
  its next `mise run agent-recreate`.
