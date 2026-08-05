#!/bin/bash
set -euo pipefail

export NVM_DIR="${HOME}/.nvm"

if [ ! -f "${NVM_DIR}/nvm.sh" ]; then
  nvm_version="$(curl -fsSL https://api.github.com/repos/nvm-sh/nvm/releases/latest | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
  curl -fsSL "https://raw.githubusercontent.com/nvm-sh/nvm/${nvm_version}/install.sh" | PROFILE="${BASH_ENV}" bash
fi

# shellcheck source=/dev/null
. "${NVM_DIR}/nvm.sh"

# Pinned to the patch level, and it MUST match the absolute paths in command.json: the agent CLI
# is exec'd without nvm's shell setup, so the command names the interpreter by full path. A
# floating `nvm install 22` silently drifts to a newer patch and the launch fails with ENOENT.
nvm install 22.22.2
nvm use 22.22.2

npm install -g @google/gemini-cli
