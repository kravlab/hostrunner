# hostrunner

Run host commands from inside a devcontainer: `hostrun git push` in the
container runs `git push` on the host, in the host directory that mirrors
the container's current directory, with the host's environment and
credentials. Stdin, stdout and stderr are streamed; the exit code is
propagated.

The host side (`hostrunner`) starts with the devcontainer and exits after
the container stops. Linux hosts; Docker and Podman.

> Status: MVP in progress. There are no rules yet: every command the
> container sends is run. Rules in `.devcontainer/hostrun.yaml` are next.

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

Then, inside the container:

```sh
hostrun git push
hostrun fakehost deploy --only hosting
```

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
  cannot rewrite the rules that will govern it.

## Exit codes

| Code | Meaning |
|---|---|
| command's own | the command ran |
| 128+N | the command was killed by signal N |
| 125 | hostrun failed: daemon unreachable, protocol error, or no command given |
| 126 | refused: working directory outside the workspace, or not executable |
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
make e2e    # brings up examples/devcontainer on docker and podman
```
