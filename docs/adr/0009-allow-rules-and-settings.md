---
status: accepted
---

# Allow the rules and settings a daemon serves

Whether the container could write the rules file or the workspace
settings file depended only on the mounts in `devcontainer.json`, which
hostrunner does not own (ADR 0008), and a command run through `hostrun`
on the host (`git checkout`, any tool writing the workspace) changes
them past every read-only mount. A daemon now serves only allowed rules
and settings: a human on the host runs `hostrunner allow` in the
workspace, as with direnv, which records the digest of the rules file
together with the daemon settings resolved for it in
`$XDG_DATA_HOME/hostrunner/allow`; `hostrunner deny` removes it.

## Considered Options

- **Denying every command once the rules change to content not
  allowed**: the agent sees the change at once, but a human's own edit
  stops the work until the allow. Rejected: a running daemon keeps
  serving the allowed rules and reports the pending change.
- **Keeping a copy of the allowed rules on the host** so that `up` could
  start a daemon with them: one more store to keep in step with the
  rules file. Rejected for now; `up` fails instead.
- **Asking in the approval session**: it serves a running daemon only,
  and nothing answers when `up` finds no daemon. Rejected for a command.
- **Allowing the rules file alone**: the workspace settings file could
  still change the timeouts, `reread` and `ask_timeout` unasked.
  Rejected: the allow covers the resolved daemon settings.
- **`hostrunner allow` resolving the settings itself, with no request
  from `up`**: the settings flags `up` gets from `initializeCommand`
  (`--grace`, `--no-mount-check`, a `--rules-file`) cannot be reproduced
  by a plain `hostrunner allow`, and a human retyping them would get
  them wrong. Rejected: a refused `up` records what it asked for and
  `allow` allows exactly that. The risk is that the human allows flags
  they did not type, which whoever can write `devcontainer.json` chose;
  `allow` prints every setting that differs from its default with the
  default (`mount_check: false (default true)`), so they see them.
- **One allow per workspace**: two devcontainer configs of one workspace
  whose `initializeCommand`s give `up` different settings flags would
  replace each other's allow at every `hostrunner allow`. **One allow per
  settings set of a workspace** would keep every set ever allowed, an
  older `--grace` included, until `deny`, and still share one pending
  request. Rejected: an allow and a pending request belong to one runtime
  directory of a workspace, which is one per devcontainer config.

## Consequences

- `hostrunner up` with rules or settings not allowed arms a running
  daemon serving allowed ones, which stays, and otherwise fails, so the
  container does not start; also on the first start after an upgrade.
- A daemon that rereads by itself looks at the allow store with the
  rules file and serves new content once it is allowed; with `reread`
  off, it takes effect on the next `up`.
- The daemon log, the approval session and every `hostrun` call report
  rules or settings changed but not allowed.
- Only the host arms a daemon: `up` arms it on its approval socket, and
  the container's socket refuses an arm, so the container cannot report
  a change not allowed of its own, end one, or put the mount check back
  to pending. A daemon from before this refuses the arm, and `up` fails
  until it is stopped.
- A rule added from the approval session is allowed with the content the
  session wrote, provided the file was allowed before the write, for
  every runtime directory of the workspace whose allow covered that file
  and the content before the write.
- `hostrunner allow` allows every pending request of the workspace, or
  none when one of them is out of date, and fails when there is none;
  `hostrunner deny` removes the allows of all its runtime directories. The
  allow of a devcontainer config that is gone stays until a `deny`.
- ADR 0008's "hostrunner does not protect the workspace settings file or
  the rules file" no longer holds for their content.
