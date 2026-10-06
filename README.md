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

From a [release](https://github.com/kravlab/hostrunner/releases)
(`gh release download` or the Releases page):

```sh
v=0.1.0 arch=amd64                  # the release, without the v; amd64 or arm64
archive=hostrunner_${v}_linux_${arch}.tar.gz
gh release download "v$v" --repo kravlab/hostrunner -p "$archive" -p checksums.txt
sha256sum -c --ignore-missing checksums.txt
gh attestation verify "$archive" --repo kravlab/hostrunner  # optional: built by this repo's CI
mkdir -p ~/.local/bin               # any directory in PATH
tar -xzf "$archive" -C ~/.local/bin hostrunner hostrun
hostrunner version
```

From source:

```sh
mise install                        # the pinned toolchain (see mise.toml)
mise run install                    # builds with CGO_ENABLED=0 into ~/.local/bin
PREFIX=/usr/local mise run install
```

`hostrunner` must be in the `PATH` of the process that starts your
devcontainer (VS Code, the devcontainer CLI). The `hostrun` client is
installed next to it; it must stay statically linked, which
`mise run install` guarantees.

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
```

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
```

- A command no rule matches is denied, and so is everything when the file
  is missing. An invalid file (unknown key, two keys on one list such as
  both `allow` and `deny`, a regex that does not compile, `args` together
  with `flags`/`positional`, an empty section, a rule without any of them,
  a relative program path, …) makes `hostrunner up` fail, and with it
  `devcontainer up`.
- Changes apply on the next container start (reopen, restart or rebuild):
  `hostrunner up` notices that the file differs from what the running
  daemon loaded and replaces the daemon. The file lives in `.devcontainer/`,
  which is read-only inside the container, so the agent can read but not
  change its rules.
- Matching is literal on argv: `/usr/bin/git push` and `git -C dir push` do
  not match `git push`. The program must be a name or an absolute path.
- Arguments are parsed like getopt: `--flag=value`, `--flag value`,
  `-abc` (= `-a -b -c`), `-ovalue`, and `--` ending the flags. Only flags
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
- Flag names are `-x` or `--name`. Filters apply to the names listed only:
  list every spelling of a flag, e.g. both `-P` and `--project` under
  `values`. An abbreviated value flag must carry its value inline
  (`--proj=dev`), otherwise it is denied.
- A token with one dash and several characters is read as short flags,
  never as a long one: `-asap` is `-a -s -a -p`, or `-a` with the value
  `git` when `-a` is under `values`. Some programs, such as those built on
  urfave/cli, take it as the long flag `--asap` instead. A `flags.deny`
  list does not catch that spelling, and a short flag under `values` lets
  it through, so give such a program a `flags.allow` list, and `values`,
  with long names only.
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

How it works:

- `hostrunner up` runs on the host before every container start. It
  creates the per-container runtime directory and either re-arms the
  daemon already serving it or copies `hostrun` into it and starts a
  detached daemon.
- The directory is mounted read-only at `/run/hostrunner`; the client
  talks to the daemon over the Unix socket in it.
- The daemon polls `docker`/`podman` for the container and exits about
  15 s after it stops. Re-arming makes it wait (up to 30 min) for the new
  container of a rebuild, however long the image build takes. Its log is
  `daemon.log` in the runtime directory.
- `.devcontainer/` is read-only inside the container, so the container
  cannot rewrite the rules that govern it. `hostrun` warns on stderr when
  the nearest `.devcontainer/` above its working directory, or the
  `hostrun.yaml` in it, is writable (the default location only, not a
  `--config` file).
- The working directory is opened, not re-resolved, when the command
  starts, so a symlink swapped in by the container cannot redirect it.
  Symlinks inside the workspace must be relative.

## Exit codes

| Code | Meaning |
|---|---|
| command's own | the command ran |
| 128+N | the command was killed by signal N |
| 125 | hostrun failed: daemon unreachable, protocol error, or no command given |
| 126 | refused: denied by the rules, working directory outside the workspace, or not executable |
| 127 | command not found on the host |
| 130 | interrupted (Ctrl+C); the host command is killed |

## Limitations

- A running daemon is re-armed, not replaced: after upgrading hostrunner
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
It owns `.devcontainer/` except `hostrun.yaml`, so those files are not
edited here: a change is made in the template and taken with
`uvx copier update`, run on the host with nothing uncommitted
(`.copier-answers.yml` records the version in use). That needs
[uv](https://docs.astral.sh/uv/), which `mise run setup` does not install.
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
`.devcontainer/mise-tasks.toml`, and `mise.toml` brings them in with

```toml
[task_config]
includes = [".devcontainer/mise-tasks.toml"]
```

They run on the host; inside a container `mise tasks` lists them as well,
but they cannot work there.

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
