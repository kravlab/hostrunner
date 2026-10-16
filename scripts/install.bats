#!/usr/bin/env bats
# Tests for scripts/install.sh (docs/specs/install-script.md). Nothing
# reaches the network: a curl stub in PATH serves the files of a fixture
# tree built per test and logs every URL it is asked for. The release
# binaries are shell scripts that print their version.

bats_require_minimum_version 1.5.0

# setup builds the stubs and an empty prefix, and puts the stubs first
# in PATH.
setup() {
  SCRIPT="$BATS_TEST_DIRNAME/install.sh"
  FIX="$BATS_TEST_TMPDIR/fixtures"
  STUBS="$BATS_TEST_TMPDIR/stubs"
  CURL_LOG="$BATS_TEST_TMPDIR/curl.log"
  export FIX CURL_LOG
  export PREFIX="$BATS_TEST_TMPDIR/prefix"
  # The script's download directory (mktemp -d) lands here, so tests
  # can check it is removed.
  export TMPDIR="$BATS_TEST_TMPDIR/tmp"
  mkdir -p "$TMPDIR"
  ROOT="$PREFIX/lib/hostrunner"
  mkdir -p "$FIX" "$STUBS"
  : >"$CURL_LOG"
  # The stub takes the last argument as the URL and -o as the output
  # file, as install.sh calls it; an unknown URL fails like curl -f.
  cat >"$STUBS/curl" <<'EOF'
#!/usr/bin/env bash
out= url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    https://*) url="$1" ;;
  esac
  shift
done
echo "$url" >>"$CURL_LOG"
file="$FIX/${url#https://}"
[ -f "$file" ] || { echo "curl: (22) 404 $url" >&2; exit 22; }
if [ -n "$out" ]; then cp "$file" "$out"; else cat "$file"; fi
EOF
  chmod +x "$STUBS/curl"
  export PATH="$STUBS:$PATH"
}

# fake_binaries writes hostrunner and hostrun printing version $2 into
# directory $1.
fake_binaries() {
  mkdir -p "$1"
  local name
  for name in hostrunner hostrun; do
    printf '#!/bin/sh\n[ "$1" = version ] && echo %s\n' "$2" >"$1/$name"
    chmod 0700 "$1/$name"
  done
}

# pack writes archive $1 holding the files $3… of directory $2 and a
# checksums.txt listing it next to it.
pack() {
  local archive="$1" src="$2"
  shift 2
  tar -czf "$archive" -C "$src" "$@"
  (cd "$(dirname "$archive")" && sha256sum "$(basename "$archive")" >checksums.txt)
}

# release publishes tag $1 for amd64 in the fixture tree: its archive,
# checksums.txt and, unless $2 is "nolatest", the latest-release API
# answer. RELEASE_VERSION overrides the version the binaries print.
release() {
  local tag="$1" v="${1#v}" dl="$FIX/github.com/kravlab/hostrunner/releases/download/$1"
  local archive="hostrunner_${v}_linux_amd64.tar.gz" src="$BATS_TEST_TMPDIR/src-$1"
  fake_binaries "$src" "${RELEASE_VERSION:-$tag}"
  mkdir -p "$dl"
  pack "$dl/$archive" "$src" hostrunner hostrun
  if [ "${2-}" != nolatest ]; then
    mkdir -p "$FIX/api.github.com/repos/kravlab/hostrunner/releases"
    printf '{\n  "url": "x",\n  "tag_name": "%s",\n  "name": "%s"\n}\n' "$tag" "$tag" \
      >"$FIX/api.github.com/repos/kravlab/hostrunner/releases/latest"
  fi
}

# uname_stub makes uname report machine $1 (and Linux for -s).
uname_stub() {
  printf '#!/bin/sh\ncase "$1" in -s) echo Linux ;; -m) echo %s ;; esac\n' "$1" >"$STUBS/uname"
  chmod +x "$STUBS/uname"
}

# run_install runs the script with the given arguments; stdout and stderr
# are kept apart.
run_install() {
  run --separate-stderr bash "$SCRIPT" "$@"
}

# assert_installed checks the layout of version $1: the version
# directory with both binaries at mode 0755, the relative current link
# and the absolute bin/ links through it.
assert_installed() {
  [ "$(readlink "$ROOT/current")" = "$1" ]
  [ "$(stat -c %a "$ROOT/$1/hostrunner")" = 755 ]
  [ "$(stat -c %a "$ROOT/$1/hostrun")" = 755 ]
  [ "$(readlink "$PREFIX/bin/hostrunner")" = "$ROOT/current/hostrunner" ]
  [ "$(readlink "$PREFIX/bin/hostrun")" = "$ROOT/current/hostrun" ]
  [ "$("$PREFIX/bin/hostrunner" version)" = "$1" ]
}

