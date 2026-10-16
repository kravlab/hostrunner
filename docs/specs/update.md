# Spec: `hostrunner update`

Vocabulary in [CONTEXT.md](../../CONTEXT.md). Releases:
[release.md](release.md).

## Goal

Update an installed hostrunner to the latest release with one command,
instead of repeating the install steps from the README by hand.

## Command

`hostrunner update [--check]`, on the host.

- Flags are parsed like the other subcommands' (`parseFlags`): `-h`
  prints the flag help of Go's flag package and exits 1, extra arguments
  are an error.
- The top-level usage lists the subcommand:
  `up|serve|test|update|version`.
- `hostrun` gets no update command: its arguments are the host command
  to run.

## Current and latest version

- Current: `buildVersion` (what `hostrunner version` prints).
- Latest: `tag_name` of
  `GET https://api.github.com/repos/kravlab/hostrunner/releases/latest`
  (GitHub excludes drafts and pre-releases there). No authentication. A
  `tag_name` that is not a canonical semver version
  (`vMAJOR.MINOR.PATCH`, optionally with a pre-release; not a shorthand
  like `v1.2`, nor with build metadata) is refused: `latest release
  "<tag>" is not a version`.
- Versions are compared as semver. A pseudo-version
  (`v0.4.1-0.2026…-abcdef123456`, with or without `+dirty`) sorts
  below the release it precedes, so a build from `main` is updated only
  by a newer tag. Build metadata is ignored: `v0.5.0+dirty` (the tag
  built with uncommitted changes) is up to date with `v0.5.0`.
- Go derives a source build's pseudo-version from the git tags of the
  clone it is built in. In a clone without the release tags it is
  `v0.0.0-<date>-<hash>`, below every release, so `update` replaces even
  a build newer than the latest release with that release. A source
  build must be made in a clone with the tags (`git fetch --tags`). A
  source build updated to a release older than this command (v0.4.0 and
  earlier) loses `update` until a release that has it is installed.
- From `v2.0.0` on, binaries built without a `/v2` module path report a
  pseudo-version ([release.md](release.md)), not the tag, so the
  version directory named after the tag no longer matches the running
  version and every later `update` is refused (step 1). Releasing
  `v2.0.0` needs the module path change first.
- Current `dev` (no build info): refused before any request,
  `hostrunner: update: cannot compare a dev build with releases;
  install a release`, exit 1.
- Latest not newer than current: `hostrunner: already up to date
  (<current>)` on stdout, exit 0, nothing is downloaded. Never a
  downgrade.

## `--check`

Only reports, downloads nothing, writes nothing:

- newer release for a platform without release builds: refused as
  `update` refuses it (Update, before step 1), exit 1;
- newer release and an install outside the layout: refused as `update`
  refuses it (Update, step 1), exit 1;
- newer release: `hostrunner: <latest> available (current <current>)`,
  exit 0;
- otherwise the "already up to date" line, exit 0.

## Install layout

`update` replaces both binaries with one atomic rename, so it needs them
in a directory per version behind one symlink:

```
<prefix>/lib/hostrunner/<version>/hostrunner
<prefix>/lib/hostrunner/<version>/hostrun
<prefix>/lib/hostrunner/current -> <version>          (relative; the current link)
<prefix>/bin/hostrunner -> <prefix>/lib/hostrunner/current/hostrunner
<prefix>/bin/hostrun    -> <prefix>/lib/hostrunner/current/hostrun
```

