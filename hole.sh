#!/usr/bin/env bash
set -euo pipefail

# Constants
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
HOLE_DATA_DIR="$HOME/.hole"
VALID_AGENTS=("claude" "gemini")
VALID_COMMANDS=("start" "destroy" "help")

# Show help message
show_help() {
  cat <<EOF
Usage: hole {agent} {command} {path} [options]

Agents:
  claude    Claude Code agent
  gemini    Gemini agent (experimental)

Commands:
  start     Create a new or start an existing sandbox and attach to the agent CLI
  destroy   Completely tear down sandbox and remove containers
  help      Show this help message

Options:
  --dump-network-access   After the agent exits, write distinct accessed domains
                          to {agent}-network-access.log in the project directory

Examples:
  hole claude start .
  hole claude start /path/to/project
  hole claude start . --dump-network-access
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

# Generate per-project docker-compose override file from .hole/settings.json
generate_project_compose() {
  local agent="$1"
  local project_dir="$2"
  local project_name="$3"

  local compose_dir="$HOLE_DATA_DIR/projects/$project_name"
  local compose_file="$compose_dir/docker-compose.yml"
  local settings_file="$project_dir/.hole/settings.json"

  local agent_volumes=()
  local has_custom_domains=false

  # Read exclusions from .hole/settings.json and auto-detect files vs directories
  if [[ -f "$settings_file" ]]; then
    local entries
    entries=$(jq -r '.files.exclude[]? // empty' "$settings_file" 2>/dev/null) || true
    while IFS= read -r line; do
      [[ -z "$line" ]] && continue
      # Strip trailing slashes for consistent mount paths
      line="${line%/}"
      local full_path="$project_dir/$line"
      if [[ -f "$full_path" ]]; then
        agent_volumes+=("      - /dev/null:/workspace/$line:ro")
      elif [[ -d "$full_path" ]]; then
        agent_volumes+=("      - /workspace/$line")
      else
        echo "Warning: excluded path '$line' not found in project, skipping" >&2
      fi
    done <<< "$entries"
  fi

  # Read domain whitelist from .hole/settings.json
  if [[ -f "$settings_file" ]]; then
    local domains
    domains=$(jq -r '.network.domainWhitelist[]? // empty' "$settings_file" 2>/dev/null) || true
    if [[ -n "$domains" ]]; then
      mkdir -p "$compose_dir"
      local whitelist_file="$compose_dir/tinyproxy-domain-whitelist.txt"
      # Start with default allowed domains
      cp "$SCRIPT_DIR/proxy/allowed-domains.txt" "$whitelist_file"
      # Append project-specific domains (escape dots for tinyproxy regex filter)
      echo "" >> "$whitelist_file"
      echo "# Project-specific domains" >> "$whitelist_file"
      while IFS= read -r domain; do
        [[ -z "$domain" ]] && continue
        echo "${domain//./\\.}" >> "$whitelist_file"
      done <<< "$domains"
      has_custom_domains=true
    fi
  fi

  if [[ ${#agent_volumes[@]} -gt 0 || "$has_custom_domains" == true ]]; then
    mkdir -p "$compose_dir"
    {
      echo "services:"
      if [[ "$has_custom_domains" == true ]]; then
        echo "  proxy:"
        echo "    volumes:"
        echo "      - $compose_dir/tinyproxy-domain-whitelist.txt:/etc/tinyproxy/allowed-domains.txt:ro"
      fi
      if [[ ${#agent_volumes[@]} -gt 0 ]]; then
        echo "  $agent:"
        echo "    volumes:"
        for v in "${agent_volumes[@]}"; do
          echo "$v"
        done
      fi
    } > "$compose_file"
  else
    # No overrides — remove stale files if any
    rm -f "$compose_file"
    rm -f "$compose_dir/tinyproxy-domain-whitelist.txt"
    rmdir "$compose_dir" 2>/dev/null || true
  fi

  PROJECT_COMPOSE_FILE="$compose_file"
}

# Build the docker compose command array, including optional override file
build_compose_cmd() {
  COMPOSE_CMD=(docker compose -p "$COMPOSE_PROJECT_NAME" -f "$COMPOSE_FILE")
  if [[ -f "$PROJECT_COMPOSE_FILE" ]]; then
    COMPOSE_CMD+=(-f "$PROJECT_COMPOSE_FILE")
  fi
}

# Start command: create/start sandbox and attach to agent CLI
cmd_start() {
  local agent=$1
  local project_dir=$2
  local dump_network_access=${3:-false}

  # Generate per-project compose override from .hole/settings.json
  generate_project_compose "$agent" "$project_dir" "$COMPOSE_PROJECT_NAME"
  build_compose_cmd

  echo "Launching sandbox for: $project_dir"
  echo "Project name: $COMPOSE_PROJECT_NAME"
  echo ""

  # Start proxy in detached mode with health check wait
  echo "Starting proxy..."
  "${COMPOSE_CMD[@]}" up -d proxy

  # Start agent service
  echo "Starting $agent agent..."
  echo ""
  "${COMPOSE_CMD[@]}" up -d "$agent"

  # Attach terminal to the running agent
  echo "Attaching to $agent agent..."
  echo ""
  docker attach "$COMPOSE_PROJECT_NAME-$agent-1"

  # Dump network access log if requested
  if [[ "$dump_network_access" == true ]]; then
    local log_file="$project_dir/$agent-network-access.log"
    docker logs "$COMPOSE_PROJECT_NAME-proxy-1" 2>&1 | \
      grep -oE 'CONNECT [a-zA-Z0-9._-]+:[0-9]+|filtered url "[^"]+"' | \
      sed 's/CONNECT //; s/:[0-9]*$//; s/^filtered url "//; s/"$//' | \
      sort -u > "$log_file" || true
    echo ""
    echo "Network access log written to: $log_file"
  fi

  # Stop the sandbox after user exits
  "${COMPOSE_CMD[@]}" stop

  echo ""
  echo "Exited $agent CLI."
  echo "Sandbox stopped. Run 'hole $agent start $project_dir' to start the existing sandbox again, or run 'hole $agent destroy $project_dir' to destroy the sandbox."
}

# Destroy command: completely tear down sandbox
cmd_destroy() {
  local agent=$1
  local project_dir=$2

  # Load override file path and build compose command
  local compose_dir="$HOLE_DATA_DIR/projects/$COMPOSE_PROJECT_NAME"
  PROJECT_COMPOSE_FILE="$compose_dir/docker-compose.yml"
  build_compose_cmd

  echo "Destroying sandbox for: $project_dir"
  echo "Project name: $COMPOSE_PROJECT_NAME"
  echo ""

  "${COMPOSE_CMD[@]}" down --rmi local --remove-orphans

  # Clean up per-project compose override
  if [[ -d "$compose_dir" ]]; then
    rm -rf "$compose_dir"
  fi

  echo ""
  echo "Sandbox destroyed."
}

# Main entry point
main() {
  local dump_network_access=false
  local positional=()

  for arg in "$@"; do
    case "$arg" in
      --dump-network-access) dump_network_access=true ;;
      *) positional+=("$arg") ;;
    esac
  done

  # Parse positional arguments with defaults
  local agent="${positional[0]:-}"
  local command="${positional[1]:-}"
  local target_dir="${positional[2]:-.}"

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
    start)   cmd_start "$agent" "$project_dir" "$dump_network_access" ;;
    destroy) cmd_destroy "$agent" "$project_dir" ;;
    help)    show_help ;;
    *)       echo "Unknown command: $command" >&2; exit 1 ;;
  esac
}

main "$@"
