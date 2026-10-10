#!/bin/bash
# PreToolUse hook: refuses a tool call whose input carries a Claude Code
# session link (a commit message, a PR or issue body, a file), so none
# reaches the repository or GitHub. Exit 2 blocks the call and shows the
# message to Claude.
set -euo pipefail

if grep -qE 'claude\.ai/code/session_[A-Za-z0-9]'; then
  echo "Blocked: the input contains a Claude Code session link (claude.ai/code/session_…). Remove it and retry." >&2
  exit 2
fi