The *layout root* is `<prefix>/lib/hostrunner`, a *version directory*
is `<root>/<version>`, and `<version>` is exactly what `hostrunner
version` prints for the binaries in it. The *current link* is the
symlink `<root>/current`. A directory (not a symlink) in the root named
`dev` or a semver version (`semver.IsValid`: `v1`, `v1.2`, `v1.2.3`,
with a pre-release or build metadata) is a version directory; an entry
named `.tmp.*` is a temporary entry of an update or install. Both remove
every version directory but the one the current link names, and every
temporary entry, so nothing else of theirs is left in the root; other
entries (`vim/`, a file `v1.0.0`) are kept. `<prefix>/bin` is in
`PATH`; its links are absolute (`<root>/current/hostrunner`), so they
work when `<prefix>/bin` is itself a symlink, and the README's release
install may put them in any directory in `PATH`. The README's release
install (a first install, or a reinstall of one `update` refuses; later
versions come with `update`) and `mise run install` (default prefix
`~/.local`, `PREFIX` to change it) create this layout.
`hostrunner up` keeps working unchanged: `os.Executable()` resolves the
symlinks, so the daemon and the `hostrun` it installs come from the same
version directory.

## Update

Steps 1 to 8 run only when a newer release exists (above); an install
outside the layout with no newer release is reported up to date. Any
other OS or architecture than linux/amd64, linux/arm64 is refused before
step 1, and before `--check` reports a release, as `no release builds
for <os>/<arch>`.

1. Layout: the version directory is the directory of `os.Executable()`
   with symlinks resolved; its parent is the layout root. Its name must
   be the running version, and the current link must point to that
   name; otherwise refused before downloading, naming what was found:
   `hostrunner is not installed in the versioned layout (<root>/current
   -> <target>; running <current> from <dir>); reinstall it as the
   README describes`, with the `readlink` error in place of
   `<root>/current -> <target>` when there is no link. Without this
   check a version directory named after `<latest>` would be removed in
   step 2. A
   `mise run install` killed while the current link points to its first
   copy (`.tmp.*`, see Code) is refused this way too, which also keeps
   step 2 from removing the directory in use; running the install again
   repairs it.
2. Lock: an exclusive, non-blocking `flock` on the layout root
   directory, held to the end; `mise run install` takes the same lock.
   Held by another update or install: refused, `another update or
   install is running in <root>`; `flock` failing otherwise: `lock
   <root>: <error>`. Under the lock, the current link is read again:
   when another update or install has switched it meanwhile, refused,
   `another update or install switched <root>/current to <target>`;
   when it cannot be read (removed meanwhile), refused with the
   `readlink` error. Then the leftovers of a killed or interrupted
   update or install are removed: the temporary entries (`.tmp.*`) and a
   `<root>/<latest>`, which is not the current link's target (step 1:
   that is the running version, older than `<latest>`). The first that
   cannot be removed refuses the update, `could not remove <path>: …`.
   Then the temporary directory `<root>/.tmp.<latest>-*` (mode 0755) for
   step 5 is created, so an unwritable root is refused before anything
   is downloaded.
3. Download `hostrunner_<version without v>_linux_<runtime.GOARCH>.tar.gz`
   and `checksums.txt` from
   `https://github.com/kravlab/hostrunner/releases/download/<tag>/`.
   An archive not listed in
   `checksums.txt` is refused: `<archive> is not listed in
   checksums.txt`.
4. The archive's SHA-256 must match its line in `checksums.txt`;
   otherwise refused.
5. Extract `hostrunner` and `hostrun` (mode 0755 whatever the umask,
   each synced to disk) into the temporary directory, sync it, and
   rename it to `<root>/<latest>`. An archive missing either binary is
   refused: `release archive has no <name>`. The root is synced after
   the rename.
6. Switch: unless the update was cancelled meanwhile (Ctrl-C), create a
   symlink to `<latest>` under a random name `<root>/.tmp.current-*`
   (another name on a collision) and rename it over the current link;
   sync the root. When the rename fails, the new symlink is removed; a
   failure of that removal is reported with the rename's error. This
   one rename replaces both binaries at once: a crash before it leaves
   the old pair, after it the new one, never a mix. The syncs of steps 5
   and 6 make the new directory and the current link durable before
   step 7 removes anything. A failed sync
   after the rename is reported instead of the step 8 line, as
   `hostrunner: update: updated to <latest>, but could not sync <root>,
   so <dir> is kept: …`, exit 1, and step 7 is skipped.
