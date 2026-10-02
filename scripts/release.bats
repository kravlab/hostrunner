#!/usr/bin/env bats
# Tests for scripts/release.sh (docs/specs/release-script.md). Each test
# runs in a fresh clone of a bare local `origin` whose main has 12
# commits, "commit 1" (oldest) to "commit 12"; nothing reaches the
# network or GitHub.

bats_require_minimum_version 1.5.0

# setup creates the origin and the clone, isolated from the user's git
# configuration, and cds into the clone.
setup() {
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
  export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
  export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
  SCRIPT="$BATS_TEST_DIRNAME/release.sh"
  ORIGIN="$BATS_TEST_TMPDIR/origin.git"
  git init -q --bare -b main "$ORIGIN"
  git init -q -b main "$BATS_TEST_TMPDIR/work"
  cd "$BATS_TEST_TMPDIR/work"
  git remote add origin "$ORIGIN"
  local i
  for i in $(seq 1 12); do
    git commit -q --allow-empty -m "commit $i"
  done
  git push -q origin main
}

# release runs the script with the given arguments, feeding $INPUT
# (default: none) as stdin; stdout and stderr are kept apart.
release() {
  run --separate-stderr bash "$SCRIPT" "$@" <<<"${INPUT-}"
}

# sha_of prints the full hash of the origin/main commit numbered $1 in
# the script's list (0 = newest).
sha_of() {
  git rev-parse "origin/main~$1"
}

# tag_in_origin pushes tag $1 to origin on commit $2 without keeping it
# locally, as if another clone had released it.
tag_in_origin() {
  git tag "$1" "$2"
  git push -q origin "$1"
  git tag -d "$1" >/dev/null
}

# assert_no_tag fails if tag $1 exists locally or on origin.
assert_no_tag() {
  [ -z "$(git tag -l "$1")" ]
  [ -z "$(git ls-remote --tags origin "refs/tags/$1")" ]
}

@test "-h prints the usage and exits 0" {
  release -h
  [ "$status" -eq 0 ]
  [[ "$output$stderr" == *"usage:"* ]]
}

@test "rejects a missing version with exit 2" {
  release
  [ "$status" -eq 2 ]
  [[ "$stderr" == *"usage:"* ]]
}

@test "rejects a second positional argument with exit 2" {
  release v0.1.0 extra
  [ "$status" -eq 2 ]
}

@test "rejects an unknown flag with exit 2" {
  release -x v0.1.0
  [ "$status" -eq 2 ]
}

@test "rejects -n that is not a positive integer with exit 2" {
  for n in 0 -1 abc ""; do
    release -n "$n" v0.1.0
    [ "$status" -eq 2 ]
  done
}

@test "rejects malformed versions" {
  for v in 0.1.0 v0.1 v0.01.0 v0.1.0+build v0.1.0- V0.1.0 v0.1.0-rc..1 v0.1.0-01; do
    INPUT=$'0\ny'
    release "$v"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"release: "* ]]
    assert_no_tag "$v"
  done
}

@test "rejects a major version of 2 or more" {
  INPUT=$'0\ny'
  release v2.0.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"/v2"* ]]
  assert_no_tag v2.0.0
}

@test "rejects a tag that already exists locally" {
  git tag v0.1.0 "$(sha_of 0)"
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v0.1.0"* ]]
  [ -z "$(git ls-remote --tags origin refs/tags/v0.1.0)" ]
}

@test "rejects a tag that exists only on origin" {
  tag_in_origin v0.1.0 "$(sha_of 0)"
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v0.1.0"* ]]
}

@test "rejects a version lower than the highest tag" {
  tag_in_origin v0.2.0 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v0.1.1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v0.2.0"* ]]
  assert_no_tag v0.1.1
}

@test "rejects a pre-release of the highest tag's version" {
  tag_in_origin v0.2.0 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v0.2.0-rc.1
  [ "$status" -eq 1 ]
  assert_no_tag v0.2.0-rc.1
}

@test "allows a release after its pre-release" {
  tag_in_origin v0.2.0-rc.1 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v0.2.0
  [ "$status" -eq 0 ]
  [ "$(git rev-parse 'v0.2.0^{commit}')" = "$(sha_of 0)" ]
}

@test "orders pre-releases numerically" {
  tag_in_origin v0.2.0-rc.2 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v0.2.0-rc.10
  [ "$status" -eq 0 ]
  [ "$(git rev-parse 'v0.2.0-rc.10^{commit}')" = "$(sha_of 0)" ]
}

@test "orders a numeric pre-release identifier before an alphanumeric one" {
  tag_in_origin v1.0.0-alpha.beta "$(sha_of 1)"
  INPUT=$'0\ny'
  release v1.0.0-alpha.1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v1.0.0-alpha.beta"* ]]
  assert_no_tag v1.0.0-alpha.1
}

@test "allows an alphanumeric pre-release identifier after a numeric one" {
  tag_in_origin v1.0.0-alpha.1 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v1.0.0-alpha.beta
  [ "$status" -eq 0 ]
}

@test "orders pre-release identifiers one by one, not as one string" {
  tag_in_origin v1.0.0-rc1 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v1.0.0-rc.1
  [ "$status" -eq 1 ]
  assert_no_tag v1.0.0-rc.1
}

@test "orders a shorter pre-release before a longer one with its prefix" {
  tag_in_origin v1.0.0-alpha "$(sha_of 1)"
  INPUT=$'0\ny'
  release v1.0.0-alpha.1
  [ "$status" -eq 0 ]
}

@test "compares against the highest tag, not the newest or last listed" {
  tag_in_origin v0.10.0 "$(sha_of 2)"
  tag_in_origin v0.9.0 "$(sha_of 1)"
  INPUT=$'0\ny'
  release v0.9.1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v0.10.0"* ]]
}

