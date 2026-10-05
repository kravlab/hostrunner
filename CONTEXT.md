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

**List**:
The allow or deny patterns a rule applies to positional
arguments or to the values of one value flag.
_Avoid_: filter, matcher

**Pattern**:
One entry of a list: a glob or a regex.

**Glob**:
A pattern that must match the whole value; `*` is any
sequence of characters, `/` included.

**Regex**:
A pattern that is a regular expression, matched anywhere
in the value unless it anchors itself.
