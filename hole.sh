#!/usr/bin/env bash
set -euo pipefail

# Constants
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
VALID_AGENTS=("claude" "gemini")
VALID_COMMANDS=("start" "destroy" "help")

# Show help message
show_help() {
  cat <<EOF
Usage: hole {agent} {command} {path}

Agents:
  claude    Claude Code agent
  gemini    Gemini agent (experimental)

Commands:
  start     Create a new or start an existing sandbox and attach to the agent CLI
  destroy   Completely tear down sandbox and remove containers
  help      Show this help message

Examples:
  hole claude start .
  hole claude start /path/to/project
  hole claude destroy .

After exiting the agent CLI, the agent container is automatically removed.
The proxy container remains running. Use 'destroy' to completely tear down the sandbox.
EOF
}

# Validate agent is in supported list
validate_agent() {
  local agent="$1"
  for valid in "${VALID_AGENTS[@]}"; do
    if [[ "$agent" == "$valid" ]]; then
      return 0
    fi
  done
  echo "Error: invalid agent '$agent'" >&2
  echo "Valid agents: ${VALID_AGENTS[*]}" >&2
  exit 1
}

# Validate command is in supported list
validate_command() {
  local command="$1"
  for valid in "${VALID_COMMANDS[@]}"; do
    if [[ "$command" == "$valid" ]]; then
      return 0
    fi
  done
  echo "Error: invalid command '$command'" >&2
  echo "Valid commands: ${VALID_COMMANDS[*]}" >&2
  exit 1
}

# Resolve project directory to absolute path
resolve_project_dir() {
  local target_dir="${1:-.}"

  if [[ "$target_dir" != /* ]]; then
    # Relative path
    target_dir="$(cd "$target_dir" 2>/dev/null && pwd)" || {
      echo "Error: directory '$1' does not exist" >&2
      exit 1
    }
  else
    # Absolute path
    if [[ ! -d "$target_dir" ]]; then
      echo "Error: directory '$target_dir' does not exist" >&2
      exit 1
    fi
    target_dir="$(cd "$target_dir" && pwd)"
  fi

  echo "$target_dir"
}

# Convert absolute path to valid Docker project name
sanitize_path_to_project_name() {
  local path="$1"
  # Remove leading slashes, replace / with -, lowercase, keep only valid chars
  echo "hole-$(echo "$path" | sed 's/^\/*//' | tr '/' '-' | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-')"
}

# Start command: create/start sandbox and attach to agent CLI
cmd_start() {
  local agent=$1
  local project_dir=$2

  echo "Launching sandbox for: $project_dir"
  echo "Project name: $COMPOSE_PROJECT_NAME"
  echo ""

  # Start proxy in detached mode with health check wait
  echo "Starting proxy..."
  docker compose \
    -p "$COMPOSE_PROJECT_NAME" \
    -f "$COMPOSE_FILE" \
    up \
    -d \
    proxy

  # Start agent service
  echo "Starting $agent agent..."
  echo ""
  docker compose \
    -p "$COMPOSE_PROJECT_NAME" \
    -f "$COMPOSE_FILE" \
    up \
    -d \
    "$agent"

  # Attach terminal to the running agent
  echo "Attaching to $agent agent..."
  echo ""
  docker attach "$COMPOSE_PROJECT_NAME-$agent-1"

  # Stop the sandbox after user exists
  docker compose \
    -p "$COMPOSE_PROJECT_NAME" \
    -f "$COMPOSE_FILE" \
    stop

  echo ""
  echo "Exited $agent CLI."
  echo "Sandbox stopped. Run 'hole $agent start $project_dir' to start the existing sandbox again, or run 'hole $agent destroy $project_dir' to destroy the sandbox."
}

# Destroy command: completely tear down sandbox
cmd_destroy() {
  local agent=$1
  local project_dir=$2

  echo "Destroying sandbox for: $project_dir"
  echo "Project name: $COMPOSE_PROJECT_NAME"
  echo ""

  docker compose -p "$COMPOSE_PROJECT_NAME" -f "$COMPOSE_FILE" down --rmi local --remove-orphans

  echo ""
  echo "Sandbox destroyed."
}

# Main entry point
main() {
  # Parse arguments with defaults
  local agent="${1:-}"
  local command="${2:-}"
  local target_dir="${3:-.}"

  # Handle help shortcuts
  if [[ "$agent" == "help" ]] || [[ "$command" == "help" ]] || [[ -z "$agent" ]] || [[ -z "$command" ]]; then
    show_help
    exit 0
  fi

  # Validate inputs
  validate_agent "$agent"
  validate_command "$command"

  # Resolve path
  local project_dir
  project_dir=$(resolve_project_dir "$target_dir")

  # Generate project name and export environment
  export PROJECT_DIR="$project_dir"
  export COMPOSE_PROJECT_NAME="$(sanitize_path_to_project_name "$project_dir")-$agent"

  # Dispatch to command handler
  case "$command" in
    start)   cmd_start "$agent" "$project_dir" ;;
    destroy) cmd_destroy "$agent" "$project_dir" ;;
    help)    show_help ;;
    *)       echo "Unknown command: $command" >&2; exit 1 ;;
  esac
}

main "$@"
