#!/usr/bin/env bash
# Cuts a release: tags a commit of origin/main chosen from a numbered
# list and pushes the tag, which starts .github/workflows/release.yml.
# Every check that would fail the workflow or give a wrong release runs
# before the push, because a pushed v* tag can be neither moved nor
# deleted. Spec: docs/specs/release-script.md.
#
# Usage: release.sh [-n N] <version>
set -euo pipefail

readonly USAGE='usage: release.sh [-n N] <version>
  <version>  tag to create: vX.Y.Z or vX.Y.Z-<pre-release>
  -n N       commits of origin/main to list (default 10)'

# Semver core with an optional pre-release (dot-separated identifiers,
# numeric ones without leading zeros); no build metadata.
readonly PRE_ID='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
readonly VERSION_RE="^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-$PRE_ID(\.$PRE_ID)*)?\$"

# TAGGED names the local tag created but not yet pushed. The EXIT trap
# deletes it, so neither a failure nor Ctrl-C leaves a tag behind.
TAGGED=

# die prints "release: <message>" to stderr and exits 1.
die() {
  echo "release: $*" >&2
  exit 1
}

# usage_error prints the usage to stderr and exits 2.
usage_error() {
  echo "$USAGE" >&2
  exit 2
}

# check_version refuses a malformed version and a major version of 2 or
# more, whose binaries would report a pseudo-version without a /vN
# module path.
check_version() {
  local version=$1
  [[ $version =~ $VERSION_RE ]] || die "$version is not vX.Y.Z or vX.Y.Z-<pre-release>"
  ((BASH_REMATCH[1] < 2)) || die "$version: v2 and later need a /v${BASH_REMATCH[1]} module path"
}

# cleanup is the EXIT trap: it deletes the unpushed tag in TAGGED.
cleanup() {
  [ -z "$TAGGED" ] || git tag -d "$TAGGED" >/dev/null 2>&1 || true
}

# check_tag_free refuses a tag that already exists locally. Run before
# the fetch (a local tag that differs from origin's would fail it) and
# after it (origin's tags are then local too).
check_tag_free() {
  [ -z "$(git tag -l "$1")" ] || die "tag $1 already exists"
}

