# Daemon lifecycle tied to the devcontainer

Research for issue #2 (map: #1). Question: how the host-side `hostrunner`
daemon starts with a devcontainer, finds its socket path, survives SELinux,
and exits when the container stops — for Docker and rootless Podman.

Environment used for the empirical checks (2026-09-26): Fedora, kernel
7.1.8, SELinux enforcing, Docker 29.8.0 (rootful, SELinux support **not**
enabled in dockerd), Podman 5.8.4 (rootless), `@devcontainers/cli` 0.89.0.
All tests used throwaway `busybox` containers and a throwaway workspace;
everything was removed afterwards.

## TL;DR

- `initializeCommand` is the **only** host-side hook. It blocks container
  creation/start, and runs on every `up`: first create, start of a stopped
  container, rebuild, and reconnect to a running container.
- A background child survives only if it is fully detached (new session,
  stdin/stdout/stderr redirected). A plain `cmd &` that keeps the inherited
  stdout **blocks the CLI until the child exits** (measured: 40 s).
- Put the socket in a per-container **directory** under
  `${localEnv:XDG_RUNTIME_DIR}/hostrunner/${devcontainerId}`, created by
  `initializeCommand`, and mount the directory (not the socket file) at
  `/run/hostrunner`.
- To detect a stop, poll `<runtime> ps` filtered by the
  `devcontainer.local_folder` label every few seconds. Exit after a grace
  period with no running match. This works the same with Docker and Podman,
  and it survives rebuilds.
- SELinux on this host needs no extra configuration:
  - The devcontainer CLI already adds `--security-opt label=disable` for
    Podman.
  - Docker here runs containers as `spc_t`.
  - With plain `container_t`, connecting fails **even with `:z`/`:Z`**,
    because policy forbids `container_t` → `unconfined_t` `connectto`.

## 1. Host-side hooks

| Hook | Where it runs | Notes |
|---|---|---|
| `initializeCommand` | host | The only host hook ([spec lifecycle scripts][json-ref]). |
| `onCreate`/`updateContent`/`postCreate`/`postStart`/`postAttach` | container | Not usable for starting a host process. |
| `shutdownAction` (`none`/`stopContainer`/`stopCompose`) | tool (VS Code) | Tells the tool to stop the container when the window closes ([json_reference][json-ref]; [VS Code docs][vsc-containers]). The CLI only stores it in metadata and never acts on it. There is **no stop/teardown hook**. |

### Blocking and ordering

`initializeCommand` is awaited before any container work. The CLI source
order in `src/spec-node/configContainer.ts` is:

1. `findContainerAndIdLabels`
2. `runInitializeCommand`
3. build, then `rm` of the old container on rebuild, then `start` or `run`

A non-zero exit aborts `up` with "The initializeCommand in the
devcontainer.json failed". Observed CLI trace for a rebuild
(`up --remove-existing-container`, Podman):

```
03:54:38.045Z Running the initializeCommand
03:54:38.321Z Start: Run: podman rm -f 9e9bd4a77a87…
03:54:38.640Z Start: Run: podman run --sig-proxy=false …
```

### When it runs

It runs on every resolve: first create, starting a stopped container,
rebuild, and `up`/reopen while the container is already running.

- Spec: "including during container creation and on subsequent starts. The
  command may run more than once during a given session"
  ([json_reference][json-ref]).
- VS Code maintainer: "Always run before creating/restarting/connecting to
  the container" and "add a check … that will exit if the script already
  ran" ([vscode-remote-release#9278][vrr-9278]).
- Verified with the CLI: it ran on `run` (new container), on `up` against a
  running container, and on `podman start` of a stopped container.
- It does **not** run when something else starts the container, e.g. a
  plain `docker start`.

### Working directory and environment

- The cwd is the workspace folder (observed `pwd=<workspace>`).
- The environment is the CLI's own; no devcontainer variables are injected.
- `${localWorkspaceFolder}`, `${localEnv:…}` and `${devcontainerId}` are
  substituted into the command string. Observed argv: the `devcontainerId`
  arrived as `01kec8r7s3upu285imh0d8gvt5c7i9avb0ca20fnuflumqrned62`.
- A string runs via `/bin/sh -c`, an array runs without a shell, and an
  object runs its entries in parallel.

### Background processes

