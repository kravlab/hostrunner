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

- **Denying every command once the rules change unallowed**: the agent
  sees the change at once, but a human's own edit stops the work until
  the allow. Rejected: a running daemon keeps serving the allowed rules
  and reports the pending change.
- **Keeping a copy of the allowed rules on the host** so that `up` could
  start a daemon with them: one more store to keep in step with the
  rules file. Rejected for now; `up` fails instead.
- **Asking in the approval session**: it serves a running daemon only,
  and nothing answers when `up` finds no daemon. Rejected for a command.
- **Allowing the rules file alone**: the workspace settings file could
  still change the timeouts, `reread` and `ask_timeout` unasked.
  Rejected: the allow covers the resolved daemon settings.

## Consequences

- `hostrunner up` with rules or settings not allowed arms a running
  daemon serving allowed ones, which stays, and otherwise fails, so the
  container does not start; also on the first start after an upgrade.
- A daemon that rereads by itself looks at the allow store with the
  rules file and serves new content once it is allowed; with `reread`
  off, it takes effect on the next `up`.
- The daemon log, the approval session and every `hostrun` call report
  rules or settings changed but not allowed.
- A rule added from the approval session is allowed with the content the
  session wrote, provided the file was allowed before the write.
- ADR 0008's "hostrunner does not protect the workspace settings file or
  the rules file" no longer holds for their content.
