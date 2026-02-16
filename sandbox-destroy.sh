#!/usr/bin/env bash
set -euo pipefail

# Resolve the ai-sandbox directory from this script's location
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Resolve the project directory (argument or current directory)
TARGET_DIR="${1:-.}"
if [[ "$TARGET_DIR" != /* ]]; then
  TARGET_DIR="$(cd "$TARGET_DIR" 2>/dev/null && pwd)" || {
    echo "Error: directory '$1' does not exist" >&2
    exit 1
  }
else
  if [[ ! -d "$TARGET_DIR" ]]; then
    echo "Error: directory '$TARGET_DIR' does not exist" >&2
    exit 1
  fi
  TARGET_DIR="$(cd "$TARGET_DIR" && pwd)"
fi

# Derive the project name (must match sandbox.sh)
PROJECT_NAME="sandbox-$(basename "$TARGET_DIR")"

echo "Destroying sandbox for: $TARGET_DIR"
echo "Project name: $PROJECT_NAME"

docker compose -p "$PROJECT_NAME" --profile "claude" -f "$SCRIPT_DIR/docker-compose.yml" down --rmi local --remove-orphans
