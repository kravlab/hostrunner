# Spec: releases (GoReleaser on a version tag)

## Goal

Pushing a tag `vX.Y.Z` on a commit of `main` publishes a GitHub Release
with ready-to-run Linux binaries, after the same checks a pull request
passes. `hostrunner version` reports which release (or commit) a binary
was built from.

## Cutting a release

By hand, from an up-to-date `main`:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The tag name is the version; nothing in the repository is edited to
release. Only `v0.x.y` and `v1.x.y` tags give a real version: from
`v2.0.0` on, Go requires the module path to end in `/v2`, and without it
the binaries report a pseudo-version.

## Version: `hostrunner version`

- Prints the main module version from `runtime/debug.ReadBuildInfo()`,
  followed by a newline, to stdout, and exits 0.
  - Built from a tag with a clean tree (release, `go install …@v0.1.0`):
    `v0.1.0`.
  - Built between tags (`mise run build`): the pseudo-version Go derives
    from git, e.g. `v0.1.1-0.20261001120000-abcdef123456`, with
    `+dirty` for uncommitted changes.
  - No build info, or the version `(devel)` or empty (no VCS data):
    `dev`.
- Flags are parsed like the other subcommands' (`parseFlags`): `-h`
  prints the usage, and extra arguments are an error.
- The top-level usage lists the subcommand: `up|serve|version`.
- `hostrun` gets no version command: its arguments are the host command
  to run.

No ldflags: the version comes from the Go toolchain, so `go install`
builds report it too.

## Build: `.goreleaser.yaml`

- `version: 2`; GoReleaser pinned in `mise.toml` `[tools]`.
- Two builds sharing their settings through a YAML anchor,
  `./cmd/hostrunner` and `./cmd/hostrun`, each for
  `linux/amd64` and `linux/arm64`, `CGO_ENABLED=0` (`hostrun` must be
  static, `internal/launch` refuses a dynamic one), flags `-trimpath`,
  ldflags `-s -w` (replaces GoReleaser's default, which sets an unused
  `main.version`).
- One archive per architecture,
  `hostrunner_<version>_linux_<arch>.tar.gz`, holding `hostrunner`,
  `hostrun`, `LICENSE`, `README.md`. Both binaries side by side, because
  `hostrunner up` installs the `hostrun` found next to it.
- `checksums.txt`: SHA-256 of the archives.
- Changelog from the commits since the previous tag, grouped by the
  Conventional Commits type (features, fixes, other); commits whose
  subject starts with `Merge ` are excluded (GoReleaser has no real merge
  filter; pull requests are squash-merged anyway).
- `release.replace_existing_artifacts: true`: a re-run for the same tag
  replaces the uploaded assets instead of failing on them.
- `dist/` goes into `.gitignore`: GoReleaser writes there, and an
  untracked file would make Go stamp the release `+dirty`.

## Workflow: `.github/workflows/release.yml`

- Trigger: `push` of tags `v*.*.*`.
- Job `ci`: `uses: ./.github/workflows/ci.yml`, so `ci.yml` gains a
  `workflow_call` trigger. A release never ships a commit that fails
  `check` or `e2e`.
- Job `release`, `needs: ci`, ubuntu-latest:
  - `actions/checkout` with `fetch-depth: 0` (the changelog needs the
    previous tag, the next step `origin/main`);
  - the tagged commit must be on `main`
    (`git merge-base --is-ancestor "$GITHUB_SHA" origin/main`), so only
    reviewed code is released;
  - `jdx/mise-action` installing `go goreleaser`;
  - `goreleaser release --clean` with `GITHUB_TOKEN`;
  - `actions/attest-build-provenance` with
    `subject-checksums: dist/checksums.txt`: users verify an archive with
    `gh attestation verify <archive> --repo kravlab/hostrunner`.
- Permissions: workflow default `contents: read`; job `release`:
  `contents: write`, `id-token: write`, `attestations: write`.
- Actions pinned by commit SHA (Dependabot already updates them).

## CI: `.github/workflows/ci.yml`

- New trigger `workflow_call` (see above).
- Job `check`: installs `go goreleaser`, and runs `goreleaser check`, so
  a broken `.goreleaser.yaml` fails the pull request, not the release.

## Tag protection

A repository ruleset `release tags` on `refs/tags/v*`: no deletion, no
update (a published tag cannot be moved). Creating tags stays allowed.
Configured through `gh api` after the merge.

## Documentation

- README "Install": download the archive for the host's architecture from
  the Releases page, check it (`sha256sum -c` against `checksums.txt`
  or `gh attestation verify`), unpack both binaries into a directory in
  `PATH`; building from source with `mise run install` stays.
- README: how to cut a release (the two git commands above).
- `cmd/hostrunner` package doc: the `version` subcommand.

## Edge cases

- A tag on a commit that fails CI: `ci` fails, no release is created.
- A tag that is not `vX.Y.Z` (e.g. `test`): no workflow runs.
- A pre-release tag `v0.2.0-rc.1`: GoReleaser marks the release as a
  pre-release (`prerelease: auto`).
- Re-running a failed release job: `--clean` rebuilds `dist/`, and the
  existing release's assets are replaced, so a run that failed after
  publishing (e.g. at the attestation step) can be completed.
- A tag on a commit that is not on `main`: the release job fails before
  GoReleaser runs; nothing is published.
- A tag `v2.0.0` or later: released, but the binaries report a
  pseudo-version (see "Cutting a release").
- arm64 binaries are cross-compiled and not exercised by CI.

## Tests

- Unit (TDD, `cmd/hostrunner`): the version string for build info
  `v0.1.0`, a pseudo-version, `(devel)`, empty, and missing build info;
  `run` with `version` writes the version to stdout; `version` with an
  extra argument is an error; `version -h` returns `flag.ErrHelp`.
- `goreleaser check` passes.
- `goreleaser release --snapshot --clean` locally: both archives contain
  the four files; both binaries are static ELF for their architecture;
  the amd64 `hostrunner version` prints a version, not `dev`.
- `actionlint` reports no errors.
- First real release: CI on the pull request is green; after the merge a
  pushed tag produces a release with two archives, `checksums.txt`, and
  an attestation that `gh attestation verify` accepts.