# assert_untouched checks that nothing was installed under the prefix
# and no temporary entry or download directory was left.
assert_untouched() {
  [ ! -e "$ROOT/current" ]
  [ ! -e "$PREFIX/bin/hostrunner" ]
  [ -z "$(find "$ROOT" -name '.tmp.*' 2>/dev/null)" ]
  [ -z "$(ls -A "$TMPDIR")" ]
}

# min_path makes a PATH directory holding the tools the script and its
# stubs use, without the tools given as arguments, and prints it.
min_path() {
  local bin="$BATS_TEST_TMPDIR/minbin" tool
  mkdir -p "$bin"
  for tool in bash tar flock mktemp cat awk cut cp grep sed head readlink ln mv rm mkdir chmod install sync dirname basename sha256sum; do
    ln -s "$(command -v "$tool")" "$bin/$tool"
  done
  ln -s "$STUBS/curl" "$bin/curl"
  [ ! -e "$STUBS/uname" ] || ln -s "$STUBS/uname" "$bin/uname"
  for tool in "$@"; do
    rm "$bin/$tool"
  done
  echo "$bin"
}

@test "-h prints the usage on stdout and exits 0" {
  run_install -h
  [ "$status" -eq 0 ]
  [[ "$output" == *"usage:"* ]]
}

@test "rejects an unknown flag and an extra argument with exit 2" {
  run_install -x
  [ "$status" -eq 2 ]
  [[ "$stderr" == *"usage:"* ]]
  run_install extra
  [ "$status" -eq 2 ]
  [[ "$stderr" == *"usage:"* ]]
}

@test "rejects -v together with --from with exit 2" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.0
  run_install -v v0.5.0 --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 2 ]
  [[ "$stderr" == *"usage:"* ]]
  assert_untouched
}

@test "installs the latest release into a fresh prefix" {
  uname_stub x86_64
  release v0.5.0
  run_install
  [ "$status" -eq 0 ]
  assert_installed v0.5.0
  [[ "$output" == *"hostrunner: installed v0.5.0 into $ROOT"* ]]
  [ -z "$(ls -A "$TMPDIR")" ]
  grep -qx "https://api.github.com/repos/kravlab/hostrunner/releases/latest" "$CURL_LOG"
}

@test "-v installs the given release, a pre-release included" {
  uname_stub x86_64
  release v0.6.0-rc.1 nolatest
  run_install -v v0.6.0-rc.1
  [ "$status" -eq 0 ]
  assert_installed v0.6.0-rc.1
  ! grep -q api.github.com "$CURL_LOG"
}

@test "rejects an invalid -v without any request" {
  uname_stub x86_64
  for v in 0.5.0 v0.5 v0.5.0+build v0.05.0 ""; do
    run_install -v "$v"
    [ "$status" -ne 0 ]
    [[ "$stderr" == *"install.sh: "* ]]
  done
  [ ! -s "$CURL_LOG" ]
  assert_untouched
}

@test "rejects a latest tag that is not a version" {
  uname_stub x86_64
  release v0.5.0
  sed -i 's/"tag_name": "v0.5.0"/"tag_name": "nightly"/' \
    "$FIX/api.github.com/repos/kravlab/hostrunner/releases/latest"
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *'latest release "nightly" is not a version'* ]]
  assert_untouched
}

@test "maps aarch64 to the arm64 archive" {
  uname_stub aarch64
  release v0.5.0
  run_install
  [ "$status" -eq 1 ]
  grep -q "hostrunner_0.5.0_linux_arm64.tar.gz" "$CURL_LOG"
}

@test "rejects an architecture without release builds before any request" {
  uname_stub riscv64
  release v0.5.0
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"no release builds for linux/riscv64"* ]]
  [ ! -s "$CURL_LOG" ]
}

@test "names a missing release tool" {
  uname_stub x86_64
  release v0.5.0
  local bin
  bin="$(min_path sha256sum)"
  PATH="$bin" run --separate-stderr "$bin/bash" "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"install.sh: sha256sum is required but not installed"* ]]
  assert_untouched
}

@test "names a missing flock in --from mode" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.0
  local bin
  bin="$(min_path flock)"
  PATH="$bin" run --separate-stderr "$bin/bash" "$SCRIPT" --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"install.sh: flock is required but not installed"* ]]
  assert_untouched
}

@test "--from does not need tar, curl or sha256sum" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.0
  local bin
  bin="$(min_path tar curl sha256sum awk)"
  PATH="$bin" run --separate-stderr "$bin/bash" "$SCRIPT" --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 0 ]
  assert_installed v0.5.0
}

@test "rejects a checksum mismatch and leaves the prefix untouched" {
  uname_stub x86_64
  release v0.5.0
  local sums="$FIX/github.com/kravlab/hostrunner/releases/download/v0.5.0/checksums.txt"
  sed -i -E 's/^[0-9a-f]{64}/'"$(printf '0%.0s' {1..64})"'/' "$sums"
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"checksum"* ]]
  assert_untouched
}

