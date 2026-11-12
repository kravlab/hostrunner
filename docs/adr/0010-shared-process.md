---
status: accepted
---

# Daemons share one host process

Every devcontainer had a host process of its own. To run fewer
processes, the daemons of several devcontainers now live in one
shared process by default (`process: shared`; `own` keeps the
old way). Only the process is shared: each daemon keeps its
runtime directory, sockets, log, rules, settings, allow, mount
check, approvals and approval session, and does what a daemon of
its own process would.

## Considered Options

- **One process for every version**: a new version would join
  an older process, or replace it with the daemons of every
  workspace. Rejected: one shared process per version; `up` or
  `restart` of a new version starts its daemon in the process of
  that version, and an older one lives while it holds a daemon.
- **A service started with the host** (systemd): rejected; the
  first `up` starts the shared process, which ends with its last
  daemon.
- **A supervisor that restarts a fallen process**: one more
  process, and approvals would be lost anyway. Rejected: a panic
  ends only its daemon; a fallen process waits for the next `up`.
- **One poll of the container runtimes for all daemons**:
  rejected for now; each daemon watches its container itself.
- **`process` covered by the allow**: with `shared` the default,
  every allow would be out of date after an upgrade. Rejected:
  the setting does not change what a container may run.

## Consequences

- A process that falls (out of memory, killed) leaves every
  workspace it held without `hostrun` until their next `up`.
- Stop, restart, step aside and reread act on one daemon; the
  process ends with its last daemon.
- The process logs its own events in the runtime root, out of
  the containers' reach; each daemon logs to its runtime
  directory as before.
