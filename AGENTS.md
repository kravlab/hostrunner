## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues (kravlab/hostrunner) via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default canonical labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one root `CONTEXT.md` plus `docs/adr/`. See `docs/agents/domain.md`.

## Host commands (hostrun)

When `hostrun` is in `PATH` (a devcontainer wired to hostrunner),
`.devcontainer/hostrun.yaml` (read-only in the container) lists the
commands that run on the host, where the host's credentials, tools and
network are. A command matched by one of its rules runs as
`hostrun <cmd> [args…]`: on the host, in the directory matching your
current one or in a host directory its rule names (`dir`; relative file
arguments still name files next to you), with the host's environment
(not the container's); stdin,
stdout, stderr and the exit code pass through. Every other command runs
in the container.

- A rule matches an argv prefix literally: write the program and the
  words after it exactly as the rule does, with no flags in between
  (`cd` to a directory rather than passing it as a flag).
- hostrun's own errors start with `hostrun:` on stderr. With such a
  message, exit 126 means refused or not started (no rule allows it, a
  flag or argument is denied, the directory is outside the workspace),
  127 not installed on the host, 125 hostrun itself failed (e.g. the
  daemon is unreachable). Outside the workspace, `cd` into it. Otherwise,
  and when a command needs the host but no rule allows it, report the
  command and the message to the user and wait: only the host can change
  the rules or install tools.
- To learn whether a command would run without running it, put
  `--dry-run` first: `hostrun --dry-run git push --force`. The host
  checks the rules, the directory, file arguments and that the program
  is installed and executable, then exits 0 with
  `hostrun: dry run: allowed by rule "…"` (`, runs in <dir>` when the
  rule names a directory), or with the code and message
  the run would get. Nothing runs and stdin is not read. Anywhere after
  the program, `--dry-run` is the command's own.
- No TTY: supply all input up front, through flags or stdin.

## Go

When working with Go code, use the relevant `golang-*` skills in addition
to any workflow-specific skills.

Select only the Go skills relevant to the current task rather than loading
all Go skills.

Go domain skills supplement workflow skills such as OpenSpec propose,
apply, verify, and code-review; they do not replace them.