@test "rejects an archive not listed in checksums.txt" {
  uname_stub x86_64
  release v0.5.0
  local sums="$FIX/github.com/kravlab/hostrunner/releases/download/v0.5.0/checksums.txt"
  echo "0000  other.tar.gz" >"$sums"
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"hostrunner_0.5.0_linux_amd64.tar.gz is not listed in checksums.txt"* ]]
  assert_untouched
}

@test "rejects an archive without hostrun" {
  uname_stub x86_64
  release v0.5.0
  pack "$FIX/github.com/kravlab/hostrunner/releases/download/v0.5.0/hostrunner_0.5.0_linux_amd64.tar.gz" \
    "$BATS_TEST_TMPDIR/src-v0.5.0" hostrunner
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"release archive has no hostrun"* ]]
  assert_untouched
}

@test "rejects binaries whose version differs from the tag" {
  uname_stub x86_64
  RELEASE_VERSION=v0.4.0 release v0.5.0
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"v0.4.0"* ]]
  assert_untouched
}

@test "a failed latest-release lookup prints one error" {
  uname_stub x86_64
  run_install
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"could not download https://api.github.com/repos/kravlab/hostrunner/releases/latest"* ]]
  [ "$(grep -c '^install.sh: ' <<<"$stderr")" -eq 1 ]
  assert_untouched
}

@test "a failed download fails without touching the prefix" {
  uname_stub x86_64
  release v0.5.0
  rm "$FIX/github.com/kravlab/hostrunner/releases/download/v0.5.0/checksums.txt"
  run_install
  [ "$status" -eq 1 ]
  assert_untouched
}

@test "reinstall replaces the version and removes other version directories" {
  uname_stub x86_64
  release v0.4.0
  run_install
  [ "$status" -eq 0 ]
  mkdir -p "$ROOT/v0.1.0" "$ROOT/dev" "$ROOT/vim" "$ROOT/.tmp.v0.3.0-old"
  touch "$ROOT/v9.9.9"
  release v0.5.0
  run_install
  [ "$status" -eq 0 ]
  assert_installed v0.5.0
  [ ! -e "$ROOT/v0.4.0" ] && [ ! -e "$ROOT/v0.1.0" ] && [ ! -e "$ROOT/dev" ]
  [ ! -e "$ROOT/.tmp.v0.3.0-old" ]
  [ -d "$ROOT/vim" ] && [ -f "$ROOT/v9.9.9" ]
}

@test "installs an older version over a newer one" {
  uname_stub x86_64
  release v0.5.0
  run_install
  release v0.4.0 nolatest
  run_install -v v0.4.0
  [ "$status" -eq 0 ]
  assert_installed v0.4.0
}

@test "refuses while another update or install holds the lock" {
  uname_stub x86_64
  release v0.5.0
  mkdir -p "$ROOT"
  exec 8<"$ROOT"
  flock -n 8
  run_install
  exec 8<&-
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"another update or install is running in $ROOT"* ]]
  [ ! -e "$ROOT/current" ]
}

@test "--from installs local binaries without any request" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.1-0.20261016000000-abcdef123456
  run_install --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 0 ]
  assert_installed v0.5.1-0.20261016000000-abcdef123456
  [ ! -s "$CURL_LOG" ]
}

@test "--from rejects a directory without hostrun" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.0
  rm "$BATS_TEST_TMPDIR/local/hostrun"
  run_install --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"hostrun"* ]]
  assert_untouched
}

@test "--from refuses a version that cannot name a version directory" {
  mkdir -p "$ROOT/keep"
  local v
  for v in "" current ../x "v1/../x"; do
    fake_binaries "$BATS_TEST_TMPDIR/local" "$v"
    run_install --from "$BATS_TEST_TMPDIR/local"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"is not a version"* ]]
    [ -d "$ROOT/keep" ]
    assert_untouched
  done
}

@test "a relative PREFIX is made absolute" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.0
  cd "$BATS_TEST_TMPDIR"
  PREFIX=rel run --separate-stderr bash "$SCRIPT" --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 0 ]
  [ "$(readlink rel/bin/hostrunner)" = "$BATS_TEST_TMPDIR/rel/lib/hostrunner/current/hostrunner" ]
}

@test "notes when the bin directory is not in PATH" {
  fake_binaries "$BATS_TEST_TMPDIR/local" v0.5.0
  run_install --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 0 ]
  [[ "$stderr" == *"$PREFIX/bin is not in PATH"* ]]
  PATH="$PREFIX/bin:$PATH" run_install --from "$BATS_TEST_TMPDIR/local"
  [ "$status" -eq 0 ]
  [[ "$stderr" != *"not in PATH"* ]]
}
