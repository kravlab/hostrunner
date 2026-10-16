#!/usr/bin/env bash
# Installs hostrunner into the versioned layout `hostrunner update`
# needs (docs/specs/update.md), from a GitHub release or from local
# binaries (`mise run install`). Spec: docs/specs/install-script.md.
#
#   curl -fsSL https://raw.githubusercontent.com/kravlab/hostrunner/main/scripts/install.sh | bash
#
# Usage: install.sh [-v vX.Y.Z] [--from <dir>]
set -euo pipefail
# Command substitutions keep -e: a failing step inside copy_binaries or
# latest_tag ends the substitution with its status, and the assignment
# then fails the script, instead of a later command masking the failure.
shopt -s inherit_errexit

readonly USAGE='usage: install.sh [-v vX.Y.Z] [--from <dir>]
  -v vX.Y.Z     release to install (default: the latest)
  --from <dir>  install <dir>/hostrunner and <dir>/hostrun instead of a release
environment: PREFIX (default ~/.local): installs into $PREFIX/lib/hostrunner
and links $PREFIX/bin/hostrunner and $PREFIX/bin/hostrun'

readonly REPO=kravlab/hostrunner
readonly API_URL="https://api.github.com/repos/$REPO/releases/latest"
readonly DOWNLOAD_URL="https://github.com/$REPO/releases/download"

# Release tags: semver core with an optional pre-release, no build
# metadata (as scripts/release.sh creates them).
readonly PRE_ID='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
readonly TAG_RE="^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-$PRE_ID(\.$PRE_ID)*)?\$"

# Version directories: dev or anything golang.org/x/mod/semver accepts
# (shorthands and build metadata included), as `hostrunner update` reads
# them.
readonly NUM='(0|[1-9][0-9]*)'
readonly BUILD_ID='[0-9A-Za-z-]+'
readonly DIR_VERSION_RE="^v$NUM(\.$NUM(\.$NUM(-$PRE_ID(\.$PRE_ID)*)?(\+$BUILD_ID(\.$BUILD_ID)*)?)?)?\$"

# WORK is the temporary download directory, removed on exit.
WORK=

# die prints its arguments as an error and exits 1.
die() {
  echo "install.sh: $*" >&2
  exit 1
}

# usage_error prints the usage on stderr and exits 2.
usage_error() {
  echo "$USAGE" >&2
  exit 2
}

# need fails naming the first of its arguments not found in PATH.
need() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null || die "$tool is required but not installed"
  done
}

# release_arch prints the release architecture of this host, refusing a
# host without release builds.
release_arch() {
  local os machine
  os="$(uname -s)" machine="$(uname -m)"
  os="${os,,}"
  case "$os/$machine" in
    linux/x86_64) echo amd64 ;;
    linux/aarch64 | linux/arm64) echo arm64 ;;
    *) die "no release builds for $os/$machine" ;;
  esac
}

