#!/usr/bin/env bash
set -euo pipefail

# Resolve the ai-sandbox directory from this script's location
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# todo lho validate for supported agents
AGENT=$1

# Resolve the project directory (argument or current directory)
TARGET_DIR="${2:-.}"
if [[ "$TARGET_DIR" != /* ]]; then
  TARGET_DIR="$(cd "$TARGET_DIR" 2>/dev/null && pwd)" || {
    echo "Error: directory '$2' does not exist" >&2
    exit 1
  }
else
  if [[ ! -d "$TARGET_DIR" ]]; then
    echo "Error: directory '$TARGET_DIR' does not exist" >&2
    exit 1
  fi
  TARGET_DIR="$(cd "$TARGET_DIR" && pwd)"
fi

# Derive a unique project name from the target directory
PROJECT_NAME="sandbox-$(basename "$TARGET_DIR")"

echo "Launching sandbox for: $TARGET_DIR"
echo "Project name: $PROJECT_NAME"

export PROJECT_DIR="$TARGET_DIR"
export COMPOSE_PROJECT_NAME="$PROJECT_NAME"

# Start proxy in the background
docker compose -p "$COMPOSE_PROJECT_NAME" -f "$SCRIPT_DIR/docker-compose.yml" up -d --build proxy

# Tear down containers on exit
cleanup() {
  echo ""
  echo "Shutting down sandbox..."
  docker compose -p "$COMPOSE_PROJECT_NAME" -f "$SCRIPT_DIR/docker-compose.yml" down
}
trap cleanup EXIT

# Run claude interactively as the main container process
docker compose \
  -p "$COMPOSE_PROJECT_NAME" \
  --profile $AGENT \
  -f "$SCRIPT_DIR/docker-compose.yml" \
  run \
  --rm \
  claude