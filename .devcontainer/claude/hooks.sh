#!/bin/sh
# Usage: hooks.sh <dir>
#
# Runs on the host from initializeCommand (claude/devcontainer.json) and
# writes the host user's global Claude Code hooks, the hooks key of
# ~/.claude/settings.json and nothing else of it, to <dir>/claude/hooks.json,
# which the image links /etc/claude-code/managed-settings.d/host-hooks.json
# to (docs/specs/devcontainer-claude-skills-hooks.md).
#
# A host without settings.json or without hooks is not an error: the file
# is {}. jq is needed only when settings.json exists; settings that are
# not one JSON object (an empty file and a dangling symlink included), or
# a missing jq, fail `devcontainer up` and keep the old file, rather than
# silently dropping the hooks.
#
# From the devcontainer template (github.com/kravlab/devcontainer-template),
# which owns this file: `copier update` updates it and merges local edits.
# The specs named here are in its docs/specs/.
set -eu

dir=$1
src=$HOME/.claude/settings.json
dest=$dir/claude

mkdir -p "$dest"
# Written to a temporary file and renamed over the old one, so a reader
# never sees a half-written file and hooks removed on the host disappear.
tmp=$(mktemp "$dest/.hooks.json.XXXXXX")
trap 'rm -f "$tmp"' EXIT
trap 'exit 1' INT TERM
# -L: a dangling symlink is settings jq cannot read, not a host without
# settings.
if [ -e "$src" ] || [ -L "$src" ]; then
	# -s counts the values: without it jq prints nothing for an empty
	# file and one object per value for several, and hooks.json would not
	# be valid JSON. The type is checked because has accepts null.
	jq -s '
		if length == 1 then .[0] else error("expected one JSON value, found \(length)") end
		| if type == "object" then . else error("expected a JSON object, found \(type)") end
		| if has("hooks") then {hooks} else {} end
	' "$src" >"$tmp"
else
	echo '{}' >"$tmp"
fi
# mktemp creates the file 0600; it must be readable through rootless
# Podman's user namespace.
chmod 0644 "$tmp"
mv "$tmp" "$dest/hooks.json"
