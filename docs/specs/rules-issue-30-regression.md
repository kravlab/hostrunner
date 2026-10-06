# Spec: regression tests for #30

Ticket: [Rules read a whitespace-led argument and `--x` differently from
urfave/cli programs](https://github.com/kravlab/hostrunner/issues/30).
Covered by [rules-flag-style.md](rules-flag-style.md) and
[ADR 0003](../adr/0003-go-flag-style.md); vocabulary in
[CONTEXT.md](../../CONTEXT.md).

## Goal

Under `flag_style: go` the rules engine already denies both spellings #30
reports. No test replays the issue's own commands, and the path it found
first (a positional argument, then a whitespace-led value flag whose value
is the next token) is checked by none. These tests pin the issue's
scenarios so the bypass cannot come back.

## Scope

- Tests only, in `internal/rules/flagstyle_test.go`. No change to the
  rules engine, the README, `CONTEXT.md` or an ADR.
- Under `getopt` (the default) both spellings stay allowed, as ADR 0003
  decides: a rule for a urfave/cli program sets `flag_style: go`. No test
  pins the `getopt` reading.

## Rules

The rules of the issue, with `flag_style: go` added:

```yaml
rules:
  - command: tix issues edit
    flag_style: go
    flags:
      allow: []
      values:
        --title: {}
        --description-file: { allow: ["-"] }
  - command: tix issues list
    flag_style: go
    flags:
      allow: []
  - command: tix issues create
    flag_style: go
    flags:
      allow: []
      values:
        --labels: {}
```

## Cases

Each argv is given as a token list, so whitespace stays inside a token.
"Denied" names the exact reason after `denied by rule "<command>": `.

### 1. Whitespace-led argument (`tix issues edit`, `tix issues list`)

| argv after the command | result |
| --- | --- |
| `999999999`, `" --description-file"`, `/etc/hostname` | denied: `value "/etc/hostname" of flag " --description-file" is not allowed` |
| `999999999`, `"\t--description-file"`, `/etc/hostname` | denied: `value "/etc/hostname" of flag "\t--description-file" is not allowed` |
| `" --description-file"`, `/etc/hostname` | denied: `value "/etc/hostname" of flag " --description-file" is not allowed` |
| `999999999`, `--description-file`, `/etc/hostname` | denied: `value "/etc/hostname" of flag --description-file is not allowed` |
| `--description-file`, `-`, `999999999` | allowed (control: the value filter lets `-` through) |
| `999999999`, `--description-file`, `-` | denied: `flag --description-file is followed by "-": Go flag parsers disagree on its value here` (ADR 0003: after a positional argument a value flag may not be followed by a token starting with `-`) |
| list: `" --login"`, `somename` | denied: `flag " --login" is not allowed` |
| list: `--login`, `somename` | denied: `flag --login is not allowed` |

### 2. `--x` read as `-x` (`tix issues create`)

| argv after the command | result |
| --- | --- |
| `--l=name` | denied: `flag --l is not allowed` (not an abbreviation of `--labels`) |
| `--l`, `name` | denied: `flag --l is not allowed` |
| `-l`, `name` | denied: `flag -l is not allowed` |
| `--labels=bug` | allowed (control) |
| `--labels`, `bug` | allowed (control) |

## Test shape

- One test function per section, table-driven like
  `TestGoFlagStyleChecksTokensWithWhitespaceBothWays`, using `mustParse`
  and `expectCheck`. Each function's comment names #30 and the parser
  behaviour it guards against: urfave/cli v3 trims whitespace around a
  token; urfave/cli reads `--x` as `-x` and accepts no abbreviation.
- Test names state the behaviour, e.g.
  `TestGoFlagStyleDeniesWhitespaceLedFlagsOfIssue30`.

## TDD note

The behaviour exists since #34, so the new cases are expected to pass on
first run: they are regression tests, not a red step. A case that fails
means the engine diverges from `rules-flag-style.md`; it is then
diagnosed as a bug (root cause first), not adjusted to pass.

## Done when

- All cases above pass under `go test ./internal/rules/`.
- `mise run fmt` and `mise run lint` report nothing.
- PR body says `Refs #30` (not `Closes`): #30 is closed by hand after
  `v0.2.0` ships.
