---
status: proposed
---

# A process per request behind an activation socket

A daemon is a host process that runs for as long as its container,
even when no command comes. With `process: per-request` there is no
such process: `hostrunner up` has the host system (a user systemd
socket unit, `Accept=yes`) hold the devcontainer's socket, and each
connection starts a request process of the same build as that `up`,
which serves one request as a daemon would and ends with it. The
daemon stays (`own`, `shared`); the setting chooses, and the allow
does not cover it, as with ADR 0010.

## Considered Options

- **sshd on the host, with a key restricted to a forced command**:
  needs a running sshd and network access from the container, and puts
  a credential in the container. Rejected.
- **A foreground process for the session**: a daemon in a terminal;
  closing it stops `hostrun`. Rejected.
- **Replacing the daemon**: hostrunner would not run without a user
  systemd. Rejected: a third value of `process`.
- **Unit files in `~/.config/systemd/user`**: they outlive the login
  session and pile up per workspace. Rejected for unit files in
  `$XDG_RUNTIME_DIR/systemd/user`, which `up` writes and a stop
  removes, and which end with the login session.
- **A transient unit from `systemd-run`**: with `Accept=yes` the socket
  looks for a template service, which `systemd-run` does not create,
  and fails at the first connection. Rejected.
- **Approvals through request and answer files**: a second approvals
  protocol. Rejected: the approval session listens on the approval
  socket, and request processes ask it.
- **A service started with the host**, rejected in ADR 0010, stays
  rejected: the host system holds only a socket; no hostrunner process
  runs between requests.

## Consequences

- Each request reads the allow, the rules (unless `reread: false`:
  then the copy `up` made) and runs the mount check itself; the arm is
  a record `up` writes, and the armed container is found per request.
- When the rules file is not allowed, a request process serves the
  copy of the last allowed rules that `up` and `hostrunner allow` keep
  outside the container's reach, and records a pending request.
- Standing approvals live in a file beside the runtime directory and
  end when the activation socket stops or the login session ends;
  session approvals live in the approval session.
- Commands run with the environment of the `up` that created the unit.
- A stop ends the activation socket; running requests finish, as in a
  drain, or are killed with `--kill`. Changing `process` replaces one
  mode with the other, as a step aside does.
- `startup_timeout`, `grace` and `poll_interval` have no effect;
  `ready_timeout` is how long `up` waits for the unit to listen.
- `up` fails when it cannot create the unit; it never falls back to a
  daemon.
- `up` reloads the user service manager after it writes the units; the
  socket is removed when it stops (`RemoveOnStop=yes`).
- Request processes append to the daemon log in the runtime directory.
