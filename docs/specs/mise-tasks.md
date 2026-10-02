# Spec: mise tasks for formatting, linting, hooks and the devcontainer

## Goal

Every routine check has one `mise run` entry point, used the same way by a
developer, the pre-commit hook's users and CI; and any command or mise
task can be run inside the everyday devcontainer from the host.

## Tasks (`mise.toml`)

- `fmt`: `gofmt -w .` — rewrites the Go files.
- `lint`: checks, never rewrites, failing on the first problem:
  1. `gofmt -l .`; non-empty output lists the files and fails.
  2. `go vet -tags e2e ./...` (the tag also vets `e2e/`).

  `fmt` and `lint` cover the same files (`.`), as CI does, so whatever
  `lint`'s gofmt check reports, `fmt` fixes (`go vet` findings need code
  changes). The pre-commit hook runs the same checks on commit (gofmt on
  the staged files only, `go vet` on the module); `mise.toml` and
  `lefthook.yml` point at each other so they are changed together.
- `check`: `depends = ["lint", "test"]` — the quick pre-PR check; the two
  run in parallel.
- `test`, `e2e`: unchanged.
- `pre-commit`: `lefthook run pre-commit` — the hook's jobs on the staged
  files, as `git commit` runs them. Arguments are appended:
  `mise run pre-commit -- --all-files` checks every file.
- `dc`: `mise run dc -- <cmd> [args…]` runs a command in the everyday
  devcontainer (`.devcontainer/devcontainer.json`):
  1. `devcontainer up --workspace-folder . >/dev/null </dev/null` —
     idempotent; starts the container if needed. `>/dev/null` drops its
     JSON result (build logs still go to stderr); for `</dev/null`, see
     the edge cases.
  2. `devcontainer exec --workspace-folder .` with the arguments (see
     "Arguments" below).

  The command runs directly, without a shell (like `docker exec`): the
  arguments reach it unchanged. Pipes, `&&` or `$VAR` need an explicit
  shell: `mise run dc -- bash -c '…'`. Tools come from the image's mise
  shims in `PATH`. `raw = true`, so interactive commands get the
  terminal.
- `dc-task`: `mise run dc-task -- <task> [args…]` runs
  `mise run <task> [args…]` in the same container, through `dc`
  (`mise run dc -- mise run`).
- `dev`: `mise run dc -- bash` (was its own `devcontainer up && exec
  bash`).
- `agent`: its `devcontainer up` also gets `</dev/null` (see the stdin
  edge case), so what is typed while the container starts reaches
  `claude`.

Arguments: mise appends a task's arguments, shell-quoted, to its last
command, so `dc`, `dc-task` and `pre-commit` pass them on exactly
(checked with mise 2026.9.11: spaces, `'`, `"`, `$HOME`, `*`, `$(…)`,
backticks, `;`, `-h` and `--help` arrive unchanged and nothing is
expanded). No `usage` spec and no `eval`: with `-h`/`--help` among the
arguments mise skips usage and leaves its variables empty, and an `eval`
of the appended arguments would run them as shell code on the host.

## CI (`.github/workflows/ci.yml`)

Job `check`: the `gofmt` and `vet` steps become one step,
`mise run lint`. `lefthook.yml` keeps its jobs (staged files only) and
gains a comment pointing at the `lint` task.

## Documentation

- `mise.toml` header comment: the task list.
- README "Development": `fmt`, `lint`, `check`, `pre-commit`, `dc`,
  `dc-task`.
- `docs/specs/ci.md`: the `check` job's steps.

## Edge cases

- `dc` without arguments: `devcontainer exec` fails with
  `exec: "undefined": executable file not found` (exit 127). `dc-task`
  without arguments runs a bare `mise run` in the container (mise's own
  behaviour: a task picker on a terminal, an error without one). Accepted
  in exchange for passing arguments without `usage`/`eval`.
- Arguments with spaces, quotes, `$(…)` or `--help` reach the container
  unchanged; nothing runs on the host.
- Arguments before the command that start with `-` (`mise run dc --
  --help`) are taken by `devcontainer exec` as its own options, since
  the CLI parses options up to the first non-option argument. Harmless:
  they only configure the user's own CLI call.
- A stopped or missing container: `dc` brings it up (a first run builds
  the image).
- stdin reaches the command (`echo … | mise run dc -- bash`):
  `devcontainer up` reads stdin to the end, so `dc` runs it with
  `</dev/null`. `agent` does the same: on a terminal, `up` would
  otherwise consume what is typed while the container starts.
- The command's exit code is `dc`'s exit code.
- `lint` on a tree with an unformatted file: fails and names it; `fmt`
  then fixes it.

## Tests

No Go code changes, so no unit tests. Acceptance:

- In a copy of the repository (scratchpad), an unformatted `.go` file
  makes `mise run lint` fail; after `mise run fmt`, `lint` passes.
- `mise run check` passes.
- `mise run pre-commit` runs the hook's jobs (skipped without staged
  `.go` files); `-- --all-files` runs them on all files.
- `mise run dc` without arguments fails (exit 127).
- `mise run dc -- go version` runs in the container (builds the image if
  needed; with approval).
- `mise run dc -- sh -c 'echo "$1"' _ 'a b'` prints `a b`.
- `mise run dc -- printf '[%s]' 'a b' '$(touch <host file>)' --help`
  prints the arguments unchanged and creates no host file.
- `mise run dc-task -- <task> 'a b' --help` passes both arguments to the
  task unchanged.
- `mise run dc-task -- lint` passes in the container.
- `agent`: its script with `claude` replaced by `cat` passes piped stdin
  through (`echo hi | …` prints `hi`; without `</dev/null` on `up` it
  prints nothing). Typed-ahead input on a terminal is a manual check
  only, since it needs an interactive terminal.
- `actionlint` reports no errors; `check` and `e2e` pass on the pull
  request.
