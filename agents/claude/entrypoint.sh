#!/bin/bash
set -euo pipefail

# Copy .claude.json from read-only staging mount into the writable home dir.
# This avoids bind-mount corruption from atomic writes. The .claude directory
# is mounted directly (read-write) via docker-compose.
STAGING_DIR="/home/claude/.host-config"

if [[ -f "$STAGING_DIR/.claude.json" ]]; then
    cp "$STAGING_DIR/.claude.json" /home/claude/.claude.json
fi

exec "$@"
