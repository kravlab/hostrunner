#!/bin/sh
# Usage: agents-md.sh <dir> <agents-md>
#
# Runs on the host from initializeCommand (claude/devcontainer.json) and
# makes <dir>/CLAUDE.md, the source of the read-only mount at the
# container's ~/.claude/CLAUDE.md, a symlink to the host user's global
# instructions (docs/specs/devcontainer-host-config.md). <agents-md> is
# $HOSTRUNNER_AGENTS_MD on the host, empty when it is not set.
#
# The symlink always exists, so the mount works on any host: unset, it
# points to an empty file. Docker and Podman resolve it when the container
# starts and mount the original itself, so in-place edits on the host are
# seen in the running container; nothing is copied.
set -eu

dir=$1
agents_md=$2

mkdir -p "$dir"
if [ -z "$agents_md" ]; then
	: >"$dir/empty.md"
	target=$dir/empty.md
else
	# Set on purpose: a typo must fail `devcontainer up`, not silently
	# drop the instructions. Absolute only: initializeCommand's working
	# directory is not part of its contract. -f follows symlinks.
	valid=false
	case $agents_md in
	/*) [ -f "$agents_md" ] && valid=true ;;
	esac
	if [ "$valid" = false ]; then
		echo "agents-md: HOSTRUNNER_AGENTS_MD ($agents_md) is not an absolute path to a file" >&2
		exit 1
	fi
	target=$agents_md
fi
ln -sf "$target" "$dir/CLAUDE.md"
