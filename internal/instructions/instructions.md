<!-- hostrunner instructions begin -->
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
  daemon is unreachable, an argument or the directory is not valid
  UTF-8). Outside the workspace, `cd` into it. Otherwise,
  and when a command needs the host but no rule allows it, report the
  command and the message to the user and wait: only the host can change
  the rules or install tools.
- To learn whether a command would run without running it, put
  `--dry-run` first: `hostrun --dry-run git push --force`. The host
  checks the rules, the directory, file arguments and that the program
  is installed and executable, then exits 0 with
  `hostrun: dry run: allowed by rule "…"` (`, runs in <dir>` when the
  rule names a directory; `allowed by a standing approval` or `by a
  session approval` when a human's approval lets it through), or with
  the code and message the run would get. Nothing runs and stdin is not read. Anywhere after
  the program, `--dry-run` is the command's own.
- A refusal holding `(ask for an approval: hostrun --ask …` (with
  `; guarded: needs a second confirmation` or without) can be approved
  by a human on the host: re-run the same command with
  `--ask=<duration>` first (`hostrun --ask=3m git rebase -i main`), the
  duration within your tool's timeout and at most 1 hour (125 otherwise).
  It waits for the answer, then runs or exits 126. On `refused on the
  host` (with the human's note, if any), `nobody answered on the host`
  or `the hostrunner daemon is stopping`, report it to the user and
  wait; do not ask again.
- No TTY: supply all input up front, through flags or stdin.
<!-- hostrunner instructions end -->
