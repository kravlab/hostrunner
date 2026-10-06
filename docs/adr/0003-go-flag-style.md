# Go flag style

Rules read argv like getopt, so a program built on Go's `flag` package or
urfave/cli, which takes `-asap` as the long flag `--asap`, could slip a flag
past a deny list or a value filter
([#26](https://github.com/kravlab/hostrunner/issues/26)). A rule can now say
`flag_style: go` (the default is `getopt`). The key names the whole grammar
the program's parser follows, not one feature of it, because a filter is
safe only if it reads every token as the program does.

`go` is one cautious grammar for Go's `flag` package and urfave/cli v1, v2
and v3, with or without `UseShortOptionHandling`. A token these parsers read
differently has to pass every reading.

## Considered Options

- **A toggle for single-dash long flags** (`single_dash_long: true`): it
  leaves clusters, abbreviations, inline values and where flags end
  undecided. Rejected in favour of a grammar name.
- **One grammar per parser** (`go-flag`, `urfave-v3`): fewer false denials,
  but a wrong pick opens a hole (a `go-flag` rule on a v3 program misses a
  flag after a positional argument), and v3's parser changes between minor
  versions (3.10.1 drops every token after a lone `-`, 3.14.0 does not).
  Rejected.
- **Deny every token the parsers disagree on**: simpler, but it denies
  flags after positional arguments, which tix (urfave/cli v3) users write
  routinely (`tix pr view 5 --comments`). Rejected for checking both
  readings.
- **The key under `flags`**: a rule with only `positional` could not set
  it, though the grammar decides which tokens are positional. The key sits
  on the rule.

## Consequences

- `flag_style` together with `args: any` or `args: none` is an invalid
  rules file.
- A flag name may be written with one or two dashes; both spellings name
  one flag and match both in argv. Listing one flag twice is an invalid
  rules file.
- Flag names match exactly; nothing is read as an abbreviation, since none
  of these parsers accepts one.
- A value flag takes `-name=value`, `--name=value` or the next token, even
  one starting with `-`. `-nvalue` is not a value.
- A flag not under `values` given an inline value (`--dry-run=false`) is
  denied whenever the rule has a `flags` section: a boolean flag accepts it
  and can be turned off that way. A rule that wants it lists the flag under
  `values`.
- A single-dash token of more than two characters that the rule does not
  list whole is also read as a cluster of short flags, as
  `UseShortOptionHandling` reads it: a deny list catches any of its
  letters, and a letter that is a value flag denies the token, since the
  parsers read a value inside a cluster differently.
- After the first positional argument, after a single-dash token not
  followed by a letter (`-1`), after a lone `-` and for a token with
  leading or trailing whitespace, the parsers disagree on what a token
  is. Such a token must pass both the flag check and the positional
  check, and a path check cannot apply to it. A value flag there may not
  be followed by a token starting with `-`: urfave/cli v1 leaf commands
  move a known flag in front together with the next token only if that
  token is not a flag. Which value v1 then reads was not verified, so the
  token is denied.
- A token starting with three dashes (`---asap`) is denied: no parser
  reads it as a flag with an ordinary name.
- Programs built on cobra/pflag read `-abc` as short flags and long flags
  with two dashes only; they stay on `getopt`.
