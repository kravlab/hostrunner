#!/bin/sh
# Usage: skills.sh <dir>
#
# Runs on the host from initializeCommand (claude/devcontainer.json) and
# copies the host user's global Claude Code skills, the entries of
# ~/.claude/skills, to <dir>/claude/skills, which the image links
# /etc/claude-code/.claude/skills to
# (docs/specs/devcontainer-claude-skills-hooks.md).
#
# A host without skills is not an error: the copy is an empty directory.
# A skill that cannot be copied (a dangling symlink, an unreadable file)
# fails `devcontainer up` and keeps the old copy, rather than silently
# dropping a skill the user installed.
#
# From the devcontainer template (github.com/kravlab/devcontainer-template),
# which owns this file: `copier update` updates it and merges local edits.
# The specs named here are in its docs/specs/.
set -eu

dir=$1
src=$HOME/.claude/skills
dest=$dir/claude

mkdir -p "$dest"
tmp=$(mktemp -d "$dest/.skills.XXXXXX")
old=$tmp.old
# Stopped between the two renames below, the old copy goes back in place.
cleanup() {
	if [ -d "$old" ] && [ ! -e "$dest/skills" ]; then
		mv "$old" "$dest/skills"
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' INT TERM
for entry in "$src"/*; do
	# No source or an empty one leaves the pattern as written: no skills,
	# not an error. -L keeps a dangling symlink for cp to fail on.
	[ -e "$entry" ] || [ -L "$entry" ] || continue
	# synced is Claude Code's own copy of the account's skills; the
	# container syncs its own into its ~/.claude volume.
	case ${entry##*/} in
	synced) continue ;;
	esac
	# -L: the entries are usually symlinks out of ~/.claude, which would
	# dangle in the container; the copy holds what they point to.
	cp -RL "$entry" "$tmp/"
done
# mktemp creates the directory 0700 and a skill may be private on the
# host; the copy must be readable through rootless Podman's user namespace.
# u+w: cp keeps the mode of a read-only skill directory, which the next
# run could not remove.
chmod -R u+w,a+rX "$tmp"
# rename cannot replace a non-empty directory, so the old copy is moved
# aside first: skills is missing for a moment, and a skill removed on the
# host disappears.
if [ -d "$dest/skills" ]; then
	mv "$dest/skills" "$old"
fi
mv "$tmp" "$dest/skills"
rm -rf "$old"
