#!/bin/sh
# Usage: git-identity.sh <dir>
#
# Runs on the host from initializeCommand (devcontainer.json) and writes the
# host user's git identity, and nothing else of the host's git config, to
# <dir>/gitconfig, which the Claude devcontainer mounts read-only and
# includes from its system git config (docs/specs/devcontainer-host-config.md).
#
# The identity comes from the global config only: the container can redirect
# host git to a repository config it writes, so it must not choose the
# author. --includes (off by default with --global) and running in the
# workspace, two levels above this script, keep identities set through
# includeIf "gitdir:...". A missing value fails `devcontainer up`.
set -eu

dir=$1
workspace=$(dirname "$0")/../..

# Prints the global value of git config key $1, or fails with a message.
get() {
	git -C "$workspace" config --global --includes --get "$1" || {
		echo "git-identity: $1 is not set in the host's global git config" >&2
		exit 1
	}
}

name=$(get user.name)
email=$(get user.email)

# Written to a temporary file and renamed over the old one, so a reader never
# sees a half-written file; git config escapes the values.
mkdir -p "$dir"
tmp=$(mktemp "$dir/.gitconfig.XXXXXX")
trap 'rm -f "$tmp"' EXIT
git config --file "$tmp" user.name "$name"
git config --file "$tmp" user.email "$email"
# mktemp creates the file 0600; the identity is in every commit, not secret,
# and must be readable through rootless Podman's user namespace.
chmod 0644 "$tmp"
mv "$tmp" "$dir/gitconfig"
