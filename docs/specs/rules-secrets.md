# Spec: secrets and injection in rules

Ticket: [Spec: secrets and injection in rules](https://github.com/kravlab/hostrunner/issues/58)
(from [#57](https://github.com/kravlab/hostrunner/issues/57)), split into
[#59](https://github.com/kravlab/hostrunner/issues/59) to
[#66](https://github.com/kravlab/hostrunner/issues/66).
Decision record: [ADR 0006](../adr/0006-secrets-placed-by-the-rule.md).
Extends [rules-engine.md](rules-engine.md),
[rules-path-check.md](rules-path-check.md),
[rules-fixed-directory.md](rules-fixed-directory.md) and
[dry-run.md](dry-run.md); vocabulary in [CONTEXT.md](../../CONTEXT.md).

## Goal

A rule could only filter what the container sends, so a host program that
needs a secret only the host can obtain left two poor ways: hand the
secret to the container, or a wrapper script on every host. A rule can now
obtain secrets on the host and add them, with fixed arguments, environment
variables and files, to its command. Only the rule places them; the
container never sees a value and cannot move it.

## Schema

```yaml
rules:
  - command: curl
    secrets:
      token:
        command: [authtool, print-token]   # exactly one of command, env, file
        timeout: 30s                       # command only; 30s by default
      other: { env: API_TOKEN }
      third: { file: .secrets/token }      # absolute, or from the working directory
    inject:
      program_args: […]                    # right after the program
      command_args: ["-H", "Authorization: Bearer ${token}"]  # after the rule's command
      trailing_args: ["${other}"]          # after the container's arguments; positional only
      env: { API_TOKEN: "${third}" }
      files: { cfg: 'header = "X-Key: ${third}"' }            # referred to as ${file.cfg}
    secrets_with_any_args: true            # only next to args: any
    args: any
```

- A secret name is `[a-z][a-z0-9_]*`; secrets are obtained in name order.
- A template is literal text with `${name}` (a secret), `${file.name}` (an
  injected file's descriptor path) and `$$` (a literal `$`).

## Validation errors

Each names the rule.

- A secret name outside the grammar; a secret with no source, or more than
  one; an empty `command`; a `timeout` that is not a positive Go duration,
  or one on a source other than `command`.
- In a template: a lone `$`, an unterminated `${`, a name outside the
  grammar, an unknown secret or file. A secret or file no template uses.
- A rule with `secrets` whose lists are not strict: `args: any` without
  `secrets_with_any_args: true`; no `flags.allow`; a flag under `values`
  without an `allow` or `allow_regex` list; no `positional` with `allow`
  or `allow_regex`. `deny` and `deny_regex` are therefore refused.
  `args: none` is fine.
- `secrets_with_any_args` without `secrets`, or without `args: any`.
- A rule with `inject` but no `secrets` takes any lists.

## Behavior

The daemon's pre-start step gains, after the rules, the directories and
the path checks of [rules-fixed-directory.md](rules-fixed-directory.md):

5. Every secret of the allowing rule is obtained, fresh for this request,
   once even when several templates use it. `command` runs in the working
   directory with the daemon's environment (`PWD` set as for the command),
   an empty stdin and its own process group, which is killed at the
   timeout; its stdout is the value. `env` reads the daemon's
   environment. Trailing CR and LF are trimmed.
6. The injection is rendered and placed: program, `program_args`, rest of
   the rule's command, `command_args`, the container's arguments (with the
   `path: open` replacements), `trailing_args`.

The rule's lists check the container's argv only; the added tokens are
not checked.

A secret with no value, an empty one, or one shorter than 8 characters
refuses the request with 125: `secret "<name>" is not available`, or
`secret "<name>" is too short to mask`. A command source that fails to
start, exits non-zero or times out is not available. The reason goes to
the daemon log, with a failed command source's stderr; a successful
source's stderr is discarded, and no message or log line holds a value.

The daemon log records the container's argv and the names of the
injected secrets (`secrets=[token]`), never the argv run.

## Tests

- `internal/client`, the client against an in-process daemon: what the
  program received (written to a file, not to the output the daemon
  masks), a fresh and trimmed value per request, the refusals and their
  messages, the log, an injection without secrets with `path: open`.
- `internal/rules`, parse and assemble: every validation error above, the
  accepted strict shapes, the placement of the rendered injection.

## Out of scope

Caching values between requests; masking encoded values; a placeholder
the container puts into its own argv; checking what the devcontainer
mounts.
