# Regex lists next to glob lists

A rule could not confine a path-like argument
([#18](https://github.com/kravlab/hostrunner/issues/18)): in a glob, `*` and
`?` match any character, `/` included, so an allow list for an API path also
admits a value that walks out of it (`..`, `%2e%2e`). A positional or
flag-value list can now be written as `allow_regex` or `deny_regex` instead
of `allow` or `deny`; globs keep their meaning.

## Considered Options

- **A glob dialect where `*` stops at `/`** (`**` crosses it, `\?` is a
  literal). It changes what existing rules files mean (`+*` has to match
  `+refs/heads/main`), and `*` still matches a `..` segment.
- **`allow` and `deny` on the same list.** The path is confined only by
  listing the substrings that escape it (`*..*`, `*%*`), and `?` still
  cannot be written as a literal.
- **A `path` matcher** that cleans the value and keeps it inside the
  workspace. It is a different problem: the check needs the working
  directory, which the rules do not see, and the program opens the path
  itself after the check. Left to a separate issue.

## Consequences

- A list is exactly one of `allow`, `deny`, `allow_regex`, `deny_regex`;
  any two together are an error.
- Patterns are Go regular expressions (RE2) and match as Go matches them:
  anywhere in the value. hostrunner adds no anchors, so an `allow_regex`
  pattern without `^…$` allows every value that contains a match.
- Regex lists exist where globs do, for positional arguments and flag
  values. Flag names stay literal: they decide how argv is split into flags,
  flag values and positional arguments.
- A regex sees the text of an argument only. It does not resolve symlinks,
  so it confines a file path lexically.
