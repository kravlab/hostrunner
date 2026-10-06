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

**Workspace file**:
An existing regular file inside the workspace, named by a
path relative to the working directory.

**Path check**:
The requirement a rule puts on positional arguments or on
the values of one value flag that each names a workspace
file. It is `open` (the program gets the file the check
opened) or `check` (the program gets the argument as
given).
_Avoid_: path matcher
