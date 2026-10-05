# Spec: Regex lists

Ticket: [Spec: regex lists for positional arguments and flag values](https://github.com/kravlab/hostrunner/issues/22)
(from [#18](https://github.com/kravlab/hostrunner/issues/18)).
Decision record: [ADR 0001](../adr/0001-regex-lists-next-to-globs.md).
Extends [rules-engine.md](rules-engine.md); vocabulary in
[CONTEXT.md](../../CONTEXT.md).

## Goal

A rule can confine a path-like argument. With globs it cannot: `*` and `?`
match any character, `/` included, so `allow: ["/repos/x/y/*"]` also
admits `/repos/x/y/../../../user`, `/repos/x/y/issues/%2e%2e/%2e%2e/keys`
and `/repos/x/y/keys`, and a query string cannot be pinned because `?` is
always a wildcard.

## Schema

A list, under `positional` and under each value flag in `flags.values`,
is written with exactly one of four keys:

```yaml
rules:
  - command: tix api --repo example/example-app
    flags:
      allow: []
    positional:
      allow_regex:
        - '^/repos/example/example-app/issues(/[0-9]+)?(\?state=(open|closed))?$'
  - command: git push
    flags:
      values:
        -o: { deny_regex: ['^ci\.'] }
```

- `allow`, `deny`: globs, unchanged.
- `allow_regex`, `deny_regex`: regexes.
- The `flags` section itself has no regex keys: flag names stay literal,
  because they decide how argv is split into flags, flag values and
  positional arguments.

## Matching

- A regex is a Go regular expression (`regexp`, RE2 syntax), compiled when
  the rules file is loaded.
- It is used as written. hostrunner adds no anchors and no flags, so a
  pattern matches anywhere in the value; a pattern that must cover the
  whole value says so with `^…$`. An `allow_regex` pattern without anchors
  allows every value that contains a match.
- `^` and `$` are the ends of the value, not of a line in it.
- A value matches a list when any of its patterns matches. Polarity is as
  for globs: under `allow_regex` every positional argument (every value of
  the flag) must match, under `deny_regex` none may.
- The list of a value flag applies to every spelling of the value
  (`--flag=value`, `--flag value`, `-ovalue`, `-o value`) and to every
  occurrence of a repeated flag.
- A regex, like a glob, sees only the text of an argument. It does not
  resolve symlinks, so it confines a file path lexically.

## Validation errors

- Any two of the four keys on one list:
  `use only one of allow, allow_regex` (the keys that were set).
- A regex that does not compile; the error names the rule, the section,
  the key and the whole pattern, e.g.
  ``rule 2: "tix api" positional: allow_regex: `^/issues/[0-9]++$`: error parsing regexp: …``.
  The pattern is shown as written in the rules file, not escaped.
- `allow_regex` or `deny_regex` directly under `flags`: an unknown key,
  reported with its line.
- An empty `positional` section, or one whose only key is null:
  `set allow, deny, allow_regex or deny_regex`.

A null regex key counts as not set and an explicit empty one as set
(`allow_regex: []` allows nothing), as for `allow` and `deny`. A value
flag declared with `{}` still accepts any value.

## Denial messages

Unchanged: `argument "<arg>" is not allowed` / `is denied`,
`value "<value>" of flag <flag> is not allowed` / `is denied`. They do
not say whether the list held globs or regexes.

## README

- Reference for `allow_regex` / `deny_regex`: one key per list, Go syntax,
  no anchors added, single quotes in YAML (in double quotes `\` is YAML's
  own escape character), text of the argument only.
- A complete `tix api` rule in the example block.
- Two notes on behaviour that does not change:
  - a `flags` section with `values` only does not restrict the other
    flags; `allow: []` makes it strict;
  - a single-dash token of several characters is read as short flags
    (`-asap` is `-a -s -a -p`, or `-a` with the value `sap` when `-a` is a
    value flag), while programs built on urfave/cli take it as `--asap`;
    such a program needs a `flags.allow` list, and `values`, with long
    names only.

## Tests

`internal/rules`, at the existing seam (parse a rules file, check argv):

- an anchored `allow_regex` denies the three bypasses of #18 and allows
  the intended paths; `\?` is a literal `?`;
- `deny_regex` denies a match anywhere in the value;
- an unanchored `allow_regex` pattern allows a value that contains a match;
- a value with a newline does not satisfy a `^…$` pattern;
- any pattern of a list may match;
- the list of a value flag: inline and separate values, short and long
  flags, a repeated flag;
- load errors: every pair of the four keys, a regex that does not compile
  (message), a regex key under `flags`, a null regex key;
- `allow_regex: []` allows nothing.

The README is documentation and has no tests.

## Out of scope

A `path` matcher that keeps a value inside the workspace
([#25](https://github.com/kravlab/hostrunner/issues/25)); a per-rule
switch for single-dash long flags
([#26](https://github.com/kravlab/hostrunner/issues/26)); a new glob
dialect; `allow` and `deny` together on one list; implicit anchoring;
regexes for flag names; making a `flags` section with `values` only
strict; changes to the example rules file.