7. Remove every version directory but `<latest>`: the old one, and any
   an earlier update or install could not remove. The first that cannot
   be removed ends the step; it and the ones after it stay until the
   next update or install removes them. Processes already running from
   them keep running (Linux keeps their files until they exit). A
   `hostrunner up` that started before the update and has to install
   `hostrun` or start the daemon (none runs, or it steps aside) fails,
   since it does both out of the removed directory; one that only arms
   a running daemon succeeds. Running it again uses the new version.
8. `hostrunner: updated <current> -> <latest>` on stdout, exit 0.

On any error before the rename of step 6 succeeds, the current link
still points to the old version, and everything this update created is
removed: the temporary directory and `<root>/<latest>` once renamed to
it. A failure of these removals is reported after the error, as `could
not remove <path>: …`. Only the removals of step 2 have changed the
root then. A failure in step 7 is reported
instead of the step 8 line, as `hostrunner: update: updated to <latest>,
but could not remove <dir>: …`, exit 1; the update itself is done.

The whole update (version lookup and downloads) must finish within 5
minutes, like every other external call of hostrunner is bounded; a
stalled connection fails with the deadline error instead of hanging.

Errors (layout, lock, network, timeout, a final HTTP status other than
200 after redirects, which GitHub's downloads use, tag, checksum,
archive, unwritable root, cancellation) print `hostrunner: update:
<reason>` and exit 1.

## Running daemons

Untouched: a daemon keeps running the old binary from memory until it
exits after its container stops. While it runs, `hostrunner up` (a
container restart or rebuild) arms it (`internal/launch`, `Up`):

- with the same rules, the old daemon follows the arm and the old
  `hostrun` stays in the runtime directory;
- with changed rules, or once the old daemon's watch has ended (its
  container is gone and it is about to exit), it steps aside, and `up`
  installs the new `hostrun` and starts the new daemon;
- should `protocol.Version` ever differ between the versions, the old
  daemon rejects the arm and `up` fails with `stop the process serving
  <socket> and retry`.

The first `up` after the daemon has exited starts the new daemon and
installs the new `hostrun`. Replacing a running daemon stays out of
scope ([devcontainer-integration.md](devcontainer-integration.md)).

## Out of scope

- Build provenance check (`gh attestation verify`): stays a manual step
  in the README.
- Choosing a version (`update v0.3.0`), pre-releases, authentication
  against GitHub's rate limit.

## Code

- `internal/update`: latest-version lookup, comparison, layout check,
  lock, download, checksum, extraction and switch. The base URLs of
  the API and the downloads are fields of its config, so tests point
  them at an `httptest` server.
- `cmd/hostrunner`: `parseUpdate`, the `update` case in `run`,
  `runUpdate` (resolves the executable, bounds the update with the
  5-minute deadline) and `updateMessage` (the stdout line).
- `mise.toml` `install`: builds into `bin/`, then runs
  `scripts/install.sh --from bin` ([install-script.md](install-script.md)),
  which installs the layout under `$PREFIX` (default `~/.local`; a
  relative one is made absolute), under the lock of step 2 (`flock -n`
  on the root; held: refused as in step 2; `flock` not installed:
  `flock is required but not installed`; `flock` failing otherwise:
  `cannot lock <root> (flock exit <code>)`). It installs the built version
  whatever it is, an older one than installed too: unlike `update`, it
  can downgrade. First it removes the temporary entries (`.tmp.*`) of
  the root, except one the current link points to (left by a killed
  install), and those of `<prefix>/bin` (`.tmp.hostrunner-*`,
  `.tmp.hostrun-*`); a run that fails or is killed leaves them, and the
  next run removes them. Each copy of the binaries is synced (files and
  directory) before it is used, and so is the root after each rename. A
  first copy goes into a new `<root>/.tmp.<version>-*` and the current
  link is switched to it; a second copy goes into another one, which
  replaces `<root>/<version>` (a reinstalled version included), and the
  current link is switched to that. Each switch is a rename of a
  symlink with a unique name, so the current link always names an
  existing complete pair. Then every other version directory and every
  temporary entry are removed, and the absolute `bin/` links are made,
  each by renaming a new symlink over it, and `<prefix>/bin` is synced.
  The two `bin/` links are replaced one after the other: an install
  killed between them while replacing a flat install leaves the new
  `hostrunner` and the old `hostrun` in `PATH` until it runs again. Only
  the current link never names a mix.

