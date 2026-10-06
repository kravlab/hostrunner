# Spec: Go flag style

Ticket: [Spec: flag_style: go for single-dash long flags](https://github.com/kravlab/hostrunner/issues/33)
(from [#26](https://github.com/kravlab/hostrunner/issues/26)).
Decision record: [ADR 0003](../adr/0003-go-flag-style.md).
Extends [rules-engine.md](rules-engine.md); vocabulary in
[CONTEXT.md](../../CONTEXT.md).

## Goal

A rule can filter the flags of a program built on Go's `flag` package or
urfave/cli. Read like getopt, `-asap` is the cluster `-a -s -a -p`, while
such a program takes it as `--asap`: a `flags.deny` list misses it, a short
value flag swallows the rest of it, and a single-dash long name cannot be
written in the rule.

## Schema

```yaml
rules:
  - command: tix pr
    flag_style: go             # getopt (the default) or go
    flags:
      deny: [-asap]
      values:
        -repo: { allow: [my/repo] }
```

- `flag_style` sits on the rule, next to `command`, `flags` and
  `positional`: it decides which tokens are flags, flag values and
  positional arguments.
- Under `getopt` nothing changes.
- Under `go` a flag name is `-name` or `--name`, any length; both spellings
  name one flag, in the rule and in argv.

## Parsing under `go`

`go` stands for Go's `flag` package and urfave/cli v1, v2 and v3, with or
without `UseShortOptionHandling`. These parsers disagree in places; a token
they may read differently must pass every reading.

Up to the first token they may read differently:

- `-name` and `--name` are the same flag, matched exactly: no
  abbreviations.
- A value flag takes `-name=value`, `--name=value` or the next token, even
  one starting with `-`. `-nvalue` is not a value.
- An inline value on a flag not under `values` is denied when the rule has
  a `flags` section (`--dry-run=false` turns a boolean flag off).
- A single-dash token of more than two characters that is not a value flag
  and not under an allow list may be a cluster of short flags: a `deny`
  list catches any of its letters, and a letter under `values` denies the
  token.
- A token starting with `---` is denied.
- `--` ends the flags; the rest is positional.

The parsers may disagree from the first positional argument on (Go's
`flag`, urfave/cli v1 and v2 stop reading flags there; v3 does not), and
from a single-dash token not followed by an ASCII letter (`-1`), a lone `-`,
or a token with whitespace around it (v3 trims it). From there:

- Every token is checked as a positional argument, and a token starting
  with `-` also as a flag (with its value, if it is a value flag).
- A value flag followed by a token starting with `-` is denied.
- `--` is checked as a positional argument; everything after it is
  positional in every reading.
- A token with whitespace around it is checked as given and trimmed.
- A path check cannot apply to a token read in more than one way (a flag,
  a flag's value, or a token with whitespace around it): the command is
  denied.

## Validation errors

- `flag_style` other than `getopt` or `go`:
  `flag_style must be getopt or go, not "<value>"`.
- `flag_style` together with `args`:
  `args cannot be combined with flags, positional or flag_style`.
- Under `go`, a name that is not one or two dashes followed by a name, or
  that contains `=`:
  `"<name>" is not a flag name (use -name or --name)`.
- Under `go`, one flag in both spellings in one list or under `values`:
  `--name and -name name the same flag`.
- Under `getopt`, a single-dash long name is still an error.

## Denial messages

As under getopt, naming the flag as written in argv; a token with
whitespace around it is quoted (`flag " --force" is not allowed`). New:

- `flag ---name has more than two dashes`;
- `flag -x in -yx is not allowed`, `flag -o in -yo takes a value`;
- `flag --repo is followed by "-x": Go flag parsers disagree on its value here`;
- `argument "<arg>" is read differently by Go flag parsers: a path check cannot apply to it`,
  and the same for `value "<value>" of flag <flag>`.

## README

Reference for `flag_style`, which programs it is for (Go `flag`,
urfave/cli; cobra/pflag stay on getopt), every rule above, and a `tix pr`
rule in the example block. The old note that urfave/cli programs need a
long-name allow list points to `flag_style: go` instead.

## Tests

`internal/rules`, at the existing seam (parse a rules file, check argv):

- load: both styles accepted, single-dash names under `go`, every
  validation error above, `getopt` unchanged by default;
- both spellings match one entry in `allow`, `deny` and `values`; no
  abbreviations;
- values inline and in the next token, a next token starting with `-`, a
  missing value;
- inline values on flags not under `values`, with and without a `flags`
  section;
- clusters: a denied letter, a value-flag letter, a token listed whole;
- `---name`, `--` before a positional argument;
- after a positional argument, `-1` and a lone `-`: both readings, value
  flag followed by `-…`, `--`;
- tokens with whitespace around them;
- path checks: allowed where every parser agrees, with the `PathArg`s
  returned; denied on every kind of ambiguous token.

## Out of scope

Grammars for other parser families; modelling urfave/cli versions one by
one; knowing which flags a program defines or which are boolean; changes
outside `internal/rules`; changing `getopt`.
