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

# The host's user.name and user.email, written by
# .devcontainer/git-identity.sh and mounted at /run/host-config.
# git skips a missing include, so the image works without the mount; the
# system level leaves ~/.gitconfig, which overrides it, to the container.
RUN git config --system include.path /run/host-config/gitconfig

# The host's global git excludes, copied by .devcontainer/git-excludes.sh
# to /run/host-config/gitignore (docs/specs/devcontainer-git-excludes.md).
# ~/.config/git/ignore is git's and ripgrep's default excludes path, so
# neither needs core.excludesFile; ripgrep, which Claude Code searches
# with, reads neither the system config nor includes. Created as vscode,
# so other tools can still write to ~/.config. Without the mount the link
# dangles, which both treat as no excludes.
USER vscode
RUN mkdir -p /home/vscode/.config/git \
 && ln -s /run/host-config/gitignore /home/vscode/.config/git/ignore
USER root

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

# The host user's global skills and hooks, copied by
# .devcontainer/claude/skills.sh and hooks.sh to /run/host-config/claude
# (docs/specs/devcontainer-claude-skills-hooks.md). /etc/claude-code is
# Claude Code's system directory: it loads the skills in .claude/skills
# there, and runs the hooks of a managed-settings.d file alongside the
# container's own. Not in the ~/.claude volume, which Claude Code writes
# itself (its settings, the account's synced skills), and not writable by
# the agent. Without the mount the links dangle: no skills, no hooks.
RUN install -d /etc/claude-code/.claude /etc/claude-code/managed-settings.d \
 && ln -s /run/host-config/claude/skills /etc/claude-code/.claude/skills \
 && ln -s /run/host-config/claude/hooks.json /etc/claude-code/managed-settings.d/host-hooks.json
