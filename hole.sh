#!/usr/bin/env bash
set -euo pipefail

# Constants
SCRIPT_DIR="$(cd "$(dirname "${0}")" && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
HOLE_TMP_DIR="${TMPDIR:-/tmp}/hole"
GLOBAL_SETTINGS_FILE="${HOME}/.hole/settings.json"
GITHUB_REPO="lukashornych/hole"
GITHUB_API="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
GITHUB_INSTALL_SCRIPT="https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh"
VALID_AGENTS=("claude")
VALID_COMMANDS=("start" "destroy" "help" "version" "update" "uninstall")

# Import the logging library
source "${SCRIPT_DIR}/logger.sh"

# Show help message
show_help() {
  cat <<EOF
Usage: hole {command} {agent} {path} [options]

Commands:
  start     Create a sandbox, attach to the agent CLI, and destroy on exit
  destroy   Remove all cached or orphaned Docker resources for a project
  update    Update hole to the latest release
  uninstall Uninstall hole and optionally remove Docker resources
  help      Show this help message
  version   Print the installed hole version

Agents:
  claude    Claude Code agent

Options:
  --debug                 Open a bash shell instead of the agent CLI for
                              inspecting the sandbox environment
  --dump-network-access   After the agent exits, write distinct accessed domains
                              to {agent}-network-access-{id}.log in the project directory
  --rebuild               Force rebuild of Docker images before starting

Examples:
  hole start claude .
  hole start claude /path/to/project
  hole start claude . --dump-network-access
  hole destroy .
  hole destroy /path/to/project

The sandbox is destroyed when you exit the agent CLI.
EOF
}

# Validate agent is in supported list
validate_agent() {
  local agent="${1}"
  for valid in "${VALID_AGENTS[@]}"; do
    if [[ "${agent}" == "${valid}" ]]; then
      return 0
    fi
  done
  log_error "invalid agent '${agent}'"
  log_info "Valid agents: ${VALID_AGENTS[*]}"
  exit 1
}

# Validate command is in supported list
validate_command() {
  local command="${1}"
  for valid in "${VALID_COMMANDS[@]}"; do
    if [[ "${command}" == "${valid}" ]]; then
      return 0
    fi
  done
  log_error "invalid command '${command}'"
  log_info "Valid commands: ${VALID_COMMANDS[*]}"
  exit 1
}

# Validate settings.json against JSON Schema using jv
validate_settings() {
  local settings_file="${1}"
  local label="${2:-${settings_file}}"

  # Settings file is optional
  if [[ ! -f "${settings_file}" ]]; then
    return 0
  fi

  local schema_file="${SCRIPT_DIR}/schema/settings.schema.json"
  local output
  if ! output=$(jv "${schema_file}" "${settings_file}" 2>&1); then
    log_error "${label} is not valid:"
    while IFS= read -r err_line; do
      log_error "${err_line}"
    done <<< "${output}"
    exit 1
  fi
}

# Deep-merge two settings files (global + project) and output merged JSON to stdout
# Arrays are concatenated and deduplicated (global items first), objects are recursively merged,
# scalars from project override global.
merge_settings() {
  local global_file="${1}"
  local project_file="${2}"
  local global_json="{}"
  local project_json="{}"
  [[ -f "${global_file}" ]] && global_json=$(cat "${global_file}")
  [[ -f "${project_file}" ]] && project_json=$(cat "${project_file}")
  jq -n --argjson global "${global_json}" --argjson project "${project_json}" '
    def dedup: reduce .[] as $item ([]; if (map(. == $item) | any) then . else . + [$item] end);
    def deep_merge(a; b):
      if (a | type) == "object" and (b | type) == "object" then
        (a | keys_unsorted) as $ak | (b | keys_unsorted) as $bk |
        ([$ak[], $bk[]] | unique) | reduce .[] as $k ({};
          if ($ak | index($k)) and ($bk | index($k)) then
            . + { ($k): deep_merge(a[$k]; b[$k]) }
          elif ($bk | index($k)) then
            . + { ($k): b[$k] }
          else
            . + { ($k): a[$k] }
          end
        )
      elif (a | type) == "array" and (b | type) == "array" then
        (a + b) | dedup
      else
        b
      end;
    deep_merge($global; $project)
  '
}

