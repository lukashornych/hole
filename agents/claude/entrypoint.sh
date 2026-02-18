#!/bin/bash
set -euo pipefail

# Copy host config from read-only staging mounts into the writable home dir.
# This avoids bind-mount corruption and cross-container race conditions.
STAGING_DIR="/home/claude/.host-config"

if [[ -f "$STAGING_DIR/.claude.json" ]]; then
    cp "$STAGING_DIR/.claude.json" /home/claude/.claude.json
fi

if [[ -d "$STAGING_DIR/.claude" ]]; then
    cp -a "$STAGING_DIR/.claude/." /home/claude/.claude/
fi

exec "$@"
