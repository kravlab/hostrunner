#!/bin/bash
# Installs the pre-commit hook (lefthook.yml) in Claude Code cloud
# sessions, whose containers have go but neither mise nor lefthook.
# Locally `mise run setup` installs it once, so this does nothing there.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "$CLAUDE_PROJECT_DIR"
bin="$(go env GOPATH)/bin"
echo "export PATH=\"$bin:\$PATH\"" >> "$CLAUDE_ENV_FILE"

# The version mise.toml pins, so cloud and local hooks match.
version="$(sed -n 's/^lefthook = "\([^"]*\)".*/\1/p' mise.toml)"
if [ "$("$bin/lefthook" version 2>/dev/null)" != "$version" ]; then
  go install "github.com/evilmartians/lefthook/v2@v$version"
fi
"$bin/lefthook" install