# Resolve project directory to absolute path
resolve_absolute_project_dir() {
  local target_dir="${1:-.}"

  if [[ "${target_dir}" != /* ]]; then
    # Relative path
    target_dir="$(cd "${target_dir}" 2>/dev/null && pwd)" || {
      log_error "directory '${1}' does not exist"
      exit 1
    }
  else
    # Absolute path
    if [[ ! -d "${target_dir}" ]]; then
      log_error "directory '${target_dir}' does not exist"
      exit 1
    fi
    target_dir="$(cd "${target_dir}" && pwd)"
  fi

  echo "${target_dir}"
}

# Convert absolute path to valid Docker project name
create_project_name_from_project_path() {
  local path="${1}"

  local project_dir_basename
  project_dir_basename="$(basename "${path}")"
  local sanitized_project_dir_name
  # Remove leading slashes, replace / with -, lowercase, keep only valid chars
  sanitized_project_dir_name="$(echo "${project_dir_basename}" | sed 's/^\/*//' | tr '/' '-' | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-')"

  local sanitized_project_dir_path
  sanitized_project_dir_path="$(echo "${path}" | sed 's/^\/*//' | tr '/' '-' | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-')"
  local project_dir_path_hash
  project_dir_path_hash="$(echo -n "${sanitized_project_dir_path}" | sha1sum | awk '{print $1}' | cut -c1-8)"

  echo "${sanitized_project_dir_name}-${project_dir_path_hash}"
}

# Generate a random 6-character hex instance ID
generate_instance_id() {
  LC_ALL=C tr -dc 'a-z0-9' < /dev/urandom | head -c 6; echo
}

# Generate per-project docker-compose override file from merged settings
generate_instance_compose() {
  local agent="${1}"
  local project_dir="${2}"
  local instance_name="${3}"
  local merged_settings="${4}"
  local debug_mode="${5:-false}"

  local compose_dir="${HOLE_TMP_DIR}/projects/${instance_name}"
  local compose_file="${compose_dir}/docker-compose.yml"

  local agent_volumes=()
  local has_custom_domains=false
  local whitelist_file="${compose_dir}/tinyproxy-domain-whitelist.txt"

  # Read exclusions from merged settings and auto-detect files vs directories
  local entries
  entries=$(echo "${merged_settings}" | jq -r '.files.exclude[]? // empty' 2>/dev/null) || true
  while IFS= read -r line; do
    [[ -z "${line}" ]] && continue
    # Strip trailing slashes for consistent mount paths
    line="${line%/}"
    local full_path="${project_dir}/${line}"
    if [[ -f "${full_path}" ]]; then
      agent_volumes+=("      - /dev/null:/workspace/${line}:ro")
    elif [[ -d "${full_path}" ]]; then
      agent_volumes+=("      - /workspace/${line}")
    else
      log_warn "excluded path '${line}' not found in project, skipping"
    fi
  done <<< "${entries}"

  # Read file inclusions from merged settings (host_path -> container_path)
  local include_pairs
  include_pairs=$(echo "${merged_settings}" | jq -r '.files.include // {} | to_entries[] | "\(.key)\t\(.value)"' 2>/dev/null) || true
  while IFS=$'\t' read -r host_path container_path; do
    [[ -z "${host_path}" ]] && continue
    # Strip trailing slashes
    host_path="${host_path%/}"
    container_path="${container_path%/}"
    # Resolve host path
    if [[ "${host_path}" == "~/"* ]]; then
      host_path="${HOME}/${host_path#\~/}"
    elif [[ "${host_path}" != /* ]]; then
      host_path="${project_dir}/${host_path}"
    fi
    # Validate host path exists
    if [[ ! -e "${host_path}" ]]; then
      log_warn "included path '${host_path}' not found, skipping"
      continue
    fi
    agent_volumes+=("      - ${host_path}:${container_path}")
  done <<< "${include_pairs}"

  # Read domain whitelist from merged settings
  local domains
  domains=$(echo "${merged_settings}" | jq -r '.network.domainWhitelist[]? // empty' 2>/dev/null) || true
  if [[ -n "${domains}" ]]; then
    mkdir -p "${compose_dir}"
    # Start with default allowed domains
    cp "${SCRIPT_DIR}/proxy/allowed-domains.txt" "${whitelist_file}"
    # Append project-specific domains (escape dots for tinyproxy regex filter)
    printf '\n' >> "${whitelist_file}"
    echo "# Project-specific domains" >> "${whitelist_file}"
    while IFS= read -r domain; do
      [[ -z "${domain}" ]] && continue
      echo "${domain//./\\.}" >> "${whitelist_file}"
    done <<< "${domains}"
    has_custom_domains=true
  fi

  # Read dependencies from merged settings (installed at build time via EXTRA_PACKAGES arg)
  local deps
  deps=$(echo "${merged_settings}" | jq -r '.dependencies[]? // empty' 2>/dev/null) || true
  local extra_packages=""
  if [[ -n "${deps}" ]]; then
    extra_packages=$(echo "${deps}" | tr '\n' ' ' | sed 's/ *$//')
  fi

  # Read container memory settings from merged settings
  local agent_mem_limit
  agent_mem_limit=$(echo "${merged_settings}" | jq -r '.container.memoryLimit // empty' 2>/dev/null) || true
  local agent_memswap_limit
  agent_memswap_limit=$(echo "${merged_settings}" | jq -r '.container.memorySwapLimit // empty' 2>/dev/null) || true

  # Read setup hook script from merged settings
  local setup_script_path
  setup_script_path=$(echo "${merged_settings}" | jq -r '.hooks.setup.script // empty' 2>/dev/null) || true
  local has_setup_script=false

  if [[ -n "${setup_script_path}" ]]; then
    setup_script_path="${setup_script_path%/}"
    if [[ "${setup_script_path}" == "~/"* ]]; then
      setup_script_path="${HOME}/${setup_script_path#\~/}"
    elif [[ "${setup_script_path}" != /* ]]; then
      setup_script_path="${project_dir}/${setup_script_path}"
    fi
    if [[ -f "${setup_script_path}" ]]; then
      has_setup_script=true
    else
      log_warn "setup hook script '${setup_script_path}' not found, skipping"
    fi
  fi

  if [[ "${has_setup_script}" == true ]]; then
    mkdir -p "${compose_dir}"
    cp "${setup_script_path}" "${compose_dir}/setup.sh"
  fi

  if [[ ${#agent_volumes[@]} -gt 0 || "${has_custom_domains}" == true || -n "${agent_mem_limit}" || -n "${agent_memswap_limit}" || -n "${extra_packages}" || "${has_setup_script}" == true || "${debug_mode}" == true ]]; then
    mkdir -p "${compose_dir}"
    {
      echo "services:"
      if [[ "${has_custom_domains}" == true ]]; then
        echo "  proxy:"
        echo "    volumes:"
        echo "      - ${compose_dir}/tinyproxy-domain-whitelist.txt:/etc/tinyproxy/allowed-domains.txt:ro"
      fi
      if [[ ${#agent_volumes[@]} -gt 0 || -n "${agent_mem_limit}" || -n "${agent_memswap_limit}" || -n "${extra_packages}" || "${has_setup_script}" == true || "${debug_mode}" == true ]]; then
        echo "  ${agent}:"
        if [[ -n "${extra_packages}" || "${has_setup_script}" == true ]]; then
          echo "    build:"
          if [[ -n "${extra_packages}" ]]; then
            echo "      args:"
            echo "        EXTRA_PACKAGES: \"${extra_packages}\""
          fi
          if [[ "${has_setup_script}" == true ]]; then
            echo "      additional_contexts:"
            echo "        setup-context: ${compose_dir}"
          fi
        fi
        if [[ -n "${agent_mem_limit}" ]]; then
          echo "    mem_limit: ${agent_mem_limit}"
        fi
        if [[ -n "${agent_memswap_limit}" ]]; then
          echo "    memswap_limit: ${agent_memswap_limit}"
        fi
        if [[ "${debug_mode}" == true ]]; then
          echo "    command: [\"bash\"]"
        fi
        if [[ ${#agent_volumes[@]} -gt 0 ]]; then
          echo "    volumes:"
          for v in "${agent_volumes[@]}"; do
            echo "${v}"
          done
        fi
      fi
    } > "${compose_file}"
  else
    # No overrides — remove stale files if any
    rm -f "${compose_file}"
    rm -f "${whitelist_file}"
    rmdir "${compose_dir}" 2>/dev/null || true
  fi

  echo "${compose_file}"
}

# Build the docker compose command array, including agent and optional override file
create_compose_cmd() {
  local instance_name="${1}"
  local agent="${2}"
  local project_compose_file="${3}"

  local proxy_compose_file="${SCRIPT_DIR}/proxy/docker-compose.yml"
  local agent_compose_file="${SCRIPT_DIR}/agents/${agent}/docker-compose.yml"

  COMPOSE_CMD=(docker compose -p "${instance_name}" -f "${COMPOSE_FILE}" -f "${proxy_compose_file}" -f "${agent_compose_file}")
  if [[ -f "${project_compose_file}" ]]; then
    COMPOSE_CMD+=(-f "${project_compose_file}")
  fi
}

# Ensure the persistent agent home volume exists
ensure_agent_volume() {
  local agent="${1}"
  local volume_name="hole-agent-home-${agent}"
  if ! docker volume inspect "${volume_name}" >/dev/null 2>&1; then
    log_info "Creating persistent volume: ${volume_name}"
    docker volume create "${volume_name}"
  fi
}

# Start command: create/start sandbox and attach to agent CLI
cmd_start() {
  local agent="${1}"
  local project_dir="${2}"
  local project_name=${3}
  local instance_id=${4}
  local instance_name=${5}
  local dump_network_access="${6:-false}"
  local debug_mode="${7:-false}"
   local rebuild="${8:-false}"

    local build_flag=()
    if [[ "${rebuild}" == true ]]; then
      build_flag=(--build)
    fi

  # Validate settings files if present
  validate_settings "${GLOBAL_SETTINGS_FILE}" "global settings (~/.hole/settings.json)"
  validate_settings "${project_dir}/.hole/settings.json" "project settings (.hole/settings.json)"

  # Merge global and project settings
  local merged_settings
  merged_settings=$(merge_settings "${GLOBAL_SETTINGS_FILE}" "${project_dir}/.hole/settings.json")

  # Generate per-project compose override from merged settings
  local project_compose_file
  project_compose_file=$(generate_instance_compose "${agent}" "${project_dir}" "${instance_name}" "${merged_settings}" "${debug_mode}")
  create_compose_cmd "${instance_name}" "${agent}" "${project_compose_file}"

  if [[ "${debug_mode}" == true ]]; then
    log_warn "Debug mode: opening bash shell instead of agent CLI"
    log_line
  fi
  log_info "Launching sandbox for: ${project_dir}"
  log_info "Project name: ${project_name}"
  log_info "Instance ID: ${instance_id}"
  log_line

  check_for_update

  # Ensure persistent agent home volume exists
  ensure_agent_volume "${agent}"

  # Expose project name and directory for docker-compose.yml
  export PROJECT_NAME="${project_name}"
  export PROJECT_DIR="${project_dir}"

  # Start proxy in detached mode with health check wait
  log_info "Starting proxy..."
  "${COMPOSE_CMD[@]}" up -d ${build_flag[@]+"${build_flag[@]}"} proxy

  # Start agent service
  log_info "Starting ${agent} agent..."
  log_line
  "${COMPOSE_CMD[@]}" up -d ${build_flag[@]+"${build_flag[@]}"} "${agent}"

  # Attach terminal to the running agent
  log_info "Attaching to ${agent} agent..."
  log_line
  docker attach "${instance_name}-${agent}-1"

  # Dump network access log if requested
  if [[ "${dump_network_access}" == true ]]; then
    local log_file="${project_dir}/${agent}-network-access-${instance_id}.log"
    docker logs "${instance_name}-proxy-1" 2>&1 | \
      grep -oE 'CONNECT [a-zA-Z0-9._-]+:[0-9]+|filtered url "[^"]+"' | \
      sed 's/CONNECT //; s/:[0-9]*$//; s/^filtered url "//; s/"$//' | \
      sort -u > "${log_file}" || true
    log_line
    log_info "Network access log written to: ${log_file}"
  fi

  # Destroy the sandbox after user exits
  "${COMPOSE_CMD[@]}" down --remove-orphans

  # Clean up per-project compose override
  local compose_dir="${HOLE_TMP_DIR}/projects/${instance_name}"
  if [[ -d "${compose_dir}" ]]; then
    rm -rf "${compose_dir}"
  fi

  log_line
  log_info "Exited ${agent} CLI. Sandbox destroyed."
}

# Destroy command: remove all cached Docker resources for a project
cmd_destroy() {
  local project_dir="${1}"
  local project_name="${2}"

  log_info "Destroying cached resources for project: ${project_dir}"
  log_info "Project name: ${project_name}"
  log_line

  # Stop running containers for this project
  local running_containers
  running_containers=$(docker ps -q --filter "name=hole-${project_name}-") || true
  if [[ -n "${running_containers}" ]]; then
    log_info "Stopping running containers..."
    docker stop ${running_containers} || log_warn "Failed to stop some containers"
  else
    log_info "No running containers found"
  fi

  # Remove all containers (running or stopped) for this project
  local all_containers
  all_containers=$(docker ps -aq --filter "name=hole-${project_name}-") || true
  if [[ -n "${all_containers}" ]]; then
    log_info "Removing containers..."
    docker rm -f ${all_containers} || log_warn "Failed to remove some containers"
  else
    log_info "No containers found"
  fi

  # Remove networks for this project
  local networks
  networks=$(docker network ls -q --filter "name=hole-${project_name}-") || true
  if [[ -n "${networks}" ]]; then
    log_info "Removing networks..."
    docker network rm ${networks} || log_warn "Failed to remove some networks"
  else
    log_info "No networks found"
  fi

  # Remove cached agent images for all agent types
  for agent in "${VALID_AGENTS[@]}"; do
    local agent_image="hole-sandboxes/agent-${agent}-${project_name}:latest"
    if docker image inspect "${agent_image}" >/dev/null 2>&1; then
      log_info "Removing agent image: ${agent_image}"
      docker rmi "${agent_image}" || log_warn "Failed to remove image ${agent_image}"
    else
      log_info "No cached image found for agent '${agent}'"
    fi
  done

  # Remove cached proxy image
  local proxy_image="hole-sandboxes/proxy-${project_name}:latest"
  if docker image inspect "${proxy_image}" >/dev/null 2>&1; then
    log_info "Removing proxy image: ${proxy_image}"
    docker rmi "${proxy_image}" || log_warn "Failed to remove image ${proxy_image}"
  else
    log_info "No cached proxy image found"
  fi

  # Remove temp files for this project
  local tmp_pattern="${HOLE_TMP_DIR}/projects/hole-${project_name}-*"
  local tmp_dirs
  tmp_dirs=$(compgen -G "${tmp_pattern}" 2>/dev/null) || true
  if [[ -n "${tmp_dirs}" ]]; then
    log_info "Removing temp files..."
    rm -rf ${tmp_pattern} || log_warn "Failed to remove some temp files"
  else
    log_info "No temp files found"
  fi

  log_line
  log_info "Cached resources destroyed. Shared agent home volumes were preserved."
}

# Print installed version
cmd_version() {
  local version_file="${SCRIPT_DIR}/version"
  if [[ -f "${version_file}" ]]; then
    echo "hole $(cat "${version_file}")"
  else
    echo "hole development (no version file)"
  fi
  check_for_update
}

# Fetch latest release version from GitHub API
# Args: $1 = timeout in seconds (default 10)
# Prints version string (without v prefix) on success, returns 1 on failure
fetch_latest_version() {
  local timeout="${1:-10}"
  local response=""

  if command -v curl >/dev/null 2>&1; then
    response=$(curl -fsSL --max-time "${timeout}" "${GITHUB_API}" 2>/dev/null) || return 1
  elif command -v wget >/dev/null 2>&1; then
    response=$(wget -qO- --timeout="${timeout}" "${GITHUB_API}" 2>/dev/null) || return 1
  else
    return 1
  fi

  local tag
  tag=$(echo "${response}" | jq -r '.tag_name // empty' 2>/dev/null) || return 1
  [[ -z "${tag}" ]] && return 1

  # Strip v prefix
  echo "${tag#v}"
}

# Compare two semver strings; returns 0 if $1 > $2
version_gt() {
  local IFS='.'
  # Intentional word-splitting on IFS='.' to split version components
  local -a v1=($1) v2=($2)
  local len="${#v1[@]}"
  (( ${#v2[@]} > len )) && len="${#v2[@]}"

  for (( i=0; i<len; i++ )); do
    local n1="${v1[i]:-0}"
    local n2="${v2[i]:-0}"
    if (( n1 > n2 )); then
      return 0
    elif (( n1 < n2 )); then
      return 1
    fi
  done
  return 1
}

# Silent version check (1s timeout). Prints notice if a newer version is available.
check_for_update() {
  local version_file="${SCRIPT_DIR}/version"
  # Skip in dev mode (no version file)
  [[ ! -f "${version_file}" ]] && return 0

  local installed
  installed=$(cat "${version_file}")
  local latest
  latest=$(fetch_latest_version 1) || return 0

  if version_gt "${latest}" "${installed}"; then
    log_info "A new version of hole is available: ${latest} (installed: ${installed}). Run 'hole update' to upgrade."
  fi
}

# Update hole to the latest release
cmd_update() {
  local version_file="${SCRIPT_DIR}/version"
  if [[ ! -f "${version_file}" ]]; then
    log_error "cannot update a development installation (no version file)"
    exit 1
  fi

  local installed
  installed=$(cat "${version_file}")

  log_info "Checking for updates..."
  local latest
  latest=$(fetch_latest_version) || {
    log_error "failed to check for latest version"
    exit 1
  }

  if version_gt "${latest}" "${installed}"; then
    log_info "Updating hole: ${installed} -> ${latest}"

    local uninstall_script="${SCRIPT_DIR}/uninstall.sh"
    if [[ ! -f "${uninstall_script}" ]]; then
      log_error "uninstall script not found at ${uninstall_script}"
      exit 1
    fi

    # Copy uninstall.sh to a temp file so it can delete INSTALL_DIR safely
    local tmp_uninstall
    tmp_uninstall="$(mktemp "${TMPDIR:-/tmp}/hole-uninstall.XXXXXX")"
    cp "${uninstall_script}" "${tmp_uninstall}"
    chmod +x "${tmp_uninstall}"

    # Create a wrapper script that runs uninstall then reinstall
    local tmp_wrapper
    tmp_wrapper="$(mktemp "${TMPDIR:-/tmp}/hole-update.XXXXXX")"
    cat > "${tmp_wrapper}" <<WRAPPER
#!/usr/bin/env bash
set -euo pipefail
trap 'rm -f "${tmp_uninstall}" "${tmp_wrapper}"' EXIT
bash "${tmp_uninstall}"
curl -fsSL "${GITHUB_INSTALL_SCRIPT}" | bash
WRAPPER
    chmod +x "${tmp_wrapper}"

    # exec replaces hole.sh process so INSTALL_DIR can be safely deleted
    exec bash "${tmp_wrapper}"
  else
    log_info "hole is already up to date (version ${installed})."
  fi
}

# Uninstall hole by exec-ing into a temp copy of uninstall.sh
cmd_uninstall() {
  local uninstall_script="${SCRIPT_DIR}/uninstall.sh"
  if [[ ! -f "${uninstall_script}" ]]; then
    log_error "uninstall script not found at ${uninstall_script}"
    exit 1
  fi

  # Copy to temp file and exec into it. exec replaces this process,
  # so hole.sh is no longer running when the copy deletes INSTALL_DIR.
  local tmp_file
  tmp_file="$(mktemp "${TMPDIR:-/tmp}/hole-uninstall.XXXXXX")"
  cp "${uninstall_script}" "${tmp_file}"
  chmod +x "${tmp_file}"
  trap 'rm -f "${tmp_file}"' EXIT
  exec bash "${tmp_file}"
}

# Main entry point
main() {
  local dump_network_access=false
  local debug_mode=false
  local rebuild=false
  local positional=()

  for arg in "$@"; do
    case "$arg" in
      --debug) debug_mode=true ;;
      --dump-network-access) dump_network_access=true ;;
      --rebuild) rebuild=true ;;
      *) positional+=("${arg}") ;;
    esac
  done

  # Parse positional arguments with defaults
  local command="${positional[0]:-}"
  local agent="${positional[1]:-}"
  local target_dir="${positional[2]:-.}"

  # Validate inputs
  validate_command "${command}"

  # Handle top-level commands (no agent required)
  if [[ "${command}" == "version" ]]; then
    cmd_version
    exit 0
  fi
  if [[ "${command}" == "update" ]]; then
    cmd_update
    exit 0
  fi
  if [[ "${command}" == "uninstall" ]]; then
    cmd_uninstall
    exit 0
  fi
  if [[ "${command}" == "help" ]] || [[ -z "${command}" ]]; then
    show_help
    exit 0
  fi

  # Resolve path
  local project_dir
  project_dir=$(resolve_absolute_project_dir "${target_dir}")

  # Generate project name and export environment
  local project_name
  project_name=$(create_project_name_from_project_path "${project_dir}")
  local instance_id
  instance_id=$(generate_instance_id)
  local instance_name
  instance_name="hole-${project_name}-${instance_id}"

  # Handle start command (no agent argument required)
  if [[ "${command}" == "start" ]]; then
    validate_agent "${agent}"

    cmd_start "${agent}" "${project_dir}" "${project_name}" "${instance_id}" "${instance_name}" "${dump_network_access}" "${debug_mode}" "${rebuild}"
    exit 0
  fi

  # Handle destroy command (no agent argument required)
  if [[ "${command}" == "destroy" ]]; then
    cmd_destroy "${project_dir}" "${project_name}"
    exit 0
  fi

  log_error "Unknown command: ${command}"
  exit 1
}

main "$@"
