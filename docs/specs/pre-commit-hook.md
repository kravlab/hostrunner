# Spec: pre-commit hook (lefthook)

## Goal

`git commit` with Go files in the index fails when a staged `.go` file is
not gofmt-formatted or `go vet` reports a problem. The hook works on the
host, in the everyday devcontainer and in the Claude Code devcontainer.

In the Claude container `.git/hooks` is the host's, mounted read-only
(`.devcontainer/claude/devcontainer.json`), and the agent commits with the
container's own git (`hostrun.yaml` has no `git commit`). So the hook
installed on the host also runs in that container, and the binary it calls
must be installed there too.

## Tool: lefthook, pinned by mise

- `mise.toml` `[tools]`: `lefthook = "2.1.14"` (aqua backend). A single Go
  binary: no Python in the containers, staged-file filtering and parallel
  jobs built in.
- `setup` task: installs `lefthook` with the rest of the toolchain, then
  runs `lefthook install`, which writes `.git/hooks/pre-commit`.
- `setup-agent` task: installs `go lefthook`, without `lefthook install`:
  `.git/hooks` is read-only in the Claude container and comes from the
  host.

## Checks: `lefthook.yml` at the repository root

`pre-commit`, jobs run in parallel:

- `gofmt`, glob `*.go`: `gofmt -l {staged_files}`; non-empty output prints
  the unformatted files and exits 1. Files are not rewritten.
- `go vet`, glob `*.go`: `go vet -tags e2e ./...`; the tag also vets
  `e2e/`.

## Documentation

- `.devcontainer/claude/devcontainer.json` comment ("Only go from
  mise.toml") and the `setup-agent` description: go and lefthook.
- README install section: `mise run setup` installs the hook.
- README warning: `lefthook.yml` lives in the workspace, which the
  container can write; a plain `git commit` on the host then runs the
  commands written there. The same class of risk as `.git/hooks`.

## Edge cases

- No `.go` files staged: both jobs are skipped.
- gofmt checks the working-tree file, not the staged version: lefthook
  does not hide unstaged changes by default.
- Deleted files are not in `{staged_files}`.
- `git commit --no-verify` bypasses the hook (git's behavior).

## Tests

No Go code changes, so no unit tests. Acceptance:

- `lefthook validate` passes.
- A staged unformatted `.go` file: `lefthook run pre-commit` fails.
- A staged formatted `.go` file: it passes.
- A commit without `.go` files: both jobs are skipped.
