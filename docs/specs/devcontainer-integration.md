# Spec: Devcontainer integration

Ticket: [Devcontainer integration](https://github.com/kravlab/hostrunner/issues/6)
Research: `docs/research/devcontainer-lifecycle.md` on branch
`research/devcontainer-lifecycle`.

## Goal

Adding the example snippet to a project's `devcontainer.json` is enough to
run `hostrun <cmd>` in the container: the daemon starts with the container
(from `initializeCommand`), the client and socket appear in the container,
`.devcontainer/` is read-only inside it, and the daemon exits by itself
after the container stops. Works with Docker and Podman on Linux.

## `devcontainer.json` snippet

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

The runtime directory (`<dir>`) holds `hostrunner.sock`, the client binary
`hostrun`, `daemon.log` and `up.lock`. In the container it is
`/run/hostrunner` (read-only: connecting needs no write access, and the
container must not tamper with the daemon's files), which matches the
client's default socket path. The array form of `initializeCommand` runs
without a shell, so paths with spaces survive.

## `hostrunner up --dir <dir> --workspace <host> --container-workspace <path>`

Synchronous, fast, idempotent (it runs on every `devcontainer up`):

1. Create `<dir>` with mode 0700 (parents as needed). Fail if the socket
   path would exceed the 107-byte `sun_path` limit; hint at an unset
   `XDG_RUNTIME_DIR` if the directory cannot be created.
2. Take an exclusive `flock` on `<dir>/up.lock` for the remaining steps, so
   concurrent `up`s share one daemon.
3. Send `FrameArm` to `<dir>/hostrunner.sock`. `FrameArmed` → exit 0. Nothing
   listening → continue. Anything else → fail (a foreign listener).
4. Install the client: copy the `hostrun` binary that sits next to the
   running `hostrunner` executable into `<dir>/hostrun` (mode 0755, written
   to a temp file and renamed). Fail if that binary is dynamically linked
   (ELF with `PT_INTERP`): it must run in any image.
5. Start `hostrunner serve --watch …` detached (`setsid`, cwd `<dir>`,
   stdin from `/dev/null`, stdout/stderr appended to `<dir>/daemon.log`,
   which must be a regular file: opened with `O_NOFOLLOW`).
6. Wait up to 10 s until the daemon answers `FrameArm`; exit 0. If the
   daemon exits or the wait times out (the daemon is then killed), exit
   non-zero with the log's tail, which makes `devcontainer up` fail.

## `hostrunner serve --watch`

`serve` gains `--watch`. With it the daemon also follows the container:

- Runtimes: every one of `docker` and `podman` found in `PATH`; no config.
- Every 2 s it asks each runtime (10 s timeout per query) for running
  containers labelled `devcontainer.local_folder=<workspace>`. (Since
  [watch-armed-container.md](watch-armed-container.md): for the
  workspace's containers in every state, with their start times.) Present if
  any runtime reports one; absent if some runtime answered and none does;
  unknown if none answered. Unknown does not count toward the grace period;
  a failing runtime is logged when it starts and stops failing.
- States: **waiting** for a container (bounded by `--startup-timeout`,
  default 30 min, to cover image builds) → **attached** → exit once the
  container has been observed absent for `--grace` (default 15 s), or no
  runtime answered for the startup timeout.
- `FrameArm` puts the watcher back into **waiting**. devcontainer arms
  before removing the old container on a rebuild, so for `--grace` after an
  arm the watcher does not attach to what it sees: a container that
  disappears in that window is being rebuilt, and the daemon waits for the
  new one. (Correction to the research, which assumed the grace period
  covers the rebuild gap; the build happens inside that gap.) Replaced by
  [watch-armed-container.md](watch-armed-container.md): a container
  started after the arm is attached to at once.
- Exiting cancels `Serve`: running commands are killed and the socket file
  is removed.

Without `--watch`, `serve` behaves as today (runs until signalled).

## Packages

- `internal/protocol` — `FrameArm`/`FrameArmed` (JSON `Arm{version}`); an
  arm connection carries only that exchange.
- `internal/daemon` — `WithArmHandler` option; version checked for both
  opening frames.
- `internal/watch` — `Watcher` (`Run`, `Arm`) over a `Runtime` interface
  (`Containers(ctx, workspace) ([]Container, error)`), plus the CLI-backed
  runtime (`<rt> ps -aq --filter label=…`, then `<rt> inspect`); see
  [watch-armed-container.md](watch-armed-container.md).
- `internal/launch` — `up`'s steps: runtime dir, lock, arm, client install
  with the static-binary check, detached spawn, readiness via arm.
- `cmd/hostrunner` — wires `up` and `serve --watch`.

## Build and install

`Makefile`: `build` and `install` compile both binaries with
`CGO_ENABLED=0 -trimpath`; `install` copies them to `$(PREFIX)/bin`
(default `~/.local`). Also `test` and `e2e` targets.

## Example and verification

- `examples/devcontainer/.devcontainer/devcontainer.json`: the snippet on
  a small image, ready to copy.
- `e2e/` tests behind build tag `e2e` (`make e2e`), run for each available
  runtime through the devcontainer CLI with freshly built binaries, on a
  workspace whose path contains a space and whose image build takes longer
  than the grace period:
  - `hostrun pwd` prints the host workspace path;
  - a file created via `hostrun` appears in the host workspace;
  - writing to `.devcontainer/` or `/run/hostrunner` in the container fails;
  - a second `up` reuses the daemon;
  - the daemon survives `--remove-existing-container --build-no-cache`;
  - after stopping the container the daemon exits and the socket is gone;
  - starting it again brings a new daemon.

## Test seams

- `internal/watch` — state machine with fake runtimes and short timings.
- `internal/launch` — with the test binary re-executed as a fake daemon:
  spawn, arm, lock, detached stdio and session, readiness timeout, early
  exit, symlinked log, static check, path limits.
- `cmd/hostrunner` — `serve --watch` with injected runtimes, arm wiring.
- End-to-end — the `e2e` tests above.

## Out of scope

Rules (the rules config is only mounted read-only), several containers of
one workspace, replacing a running daemon after an upgrade or a changed
`workspaceFolder` (it is armed again as is), containers started outside devcontainer tooling, Docker with
SELinux enabled in dockerd (documented workaround:
`"runArgs": ["--security-opt", "label=disable"]`).

## Docs

README: what hostrunner is, install (`make install`), the snippet, exit
codes, and the SELinux note.
