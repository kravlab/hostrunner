# Workspace settings override the host's

A daemon's settings come from flags, `HOSTRUNNER_*` variables, the
workspace settings file (`.devcontainer/hostrunner.yaml` unless
`--workspace-settings` or `HOSTRUNNER_WORKSPACE_SETTINGS` names another)
and the host settings file (`$XDG_CONFIG_HOME/hostrunner/settings.yaml`
unless `--settings` or `HOSTRUNNER_SETTINGS` names another; its section
for the workspace, then its common part), in that order. The workspace
settings file may set every setting a daemon applies, the rules file and
the approval timeout included, and wins over the host settings file.

## Considered Options

- **Only harmless keys in the workspace file** (timeouts, reread), paths
  and the approval timeout on the host alone.
- **The host file winning over the workspace file**: a workspace could
  not change a value the host file sets for all of them.

## Consequences

- hostrunner does not protect the workspace settings file or the rules
  file: whether the container can write them depends only on the mounts
  in `devcontainer.json`. A container that can write the workspace
  settings file chooses the daemon's rules file and timeouts.
- `runtime_root` and `stop_timeout` are refused in the workspace file and
  in workspace sections: only `stop`, `restart` and `approve` read them,
  before they know a workspace.
