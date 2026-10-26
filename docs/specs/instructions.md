# Spec: `hostrunner instructions`

Vocabulary in [CONTEXT.md](../../CONTEXT.md).

## Goal

An agent in a devcontainer wired to hostrunner needs to know how to run
host commands through `hostrun`: which rules apply, what the exit codes
and `hostrun:` messages mean, how a dry run and an approval work. The
text exists as the "Host commands (hostrun)" section of this
repository's AGENTS.md; a project that adopts hostrunner copies it by
hand. `hostrunner instructions` prints that text, and with `--append`
adds it to a project's instructions file, so the copy comes from the
installed version.

## Command

`hostrunner instructions [--append [<path>]]`, on the host.

- Flags are parsed like the other subcommands' (Go's flag package): `-h`
  prints the flag help and exits 1.
- Without `--append`: prints the block (below) to stdout, exit 0. A
  positional argument without `--append` is an error, exit 1.
- With `--append`: adds the block to `<path>`, default `AGENTS.md` in
  the current directory. More than one positional argument is an error,
  exit 1. A relative `<path>` is relative to the current directory.
- The top-level usage lists the subcommand:
  `up|serve|test|approve|update|instructions|version`.
- `hostrun` gets no such command: its arguments are the host command
  to run.

## The block

The text sits between two marker lines, which `--append` uses to find
an earlier copy:

```
<!-- hostrunner instructions begin -->
## Host commands (hostrun)
…
<!-- hostrunner instructions end -->
```

The printed output and the written block are the same bytes, ending in
one newline, so `hostrunner instructions >> AGENTS.md` and
`hostrunner instructions --append` give the same text.

A marker is recognised only as a whole line (no leading or trailing
characters other than a final `\r`).

## `--append`

1. `<path>` does not exist: asks `<path> does not exist; create it?
   [y/N] `. `y` or `yes` (any case) creates it with the block, mode
   0644 before umask; anything else, an empty line or end of input
   leaves the file system unchanged. Refused before the question (and
   before the terminal check), exit 1, nothing written: a missing
   parent directory (the OS error) and a symlink to a missing file
   (`<path> is a symlink to a missing file`).
2. `<path>` exists without a begin marker: the block is appended. A
   non-empty file not ending in a newline gets one first; a non-empty
   file gets a blank line before the block, unless it already ends in
   one.
3. `<path>` has a begin marker: asks `<path> already has hostrunner
   instructions: [r]eplace, [a]ppend, [c]ancel? `.
   - `r`/`replace`: the first block (begin marker through the next end
     marker) is replaced with the new one, and every later block is
     removed together with one blank line before it, if any.
   - `a`/`append`: as in step 2, another copy is added.
   - `c`/`cancel`, an empty line or end of input: nothing is written.
   - Any other answer repeats the question.
   A begin marker with no end marker after it is refused before the
   question: `<path>: hostrunner instructions begin marker has no end
   marker`, exit 1, file unchanged.
   After `r` or `a` the file is read again, so an edit made while the
   question was open is kept: the answer applies to what the file
   holds then. A begin marker left without an end marker is refused as
   above; for `r`, a file no longer holding a block gets the block
   appended as in step 2 (reported as appended).

Questions go to stderr and answers are read from stdin, one line each.
When stdin is not a terminal, a question is not asked: steps 1 and 3
are refused with `<path> does not exist; run in a terminal to create
it` or `<path> already has hostrunner instructions; run in a terminal
to choose`, exit 1, nothing written.

The file is written in place (append, or truncate and write), not
replaced by a rename: a symlink is followed and a bind-mounted file
(e.g. `HOSTRUNNER_AGENTS_MD`) shows the change. Its mode and owner
stay.

On success stdout gets one line: `hostrunner: created <path>`,
`hostrunner: appended instructions to <path>` or `hostrunner: replaced
instructions in <path>`. A cancel prints `hostrunner: <path> left
unchanged` and exits 0.

## Edge cases

- `<path>` is a directory or cannot be read or written: the OS error,
  exit 1, nothing written.
- An empty `<path>` is an error carrying the usage, exit 1.
- An end marker without a begin marker is ordinary text.
- CRLF files: markers ending in `\r` are recognised; the block is
  written with `\n`.

## Code

- `internal/instructions`: `instructions.md` (the block, embedded with
  `go:embed`), the merge of the block into a file's content (append,
  replace, marker search) as a pure function, and the file update with
  the questions, given stdin, stderr and whether stdin is a terminal.
- `cmd/hostrunner`: `parseInstructions`, the `instructions` case in
  `run`, the usage line and the package comment.
- This repository's AGENTS.md: its "Host commands (hostrun)" section
  becomes the block, markers included.

## Tests

- `internal/instructions`: the block starts and ends with the markers
  and ends in one newline; the merge for an empty file, a file with and
  without a final newline, CRLF markers, a replaced block in the middle
  of the file (text before and after kept), several blocks replaced by
  one, append next to an existing block, a begin marker without an end
  marker refused, an end marker alone treated as text, markers inside a
  line not recognised; the file update for create (y / n / empty / end
  of input), append, replace, append-again, cancel, a repeated question
  after an unknown answer, non-terminal stdin refused in both cases with
  the exact message and the file unchanged, a symlinked file updated
  through the link, a directory and a missing parent refused.
- Sync: the block between the markers in this repository's AGENTS.md
  equals the embedded block.
- `cmd/hostrunner`: flag parsing (no flags, `--append`, `--append
  <path>`, a path without `--append`, two paths, `-h`), the printed
  output equals the block.

## Docs

README: a paragraph under "Use in a devcontainer" on adding the
instructions with `hostrunner instructions --append`. CONTRIBUTING or
AGENTS.md: the hostrun section is edited in
`internal/instructions/instructions.md` and copied into AGENTS.md by
running `go run ./cmd/hostrunner instructions --append` from the
repository root in a terminal and answering `r` (an installed
hostrunner holds the text of its own build), or by copying the file
over the block.

## Out of scope

- Choosing a heading level or translating the text.
- Writing the block into files other than the one given (e.g. both
  AGENTS.md and CLAUDE.md in one run).
