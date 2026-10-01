# Spec: continuous integration (GitHub Actions)

## Goal

Every pull request into `main` runs the same checks a developer runs
locally. Their job names become required status checks in the `main`
ruleset, after which `main` only changes through pull requests. The
ruleset itself is configured by the repository owner, not by this change.

## Workflow: `.github/workflows/ci.yml`

- Triggers: `pull_request`, `push` to `main`, `workflow_dispatch`.
- `concurrency`: one group per workflow and ref; a newer run cancels an
  older one on the same pull request.
- `permissions: contents: read`.
- No path filters: a required check that never starts would block the
  pull request forever.
- Toolchain: `jdx/mise-action` installs tools from `mise.toml`, and steps
  call `mise run <task>`. Versions have a single source (`mise.toml`) and
  CI runs the same commands as a developer.

### Job `check` (ubuntu-latest)

Installs `go` only, then, failing on the first error:

1. gofmt: `gofmt -l .`; non-empty output lists the files and fails.
2. vet: `go vet -tags e2e ./...`, as in `lefthook.yml`; the tag also vets
   `e2e/`.
3. tidy: `go mod tidy`, then `git diff --exit-code go.mod go.sum`.
4. build: `mise run build` (`CGO_ENABLED=0`).
5. test: `mise run test` (`go test -race ./...`).

### Job `e2e` (ubuntu-latest)

Installs `go`, `node` and `npm:@devcontainers/cli`, then `mise run e2e`.
The runner's Docker brings up `examples/devcontainer`. Podman is not run:
the task enables it only with `HOSTRUNNER_E2E_PODMAN=1`.

- `XDG_RUNTIME_DIR`: runners do not set it, and the test skips without
  it. The job sets it to `mktemp -d /tmp/xdg.XXXXXX` (mode 0700); the path
  stays short because the daemon socket lives under it and Unix socket
  paths are length-limited (`internal/launch`).
- The test skips, rather than fails, on a missing prerequisite. The job
  fails unless the output contains
  `--- PASS: TestDevcontainerIntegration/docker`, so a skipped run is
  never green.
- Steps run with `shell: bash` (`-eo pipefail`), so `mise run e2e | tee`
  keeps the test's exit status.

## Supply chain

- Every third-party action is pinned to a full commit SHA, with the
  release tag in a trailing comment.
- `.github/dependabot.yml`: weekly updates for the `github-actions` and
  `gomod` ecosystems, so the pins do not go stale.

## Required status checks

`check` and `e2e`.

## Out of scope

golangci-lint (a new dependency), coverage upload, release pipelines, the
ruleset itself.

## Edge cases

- A pull request from a fork gets a read-only token and no secrets; the
  workflow needs neither.
- A go.mod/go.sum drift fails `check` at the tidy step, not later.
- Pushes to `main` (merges) run the workflow too, so `main` has its own
  status.

## Tests

No Go code changes, so no unit tests. Acceptance:

- `actionlint` reports no errors for the workflow.
- On a test pull request `check` and `e2e` pass.
- A pull request with an unformatted `.go` file fails `check`.