## Tests

- `internal/update` against `httptest`, on a layout in a temporary
  directory: newer release switches the current link to a new version
  directory holding both new binaries, mode 0755 under umask 077, and
  removes every other version directory, keeping entries that are not
  version directories (a `v`-named directory that is not a version, a
  file named like a version); a pre-release tag installed under its own
  name; same and older release (also `+dirty` of the same tag) write and
  download nothing; `--check` writes nothing and downloads nothing;
  `dev` refused with the exact message and without any request;
  pseudo-version, with and without `+dirty`, below its release; checksum
  mismatch, an archive missing `hostrun` or `hostrunner` (also one
  holding it as a symlink), an archive not in `checksums.txt` (also on a
  line with more than two fields), a tag that is not a canonical version
  (also one with build metadata), invalid JSON, HTTP 404 on the archive
  and on `checksums.txt`, HTTP 500 on the version lookup, a 2xx status
  other than 200, all refused with the exact message and the layout
  unchanged; a checksum in upper case accepted; unsupported architecture
  and OS refused, with `--check` too, before the layout check and the
  lock, without downloading; a flat install (no current link, or one
  pointing elsewhere) and a version directory not named after the
  running version refused with the exact message, without downloading,
  with `--check` too; a flat install with no newer release reported up
  to date; a lock held exclusively or shared refused, without
  downloading; a current link switched or removed under the lock
  refused (`checkCurrentLink`, directly); leftover temporary entries (a
  non-empty directory, a symlink, one pointing to the running version
  while the update fails) removed without following symlinks, other
  entries kept; a leftover `<root>/<latest>` replaced, and removed by
  an update that fails; a leftover that cannot be removed refused with
  its message, without downloading; a failed removal of the temporary
  directory reported after the error; a cancellation after the last
  download refused before the switch; a stalled server fails at the
  deadline, on the version lookup and on the archive download, the
  deadline expiring only once the stalled request is made; an
  unwritable root refused with a permission error, without downloading,
  the layout unchanged; an old version directory that cannot be removed
  reported with `Updated`.
  Not tested, since no test can provoke them without a hook in the
  code: the directory syncs and their failures, a failed symlink or
  rename in step 6 and a failed removal of its symlink, a symlink name
  collision, a current link switched between step 1 and the lock (Run's
  call of `checkCurrentLink`), `flock` failing other than by contention,
  and the 5-minute deadline of `runUpdate`.
- `cmd/hostrunner`: flag parsing, usage, extra arguments, the `update`
  case of `run`; `dev` refused with `update.ErrDevBuild`;
  `updateMessage` for each outcome.
- `mise run install`: no automated test; checked by hand under `sh -e`,
  `sh` and `bash`, and by killing it after every command.

## Docs

README: the release install (a first install, or a reinstall of one
`update` refuses; releases up to v0.4.0 have no `update`) and `mise run
install` (which can downgrade) create the layout, source builds need
the tags and lose `update` when replaced by an older release; an
"Update" paragraph under Install. CONTEXT.md: layout root, version
directory, current link. [release.md](release.md): the usage line, `-h`
wording and the README install description.
[devcontainer-integration.md](devcontainer-integration.md): the install
task.

## Dependencies

- `golang.org/x/mod/semver` compares versions (pseudo-versions
  included), rather than a hand-written comparison.
