# Spec: name the default rules file hostrunner-rules.yaml

Vocabulary in [CONTEXT.md](../../CONTEXT.md).

## Goal

The default rules file was `.devcontainer/hostrun.yaml`. In a directory
shared with other tools the name does not say what the file holds, and it
sits next to `hostrunner.yaml`, the workspace settings file. The default
becomes `.devcontainer/hostrunner-rules.yaml`. `.devcontainer/hostrun.yaml`
keeps working as a fallback, so an existing workspace needs no change.

## Behavior

### Choosing the file

The choice applies to the default alone: a `rules_file` from a flag, a
variable or a settings file is used as given, whatever lies in
`.devcontainer`, with no fallback and no warning.

| `hostrunner-rules.yaml` | `hostrun.yaml` | Used | Warning |
|---|---|---|---|
| present | absent | `hostrunner-rules.yaml` | no |
| absent | present | `hostrun.yaml` | no |
| present | present | `hostrunner-rules.yaml` | yes |
| absent | absent | `hostrunner-rules.yaml` | no |

- Present means the directory entry exists. A dangling
  `hostrunner-rules.yaml` link is present: it is chosen and reads as a
  missing file, so every command is denied rather than falling back.
- An error other than "does not exist" while looking at
  `hostrunner-rules.yaml` keeps it chosen, for the same reason.
- `hostrun.yml` was never read and is not read now.
- With neither file the daemon denies every command, as with a missing
  file today, and a human adding a rule creates `hostrunner-rules.yaml`.
- `hostrunner test` still takes a missing file for an error, naming
  `hostrunner-rules.yaml`.

### The warning

With both files, `up`, `restart` and `test` warn on stderr, naming the file
left unread and the file in use, and go on:

```
hostrunner: warning: /home/u/app/.devcontainer/hostrun.yaml is ignored: /home/u/app/.devcontainer/hostrunner-rules.yaml is used; remove it
```

- `restart --all` warns once per workspace it starts a new daemon for and
  that has both files. A daemon whose container stopped is only stopped,
  so it gets no warning.
- A daemon `up` starts gets the chosen path as a flag, so it does not warn
  again. Working through the fallback is silent.

### Running daemons

A daemon keeps the path it started with: the reread follows that path, and
the choice is not made again until `up` or `restart`.

- Renaming `hostrun.yaml` under a running daemon removes its rules file.
  With `reread` on, the daemon sees it go and denies every command until
  `hostrunner restart` or the next `up`. With `reread` off it does not look
  at the file, and keeps serving the rules it loaded until it is replaced
  the same way.
- Adding `hostrunner-rules.yaml` beside `hostrun.yaml` changes nothing for
  a running daemon until then.
- Either way the new daemon has another `rules_file`, so it replaces the old
  one and that daemon's approvals end, as with any change of `rules_file`.

### The container side

`hostrun`'s warning about writable rules checks the `.devcontainer`
directory and the file the same choice picks there.

## Implementation notes

- A small package without dependencies, `internal/rulesfile`, holds the two
  names and the choice: `internal/settings` needs it for the default, and
  `internal/client`, built into the container's `hostrun`, for the warning.
  `internal/client` imports neither `internal/settings` nor `internal/rules`.
- The choice is made where the default is set. When the default stays in
  use and both files exist, the resolved settings name the ignored file;
  `up`, `restart` and `test` print the warning from them. The name is not a
  setting: it has no key and is not compared on arm.
- Rule loading, the reread, approvals and the arm comparison do not change.

## Out of scope

- This repository's own `.devcontainer/hostrun.yaml`, and the devcontainer
  template that provides it: the repository stays on the fallback and moves
  with the template.

## Documentation

- README: the default name, the order of the names, the warning, and the
  migration: rename the file, then run `hostrunner restart`.
- The instructions block (`internal/instructions/instructions.md` and
  `AGENTS.md`), which names the new file and the older one.
- `docs/specs/rules-engine.md` and `docs/specs/dry-run.md`: the default
  location. The example moves to
  `examples/devcontainer/.devcontainer/hostrunner-rules.yaml`.
