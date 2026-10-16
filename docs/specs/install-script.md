# Spec: install script (`scripts/install.sh`)

Vocabulary in [CONTEXT.md](../../CONTEXT.md). Layout and lock:
[update.md](update.md).

## Goal

Install hostrunner on a host with one command, instead of the manual
release steps in the README:

```sh
curl -fsSL https://raw.githubusercontent.com/kravlab/hostrunner/main/scripts/install.sh | bash
```

Later versions come with `hostrunner update`; the script is for a first
install and for a reinstall that `update` refuses.

## Command

`scripts/install.sh [-v vX.Y.Z] [--from <dir>]`, bash (`set -euo pipefail`),
like `scripts/release.sh`.

- No flags: the latest release (`tag_name` of
  `GET https://api.github.com/repos/kravlab/hostrunner/releases/latest`,
  same source as `update`). A tag that is not `vX.Y.Z[-pre]` is refused.
- `-v vX.Y.Z`: that release (pre-releases allowed). Invalid version:
  refused before any request.
- `--from <dir>`: no download; installs `<dir>/hostrunner` and
  `<dir>/hostrun`, version taken from `<dir>/hostrunner version`. Used by
  `mise run install` (see Refactoring).
- `PREFIX` env (default `~/.local`; a relative one is made absolute), as
  in `mise run install`.
- `-h`: usage on stdout, exit 0. Unknown flag, extra argument, or `-v`
  together with `--from`: usage on stderr, exit 2.

## Checks before writing anything

- Required tools: `flock`, `mktemp`, `install`, `sync`; a missing one is
  named (`<tool> is required but not installed`), exit 1.
- Release mode only (with `--from` the binaries are already built for
  the host): OS must be Linux, arch `x86_64`→`amd64`,
  `aarch64`/`arm64`→`arm64`; otherwise `no release builds for
  <os>/<arch>`, exit 1, before any request. Then `curl`, `sha256sum`,
  `awk` and `tar` are required too.

## Download and verify (release mode)

1. Into a temporary directory (removed on exit, also on error/Ctrl-C):
   `hostrunner_<version without v>_linux_<arch>.tar.gz` and
   `checksums.txt` from
   `https://github.com/kravlab/hostrunner/releases/download/<tag>/`
   (`curl -fsSL`, bounded by `--max-time`, so a stalled connection
   fails instead of hanging).
2. Archive missing from `checksums.txt`: refused,
   `<archive> is not listed in checksums.txt`. SHA-256 mismatch: refused.
3. Extract only `hostrunner` and `hostrun`; a missing one is refused,
   `release archive has no <name>`.
4. The extracted `hostrunner version` must equal the tag; otherwise
   refused (protects the version directory name `update` relies on).

In both modes, the version `hostrunner version` prints must be a
version directory name (`dev` or a semver version, as update.md reads
one); otherwise refused, `<dir>/hostrunner version "<version>" is not a
version`, before anything is written. An empty or path-like version
would otherwise name the root itself or an entry outside the version
directories.

Build provenance (`gh attestation verify`) is not checked: stays a manual
README step, as for `update`.

## Install into the layout

Exactly the versioned layout of `docs/specs/update.md` (Install layout),
with the same lock and the same crash-safety guarantees as today's
`mise run install`: flock on the root (held: `another update or install is
running in <root>`), cleanup of `.tmp.*` leftovers, two synced copies so
the `current` link always names a complete pair, removal of other version
directories, absolute `bin/` links replaced by rename. Like `mise run
install`, it installs the requested version whatever is installed, a
downgrade included (it is the way to reinstall).

Last line on stdout: `hostrunner: installed <version> into <root>`; when
`<prefix>/bin` is not in `PATH`, a note on stderr,
`install.sh: note: <prefix>/bin is not in PATH`. Errors: `install.sh: <reason>`
on stderr, exit 1.

## Refactoring

Today the layout logic lives only in the `install` task of `mise.toml`
(one long `&&` chain). The script would need the same logic, so instead
of duplicating it, it moves into `scripts/install.sh` and the task
becomes `scripts/install.sh --from bin` (still depends on `build`).
Behaviour of `mise run install` does not change. Affected files:
`mise.toml`, `docs/specs/update.md` (Code section names the script),
README.

## README

The Install section starts with the one-line `curl … | bash` command
(with `-v` and `PREFIX` examples); the manual steps stay below it for
those who want `gh attestation verify`.

## Tests

`scripts/install.bats` (bats, run by `mise run test-scripts`), no
network: `curl` is replaced by a stub in `PATH` that serves files from a
fixture directory (a tar.gz with fake `hostrunner`/`hostrun` scripts that
print their version, plus a `checksums.txt`). Cases:

- fresh install: layout, `current` link, absolute `bin/` links, mode 0755;
- reinstall over an existing version removes the other version
  directories, keeps non-version entries;
- `-v` picks the tag; invalid `-v` refused without requests;
- checksum mismatch, archive not listed, archive without a binary,
  version mismatch: refused, layout untouched, temp dir removed;
- unsupported arch; missing release tool; missing `flock` with `--from`;
  `--from` without `tar`, `curl` and `sha256sum`; lock held by another
  process;
- `-v` together with `--from`; a failed latest-release lookup prints one
  error;
- `--from <dir>` installs local binaries without any request; a version
  that cannot name a version directory (empty, `current`, `../x`) is
  refused and the root's other entries stay.

Extracting only the two binaries has no test: the download directory is
removed on exit, so what else the archive held is not observable.

## Out of scope

- macOS and other OSes (no release builds).
- Uninstall.
- `wget` fallback (curl only).