# fetch downloads URL $1 into file $2, or prints it without $2. Bounded,
# so a stalled connection fails instead of hanging.
fetch() {
  if [ $# -eq 2 ]; then
    curl -fsSL --max-time 300 -o "$2" "$1" || die "could not download $1"
  else
    curl -fsSL --max-time 60 "$1" || die "could not download $1"
  fi
}

# latest_tag prints the tag of the latest release (GitHub excludes
# drafts and pre-releases there).
latest_tag() {
  local json tag
  json="$(fetch "$API_URL")"
  tag="$(grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' <<<"$json" | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')" || true
  [[ "$tag" =~ $TAG_RE ]] || die "latest release \"$tag\" is not a version"
  echo "$tag"
}

# download_release downloads release $1 for architecture $2 into $WORK,
# checks it against checksums.txt and extracts only the two binaries
# into $WORK/bin.
download_release() {
  local tag="$1" arch="$2" archive sum name
  archive="hostrunner_${tag#v}_linux_${arch}.tar.gz"
  fetch "$DOWNLOAD_URL/$tag/$archive" "$WORK/$archive"
  fetch "$DOWNLOAD_URL/$tag/checksums.txt" "$WORK/checksums.txt"
  sum="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "$WORK/checksums.txt")"
  [ -n "$sum" ] || die "$archive is not listed in checksums.txt"
  [ "$(sha256sum "$WORK/$archive" | cut -d ' ' -f 1)" = "$sum" ] \
    || die "$archive does not match its checksum in checksums.txt"
  mkdir "$WORK/bin"
  for name in hostrunner hostrun; do
    tar -tzf "$WORK/$archive" "$name" >/dev/null 2>&1 || die "release archive has no $name"
  done
  tar -xzf "$WORK/$archive" -C "$WORK/bin" hostrunner hostrun || die "could not extract $archive"
}

# is_dir_version reports whether name $1 is a version directory name;
# none of them is empty or holds a slash, so it is safe in a path under
# the root.
is_dir_version() {
  [ "$1" = dev ] || [[ "$1" =~ $DIR_VERSION_RE ]]
}

# relink points symlink $2 to $1 by renaming a new symlink over it, so
# $2 never goes missing.
relink() {
  local link
  link="$(mktemp -u "$(dirname "$2")/.tmp.$(basename "$2")-XXXXXXXX")"
  ln -s "$1" "$link"
  mv -T "$link" "$2"
}

# copy_binaries copies $1/hostrunner and $1/hostrun, mode 0755 whatever
# the umask, into a new temporary directory of root $2 named after
# version $3, syncs the copy and prints the directory.
copy_binaries() {
  local src="$1" root="$2" version="$3" dir
  dir="$(mktemp -d "$root/.tmp.$version-XXXXXXXX")"
  chmod 0755 "$dir"
  install -m 0755 "$src/hostrunner" "$src/hostrun" "$dir/"
  sync "$dir/hostrunner" "$dir/hostrun" "$dir" "$root"
  echo "$dir"
}

# switch_current points the current link of root $1 to entry $2 with one
# rename and syncs the root.
switch_current() {
  relink "$2" "$1/current"
  sync "$1"
}

# install_layout installs $1/hostrunner and $1/hostrun, version $2, into
# the layout under prefix $3, under the lock `hostrunner update` takes
# (a flock on the root). The current link first points to one synced
# copy while a second one replaces <version>, so it always names an
# existing complete pair, a reinstalled version included. Temporary
# entries start with .tmp., in the root and in bin/; a run that fails or
# is killed leaves them, and the next one removes them under the lock,
# except one the current link names (left by a killed install). Every
# other version directory is removed; other entries of the root are
# kept. Whatever is installed, version $2 replaces it: unlike update,
# this can downgrade.
install_layout() {
  local src="$1" version="$2" prefix="$3" root old entry name rc first dir
  root="$prefix/lib/hostrunner"
  install -d "$root" "$prefix/bin"
  exec 9<"$root"
  flock -n 9 || {
    rc=$?
    [ "$rc" = 1 ] && die "another update or install is running in $root"
    die "cannot lock $root (flock exit $rc)"
  }
  old="$(readlink "$root/current")" || old=
  for entry in "$root"/.tmp.*; do
    { [ -e "$entry" ] || [ -L "$entry" ]; } || continue
    [ "${entry##*/}" = "$old" ] || rm -rf "$entry"
  done
  rm -f "$prefix/bin"/.tmp.hostrunner-* "$prefix/bin"/.tmp.hostrun-*

  first="$(copy_binaries "$src" "$root" "$version")"
  switch_current "$root" "${first##*/}"
  dir="$(copy_binaries "$src" "$root" "$version")"
  rm -rf "${root:?}/$version"
  mv -T "$dir" "$root/$version"
  sync "$root"
  switch_current "$root" "$version"

  for entry in "$root"/* "$root"/.tmp.*; do
    name="${entry##*/}"
    case "$name" in
      "$version") continue ;;
      .tmp.*) { [ -e "$entry" ] || [ -L "$entry" ]; } || continue ;;
      *) { [ -d "$entry" ] && [ ! -L "$entry" ] && is_dir_version "$name"; } || continue ;;
    esac
    rm -rf "$entry"
  done
  relink "$root/current/hostrunner" "$prefix/bin/hostrunner"
  relink "$root/current/hostrun" "$prefix/bin/hostrun"
  sync "$prefix/bin"
  echo "hostrunner: installed $version into $root"
  case ":$PATH:" in
    *":$prefix/bin:"*) ;;
    *) echo "install.sh: note: $prefix/bin is not in PATH" >&2 ;;
  esac
}

main() {
  local tag= tag_given= from= arch version prefix name
  while [ $# -gt 0 ]; do
    case "$1" in
      -h | --help) echo "$USAGE"; exit 0 ;;
      -v) [ $# -ge 2 ] || usage_error; tag="$2" tag_given=1; shift ;;
      --from) [ $# -ge 2 ] || usage_error; from="$2"; shift ;;
      *) usage_error ;;
    esac
    shift
  done
  [ -z "$tag_given" ] || [ -z "$from" ] || usage_error
  if [ -n "$tag_given" ] && ! [[ "$tag" =~ $TAG_RE ]]; then
    die "\"$tag\" is not a release version (vX.Y.Z or vX.Y.Z-<pre-release>)"
  fi

  need flock mktemp install sync
  prefix="${PREFIX:-$HOME/.local}"
  case "$prefix" in /*) ;; *) prefix="$PWD/$prefix" ;; esac

  if [ -n "$from" ]; then
    for name in hostrunner hostrun; do
      [ -f "$from/$name" ] || die "$from/$name not found"
    done
  else
    arch="$(release_arch)"
    need curl sha256sum awk tar
    [ -n "$tag" ] || tag="$(latest_tag)"
    trap 'rm -rf "$WORK"' EXIT
    WORK="$(mktemp -d)"
    download_release "$tag" "$arch"
    from="$WORK/bin"
  fi
  # The version names the version directory: it must be one, so that
  # it cannot point elsewhere under the root (an empty one would make
  # the root itself the directory to replace).
  version="$("$from/hostrunner" version)" || die "$from/hostrunner version failed"
  is_dir_version "$version" || die "$from/hostrunner version \"$version\" is not a version"
  [ -z "$tag" ] || [ "$version" = "$tag" ] || die "release $tag holds hostrunner $version"
  install_layout "$from" "$version" "$prefix"
}

main "$@"
