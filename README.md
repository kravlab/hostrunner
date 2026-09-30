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
> hooks from the workspace — git (`.git/config`, `.git/hooks`), firebase
> (`firebase.json` predeploy scripts), … — can be steered by the container,
> which can write the workspace. Read-only mounts of such files do not help
> (the container can rename the directory around them or create a nested
> repository). Allowing such a tool against an agent you do not trust is a
> conscious risk of code execution on the host.

## Install

```sh
make install            # builds with CGO_ENABLED=0 into ~/.local/bin
make install PREFIX=/usr/local
```

`hostrunner` must be in the `PATH` of the process that starts your
devcontainer (VS Code, the devcontainer CLI). The `hostrun` client is
installed next to it; it must stay statically linked, which `make install`
guarantees.

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
  - command: tea pr list
    args: none                 # the bare command only
  - command: git push
    flags:
      allow: [-u, --set-upstream, --tags]   # or deny: [...], never both
      values:                  # flags that take a value, with a value filter
        -o: { allow: [ci.skip] }
        --repo: {}             # any value
    positional:
      deny: [main, "+*"]       # or allow: [...], never both
```

- A command no rule matches is denied, and so is everything when the file
  is missing. An invalid file (unknown key, both `allow` and `deny`, `args`
  together with `flags`/`positional`, an empty section, a rule without any
  of them, a relative program path, …) makes `hostrunner up` fail, and with
  it `devcontainer up`.
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
- Flag names are `-x` or `--name`. Filters apply to the names listed only:
  list every spelling of a flag, e.g. both `-P` and `--project` under
  `values`. An abbreviated value flag must carry its value inline
  (`--proj=dev`), otherwise it is denied.
- Values and positional arguments are matched with globs: `*` matches
  anything, including `/`; `?` one character. Under `allow` every argument
  must match; under `deny` none may.

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
  cannot rewrite the rules that govern it.
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

```sh
make test   # unit and integration tests (go test -race ./...)
make e2e    # brings up examples/devcontainer on docker
HOSTRUNNER_E2E_PODMAN=1 make e2e   # also on podman
```

The podman run is opt-in because devcontainer CLI 0.89 sometimes waits
forever for the container's start event from `podman events`, even
without hostrunner.
