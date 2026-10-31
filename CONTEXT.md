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

**Secret**:
A value a rule obtains on the host for each request and adds
to its command; the container never sees it.
_Avoid_: credential, token

**Secret source**:
Where a secret's value comes from: the output of a host
command, a host environment variable, or a host file.
_Avoid_: provider

**Injection**:
What a rule adds to its command after the checks: arguments,
environment variables or files, with or without secrets.
_Avoid_: rewrite, wrapper

### Lifecycle

**Daemon**:
The host process that runs one devcontainer's commands and
lives as long as its armed container.

**Arm**:
The signal `hostrunner up` gives the daemon each time the
devcontainer tooling brings a container of its workspace up,
carrying the digest of the rules and the daemon settings it
resolved.
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
_Avoid_: restart (a human's decision, not the daemon's)

**Stop**:
A human on the host ending a daemon.
_Avoid_: kill, shutdown

**Restart**:
A human on the host replacing a daemon with a new one for
the same runtime directory.
_Avoid_: step aside (the daemon's own decision), reload

**Reread**:
The daemon replacing the rules it serves with the rules
file's current content, without stopping.
_Avoid_: reload, restart (a human replacing the daemon)

**Drain**:
The state of a daemon that a stop or restart waits for: it
takes no new requests and finishes the running ones.

**Startup timeout**:
How long a waiting daemon waits for its armed container
before it exits.

**Ready timeout**:
How long `hostrunner up` waits for the daemon it starts to
listen; it tunes `up`, not the daemon.

**Stop timeout**:
How long a stop or restart waits for a daemon it stopped,
past any drain, to close its sockets; it tunes the stop, not
the daemon.

**Runtime directory**:
The host directory, one per devcontainer, that holds a
daemon's socket, the hostrun client and the daemon log, and
that the container mounts read-only.

**Runtime root**:
The host directory the runtime directories lie in, where a
stop, a restart and an approval session look for daemons.

**Layout root**:
The directory an install keeps its versions in,
`<prefix>/lib/hostrunner`.

**Version directory**:
A directory in the layout root named after the version of
the `hostrunner` and `hostrun` it holds.

**Current link**:
The symlink `current` in the layout root to the version
directory in use; `hostrunner update` switches it with one
rename.
_Avoid_: current (in code and specs that is the running version)

### Settings

**Settings**:
What tunes a daemon besides its rules (its timeouts, whether it
rereads, where its rules file is), and the commands that start,
stop or approve it (how long they wait, where they look for
daemons).
_Avoid_: config (`--config` names the rules file)

**Host settings file**:
The settings file a human keeps on the host, outside every
workspace, with sections that apply to single workspaces.

**Workspace settings file**:
The settings file of one workspace, kept in the workspace unless
its path is given; what it sets overrides the host settings file.

### Approvals

**Approval request**:
A command the rules do not let through, sent with `--ask` and
waiting for a human's answer.
_Avoid_: prompt, confirmation

**Ask timeout**:
How long an approval request waits for a human's answer when
hostrun sets no limit; the host chooses it, and it may exceed
the longest limit hostrun may set.
_Avoid_: approval timeout

**Approval session**:
The host terminal process, `hostrunner approve`, in which a
human sees and answers one daemon's approval requests.
_Avoid_: approver, approval UI

**Serve**:
What an approval session does for the one daemon whose
requests it shows; approvals are on while one does.
_Avoid_: attach (the daemon's state towards its armed container)

**Take over**:
An approval session starting to serve a daemon another session
served, which ends the other.

**Approvable**:
Said of a refusal by the rules that a human may answer with an
approval.

**Guarded refusal**:
An approvable refusal that needs a warning and a second
confirmation: a deny pattern, a failed path check, an argv the
rules cannot read, or a rule with secrets.

**Explicitly denied**:
Refused by a deny pattern; one kind of guarded refusal.

**Standing approval**:
An approval of one argv from one container directory that lets
it through until its daemon ends or the human revokes it.
_Avoid_: lifetime approval, grant

**Session approval**:
An approval of one argv from one container directory that lets
it through until the approval session that granted it stops
serving its daemon, or the human revokes it.
_Avoid_: temporary approval

**Approval socket**:
The daemon's socket for approval sessions, beside its runtime
directory and outside the container's reach.
