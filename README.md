# hostrunner

Run host commands from inside a devcontainer: `hostrun git push` in the
container runs `git push` on the host, in the host directory that mirrors
the container's current directory, with the host's environment and
credentials. Stdin, stdout and stderr are streamed; the exit code is
propagated.

Only commands allowed by `.devcontainer/hostrun.yaml` run. The host side
(`hostrunner`) starts with the devcontainer and exits after the container
stops. Linux hosts; Docker and Podman.

> **Rules restrict argv only.** A host tool that reads configuration or
> hooks from the workspace — git (`.git/config`, `.git/hooks`), fakehost
> (`fakehost.json` predeploy scripts), … — can be steered by the container,
> which can write the workspace. Read-only mounts of such files do not help
> (the container can rename the directory around them or create a nested
> repository). Allowing such a tool against an agent you do not trust is a
> conscious risk of code execution on the host.

## Install

The latest release, into `~/.local` (`hostrunner` and `hostrun` in
`~/.local/bin`, which must be in `PATH`):

```sh
curl -fsSL https://raw.githubusercontent.com/kravlab/hostrunner/main/scripts/install.sh | bash
curl -fsSL https://raw.githubusercontent.com/kravlab/hostrunner/main/scripts/install.sh | bash -s -- -v v0.5.0
curl -fsSL https://raw.githubusercontent.com/kravlab/hostrunner/main/scripts/install.sh | PREFIX=/usr/local bash
```

The script checks the archive against the release's `checksums.txt`
and installs the layout described below; it installs the version asked
for whatever is installed, an older one too.

