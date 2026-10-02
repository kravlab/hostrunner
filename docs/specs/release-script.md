# Spec: release script (`mise run release`)

## Goal

Cutting a release takes one command instead of the two hand-typed git
commands in `release.md`, and lets the releaser pick a commit of
`origin/main` other than the latest. Every check the release workflow
would fail on, and those that would give a wrong release, runs before
the tag is pushed: a pushed `v*` tag can be neither moved nor deleted
(ruleset `release tags`).

The workflow (`release.yml`) does not change: it already builds the
commit the tag points to and refuses one that is not on `main`.

## Usage

```sh
mise run release -- [-n N] <version>
```

- `<version>`: the tag to create, `vX.Y.Z` or `vX.Y.Z-<pre-release>`
  (e.g. `v0.2.0-rc.1`).
- `-n N`: how many commits of `origin/main` to list, default `10`.
- `-h`: prints the usage and exits 0.

Runs on the host: it pushes with the host's git credentials.

## Behavior

1. Parse the arguments. Errors (exit 2, usage on stderr): no version,
   more than one positional argument, an unknown flag, `-n` not a
   positive integer.
2. Check the version format: semver 2.0.0 with a `v` prefix and
   without build metadata (pre-release identifiers non-empty, numeric
   ones without leading zeros).
   A major version of 2 or more is refused: Go needs a `/v2` module path,
   without it the binaries report a pseudo-version (`release.md`).
3. `git fetch --tags origin`: the list, the tag checks and the main
   check use the remote's current state.
4. The tag must not exist locally (checked before the fetch too, so a
   local tag that differs from origin's is reported as existing) or on
   `origin`.
5. The version must be greater than the highest existing version tag
   (`v*` tags that are not versions are ignored), by semver 2.0.0
   precedence: a pre-release sorts before its release
   (`v0.2.0-rc.1 < v0.2.0`), identifiers compare one by one, numeric
   ones numerically and below alphanumeric ones. Implemented in bash, so
   it does not depend on the host's `sort`.
6. List the last `N` commits of `origin/main` (first parent; pull
   requests are squash-merged), newest first, numbered from `0`:

   ```text
   0  7dea4a0  docs(agents): add hostrun usage (#15)
   1  c298e19  fix(tooling): keep agent stdin (#14)
   …
   ```

   Fewer than `N` commits on `origin/main`: list all of them.
7. Prompt `commit number [0-<last>]: ` on stderr and read one line from
   stdin. Anything but an integer in the listed range, or end of input,
   is an error (exit 1); nothing is created.
8. The selected commit must contain the highest existing tag's commit
   (`git merge-base --is-ancestor <tag> <commit>`): otherwise the
   changelog ("commits since the previous tag") and the version order
   disagree. No tag yet: no check.
9. Prompt `push <version> -> <short hash>? [y/N] ` on stderr and read
   one line from stdin (a last line without a newline counts). Only `y`
   or `Y` continues; anything else, or end of input, exits 1 with
   nothing created.
10. Create an annotated tag `<version>` on the selected commit, message
    `<version>`, and push it: `git push origin <version>` (allowed by
    the `git push` rule in `.devcontainer/hostrun.yaml`).
11. Print the tag, the commit and, for a GitHub `origin`, the Actions
    URL of the release workflow, and exit 0.

Every failure before step 10 leaves no tag behind. If the push in
step 10 fails, or the script is interrupted (Ctrl-C, SIGTERM) after the
tag is created and before it is pushed, the local tag is deleted, so a
retry starts clean.

Messages go to stderr, prefixed `release: `; only the list (step 6)
and the result (step 11) go to stdout.

## Files

- `scripts/release.sh`: the script (bash, `set -euo pipefail`),
  each function documented.
- `mise.toml`: task `release`, `raw = true` so the prompt reads the
  terminal, running `scripts/release.sh`.
- `docs/specs/release.md` "Cutting a release" and README "Releases":
  `mise run release -- v0.2.0` replaces the two git commands (kept as
  the manual fallback).

## Edge cases

- Version without `v`, with a leading zero (`v0.01.0`), or with build
  metadata (`v0.2.0+x`): refused by the format check.
- Tag exists only on `origin`: refused (step 4, after the fetch).
- `v0.2.0` after `v0.2.0-rc.1`: allowed (greater by semver).
- `v0.1.1` when `v0.2.0` exists: refused (step 5); hotfixes of older
  lines need a release branch, which `release.yml` does not allow.
- A selected commit older than the latest tag's commit: refused
  (step 8).
- `-n` larger than the history: all commits are listed.
- Empty input, `-1`, `abc`, a number past the list (of any length,
  without wrapping around): refused (step 7).
- Confirmation answered with anything but `y`/`Y`: nothing is created
  (step 9).
- No network or no `origin`: the fetch fails, nothing is created.

## Tests

bats-core, pinned in `mise.toml` `[tools]`, tests in
`scripts/release.bats`, run by `mise run test-scripts`. They run
against a temporary repository with a bare `origin`
(no network, no GitHub): arguments, format and major-version checks,
existing local and remote tags, semver order including pre-releases,
the numbered list and `-n`, each kind of invalid input, the
ancestor check, and the happy path (annotated tag on the selected
commit, present on `origin`), a declined confirmation.