The npm CLI ships without `node-pty` ([devcontainers/cli#585][cli-585]), so
it spawns with `cp.spawn(..., {stdio: 'pipe'})` (non-TTY). It has no
`detached` option and no process-group kill. It finishes on the child's
`'close'` event, which Node emits only after the process has ended **and
its stdio streams are closed** ([Node docs][node-close]).

With `node-pty` (possibly inside VS Code; closed source, not verified), a
plain `&` child can instead get SIGHUP when the pty closes.

Measured with `devcontainer up`, where the init script backgrounds a
`sleep 40`:

| Mode | `up` duration | Child survived CLI exit |
|---|---|---|
| `(sleep 40; …) &` (inherits stdout/stderr) | **42 s** (init done at :14.56, `podman run` at :54.76) | yes |
| `nohup sh -c … >/dev/null 2>&1 </dev/null &` | 2 s | yes |
| `setsid -f sh -c … >/dev/null 2>&1 </dev/null` | 1 s | yes |

**Rule:** the command in `initializeCommand` must:

- detach fully (`setsid`, all three fds redirected to a log file or
  `/dev/null`);
- return quickly;
- be idempotent.

For hostrunner, the binary should do this itself, e.g. `hostrunner up`
spawns the daemon with `SysProcAttr{Setsid: true}`, waits for readiness,
and exits 0.

## 2. Detecting that the container stopped

Container identification: the CLI labels every container with its id
labels, `devcontainer.local_folder=<abs workspace>` and
`devcontainer.config_file=<abs devcontainer.json>`, plus
`devcontainer.metadata`. These are the same for Docker and Podman
(verified with `inspect`). The container **does not exist yet** when
`initializeCommand` runs.

| Mechanism | Docker | Podman | Verdict |
|---|---|---|---|
| `<rt> ps -q --filter label=devcontainer.local_folder=<ws> --filter status=running` (poll) | works | works; identical flags and output | **recommended** |
| `<rt> inspect -f '{{.State.Running}}' <id>` (poll) | works; `true running` → `false exited`; "no such object" after rm | same | fine, but tied to one container ID |
| `<rt> wait <id>` (blocking) | returns the exit code (137) right after stop | returns 137, ~1.5 s later than Docker in the same test | tied to one ID; a rebuild replaces the ID |
| `<rt> events --filter label=… --format '{{json .}}'` | emits `"Action":"stop"` and `"Action":"die"` | emits only `"Status":"died"`; different JSON schema; depends on the events backend (journald) | not uniform |
| Engine API over a socket (Go SDK) | `/var/run/docker.sock` | needs `podman.socket` enabled (compat API) | extra setup for Podman |
| hostrunner socket / connection | clients connect per command; no long-lived peer | same | a keepalive client inside the container (`postStartCommand`) would work, but adds a fragile in-container process |

### Recommendation

Label-based polling through the runtime CLI:

- Choose the runtime binary with a flag or config (`docker`/`podman`),
  matching VS Code's `dev.containers.dockerPath`.
- Every ~2 s, list running containers with
  `label=devcontainer.local_folder=<ws>`. To match `devcontainerId`
  exactly, also filter by `devcontainer.config_file`, or recompute the ID
  from the labels (see §3).
- State machine:
  - **waiting** until a match is first seen, bounded by a startup timeout
    that covers image builds (e.g. 10–30 min, configurable);
  - **attached** while a match exists;
  - **exit** after no running match has been seen for a grace period
    (e.g. 15 s).
- The grace period, and keying on labels rather than on a container ID, are
  what make rebuilds work:
  1. On a rebuild, `initializeCommand` runs while the old container is
     still alive.
  2. The new invocation finds the existing daemon and exits.
  3. The old container is removed and the new one starts less than 1 s
     later (observed).
  4. The same daemon sees a matching container again and keeps running.
  5. A daemon tied to the old container ID would have exited and left no
     daemon.

## 3. Per-container socket path

### Variables

`${devcontainerId}` is allowed in `mounts`, `runArgs`, `initializeCommand`,
`containerEnv` and others. `${localEnv:VAR[:default]}` and
`${localWorkspaceFolder}` work anywhere ([json_reference][json-ref]).

`devcontainerId` is `sha256(JSON.stringify(idLabels with sorted keys))`,
converted to base-32 and padded to 52 characters. It is "stable across
rebuilds" ([devcontainer-id spec][id-spec]). Verified:

- the same ID on create, restart and rebuild;
- the same ID for the Docker and Podman containers of the same workspace;
- it changes if the workspace path or the config file changes.

### Mount source must exist

Every `mounts` entry becomes `--mount` (only the `wslc` variant uses `-v`),
and a missing source fails. Verified:

- Docker `--mount`: "bind source path does not exist".
- Podman `--mount` and `-v`: "statfs …: no such file or directory".
- Docker `-v` silently creates a **root-owned** directory, and later bind
  mounts of a non-existent socket *file* also turn into root-owned
  directories.

`initializeCommand` runs first (see §1), so it must create the directory
**synchronously** before it returns.

### `sun_path` limit

`sun_path` is 108 bytes. `bind` on a longer path failed with
`bind: invalid argument` (EINVAL) during testing. Keep the host path short,
or `chdir` into the directory and bind a relative name.

### Recommended layout

```jsonc
"initializeCommand": "hostrunner up --id ${devcontainerId} --workspace ${localWorkspaceFolder}",
"mounts": [
  "source=${localEnv:XDG_RUNTIME_DIR}/hostrunner/${devcontainerId},target=/run/hostrunner,type=bind"
]
```

- The host socket is `/run/user/1000/hostrunner/<52-char id>/hostrun.sock`,
  **97 bytes** (verified to bind and to connect from both runtimes).
  In the container it is `/run/hostrunner/hostrun.sock`.
- `$XDG_RUNTIME_DIR` is a per-user 0700 tmpfs, which suits sockets. Rootful
  dockerd (root) and rootless Podman (same user) can both mount it
  (verified).
- The process that evaluates `devcontainer.json` must have
  `XDG_RUNTIME_DIR` set. That holds in a systemd login session; otherwise
  give a `:default`.
- Mount the **directory**, not the socket file:
  - If the daemon restarts, it recreates the socket, and a file bind mount
    would keep pointing at the stale inode.
  - A file mount also requires the socket to exist before `run`.
  - A socket recreated inside a mounted directory was reachable (verified).
- Not recommended: a socket inside the workspace (e.g. under
  `.devcontainer/`). Its path length depends on the workspace depth, it
  needs a `.gitignore` entry, and it breaks for clone-in-volume workspaces.
  It would work technically: connecting through a read-only mount succeeds
  (verified with `:ro`), because `connect()` is not a write to the
  filesystem.

## 4. SELinux

The domain the container runs in decides the outcome.

| Container started by | Container domain (observed) |
|---|---|
| `docker run` on this host (dockerd without SELinux support; `docker info` SecurityOptions has no `selinux`) | `system_u:system_r:spc_t:s0` |
| `podman run` (rootless, default) | `system_u:system_r:container_t:s0:cX,cY` |
| `podman run --security-opt label=disable` | `unconfined_u:unconfined_r:spc_t:s0` |
| **`devcontainer up --docker-path podman`** | `unconfined_u:unconfined_r:spc_t:s0`; the CLI adds `--security-opt label=disable` (and `--userns=keep-id` for a non-root remoteUser) on Linux (`singleContainer.ts`) |
| `devcontainer up` with Docker | `system_u:system_r:spc_t:s0` |

### Connect matrix

Host daemon is `unconfined_t`; socket directory is `user_tmp_t` or
`user_home_t`; rootless Podman:

| Mount | default socket label (`unconfined_t`) | socket labelled `spc_t` via `setsockcreatecon` |
|---|---|---|
| `-v dir:/run/h` / `--mount` (no relabel) | EACCES | EACCES (sock_file `user_tmp_t` not writable) |
| `-v dir:/run/h:ro` | EACCES | EACCES |
| `-v dir:/run/h:z` / `:ro,z` / `:Z` / `--mount …,relabel=shared` | **EACCES** | **OK** |
| any mount + `--security-opt label=disable` | OK | OK |

With Docker on this host (`spc_t`), every variant worked (file mount,
directory mount, `:ro`, `:z`, `:Z`, `label=disable`).

### Why `:z` alone is not enough

A Unix stream connect needs two permissions:

- `sock_file write` on the file, which `:z` fixes by relabelling it to
  `container_file_t`;
- `unix_stream_socket connectto` on the **listening process's** domain.

The loaded policy allows `container_t` to `connectto` only `container_t`,
`spc_t` and a few system services (from `sesearch -A -s container_t -c
unix_stream_socket -p connectto`: `allow domain spc_t:unix_stream_socket
connectto`, `allow container_t container_t:…`, sssd, gssproxy, resolved, …).
There is no rule for `unconfined_t`.

The daemon can have its listening socket labelled `spc_t` by writing
`/proc/thread-self/attr/sockcreate` before `listen`, with
`runtime.LockOSThread`. `unconfined_t` holds `setsockcreate`, and
`unconfined_domain_type` may create sockets of any domain type.

### Other observations

- A socket recreated after a `:z` relabel inherits `container_file_t` from
  the directory and stays reachable, so a daemon restart is fine (verified
  with a running container and `podman exec`).
- Docker's `--mount` rejects `relabel=` ("unknown option 'relabel'"). A
  `mounts` entry that relabels is therefore Podman-only.
- A **client binary** bind-mounted unrelabelled (`user_tmp_t`/`user_home_t`)
  into a `container_t` Podman container exited with 139 (SIGSEGV); with `:z`
  it ran. Under `spc_t` (the devcontainer CLI path and Docker) it ran
  unrelabelled.

### Conclusion for the MVP

Nothing extra is needed on this host:

- Podman through the devcontainer CLI or VS Code runs `spc_t`.
- Docker here runs `spc_t`.
- The end-to-end test passed: `devcontainer up` with the socket directory
  in `mounts`, then `exec` a dial, returned OK on both runtimes.

For hosts where dockerd has `--selinux-enabled` (e.g. Fedora's `moby-engine`
package), containers run as `container_t` and the connect is denied by the
same policy. Two options:

- document `"runArgs": ["--security-opt", "label=disable"]`;
- later, harden the daemon to label its socket `spc_t`, and relabel the
  directory through `runArgs` `-v …:z`, because `--mount` cannot relabel on
  Docker.

This case was reasoned from policy, not reproduced; dockerd config was not
changed.

## 5. Recommended MVP lifecycle

1. **devcontainer.json**:
   - `initializeCommand: hostrunner up --id ${devcontainerId} --workspace ${localWorkspaceFolder} [--runtime podman]`
   - a `mounts` entry binding `${localEnv:XDG_RUNTIME_DIR}/hostrunner/${devcontainerId}`
     to `/run/hostrunner`
   - the existing read-only mounts for the rules config and the `hostrun`
     client
2. **`hostrunner up`** (synchronous, fast, idempotent):
   1. `mkdir -p` the socket directory (0700).
   2. If the socket answers a ping, exit 0: a daemon is already running for
      this ID.
   3. Otherwise remove the stale socket, spawn `hostrunner daemon` with
      `Setsid`, stdio going to a log file in the directory, and wait until
      it listens.
   4. Exit 0 on success; a non-zero exit fails `up` visibly.
3. **daemon**:
   - Listens on `hostrun.sock` (bind via a relative name after `chdir` to
     stay under 108 bytes). A lock file (`flock`) in the directory
     guarantees a single instance.
   - Every 2 s, polls `<runtime> ps -q --filter label=devcontainer.local_folder=<ws> --filter status=running`,
     and checks that the ID computed from the labels equals `--id`.
   - Exits when no match has been seen for 15 s after attaching, or when
     none has appeared within the startup timeout. On exit it removes the
     socket.
4. **Stop, rebuild, reopen**:
   - Stop (VS Code `shutdownAction`, or a manual stop): the poll sees no
     match, and the daemon exits after the grace period.
   - Rebuild: the daemon keeps running across the rm/run gap.
   - Reopen or restart: `initializeCommand` runs again, and `up` either
     finds the live daemon or starts a new one.
5. **Out of scope / caveats**:
   - A container started outside devcontainer tooling gets no daemon.
   - VS Code's stop-on-close is itself unreliable in some setups
     ([vscode-remote-release#8991][vrr-8991]). The daemon only follows
     the container state.

## Sources

- Dev Container spec, devcontainer.json reference (lifecycle scripts,
  variables, `shutdownAction`): [containers.dev/implementors/json_reference][json-ref]
- Dev Container ID variable spec: [devcontainers/spec devcontainer-id-variable.md][id-spec]
- `@devcontainers/cli` source (`src/spec-node/configContainer.ts`,
  `singleContainer.ts`, `spec-common/commonUtils.ts`,
  `variableSubstitution.ts`, `spec-shutdown/dockerUtils.ts`):
  [github.com/devcontainers/cli][cli]; installed bundle 0.89.0 inspected
  locally. Publishing without node-pty: [devcontainers/cli#585][cli-585]
- VS Code: [Developing inside a Container][vsc-containers],
  [Docker options / Podman][vsc-podman],
  [vscode-remote-release#9278][vrr-9278], [#8991][vrr-8991]
- Node.js `child_process` `'close'` event: [nodejs.org][node-close]
- Docker bind mounts (`--mount` does not create the source):
  [docs.docker.com/engine/storage/bind-mounts][docker-bind]
- Podman `--volume` (source must exist; `z`/`Z`), `--mount relabel`:
  [docs.podman.io podman-run][podman-run]
- SELinux policy: local `sesearch`/`getsebool` queries of the loaded
  targeted policy (listed in §4)

[json-ref]: https://containers.dev/implementors/json_reference/
[id-spec]: https://github.com/devcontainers/spec/blob/main/docs/specs/devcontainer-id-variable.md
[cli]: https://github.com/devcontainers/cli
[cli-585]: https://github.com/devcontainers/cli/pull/585
[vsc-containers]: https://code.visualstudio.com/docs/devcontainers/containers
[vsc-podman]: https://code.visualstudio.com/remote/advancedcontainers/docker-options
[vrr-9278]: https://github.com/microsoft/vscode-remote-release/issues/9278
[vrr-8991]: https://github.com/microsoft/vscode-remote-release/issues/8991
[node-close]: https://nodejs.org/api/child_process.html#event-close
[docker-bind]: https://docs.docker.com/engine/storage/bind-mounts/
[podman-run]: https://docs.podman.io/en/latest/markdown/podman-run.1.html
