#!/bin/sh
# Usage: git-excludes.sh <dir>
#
# Runs on the host from initializeCommand (both devcontainer.json files) and
# copies the host user's global git excludes file to <dir>/gitignore, which
# the containers mount read-only at /run/host-config. The image links git's
# and ripgrep's default excludes path, ~/.config/git/ignore, to it
# (docs/specs/devcontainer-git-excludes.md).
#
# The path comes from the global config only, as for the identity in
# git-identity.sh: --includes and running in the workspace, one level above
# this script, keep a path set through includeIf "gitdir:...", and
# --type=path expands "~/". Not set, it is git's default. A relative path is
# resolved from the workspace, as host git run at the repository's root
# resolves it. A missing file or an empty value is not an error, as for
# host git: the copy is empty. Any other failure (a malformed config, a
# directory or unreadable file) fails `devcontainer up`.
#
# From the devcontainer template (github.com/kravlab/devcontainer-template),
# which owns this file: `copier update` updates it and merges local edits.
# The specs named here are in its docs/specs/.
set -eu

dir=$1
workspace=$(dirname "$0")/..

status=0
path=$(git -C "$workspace" config --global --includes --type=path --get core.excludesFile) || status=$?
case $status in
0) ;;
1) path=${XDG_CONFIG_HOME:-$HOME/.config}/git/ignore ;;
*) exit "$status" ;;
esac
case $path in
'' | /*) ;;
*) path=$workspace/$path ;;
esac

mkdir -p "$dir"
# Written to a temporary file and renamed over the old one, so a reader
# never sees a half-written file and patterns removed on the host
# disappear from it.
tmp=$(mktemp "$dir/.gitignore.XXXXXX")
trap 'rm -f "$tmp"' EXIT
trap 'exit 1' INT TERM
if [ -n "$path" ] && [ -e "$path" ]; then
	cp "$path" "$tmp"
fi
# mktemp creates the file 0600; patterns are not secret and must be
# readable through rootless Podman's user namespace.
chmod 0644 "$tmp"
mv "$tmp" "$dir/gitignore"
