#!/bin/bash
set -euo pipefail

export NVM_DIR="${HOME}/.nvm"

if [ ! -f "${NVM_DIR}/nvm.sh" ]; then
  # Resolve the latest tag from the releases/latest redirect, not api.github.com: the REST API
  # allows only 60 anonymous requests per hour per IP, which a shared NAT exhausts (curl 403).
  nvm_version="$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/nvm-sh/nvm/releases/latest)"
  nvm_version="${nvm_version##*/}"
  # METHOD=script fetches nvm over plain HTTPS instead of `git clone` — GitHub throttles
  # anonymous git-over-HTTPS per IP (401 mid-handshake), which broke image builds.
  curl -fsSL "https://raw.githubusercontent.com/nvm-sh/nvm/${nvm_version}/install.sh" | METHOD=script PROFILE="${BASH_ENV}" bash
fi

# shellcheck source=/dev/null
. "${NVM_DIR}/nvm.sh"

# Pinned to the patch level, and it MUST match the absolute paths in command.json: the agent CLI
# is exec'd without nvm's shell setup, so the command names the interpreter by full path. A
# floating `nvm install 22` silently drifts to a newer patch and the launch fails with ENOENT.
nvm install 22.22.2
nvm use 22.22.2

npm install -g @google/gemini-cli

# The npm cache is ~300 MB of tarballs nothing in the sandbox reads again.
npm cache clean --force
