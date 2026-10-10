# hostrunner

Lets a devcontainer run chosen commands on the host.

## Language

**Rule**:
An entry of the rules file that says which arguments
one command may be given.

**Command**:
The argv prefix a rule is keyed by, e.g. `git push`.
_Avoid_: prefix

**Positional argument**:
An argument that is neither a flag nor a flag's value.

**Value flag**:
A flag a rule declares as taking a value.

**Flag style**:
The grammar a rule reads the program's argv
with: `getopt` (`-abc` is three short flags,
long flags take two dashes and may be
abbreviated) or `go` (one or two dashes name
the same flag, as Go's flag package and
urfave/cli read it).
_Avoid_: flag syntax, parser mode

**List**:
The allow or deny patterns a rule applies to positional
arguments or to the values of one value flag, and the path
check it may add.
_Avoid_: filter, matcher

**Pattern**:
One entry of a list: a glob or a regex.

**Glob**:
A pattern that must match the whole value; `*` is any
sequence of characters, `/` included.

**Regex**:
A pattern that is a regular expression, matched anywhere
in the value unless it anchors itself.

**Mirrored directory**:
The host directory that matches the container's current
directory inside the workspace.

**Fixed directory**:
A host directory outside the workspace that a rule names
for its command to run in.

**Working directory**:
The directory a command runs in: the rule's fixed directory
if it names one, the mirrored directory otherwise.
_Avoid_: cwd

**Workspace file**:
An existing regular file inside the workspace, named by a
path relative to the mirrored directory.

**Path check**:
The requirement a rule puts on positional arguments or on
the values of one value flag that each names a workspace
file. It is `open` (the program gets the file the check
opened) or `check` (the program gets the argument as
given).
_Avoid_: path matcher

**Dry run**:
A request from the container that goes through every check
a command would meet before it starts, and reports whether
it would run instead of running it.
_Avoid_: check, test run

**Rules test**:
Testing a command against a rules file alone, on the host,
without a daemon or a container.
_Avoid_: check, dry run

### Lifecycle

**Daemon**:
The host process that runs one devcontainer's commands and
lives as long as its armed container.

**Arm**:
The signal `hostrunner up` gives the daemon each time the
devcontainer tooling brings a container of its workspace up.
_Avoid_: rearm

**Armed container**:
The container the last arm was for: one started after the
arm or, when none starts, a container started before it
that is still running a grace after it.
_Avoid_: current container, the devcontainer

**Waiting**:
The daemon's state between an arm and finding its armed
container.

**Attached**:
The daemon's state while it knows its armed container.

**Grace**:
How long the armed container must be seen absent before
the daemon exits.

**Step aside**:
The daemon's answer to an arm it will not follow: it stops,
and `hostrunner up` starts a new daemon.
_Avoid_: restart

**Startup timeout**:
How long a waiting daemon waits for its armed container
before it exits.
