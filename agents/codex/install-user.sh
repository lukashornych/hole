#!/bin/bash
set -euo pipefail

export NVM_DIR="${HOME}/.nvm"

if [ ! -f "${NVM_DIR}/nvm.sh" ]; then
  nvm_version="$(curl -fsSL https://api.github.com/repos/nvm-sh/nvm/releases/latest | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
  curl -fsSL "https://raw.githubusercontent.com/nvm-sh/nvm/${nvm_version}/install.sh" | bash
fi

# shellcheck source=/dev/null
. "${NVM_DIR}/nvm.sh"

nvm install 22

npm install -g @openai/codex
