#!/bin/bash
set -euo pipefail

# Copy .claude.json from read-only staging mount into the writable home dir.
# This avoids bind-mount corruption from atomic writes. The .claude directory
# is mounted directly (read-write) via docker-compose.
STAGING_DIR="/home/claude/.host-config"

if [[ -f "$STAGING_DIR/.claude.json" ]]; then
    cp "$STAGING_DIR/.claude.json" /home/claude/.claude.json
fi

# Install additional dependencies if a deps file was mounted
DEPS_FILE="/tmp/hole-dependencies.txt"
if [[ -f "$DEPS_FILE" ]]; then
    mapfile -t packages < "$DEPS_FILE"
    if [[ ${#packages[@]} -gt 0 ]]; then
        echo "Installing dependencies: ${packages[*]}"
        sudo -E apt-get update -q && sudo -E apt-get install -q --no-install-recommends -y "${packages[@]}"
    fi
fi

exec "$@"