@test "ignores v* tags that are not versions" {
  tag_in_origin vnext "$(sha_of 1)"
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 0 ]
}

@test "lists the last 10 commits of origin/main by default, numbered from 0" {
  INPUT=$'0\nn'
  release v0.1.0
  [ "${#lines[@]}" -eq 10 ]
  [ "${lines[0]}" = "0  $(git rev-parse --short "$(sha_of 0)")  commit 12" ]
  [ "${lines[9]}" = "9  $(git rev-parse --short "$(sha_of 9)")  commit 3" ]
}

@test "-n sets how many commits are listed" {
  INPUT=$'0\nn'
  release -n 3 v0.1.0
  [ "${#lines[@]}" -eq 3 ]
  [ "${lines[2]}" = "2  $(git rev-parse --short "$(sha_of 2)")  commit 10" ]
}

@test "-n larger than the history lists every commit" {
  INPUT=$'0\nn'
  release -n 100 v0.1.0
  [ "${#lines[@]}" -eq 12 ]
  [ "${lines[11]}" = "11  $(git rev-parse --short "$(sha_of 11)")  commit 1" ]
}

@test "lists origin/main, not the local branch" {
  git commit -q --allow-empty -m "local only"
  INPUT=$'0\nn'
  release -n 1 v0.1.0
  [ "${lines[0]}" = "0  $(git rev-parse --short "$(sha_of 0)")  commit 12" ]
}

@test "rejects an invalid commit number" {
  for choice in "" -1 abc 3 1.0 " 1" 18446744073709551617; do
    INPUT="$choice"$'\ny'
    release -n 3 v0.1.0
    [ "$status" -eq 1 ]
    assert_no_tag v0.1.0
  done
}

@test "rejects end of input at the commit prompt" {
  run --separate-stderr bash "$SCRIPT" v0.1.0 </dev/null
  [ "$status" -eq 1 ]
  assert_no_tag v0.1.0
}

@test "rejects a commit older than the highest tag's commit" {
  tag_in_origin v0.1.0 "$(sha_of 2)"
  INPUT=$'3\ny'
  release v0.2.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v0.1.0"* ]]
  assert_no_tag v0.2.0
}

@test "creates nothing unless the confirmation is y or Y" {
  for answer in "" n N yes; do
    INPUT=$'1\n'"$answer"
    release v0.1.0
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"v0.1.0 -> $(git rev-parse --short "$(sha_of 1)")"* ]]
    assert_no_tag v0.1.0
  done
}

@test "rejects end of input at the confirmation prompt" {
  INPUT=0
  release v0.1.0
  [ "$status" -eq 1 ]
  assert_no_tag v0.1.0
}

@test "accepts a confirmation without a trailing newline" {
  run --separate-stderr bash "$SCRIPT" v0.1.0 < <(printf '0\ny')
  [ "$status" -eq 0 ]
  [ "$(git rev-parse 'v0.1.0^{commit}')" = "$(sha_of 0)" ]
}

@test "pushes an annotated tag on the selected commit" {
  INPUT=$'1\nY'
  release v0.1.0
  [ "$status" -eq 0 ]
  [ "$(git cat-file -t v0.1.0)" = tag ]
  [ "$(git tag -l --format='%(contents:subject)' v0.1.0)" = v0.1.0 ]
  [ "$(git rev-parse 'v0.1.0^{commit}')" = "$(sha_of 1)" ]
  [ "$(git ls-remote origin 'refs/tags/v0.1.0^{}' | cut -f1)" = "$(sha_of 1)" ]
  [[ "$output" == *"v0.1.0"*"$(git rev-parse --short "$(sha_of 1)")"* ]]
}

@test "deletes the local tag when the push fails" {
  printf '#!/bin/sh\nexit 1\n' >"$ORIGIN/hooks/pre-receive"
  chmod +x "$ORIGIN/hooks/pre-receive"
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"git push failed"* ]]
  assert_no_tag v0.1.0
}

@test "deletes the local tag when killed during the push" {
  printf '#!/bin/sh\ntouch "%s"\nsleep 30\n' "$BATS_TEST_TMPDIR/pushing" >"$ORIGIN/hooks/pre-receive"
  chmod +x "$ORIGIN/hooks/pre-receive"
  setsid bash "$SCRIPT" v0.1.0 <<<$'0\ny' >/dev/null 2>&1 &
  local pid=$!
  local i
  for i in $(seq 1 100); do
    [ -e "$BATS_TEST_TMPDIR/pushing" ] && break
    sleep 0.1
  done
  [ -e "$BATS_TEST_TMPDIR/pushing" ]
  kill -TERM -- "-$pid"
  wait "$pid" || true
  assert_no_tag v0.1.0
}

@test "prints the release workflow URL for a GitHub origin" {
  git config url."$ORIGIN".insteadOf https://github.com/acme/tool.git
  git remote set-url origin https://github.com/acme/tool.git
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 0 ]
  [ "${lines[-1]}" = "https://github.com/acme/tool/actions/workflows/release.yml" ]
}

@test "prints no workflow URL for another origin" {
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 0 ]
  [[ "$output" != *"https://"* ]]
}

@test "fails before listing when origin cannot be fetched" {
  rm -rf "$ORIGIN"
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"git fetch origin failed"* ]]
  [ -z "$output" ]
  [ -z "$(git tag -l v0.1.0)" ]
}

@test "reports an existing local tag even when it differs from origin's" {
  tag_in_origin v0.1.0 "$(sha_of 1)"
  git tag v0.1.0 "$(sha_of 0)"
  INPUT=$'0\ny'
  release v0.1.0
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"tag v0.1.0 already exists"* ]]
}
