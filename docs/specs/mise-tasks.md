# Spec: mise tasks for formatting, linting and hooks

## Goal

Every routine check has one `mise run` entry point, used the same way by a
developer, the pre-commit hook's users and CI.

The tasks that run things in the devcontainers (`dc`, `dc-task`, `dev`,
`agent` and the recreate tasks) were specified here too. They moved to
the devcontainer template with `.devcontainer/` (see
`devcontainer-template.md`); the template's own `docs/specs/mise-tasks.md`
has them.

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

Arguments: mise appends a task's arguments, shell-quoted, to its last
command, so `pre-commit` passes them on as given.

## CI (`.github/workflows/ci.yml`)

Job `check`: the `gofmt` and `vet` steps become one step,
`mise run lint`. `lefthook.yml` keeps its jobs (staged files only) and
gains a comment pointing at the `lint` task.

## Documentation

- `mise.toml` header comment: the task list.
- README "Development": `fmt`, `lint`, `check`, `pre-commit`.
- `docs/specs/ci.md`: the `check` job's steps.

## Edge cases

- `lint` on a tree with an unformatted file: fails and names it; `fmt`
  then fixes it.

## Tests

No Go code changes, so no unit tests. Acceptance:

- In a copy of the repository (scratchpad), an unformatted `.go` file
  makes `mise run lint` fail; after `mise run fmt`, `lint` passes.
- `mise run check` passes.
- `mise run pre-commit` runs the hook's jobs (skipped without staged
  `.go` files); `-- --all-files` runs them on all files.
- `actionlint` reports no errors; `check` and `e2e` pass on the pull
  request.
