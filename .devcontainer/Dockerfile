# Images for this repository's devcontainers (docs/specs/claude-devcontainer.md).
# Each configuration picks a stage with build.target: `base` for everyday
# development, `claude` for running Claude Code wired to hostrunner.

FROM mcr.microsoft.com/devcontainers/base:trixie AS base

# pipefail: a failed download must fail the build, not feed an empty
# script to sh.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# mise brings the toolchain pinned in mise.toml (postCreateCommand runs
# `mise install`); its shims put the tools in PATH.
ARG MISE_VERSION=v2026.9.11
RUN curl -fsSL https://mise.run \
      | MISE_VERSION="$MISE_VERSION" MISE_INSTALL_PATH=/usr/local/bin/mise sh
ENV PATH=/home/vscode/.local/share/mise/shims:$PATH

# Bash history lives in a named volume at ~/.commandhistory. It is under
# the home folder because that is what the devcontainer CLI chowns when it
# changes vscode's UID to the host's (updateRemoteUserUID); a new, empty
# volume then takes the directory's ownership. `history -a` saves every
# command at once: a shell killed by a container stop never writes its
# history on exit.
RUN install -d -o vscode -g vscode /home/vscode/.commandhistory \
 && printf '%s\n' \
      'HISTFILE=~/.commandhistory/.bash_history' \
      'PROMPT_COMMAND="history -a${PROMPT_COMMAND:+; $PROMPT_COMMAND}"' \
      >> /home/vscode/.bashrc

FROM base AS claude

# Pinned rather than the `stable` channel: a channel name would make the
# layer depend on when the build cache was filled. Bump it to upgrade.
ARG CLAUDE_CODE_VERSION=2.1.280

# Claude's config lives in a named volume at ~/.claude; created here so the
# volume is owned by vscode. CLAUDE_CONFIG_DIR keeps .claude.json in it too,
# and the image, not the auto-updater, decides the version. The installer
# puts `claude` in ~/.local/bin.
ENV CLAUDE_CONFIG_DIR=/home/vscode/.claude \
    DISABLE_AUTOUPDATER=1 \
    PATH=/home/vscode/.local/bin:$PATH

USER vscode
RUN install -d /home/vscode/.claude \
 && curl -fsSL https://claude.ai/install.sh | bash -s "$CLAUDE_CODE_VERSION"
USER root

# The host's user.name and user.email, written by
# .devcontainer/claude/git-identity.sh and mounted at /run/git-identity.
# git skips a missing include, so the image works without the mount; the
# system level leaves ~/.gitconfig, which overrides it, to the container.
RUN git config --system include.path /run/git-identity/gitconfig
