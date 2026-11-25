---
status: accepted
---

# Mount requirements, checked on the live mount table

A read-only bind mount vanishes from the container when a host
command replaces the file (`config.lock` + rename), while
`<engine> inspect` still lists it read-only, so a check made once on
attach through `inspect` can pass while the container writes the
file. The mount check now also runs before each request (when it
arrives and again just before the process starts), reading the
mount table of each armed container's init process
(`/proc/<pid>/mountinfo`) together with `inspect`: `inspect` says
which host source is mounted where, the mount table says which of
those mounts are still there and read-only. A rule may name mount
requirements, paths from the workspace root that must be a mount
point of their own (`mounted`) or not writable (`read-only`), such
as `.git`, `.git/config` and `.git/hooks` for git; they are checked
for every request whose command starts with that rule's command, so a
longer rule inherits them. A failure refuses the request (126), and no
approval lets it through.

## Considered Options

- **Warn, as the mount-check profiles of the MVP plan did**:
  rejected; a warning does not stop the command a hostile container
  has just prepared.
- **A separate file of profiles, updatable without a new build**:
  rejected; it would need its own protection and allow. Mount
  requirements live in the rules file, under its digest, allow and
  reread.
- **The mount table alone**: rejected; it holds no host source, so a
  second read-write mount of the same directory elsewhere would go
  unseen.
- **The mount table of the requesting process, or of every process
  of the container**: rejected; the init process holds the mounts
  the devcontainer made, and the others add cost without closing the
  gap below.
- **Fall back to `inspect` when the mount table cannot be read**
  (an engine in a VM): rejected; the protection would weaken
  silently. Every command is refused, as when the mounts cannot be
  read today; a human on the host may turn the mount check off.

## Consequences

- hostrunner still does not touch a tool's config or hooks: a mount
  requirement says the devcontainer's protection is in place, not
  that the tool is safe. A nested repository the container creates
  (`git init sub`, then `hostrun git …` from `sub/`) is not covered.
- A container that can unmount its own mounts in a mount namespace
  of its own (`CAP_SYS_ADMIN`, privileged) is beyond what the check
  sees.
- A container made before its devcontainer added the mounts is
  refused until it is made again.
- A request that passed the check just before the host rewrites a
  file can read what the container writes in that moment; rules
  that let the container trigger such rewrites (`git push -u`) are
  dropped from the examples.
- `mount_check: false` turns mount requirements off as well.
