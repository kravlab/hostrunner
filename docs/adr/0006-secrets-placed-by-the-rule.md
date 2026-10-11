# Secrets placed by the rule

A rule could only filter what the container sends, so a host program that
needs a secret only the host can obtain left two poor ways
([#57](https://github.com/kravlab/hostrunner/issues/57)): hand the secret
to the container, which then holds it for everything it is good for and
puts it in the agent's transcript, or write a wrapper script on every host.
A rule can now name secrets (`secrets:`) and add arguments, environment
variables and files to its command (`inject:`) that refer to them as
`${name}`. Only the rule places a secret; the container never sees it and
cannot move it.

## Considered Options

- **A placeholder the container puts into its own argv**: the container
  could move the secret to where the program echoes it or sends it
  elsewhere, and every position would have to be filtered for that.
  Rejected for fixed positions the rule names.
- **Only the environment and descriptor forms**, as argv is readable in
  the host's process list: programs that take a secret only as an
  argument could not be served, and the host's process list is not what
  the container reaches. Rejected; argv is allowed.
- **Deny lists in a rule with secrets**: a program has more ways to give
  its secret away (verbose output, writing headers to a file, a proxy, a
  config of its own) than a deny list can name. Refused; such a rule takes
  allow lists only, or `args: any` with `secrets_with_any_args: true` as a
  deliberate choice.
- **Filtering the added arguments with the rule's own lists**: the rule
  would have to allow the container the flag it adds itself (`-H`).
  Rejected; the lists check the container's argv only.
- **Caching a secret's value**: an expired or revoked token would fail
  requests until the cache ran out. Rejected for a fresh value on every
  request.

## Consequences

- A secret has exactly one source: `command`, `env` or `file`. A
  `command` source runs, and a relative `file` path resolves, in the
  rule's working directory; a `command` source has a `timeout` (Go
  duration, `30s` by default) and an empty stdin. Trailing `\r` and `\n`
  are trimmed.
- A source that fails, times out, or gives a value that is empty or
  shorter than 8 characters stops the request with 125 and a message
  naming the secret only; the details, and a failed command's stderr, go
  to the daemon log.
- `inject` places values after the program (`program_args`), after the
  rule's command (`command_args`), after the container's arguments
  (`trailing_args`, positional only), in the environment (`env`, which
  overrides the daemon's; `PWD` is refused) and in files (`files`, handed
  over as a read-only descriptor, referred to as `${file.name}`). An
  unknown or unused name, or a lone `$`, is a parse error; `$$` is a
  literal `$`.
- The daemon masks each secret's exact value in the command's output. An
  encoded value is not masked: masking is a safeguard, not a boundary.
- The daemon log keeps the container's argv and the secrets' names, never
  the argv, environment or files with the values in them.
- A dry run checks the sources without running a `command` source and
  names the secrets; the Rules test names them only.
- `inject` without secrets needs no strict lists.