# num_gt succeeds when the decimal $1 is greater than $2. Both are digit
# strings without leading zeros, compared as strings so that any length
# works (bash arithmetic wraps around past 2^63).
num_gt() {
  local LC_ALL=C
  if ((${#1} != ${#2})); then
    ((${#1} > ${#2}))
  else
    [[ $1 > $2 ]]
  fi
}

# id_gt succeeds when the pre-release identifier $1 has a higher
# precedence than $2 (semver 11.4.1-3): numeric ones compare
# numerically and rank below alphanumeric ones, which compare in ASCII
# order.
id_gt() {
  local LC_ALL=C num='^[0-9]+$'
  if [[ $1 =~ $num && $2 =~ $num ]]; then
    num_gt "$1" "$2"
  elif [[ $1 =~ $num || $2 =~ $num ]]; then
    [[ $2 =~ $num ]]
  else
    [[ $1 > $2 ]]
  fi
}

# version_gt succeeds when version $1 has a higher semver precedence than
# $2 (semver 11): the core numbers first, then a release above its
# pre-releases, then the pre-release identifiers one by one, a shorter
# list below a longer one it prefixes. Both must match VERSION_RE.
version_gt() {
  local a=${1#v} b=${2#v} a_pre='' b_pre='' i
  [[ $a == *-* ]] && a_pre=${a#*-}
  [[ $b == *-* ]] && b_pre=${b#*-}
  local -a x y
  IFS=. read -ra x <<<"${a%%-*}"
  IFS=. read -ra y <<<"${b%%-*}"
  for i in 0 1 2; do
    [ "${x[i]}" = "${y[i]}" ] || { num_gt "${x[i]}" "${y[i]}"; return; }
  done
  [ "$a_pre" != "$b_pre" ] || return 1
  [ -n "$a_pre" ] || return 0
  [ -n "$b_pre" ] || return 1
  IFS=. read -ra x <<<"$a_pre"
  IFS=. read -ra y <<<"$b_pre"
  for ((i = 0; i < ${#x[@]} && i < ${#y[@]}; i++)); do
    [ "${x[i]}" = "${y[i]}" ] || { id_gt "${x[i]}" "${y[i]}"; return; }
  done
  ((${#x[@]} > ${#y[@]}))
}

# highest_tag prints the highest v* tag by semver precedence, or nothing
# when there is none; v* tags that are not versions are ignored.
highest_tag() {
  local tag highest=''
  while read -r tag; do
    [[ $tag =~ $VERSION_RE ]] || continue
    { [ -z "$highest" ] || version_gt "$tag" "$highest"; } && highest=$tag
  done < <(git tag -l 'v*')
  echo "$highest"
}

# check_order refuses a version not greater than the highest tag, which
# would put the changelog ("commits since the previous tag") and the
# version order at odds.
check_order() {
  local version=$1 highest=$2
  [ -z "$highest" ] || version_gt "$version" "$highest" ||
    die "$version is not greater than the latest tag $highest"
}

# read_line reads one line from stdin into the variable named $1; a last
# line without a newline counts, end of input is an error.
read_line() {
  IFS= read -r "$1" || [ -n "${!1}" ] || die "no input"
}

# choose_commit lists the last $1 commits of origin/main (first parent,
# newest first, numbered from 0) on stdout, reads a number and stores
# the chosen commit's full hash in the global CHOSEN.
choose_commit() {
  local count=$1
  local -a hashes
  mapfile -t hashes < <(git rev-list --first-parent -n "$count" origin/main)
  git log --no-walk=unsorted --format='%h  %s' "${hashes[@]}" | awk '{ print NR - 1 "  " $0 }'
  local last=$((${#hashes[@]} - 1)) choice
  printf 'commit number [0-%d]: ' "$last" >&2
  read_line choice
  [[ $choice =~ ^(0|[1-9][0-9]*)$ ]] && ! num_gt "$choice" "$last" ||
    die "not a number from 0 to $last: $choice"
  CHOSEN=${hashes[choice]}
}

# actions_url prints the release workflow's Actions page for a GitHub
# origin, or nothing for any other remote. It reads the URL as
# configured: one rewritten by url.*.insteadOf may no longer name the
# GitHub repository.
actions_url() {
  local url repo
  url=$(git config --get remote.origin.url)
  [[ $url =~ github\.com[:/]([^/]+/[^/]+)$ ]] || return 0
  repo=${BASH_REMATCH[1]%.git}
  echo "https://github.com/$repo/actions/workflows/release.yml"
}

# main parses the arguments, runs the checks in the spec's order, and
# tags and pushes the chosen commit.
main() {
  local count=10 opt
  while getopts ':n:h' opt; do
    case $opt in
      n) count=$OPTARG ;;
      h) echo "$USAGE"; exit 0 ;;
      *) usage_error ;;
    esac
  done
  shift $((OPTIND - 1))
  [ $# -eq 1 ] || usage_error
  [[ $count =~ ^[1-9][0-9]*$ ]] || usage_error
  local version=$1

  check_version "$version"
  check_tag_free "$version"
  git fetch -q --tags origin || die "git fetch origin failed"
  check_tag_free "$version"
  local highest
  highest=$(highest_tag)
  check_order "$version" "$highest"

  local commit
  choose_commit "$count"
  commit=$CHOSEN
  if [ -n "$highest" ]; then
    git merge-base --is-ancestor "$highest" "$commit" ||
      die "$(git rev-parse --short "$commit") is older than the latest tag $highest"
  fi

  local short answer
  short=$(git rev-parse --short "$commit")
  printf 'push %s -> %s? [y/N] ' "$version" "$short" >&2
  read_line answer
  [[ $answer == [yY] ]] || die "cancelled"

  TAGGED=$version
  git tag -a -m "$version" "$version" "$commit"
  git push -q origin "$version" || die "git push failed; the local tag $version was deleted"
  TAGGED=''
  echo "released $version at $short"
  actions_url
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
main "$@"
