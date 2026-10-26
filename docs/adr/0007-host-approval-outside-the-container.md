# Host approval outside the container

A command the rules do not let through was refused, and the agent could
only stop and ask in chat; the human then edited the rules file and
restarted the container. A human can now approve such a command on the
host, per request, choosing how far each approval reaches
([#71](https://github.com/kravlab/hostrunner/issues/71), spec
[host-approvals.md](../specs/host-approvals.md)): the agent asks with
`hostrun --ask`, and the human answers in an approval session,
`hostrunner approve`, reached over an approval socket beside the runtime
directory, outside everything the container mounts. Approvals are on only
while a session serves the daemon.

## Considered Options

- **The container's own socket, telling the session apart by
  `SO_PEERCRED`**: a container process can have the same uid as the
  human (a matching user under rootful Docker, root under rootless
  Podman), so the daemon could not tell who answers.
  Rejected for a second socket the container cannot reach; the
  container's socket refuses session frames.
- **Waiting for a human on every refusal**: scripts and hooks would block,
  an agent probing a command would hang until a timeout, and an older
  client could not tell a wait from a hang. Rejected for an explicit
  `--ask`, with its own frame type so an older daemon refuses it.
- **A switch in the rules file, a `hostrunner up` flag or the host
  environment**: one more place to configure per workspace, a flag the
  arm digest does not cover, or an invisible variable of whatever started
  `initializeCommand`. Rejected: starting a session is the switch, and a
  rules file cannot forbid approvals.
- **Waiting for the next container start after "add a rule"**: every
  repeat would ask again until a restart, which also drops standing
  approvals. Rejected: the daemon rereads the rules the session wrote;
  a manual edit still waits for the next start, unless the human, adding
  a rule, agrees to have it reread first, so the rule is proposed against
  the rules the daemon serves.
- **Desktop dialogs or notification actions as the answer channel**: they
  need a display or a session bus the detached daemon may lack, and do not
  work on headless or SSH hosts. Rejected for a terminal session; a
  desktop notification may later nudge
  ([#83](https://github.com/kravlab/hostrunner/issues/83)).

## Consequences

- `hostrunner serve` refuses to start when the approval socket would lie
  inside the host workspace, which the container mounts.
- Guarded refusals (deny patterns, failed path checks, an argv the rules
  cannot read, rules with secrets) need a second confirmation and get no
  "add a rule" answer.
- Standing approvals live in the daemon's memory and end with it;
  session approvals end sooner, when the session that granted them stops
  serving the daemon.
- The daemon log, which the container can read, records approvals but
  never secrets' values or the session's environment.
