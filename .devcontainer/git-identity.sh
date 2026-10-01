#!/bin/sh
# Usage: git-identity.sh <dir>
#
# Runs on the host from initializeCommand (both devcontainer.json files) and
# writes the host user's git identity, and nothing else of the host's git
# config, to <dir>/gitconfig, which the containers mount read-only at
# /run/host-config and include from the image's system git config
# (docs/specs/devcontainer-host-config.md).
#
# The identity comes from the global config only: in the Claude container
# the agent can redirect host git to a repository config it writes, so it
# must not choose the author. --includes (off by default with --global) and
# running in the workspace, one level above this script, keep identities set
# through includeIf "gitdir:...". A missing value is left out rather than
# failing `devcontainer up`: the container's git then asks for it on commit.
# Any other git failure (e.g. a malformed config) fails it.
set -eu

dir=$1
workspace=$(dirname "$0")/..

mkdir -p "$dir"
# Written to a temporary file and renamed over the old one, so a reader
# never sees a half-written file and a value removed on the host disappears.
tmp=$(mktemp "$dir/.gitconfig.XXXXXX")
trap 'rm -f "$tmp"' EXIT
trap 'exit 1' INT TERM
for key in user.name user.email; do
	# git config exits 1 when the key is not set; git escapes the value.
	status=0
	value=$(git -C "$workspace" config --global --includes --get "$key") || status=$?
	case $status in
	0) git config --file "$tmp" "$key" "$value" ;;
	1) ;;
	*) exit "$status" ;;
	esac
done
# mktemp creates the file 0600; the identity is in every commit, not secret,
# and must be readable through rootless Podman's user namespace.
chmod 0644 "$tmp"
mv "$tmp" "$dir/gitconfig"