By hand, with a build provenance check,
from a [release](https://github.com/kravlab/hostrunner/releases)
(`gh release download` or the Releases page):

```sh
v=0.1.0 arch=amd64                  # the release, without the v; amd64 or arm64
archive=hostrunner_${v}_linux_${arch}.tar.gz
gh release download "v$v" --repo kravlab/hostrunner -p "$archive" -p checksums.txt
sha256sum -c --ignore-missing checksums.txt
gh attestation verify "$archive" --repo kravlab/hostrunner  # optional: built by this repo's CI
root=~/.local/lib/hostrunner bin=~/.local/bin   # bin: any directory in PATH
mkdir -p "$root/v$v" "$bin"
tar -xzf "$archive" -C "$root/v$v" hostrunner hostrun
ln -sfn "v$v" "$root/current"
ln -sfn "$root/current/hostrunner" "$bin/hostrunner"
ln -sfn "$root/current/hostrun" "$bin/hostrun"
hostrunner version
```

Each version gets its own directory and the `current` link points to
the one in use: `hostrunner update` needs this layout to replace both
binaries at once. These steps are for a first install, or to reinstall
one `hostrunner update` refuses; later versions come with
`hostrunner update`, which v0.4.0 and older releases do not have.

From source, with mise 2026.5.0 or newer (an older one refuses the
repository's mise config):

```sh
git fetch --tags                    # the version comes from the tags
mise install                        # the pinned toolchain (see mise.toml)
mise run install                    # builds with CGO_ENABLED=0 into the layout under ~/.local
PREFIX=/usr/local mise run install
```

The build's version comes from the clone's git tags: without them it is
`v0.0.0-…`, which `hostrunner update` replaces with the latest release
even when the build is newer. Updated to v0.4.0 or an older release, a
source build loses `update` until a release that has it is installed.
`mise run install` installs the version it builds, an older one than
installed too.

`hostrunner` must be in the `PATH` of the process that starts your
devcontainer (VS Code, the devcontainer CLI). The `hostrun` client is
installed next to it; it must stay statically linked, which
`mise run install` guarantees.

### Update

```sh
hostrunner update --check   # hostrunner: v0.5.0 available (current v0.4.0)
hostrunner update           # hostrunner: updated v0.4.0 -> v0.5.0
```

`update` downloads the latest release for the host's architecture, checks
it against the release's `checksums.txt`, extracts it into a new version
directory, switches the `current` link to it with one rename and removes
the other version directories. Both binaries change together: on an
error or a crash the `current` link points to the old pair or the new
one, never a mix.
When a newer release exists, an install outside the layout above is
refused (`--check` too): reinstall it as described. It never downgrades,
refuses a build from source without a version (`dev`), runs only while
no other update or `mise run install` is running and gives up after 5
minutes. Build provenance is
not checked: use the manual steps above for `gh attestation verify`.

A running daemon keeps the old version until it exits after its
container stops; a container restart or rebuild before that reuses it
with the old `hostrun`, unless the rules changed: then the new version
takes over. The first `hostrunner up` after it has exited runs the new
version. A `hostrunner up` already in progress during the update may
fail; run it again.

## Use in a devcontainer

Add to `.devcontainer/devcontainer.json`
(full example: [examples/devcontainer](examples/devcontainer/.devcontainer/devcontainer.json)):

```jsonc
"initializeCommand": [
  "hostrunner", "up",
  "--dir", "${localEnv:XDG_RUNTIME_DIR}/hostrunner/${devcontainerId}",
  "--workspace", "${localWorkspaceFolder}",
  "--container-workspace", "${containerWorkspaceFolder}"
],
"mounts": [
  "source=${localEnv:XDG_RUNTIME_DIR}/hostrunner/${devcontainerId},target=/run/hostrunner,type=bind,readonly",
  "source=${localWorkspaceFolder}/.devcontainer,target=${containerWorkspaceFolder}/.devcontainer,type=bind,readonly"
],
"remoteEnv": { "PATH": "${containerEnv:PATH}:/run/hostrunner" }
```

Keep `initializeCommand` in the array form: it runs without a shell, so
workspace paths with spaces work. `XDG_RUNTIME_DIR` must be set for the
process that starts the devcontainer (it is in a normal desktop session).
If `hostrunner up` fails, `devcontainer up` fails with its message.

Add the rules as `.devcontainer/hostrun.yaml` (start from
[the example](examples/devcontainer/.devcontainer/hostrun.yaml)), then,
inside the container:

```sh
hostrun git push
hostrun git push --force   # hostrun: denied by rule "git push": flag --force is not allowed
hostrun --dry-run git push # hostrun: dry run: allowed by rule "git push"
```

A dry run (`--dry-run` as hostrun's first argument) asks the daemon
whether the command would run, without running it: the daemon checks the
rules, the working directory and the path checks, then looks the program
up as starting it would (127 when the host lacks it, 126 when it cannot
execute it) and answers. It exits 0
with the allowing rule, the rule's `dir` when it has one
(`allowed by rule "tix issues", runs in /`) and the names of the secrets
it would inject (`, injects secrets: token`), or with the code and
message the run would get. It checks a secret's source without obtaining
the value: a `command` source's program is looked up but not run, an
`env` variable must be set, a `file` must open.
It does not read stdin, and the daemon log records it with
`dry_run=true`. After the program, `--dry-run` is the command's own
(`hostrun git push --dry-run` runs git's dry run). A daemon older than
dry runs refuses one with 125 rather than running the command.

### Approvals on the host

A command the rules refuse can be approved by a human on the host, one
request at a time. In a host terminal (not one inside the container),
start an approval session for the devcontainer's runtime directory, the
`--dir` its `initializeCommand` gives `hostrunner up`:

```sh
hostrunner approve --dir "$XDG_RUNTIME_DIR/hostrunner/<devcontainerId>"
```

It serves that devcontainer's daemon: it prints each approval request as
it arrives, with the container directory, the argv and why the rules
refused it, rings the terminal bell, and takes the answer, a letter and
Enter: `r` refuses it (then an optional note for the agent), `o` approves
this request only, `l` grants a standing approval (below). Input typed
before the prompt is discarded, and what came from the container is
shown quoted, so it cannot steer the terminal.
Requests are shown one at a time, first in, first out; a new one rings
the bell without interrupting the prompt and is counted in the next
block (`(1 more waiting)`). A shown request whose agent goes away or
whose time runs out is announced (`request 7 withdrawn`, `request 7
expired`) and the next is shown; a queued one drops out silently.
Leaving the session (Ctrl+C, end of input) refuses the requests still
waiting and says how many.

A standing approval (`l`, "for the daemon's lifetime") lets the same argv,
byte for byte, from the same container directory through from then on,
with or without `--ask`, printing nothing of hostrun's own; it runs as
the approved request did. A guarded request gets one after one `yes`,
when it is granted. Standing approvals live in the daemon's memory: they
work without a session, survive a container restart or rebuild that
keeps the daemon, and end with it. While no request is shown, the
session's `>` prompt takes `standing`, which lists them (number, argv,
directory, `guarded`), and `revoke <n>`, which ends one; a command it
let through keeps running. A dry run of a command one lets through
reports `hostrun: dry run: allowed by a standing approval`.

In the container, ask with `--ask` as hostrun's first argument:

```sh
hostrun --ask git rebase -i main
# hostrun: waiting for approval on the host: no rule allows "git rebase"
# hostrun: approved on the host: this request only
```

- A command that would run without `--ask` runs at once, without asking.
- While a session serves the daemon, a run or dry run that the rules or
  a path check refuse ends with a hint when `--ask` would ask a human
  about it, marked guarded when any of its reasons is:
  `hostrun: denied by rule "git push": flag --force is not allowed (ask
  for an approval: hostrun --ask …; guarded: needs a second
  confirmation)`. No hint comes when `--ask` would be refused anyway:
  the working directory, the rule's `dir`, a secret's source or the
  program.
- `--dry-run` and `--ask` cannot be combined: either followed by the
  other fails with 125.
- Approvals are on only while a session serves the daemon: without one,
  `--ask` is refused at once with 126 and `not approved: no approval
  session serves this devcontainer`.
- A refusal exits 126 with `refused on the host` and the human's note
  (`hostrun: refused on the host: use git rebase main`).
- A request waits 10 minutes, or as long as `--ask=<duration>` says (a
  Go duration: `--ask=3m`, `--ask=90s`); then it is refused with 126
  `not approved: nobody answered on the host`. A limit that is not a
  positive duration fails with 125 before connecting; the daemon refuses
  one over 1 hour with 125. Set it within your tool's timeout.
- Ctrl+C, or anything else that drops the connection, withdraws the
  request. A session that leaves refuses every request still waiting
  (126 `refused on the host`); a daemon that stops or steps aside
  answers every waiting request with 126 `not approved: the hostrunner
  daemon is stopping`.
- A command the daemon would refuse anyway (the working directory, the
  program) is refused at once, before anyone is asked. So is one too
  large to show (`not approved: the request is too large to show`).
- An approved command runs as the rule that refused it runs the commands
  it allows: in its `dir`, with its injection and its secrets masked; one
  no rule matches runs in the mirrored directory with nothing injected.
  Arguments that pass their path check are handed over as for an allowed
  command; one approved after failing it is passed as given.
- Some refusals are guarded: by a deny list, by a path check, an argv the
  rules cannot read (an ambiguous flag), or a rule that injects secrets.
  The request shows each as `⚠ guarded: …`, the agent's wait line says
  `(guarded)`, and `o` approves it only after the warning is read and
  `yes` typed; anything else returns to the answer prompt.

The session reaches the daemon over its approval socket,
`<runtime directory>.approval.sock`, beside the runtime directory: the
container mounts the runtime directory alone, so it cannot answer its own
requests, and only the daemon's user may connect; `hostrunner serve`
refuses to start when the approval socket would lie inside the
workspace, and a session that stops reading (suspended) is dropped
rather than holding up its daemon. A daemon older than
`--ask` refuses it with 125 rather than running the command. The daemon
log, `daemon.log` in the runtime directory, records the requests and
answers, standing approvals granted and revoked, and marks the runs one
lets through `approved=standing`; the container can read it.

## Rules

```yaml
rules:
  - command: git status        # argv prefix; the longest matching one decides
    args: any                  # any flags and arguments
  - command: tix pr list
    args: none                 # the bare command only
  - command: git push
    flags:
      allow: [-u, --set-upstream, --tags]   # or deny: [...], never both
      values:                  # flags that take a value, with a value filter
        -o: { allow: [ci.skip] }
        --repo: {}             # any value
    positional:
      deny: [main, "+*"]       # or allow: [...], never both
  - command: tix api --repo example/example-app
    flags:
      allow: []                # no flags
    positional:
      allow_regex:             # regular expressions instead of globs
        - '^/repos/example/example-app/issues(/[0-9]+)?(\?state=(open|closed))?$'
  - command: abx install
    flags:
      allow: [-r]
    positional:
      allow: ["*.pkg"]
      path: open               # and a file inside the workspace
  - command: tix pr
    flag_style: go             # -name and --name are one flag (urfave/cli)
    flags:
      deny: [-asap]
      values:
        -repo: { allow: [my/repo] }
  - command: tix issues
    dir: /                     # run in / on the host, not where the container stands
    args: any
```

- A command no rule matches is denied, and so is everything when the file
  is missing. An invalid file (unknown key, two keys on one list such as
  both `allow` and `deny`, a regex that does not compile, `args` together
  with `flags`/`positional`/`flag_style`, an empty section, a rule
  without any of them, a relative program path or `dir`, `dir` with
  `path: check`, …) makes `hostrunner up`
  fail, and with it `devcontainer up`.
- Changes apply on the next container start (reopen, restart or rebuild):
  `hostrunner up` notices that the file differs from what the running
  daemon loaded and replaces the daemon. The file lives in `.devcontainer/`,
  which is read-only inside the container, so the agent can read but not
  change its rules.
- Matching is literal on argv: `/usr/bin/git push` and `git -C dir push` do
  not match `git push`. The program must be a name or an absolute path.
- A command runs in the mirrored directory: the host directory that
  matches the container's current one. A tool that reads configuration
  from its working directory (`tix` probes it with git and reads
  `--repo owner/name` as a path when that directory exists there; a mise
  shim reads the `mise.toml` above it) is then steered by whatever the
  container puts there. Give its rule `dir:` with an absolute host path to
  run the command there instead; `PWD` is set to it. It must be an
  existing directory outside the workspace, symlinks followed: otherwise the
  command is refused (126). This is checked on every command, not when
  the rules load. The container must still stand inside the workspace. A
  longer rule does not inherit `dir`: `tix issues list` above runs in `/`,
  but a `tix issues list` rule of its own runs in the mirrored directory
  unless it says `dir` too. `dir: inherit` takes the `dir` of the nearest
  shorter rule whose command starts the same way (`tix issues` for
  `tix issues list`), following further `inherit`s; it is an error when
  there is no such rule or it has no `dir`.
- Arguments are parsed like getopt, unless the rule says `flag_style: go`
  (below): `--flag=value`, `--flag value`, `-abc` (= `-a -b -c`),
  `-ovalue`, and `--` ending the flags. Only flags
  listed under `values` take a value; that is how flag values are told
  apart from positional arguments.
- `flags.allow` is strict: an unlisted flag is denied. `flags.deny` is
  best effort: it also catches abbreviations (`--forc`) and combined short
  flags, but a tool may offer other ways to the same effect (e.g. git's
  `+refspec` for a force push, or `--upload-pack`, which makes git run a
  command you name), so prefer `allow`, especially for git.
- A `flags` section with `values` only does not restrict the other flags:
  it filters the values of the flags it lists and lets every other flag
  pass. Add `allow: []` to deny all flags that are not under `values`:

  ```yaml
  flags:
    allow: []                # no flag but --repo
    values:
      --repo: { allow: [my/repo] }
  ```
- Flag names are `-x` or `--name` (see `flag_style: go` below for
  `-name`). Filters apply to the names listed only:
  list every spelling of a flag, e.g. both `-P` and `--project` under
  `values`. An abbreviated value flag must carry its value inline
  (`--proj=dev`), otherwise it is denied.
- A token with one dash and several characters is read as short flags:
  `-asap` is `-a -s -a -p`, or `-a` with the value `sap` when `-a` is
  under `values`. Programs built on Go's `flag` package or on urfave/cli
  (such as `tix`) take it as the long flag `--asap` instead: give their
  rules `flag_style: go`. Programs built on cobra/pflag read flags like
  getopt and need nothing.
- Under `flag_style: go` (urfave/cli v1 to v3, with or without
  `UseShortOptionHandling`):
  - Flag names are `-name` or `--name`, any length; both spellings name
    one flag, in the rule and in argv, so list it once. Names match
    exactly: there are no abbreviations.
  - A value flag takes `-name=value`, `--name=value` or the next token,
    even one starting with `-`; `-nvalue` is not a value.
  - An inline value on a flag not under `values` is denied
    (`--dry-run=false` would turn a boolean flag off). To allow it, list
    the flag under `values`, e.g. `--dry-run: { allow: ["false"] }`.
  - A single-dash token the rule does not list whole may also be a
    cluster of short flags: a `deny` list catches any of its letters, and
    a letter under `values` denies the token.
  - A token starting with `---` is denied, and `--` before any positional
    argument ends the flags.
  - These parsers disagree on what follows the first positional argument,
    a single-dash token not followed by a letter (`-1`) or a lone `-`, and
    on a token with whitespace around it: Go's `flag` and urfave/cli v1
    and v2 read positional arguments there, v3 still reads flags. Such a
    token must pass both the flag and the positional filters
    (`tix pr view 5 --comments` needs `--comments` to pass the positional
    list too), a value flag there may not be followed by a token starting
    with `-`, and a path check denies it.
- The patterns of an `allow` or `deny` list for values and positional
  arguments are globs: `*` matches anything, including `/`; `?` one
  character. Under `allow` every argument must match; under `deny` none
  may.
- A glob cannot keep a path inside a prefix: `/repos/x/*` also matches
  `/repos/x/../../user`. For that, write the list as `allow_regex` or
  `deny_regex` instead of `allow` or `deny` (one of the four per list).
  Its patterns are [Go regular expressions](https://pkg.go.dev/regexp/syntax),
  used as written: hostrunner adds no anchors, so a pattern matches anywhere
  in the value, and an `allow_regex` pattern without `^…$` allows every
  value that contains a match. Quote a regex with single quotes: in double
  quotes YAML reads `\` as its own escape character.
- A glob or a regex sees only the text of an argument. It does not resolve
  symlinks, so it keeps a file argument inside a directory by its spelling
  only.
- To keep a file argument inside the workspace, add `path: open` or
  `path: check` to its list, next to the patterns or alone (`positional:
  { path: open }`, `--file: { path: check }`). Every argument the list
  applies to (every positional argument of the rule, or every value of
  the flag in any spelling) must then be a relative path, from the mirrored
  directory, to an existing regular file whose real location on the host,
  symlinks followed, is inside the workspace. Absolute paths, missing
  files, directories, FIFOs and devices are refused, and so is a symlink
  that leads out of the workspace or is absolute. The patterns still see
  the argument as given.
  - `path: open`: the daemon opens the file, and the program gets
    `/proc/self/fd/N` in its place (`--file=/proc/self/fd/3` for
    `--file=app.pkg`), numbered in argument order. A symlink swapped in
    after the check cannot redirect the program, but the program never
    sees the file's name: one that looks at the name or its extension
    needs `path: check`. The program's own children inherit the file.
  - `path: check`: the program gets the argument as given and opens it
    itself, after the check. Between the two the container can still swap
    a symlink into the path and point it out of the workspace; `check`
    only stops what is already there when the command starts.
  - A rule with `dir` can use `path: open` (the file is found from the
    mirrored directory and handed over open) but not `path: check`: the
    program would look for the argument in `dir`, not where it was
    checked, so the rules file is invalid.

### Secrets and injection

A rule can add to its command, on the host, what the container never
sees: a token only the host can obtain. The rule alone decides where it
goes; the container cannot move it.

```yaml
  - command: curl
    secrets:
      token: { env: API_TOKEN }  # the daemon's environment variable
    inject:
      command_args: ["-H", "Authorization: Bearer ${token}"]
    flags:
      allow: [-s]
      values:
        -X: { allow: [GET, PUT] }
    positional:
      allow_regex: ['^https://api\.example\.com/v1/items/42$']
```

- `secrets` names the values a rule obtains, each from one source. A
  name is lower-case letters, digits and `_`, starting with a letter.
  - `command: [authtool, print-token]` takes the stdout of a host
    command. It runs where the rule's command would (the rule's `dir`, or
    the mirrored directory, which the container can write to), with the
    daemon's environment and an empty stdin, for at most `timeout` (a Go
    duration, `30s` by default), after which it and whatever it started
    are killed. A failed source's stderr goes to the daemon log; a
    successful one's is discarded.
  - `env: API_TOKEN` reads a variable of the daemon's environment (the
    one `hostrunner up` was started in).
  - `file: /home/u/.config/app/token` reads a host file; a relative path
    is read from the same working directory as a command source. The file
    may lie anywhere, the workspace included: what the devcontainer mounts,
    and so what the container can read or replace, is yours to decide.
- The value is obtained fresh for every request; trailing CR and LF are
  trimmed. A source that yields nothing, or a value shorter than 8
  characters, stops the request with 125 and `secret "token" is not
  available` or `… is too short to mask`; the reason is in the daemon log.
- `inject` adds arguments at three places: `program_args` right after the
  program (global flags, as in `git -c …`), `command_args` right after the
  rule's command (`gh api -H …`), and `trailing_args` after the
  container's arguments. For `git push` that is `git <program_args> push
  <command_args> <container's arguments> <trailing_args>`. A trailing
  argument is positional only: one starting with `-` makes the rules file
  invalid, and a value that makes one start with `-` stops the request
  with 125, so a `--` from the container cannot turn it into something the
  program echoes.
- `inject.env` sets environment variables for the command, as
  `{API_TOKEN: "${token}"}`, over the daemon's variable of the same name.
  `PWD` is the working directory and cannot be set.
- `inject.files` hands the command files whose content is a template, for
  a program that reads its secret from a config file:
  `{cfg: 'header = "Authorization: Bearer ${token}"'}`. The program gets
  each as an inherited descriptor, after the `path: open` files in the
  order the rules file writes them, and `${file.cfg}` in an argument or a
  variable is its path (`curl -K ${file.cfg}` runs as
  `curl -K /proc/self/fd/4`). The file is read-only, can be read again and
  sought in, and lives in memory only (a sealed memfd), never on a disk.
  Its content may refer to secrets, not to files.
- Their strings are templates: `${token}` is the secret's value,
  `${file.cfg}` an injected file's path, `$$` a literal `$`; any other `$`,
  an unknown name, or a secret or file no template uses makes the rules
  file invalid. A `path: open` argument is still replaced
  where it moved to.
- The rule's lists check the container's arguments only, as without an
  injection; the added ones are not checked, so the rule need not allow the
  container the flags it adds itself.
- A program can give its secret away itself (`-v`, `--trace-ascii -`, `-o`,
  a proxy, a config of its own), more ways than a deny list can name. A
  rule with `secrets` therefore takes allow lists only: `flags.allow`
  (`[]` for no flags), an `allow` or `allow_regex` list for every flag
  under `values`, and `positional` with `allow` or `allow_regex`; `deny`,
  `deny_regex` and a missing list make the rules file invalid. `args: none`
  is fine; `args: any` needs `secrets_with_any_args: true` next to it, as a
  deliberate choice.
- The daemon replaces a secret's exact value with `***` wherever the
  command writes it to stdout or stderr, a server echoing the request
  included, even split across writes. It does not catch an encoded value
  (base64, URL or JSON escaping, a hash): masking is a safeguard, not a
  boundary. What keeps a secret from leaking is the strict allow lists
  above and the API you allow.
- An `inject` without `secrets` adds fixed arguments and needs no strict
  lists.
- The daemon log records the container's argv and the names of the
  secrets, never the argv, environment or files with the values in them.

To see whether a rules file allows a command before restarting the
container, run the Rules test on the host, in the project root:

```sh
hostrunner test git push -u origin main   # hostrunner: allowed by rule "git push"
hostrunner test git push --force          # hostrunner: denied by rule "git push": flag --force is not allowed (approvable)
hostrunner test --config draft.yaml -- tix pr list
```

It reads `.devcontainer/hostrun.yaml` under the current directory, or the
`--config` file, and tests the command against the rules alone: not the
working directory, path checks or the host's programs. It reports the
rule's `dir` (`inherit` resolved) and its secrets' names as a dry run
does, without checking that the directory exists or lies outside the
workspace, or touching any secret's source. It exits 0 when
the command is allowed, 126 when it is denied, and 1 when the file is
missing or invalid (unlike the daemon, which denies everything without a
file) or no command is given. Its flags end at the first argument that is
not one of them, or at `--`.

Unlike a refusal from the daemon, which names the first reason, a denied
command's line lists every reason, joined by `; `, and ends with the class
an approval would get: `(approvable)`, or `(guarded: approvable after a
second confirmation)` for a deny pattern, an argv the rules cannot read
(a flag missing its value, an abbreviated flag without `=`, an ambiguous
abbreviation, a value for a flag that takes none, tokens Go flag parsers
read differently) or a rule with secrets. A failed path check
makes a refusal guarded too, but the Rules test does no path checks.

How it works:

- `hostrunner up` runs on the host before every container start. It
  creates the per-container runtime directory and either arms the daemon
  already serving it or copies `hostrun` into it and starts a detached
  daemon.
- The directory is mounted read-only at `/run/hostrunner`; the client
  talks to the daemon over the Unix socket in it.
- The daemon follows its armed container, the one the latest `up` started
  (or found running): it polls `docker`/`podman` for the workspace's
  containers, stopped ones included, and exits about 15 s after that
  container stops, however briefly it ran.
  After an arm it waits (up to 30 min) for the new container of a
  rebuild, however long the image build takes. A container run with
  `--rm` (through `runArgs`) leaves no trace when it stops, so if it stops
  before a poll (every 2 s) sees it, the daemon waits out those 30 min or
  the next `up`. Its log is `daemon.log` in the runtime directory.
- `.devcontainer/` is read-only inside the container, so the container
  cannot rewrite the rules that govern it. `hostrun` warns on stderr when
  the nearest `.devcontainer/` above its working directory, or the
  `hostrun.yaml` in it, is writable (the default location only, not a
  `--config` file).
- The working directory (the mirrored directory, or the rule's `dir`) is
  opened, not re-resolved, when the command starts, so a symlink swapped
  in by the container cannot redirect it.
  Symlinks inside the workspace must be relative.

## Exit codes

| Code | Meaning |
|---|---|
| command's own | the command ran |
| 0 | `--dry-run`: the command would run |
| 128+N | the command was killed by signal N |
| 125 | hostrun failed: daemon unreachable, protocol error, no command given, or `--dry-run` and `--ask` combined |
| 126 | refused: denied by the rules, an argument not a workspace file, working directory outside the workspace, a rule's `dir` missing, not a directory or inside the workspace, or not executable; with `--ask`, not approved on the host (refused, unanswered, no approval session) |
| 127 | command not found on the host |
| 130 | interrupted (Ctrl+C); the host command is killed |

## Limitations

- A running daemon is armed again, not replaced: after upgrading hostrunner
  or changing `workspaceFolder`, stop the container (the daemon exits
  about 15 s later) before starting it again.
- The container user must be root or have your host UID (devcontainers
  do this by default on Linux via `updateRemoteUserUID`): the runtime
  directory and socket are owner-only.
- Docker with SELinux enabled in dockerd (e.g. Fedora's `moby-engine`)
  confines containers so they cannot connect to the socket. Add
  `"runArgs": ["--security-opt", "label=disable"]`. Podman through the
  devcontainer tooling is not affected.
- No TTY and no signal forwarding yet: interactive prompts do not work.
- `path: check` checks a file argument when the command starts, not when
  the program opens it: the container can swap a symlink in between. Use
  `path: open` where the program accepts `/proc/self/fd/N`.

## Development

`mise run setup` installs the toolchain and a pre-commit hook
([lefthook.yml](lefthook.yml)): a commit with staged `.go` files fails
when they are not gofmt-formatted or `go vet` reports a problem. The
Claude container runs the host's hook too, through its `.git/hooks`
mount. `lefthook.yml` is in the workspace, which the containers can
write, so a plain `git commit` on the host runs whatever they put there:
the same risk as `.git/hooks` (see the warning at the top).

The devcontainers come from a Copier template,
[kravlab/devcontainer-template](https://github.com/kravlab/devcontainer-template).
It owns `.config/mise/conf.d/devcontainer.toml` and everything under
`.devcontainer/` except `hostrun.yaml`, so those files are not edited
here: a change is made in the template and taken with
`mise run template-update`, run on the host with nothing uncommitted
(`.copier-answers.yml` records the version in use). The task needs only
mise, which brings uv to run Copier.
The template's README and `docs/specs/` have the details of what follows.

The repository has two devcontainers; both take the toolchain from
`mise.toml` and keep bash history across rebuilds:

- `.devcontainer/devcontainer.json` — everyday development, without
  hostrunner (`devcontainer up`). Container and host name
  `<folder>-dev`; tools from `mise run setup-dev`.
- `.devcontainer/claude/devcontainer.json` — the same image plus Claude
  Code (pinned in `.devcontainer/Dockerfile`), wired to hostrunner: the
  container may run the host's `git` (as in the example rules) and
  `gh issue`/`gh pr` through `hostrun`. Claude's login and settings
  survive rebuilds. Container and host name `<folder>-agent`; only go,
  lefthook and bats, from `mise run setup-agent`. Run `mise run install` on the host first
  so `hostrunner` is in `PATH`, then
  `devcontainer up --config .devcontainer/claude/devcontainer.json`
  (or pick it in VS Code). It needs a regular clone (`.git` a directory).
  Optionally, set `HOSTRUNNER_AGENTS_MD` on the host to the absolute
  path of a global instructions file (e.g. `AGENTS.md`): it is mounted
  read-only as Claude's `~/.claude/CLAUDE.md`, and edits written in
  place show up in the running container (a save that renames a new
  file over it does not, until a restart). Unset, Claude gets no global
  instructions.
  Claude in the container also loads the host's global skills
  (`~/.claude/skills`, with symlinked skills copied as what they point
  to) and runs the host's global hooks (the `hooks` key of
  `~/.claude/settings.json`; a host that has this file needs `jq`). Both
  are copied on every `devcontainer up`, read-only for the agent, and
  nothing else of the host's `~/.claude` is: not the rest of the
  settings, not the login, not the history.
  A hook runs in the container, so it can use only what the image has; the
  copy is also mounted at the host's `~/.claude` path, so a hook that
  reads a skill through that path works unchanged
  (`docs/specs/devcontainer-claude-skills-hooks.md` in the template).

Both containers' git uses `user.name` and `user.email` from the host's
global git config, when set, for commits, and both git and ripgrep (so
Claude Code's searches) ignore what the host's global excludes file
(`core.excludesFile`, or `~/.config/git/ignore`) ignores; it is copied on
every `devcontainer up`, so host edits show up after the next one.
Nothing else of the host's git config is copied. In VS Code, set
`"dev.containers.copyGitConfig": false` in your user settings: otherwise
the Dev Containers extension copies the whole host `~/.gitconfig` into
the container, and a `core.excludesFile` set there points to a host path
the container lacks, which hides the excludes.

The mise tasks for the containers are the template's too. They are in
`.config/mise/conf.d/devcontainer.toml`, a mise config that mise reads by
itself next to `mise.toml`: no setting in `mise.toml` names it. The same
file sets a cooldown, `minimum_release_age = "5d"` (mise does not install
a release until it has been out for five days; the tools in `mise.toml`
are pinned exactly, and mise installs an exact pin whatever its age), and
requires mise 2026.5.0 or newer on the host, the oldest known to apply it.

The tasks run on the host; inside a container `mise tasks` lists them as
well, but the container tasks cannot work there, for want of a container
engine.

`mise run dev` brings the everyday container up and opens bash in it;
`mise run agent` brings the Claude container up and runs `claude` in it
(the devcontainer CLI comes from `mise run setup`).
`mise run dev-recreate` and `mise run agent-recreate` replace the
container with a new one, rebuilding the image where
`.devcontainer/Dockerfile` changed; named volumes (bash history,
Claude's login) survive.

To run something in the everyday container from the host (bringing it
up first):

```sh
mise run dc -- go test ./internal/rules/        # a command, run without a shell
mise run dc -- bash -c 'go vet ./... | head'    # pipes etc. need bash -c
mise run dc-task -- check                       # a mise task
```

The container names are unique per host, so two clones with the same
folder name cannot run the same configuration at once.

In the Claude container `.git/config` and `.git/hooks` are read-only, so
the agent cannot edit them in place. That does not make `hostrun git`
safe (see the warning at the top): the agent can still point host git at
a config and hooks it writes with a `.git/commondir` file, rename `.git`
and create a new one, or `hostrun git` from a nested repository. Writing
the git config from inside the container (`git config`,
`git remote add`, `git branch --set-upstream-to`) fails, and since
`.git/config` is a single-file mount, host-side config changes (e.g. the
upstream set by `hostrun git push -u`) show up in the container only
after a restart.

The e2e suite needs Docker or Podman on the host, so run it there, not
through `hostrun`: `go test` would execute workspace code the container
can change.

```sh
mise run fmt    # gofmt -w .
mise run lint   # gofmt check and go vet, as CI runs them
mise run test   # unit and integration tests (go test -race ./...)
mise run test-scripts  # the release script's tests (bats, from setup-dev)
mise run check  # lint and test
mise run pre-commit   # the hook's jobs on the staged files (-- --all-files: all)
mise run e2e    # brings up examples/devcontainer on docker
HOSTRUNNER_E2E_PODMAN=1 mise run e2e   # also on podman
```

The podman run is opt-in because devcontainer CLI 0.89 sometimes waits
forever for the container's start event from `podman events`, even
without hostrunner.

A pushed `vX.Y.Z` tag on `main` publishes a release: CI runs again, then
GoReleaser ([.goreleaser.yaml](.goreleaser.yaml)) uploads the archives,
`checksums.txt` and a build provenance attestation. `mise run release`
lists the last commits of `origin/main` (`-n N`, default 10), asks which
one to tag, checks the version, and pushes the tag after a confirmation
([spec](docs/specs/release-script.md)).

```sh
mise run release -- v0.2.0               # pick the commit from the list
git tag v0.1.0 && git push origin v0.1.0 # by hand, on the current commit
mise exec goreleaser -- goreleaser release --snapshot --clean   # local dry run into dist/
```
