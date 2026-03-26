#!/bin/bash
set -euo pipefail
if ! command -v node &>/dev/null; then
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
  apt-get install -y nodejs
  rm -rf /var/lib/apt/lists/*
fi
npm install -g @google/gemini-cli
