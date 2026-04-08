#!/usr/bin/env bash
set -euo pipefail

# Constants
SCRIPT_DIR="$(cd "$(dirname "${0}")" && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
HOLE_TMP_DIR="" # set later in cmd_start
GLOBAL_SETTINGS_FILE="${HOME}/.hole/settings.json"
GITHUB_REPO="lukashornych/hole"
GITHUB_API="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
GITHUB_INSTALL_SCRIPT="https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh"
VALID_AGENTS=("claude" "gemini" "codex")
VALID_COMMANDS=("start" "destroy" "help" "version" "update" "uninstall")

# Import the logging library
source "${SCRIPT_DIR}/logger.sh"
# Import utility functions
source "${SCRIPT_DIR}/utils.sh"

# Detect container runtime (docker or podman)
# Priority: $HOLE_RUNTIME env var → docker → podman → error
detect_container_runtime() {
  if [[ -n "${HOLE_RUNTIME:-}" ]]; then
    if ! command -v "${HOLE_RUNTIME}" >/dev/null 2>&1; then
      log_error "HOLE_RUNTIME is set to '${HOLE_RUNTIME}' but it is not installed or not in PATH"
      exit 1
    fi
    CONTAINER_RUNTIME="${HOLE_RUNTIME}"
  elif command -v docker >/dev/null 2>&1; then
    CONTAINER_RUNTIME="docker"
  elif command -v podman >/dev/null 2>&1; then
    CONTAINER_RUNTIME="podman"
  else
    log_error "neither docker nor podman is installed or in PATH"
    exit 1
  fi

  # Validate compose subcommand works
  if ! "${CONTAINER_RUNTIME}" compose version >/dev/null 2>&1; then
    log_error "'${CONTAINER_RUNTIME} compose' is not available. Please install the compose plugin."
    exit 1
  fi
}

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
  gemini    Gemini CLI agent
  codex     Codex CLI agent

Options:
  -d, --debug                 Open a bash shell instead of the agent CLI for
                                  inspecting the sandbox environment
  -n, --dump-network-access   After the agent exits, write distinct accessed domains
                                  to {agent}-network-access-{id}.log in the project directory
  -r, --rebuild               Force rebuild of Docker images before starting
  -u, --unrestricted-network  Disable domain whitelist filtering; allow all network access
      --with-docker           Enable Docker-in-Docker sidecar for the sandbox
  --                      Separator for agent-specific arguments;
                              everything after -- is passed to the agent CLI

Configure file exclusions, inclusions, libraries, domain whitelist,
dependencies, environment variables, container settings and hooks via .hole/settings.json
(per-project) or ~/.hole/settings.json (global).

Examples:
  hole start claude .
  hole start claude /path/to/project
  hole start claude . --dump-network-access
  hole start claude . -- -p "explain this function"
  hole start claude . --rebuild -- --output-format stream-json
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
  require_cmd "jv"
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
  require_cmd "jq"
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
  require_cmd "sha1sum"

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

# Find the first available /24 subnet index in the 172.28.{1-254}.0/24 range
# by inspecting all existing Docker networks.
find_available_subnet_index() {
  local used_subnets
  used_subnets=$("${CONTAINER_RUNTIME}" network ls -q 2>/dev/null \
    | xargs -r "${CONTAINER_RUNTIME}" network inspect --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null) || true

  local index
  for index in $(seq 1 254); do
    if [[ "${used_subnets}" != *"172.28.${index}.0/24"* ]]; then
      echo "${index}"
      return 0
    fi
  done

  log_error "No available subnet found (all 254 slots in 172.28.x.0/24 are in use)"
  exit 1
}

# Check if a string contains glob metacharacters (*, ?, [)
has_glob_chars() {
  [[ "${1}" == *[\*\?\[]* ]]
}

# Expand environment variables ($VAR and ${VAR}) in a string using indirect expansion.
# Undefined variables produce a warning and are left unexpanded.
expand_env_vars() {
  local input="${1}"
  local result="${input}"

  # Replace ${VAR_NAME} patterns first
  while [[ "${result}" =~ \$\{([a-zA-Z_][a-zA-Z0-9_]*)\} ]]; do
    local var_name="${BASH_REMATCH[1]}"
    local full_match="\${${var_name}}"
    if [[ -n "${!var_name+x}" ]]; then
      result="${result//"${full_match}"/${!var_name}}"
    else
      log_warn "undefined environment variable '\${${var_name}}', leaving unexpanded"
      # Use placeholder to avoid infinite loop on undefined vars
      result="${result//"${full_match}"/__HOLE_UNDEF_BRACE_${var_name}__}"
    fi
  done

  # Replace $VAR_NAME patterns (not followed by {, already handled above)
  while [[ "${result}" =~ \$([a-zA-Z_][a-zA-Z0-9_]*) ]]; do
    local var_name="${BASH_REMATCH[1]}"
    local full_match="\$${var_name}"
    if [[ -n "${!var_name+x}" ]]; then
      result="${result//"${full_match}"/${!var_name}}"
    else
      log_warn "undefined environment variable '\$${var_name}', leaving unexpanded"
      result="${result//"${full_match}"/__HOLE_UNDEF_${var_name}__}"
    fi
  done

  # Restore undefined var placeholders to original syntax
  while [[ "${result}" =~ __HOLE_UNDEF_BRACE_([a-zA-Z_][a-zA-Z0-9_]*)__ ]]; do
    local var_name="${BASH_REMATCH[1]}"
    result="${result//__HOLE_UNDEF_BRACE_${var_name}__/\$\{${var_name}\}}"
  done
  while [[ "${result}" =~ __HOLE_UNDEF_([a-zA-Z_][a-zA-Z0-9_]*)__ ]]; do
    local var_name="${BASH_REMATCH[1]}"
    result="${result//__HOLE_UNDEF_${var_name}__/\$${var_name}}"
  done

  echo "${result}"
}

# Resolve a host path: expand ~/, resolve relative paths against base_dir, strip trailing slashes
resolve_host_path() {
  local raw_path="${1}"
  local base_dir="${2}"
  raw_path="${raw_path%/}"
  raw_path=$(expand_env_vars "${raw_path}")
  if [[ "${raw_path}" == "~/"* ]]; then
    echo "${HOME}/${raw_path#\~/}"
  elif [[ "${raw_path}" != /* ]]; then
    echo "${base_dir}/${raw_path}"
  else
    echo "${raw_path}"
  fi
}

# Resolve file exclusion entries (literal paths + globs) and output volume mount lines.
# Args: $1 = source directory, $2 = mount point prefix, $3 = newline-separated entries
# Outputs volume mount lines to stdout, one per line.
resolve_file_exclusions() {
  local source_dir="${1}"
  local mount_point="${2}"
  local entries="${3}"

  local -a resolved_paths=()
  while IFS= read -r line; do
    [[ -z "${line}" ]] && continue
    # Strip trailing slashes for consistent mount paths
    line="${line%/}"
    line=$(expand_env_vars "${line}")
    if has_glob_chars "${line}"; then
      # Expand glob pattern in a subshell to isolate shopt changes
      local -a matches=()
      while IFS= read -r match; do
        [[ -n "${match}" ]] && matches+=("${match}")
      done < <(cd "${source_dir}" && bash -c 'shopt -s globstar nullglob; eval "for f in $1; do printf \"%s\n\" \"\$f\"; done"' _ "${line}" 2>/dev/null)
      if [[ ${#matches[@]} -eq 0 ]]; then
        log_warn "excluded pattern '${line}' matched no paths, skipping"
      else
        resolved_paths+=("${matches[@]}")
      fi
    else
      local full_path="${source_dir}/${line}"
      if [[ -e "${full_path}" ]]; then
        resolved_paths+=("${line}")
      else
        log_warn "excluded path '${line}' not found in ${source_dir}, skipping"
      fi
    fi
  done <<< "${entries}"

  # Deduplicate resolved paths and generate volume mounts
  local seen_excludes=""
  for resolved in ${resolved_paths[@]+"${resolved_paths[@]}"}; do
    [[ -z "${resolved}" ]] && continue
    # Strip trailing slashes again (glob expansion may include them)
    resolved="${resolved%/}"
    # Skip duplicates
    case "${seen_excludes}" in
      *"|${resolved}|"*) continue ;;
    esac
    seen_excludes="${seen_excludes}|${resolved}|"
    local full_path="${source_dir}/${resolved}"
    if [[ -f "${full_path}" ]]; then
      echo "      - /dev/null:${mount_point}/${resolved}:ro"
    elif [[ -d "${full_path}" ]]; then
      echo "      - ${mount_point}/${resolved}"
    else
      log_warn "excluded path '${resolved}' not found in ${source_dir}, skipping"
    fi
  done
}

# Read the base command for an agent from its command.json config file.
# Args: $1 = agent name
# Outputs command parts to stdout, one per line.
get_agent_base_command() {
  local agent="${1}"
  local command_file="${SCRIPT_DIR}/agents/${agent}/command.json"
  if [[ ! -f "${command_file}" ]]; then
    log_error "command.json not found for agent '${agent}'"
    exit 1
  fi
  jq -r '.[]' "${command_file}"
}

# Get enabled agents from merged settings. Defaults to all valid agents.
# Args: $1 = merged settings JSON
# Outputs agent names to stdout, one per line.
get_enabled_agents() {
  local merged_settings="${1}"
  local enabled
  enabled=$(echo "${merged_settings}" | jq -r '.container.enabledAgents // empty | .[]' 2>/dev/null) || true
  if [[ -z "${enabled}" ]]; then
    printf '%s\n' "${VALID_AGENTS[@]}"
  else
    echo "${enabled}"
  fi
}

# Generate per-project docker-compose override file from merged settings
generate_instance_compose() {
  local agent="${1}"
  local project_dir="${2}"
  local instance_name="${3}"
  local merged_settings="${4}"
  local debug_mode="${5:-false}"
  local unrestricted_network="${6:-false}"
  local with_docker="${7:-false}"
  local subnet="${8}"
  local dns_ip="${9}"
  local agent_args=("${@:10}")

  local compose_file="${HOLE_TMP_DIR}/docker-compose.yml"

  local agent_volumes=()
  # Whitelist is always generated (default + agent-specific + user + host-gateway domains)
  local whitelist_file="${HOLE_TMP_DIR}/tinyproxy-domain-whitelist.txt"

  # Read exclusions from merged settings and resolve to volume mounts
  local entries
  entries=$(echo "${merged_settings}" | jq -r '.files.exclude[]? // empty' 2>/dev/null) || true
  if [[ -n "${entries}" ]]; then
    while IFS= read -r mount_line; do
      [[ -n "${mount_line}" ]] && agent_volumes+=("${mount_line}")
    done < <(resolve_file_exclusions "${project_dir}" "${project_dir}" "${entries}")
  fi

  # Read file inclusions from merged settings (host_path -> container_path)
  local include_pairs
  include_pairs=$(echo "${merged_settings}" | jq -r '.files.include // {} | to_entries[] | "\(.key)\t\(.value)"' 2>/dev/null) || true
  while IFS=$'\t' read -r host_path container_path; do
    [[ -z "${host_path}" ]] && continue
    # Strip trailing slashes
    host_path="${host_path%/}"
    container_path="${container_path%/}"
    container_path=$(expand_env_vars "${container_path}")
    # Expand tilde in container path using sandbox home
    if [[ "${container_path}" == "~/"* ]]; then
      container_path="${SANDBOX_HOME:-/home/agent}/${container_path#\~/}"
    fi
    # Resolve host path
    host_path=$(resolve_host_path "${host_path}" "${project_dir}")
    # Validate host path exists
    if [[ ! -e "${host_path}" ]]; then
      log_warn "included path '${host_path}' not found, skipping"
      continue
    fi
    agent_volumes+=("      - ${host_path}:${container_path}")
  done <<< "${include_pairs}"

  # Process libraries: mount read-only (default) or read-write + apply per-library file exclusions
  local lib_pairs
  lib_pairs=$(echo "${merged_settings}" | jq -r '
    .libraries // {} | to_entries[] |
    if (.value | type) == "string" then "\(.key)\t\(.value)\tfalse"
    else "\(.key)\t\(.value.path)\t\(.value.readwrite // false)"
    end
  ' 2>/dev/null) || true
  while IFS=$'\t' read -r lib_host_path lib_container_path lib_readwrite; do
    [[ -z "${lib_host_path}" ]] && continue
    lib_host_path="${lib_host_path%/}"
    lib_container_path="${lib_container_path%/}"
    lib_container_path=$(expand_env_vars "${lib_container_path}")
    # Expand tilde in container path using sandbox home
    if [[ "${lib_container_path}" == "~/"* ]]; then
      lib_container_path="${SANDBOX_HOME:-/home/agent}/${lib_container_path#\~/}"
    fi
    lib_host_path=$(resolve_host_path "${lib_host_path}" "${project_dir}")
    if [[ ! -d "${lib_host_path}" ]]; then
      log_warn "library '${lib_host_path}' not found or not a directory, skipping"
      continue
    fi
    if [[ "${lib_readwrite}" == "true" ]]; then
      agent_volumes+=("      - ${lib_host_path}:${lib_container_path}")
    else
      agent_volumes+=("      - ${lib_host_path}:${lib_container_path}:ro")
    fi
    # Apply library's own .hole/settings.json file exclusions (scoped to library mount)
    local lib_settings_file="${lib_host_path}/.hole/settings.json"
    if [[ -f "${lib_settings_file}" ]]; then
      validate_settings "${lib_settings_file}" "library settings (${lib_settings_file})"
      local lib_entries
      lib_entries=$(jq -r '.files.exclude[]? // empty' "${lib_settings_file}" 2>/dev/null) || true
      if [[ -n "${lib_entries}" ]]; then
        while IFS= read -r mount_line; do
          [[ -n "${mount_line}" ]] && agent_volumes+=("${mount_line}")
        done < <(resolve_file_exclusions "${lib_host_path}" "${lib_container_path}" "${lib_entries}")
      fi
    fi
  done <<< "${lib_pairs}"

  # Read host gateway domains early (needed for both whitelist and Corefile)
  local host_gateway_domains
  host_gateway_domains=$(echo "${merged_settings}" | jq -r '.network.hostGatewayDomains[]? // empty' 2>/dev/null) || true
  local has_host_gateway_domains=false
  if [[ -n "${host_gateway_domains}" ]]; then
    has_host_gateway_domains=true
  fi

  # Build merged whitelist: default + all enabled agents' domains + user domains + host gateway domains
  local domains
  domains=$(echo "${merged_settings}" | jq -r '.network.domainWhitelist[]? // empty' 2>/dev/null) || true
  # Start with default allowed domains
  cp "${SCRIPT_DIR}/proxy/allowed-domains.txt" "${whitelist_file}"
  # Append domains from all enabled agents
  local -a enabled_agents_list=()
  while IFS= read -r ea; do
    [[ -n "${ea}" ]] && enabled_agents_list+=("${ea}")
  done < <(get_enabled_agents "${merged_settings}")
  for enabled_agent in "${enabled_agents_list[@]}"; do
    local agent_whitelist="${SCRIPT_DIR}/agents/${enabled_agent}/allowed-domains.txt"
    if [[ -f "${agent_whitelist}" ]]; then
      printf '\n' >> "${whitelist_file}"
      cat "${agent_whitelist}" >> "${whitelist_file}"
    fi
  done
  # Append user-defined domains (from settings.json)
  if [[ -n "${domains}" ]]; then
    printf '\n' >> "${whitelist_file}"
    echo "# Project-specific domains" >> "${whitelist_file}"
    while IFS= read -r domain; do
      [[ -z "${domain}" ]] && continue
      echo "${domain//./\\.}" >> "${whitelist_file}"
    done <<< "${domains}"
  fi
  # Append host gateway domains (auto-whitelisted so proxy allows traffic to them)
  if [[ "${has_host_gateway_domains}" == true ]]; then
    printf '\n' >> "${whitelist_file}"
    echo "# Host gateway domains" >> "${whitelist_file}"
    while IFS= read -r domain; do
      [[ -z "${domain}" ]] && continue
      echo "${domain//./\\.}" >> "${whitelist_file}"
    done <<< "${host_gateway_domains}"
  fi

  # Build CoreDNS Corefile: custom domain blocks + catch-all forward
  local corefile="${HOLE_TMP_DIR}/Corefile"

  if [[ "${has_host_gateway_domains}" == true ]]; then
    # Validate domain names before generating Corefile
    while IFS= read -r domain; do
      [[ -z "${domain}" ]] && continue
      if [[ ! "${domain}" =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ ]]; then
        log_error "Invalid hostGatewayDomains entry: '${domain}' — must be a valid domain name (no ports, spaces, or special characters)"
        exit 1
      fi
      if [[ "${domain}" == "localhost" || "${domain}" == "127.0.0.1" ]]; then
        log_warn "hostGatewayDomains entry '${domain}' is in the agent's NO_PROXY list and will bypass the proxy, preventing host access. Use a different domain name instead."
      fi
    done <<< "${host_gateway_domains}"
    {
      while IFS= read -r domain; do
        [[ -z "${domain}" ]] && continue
        cat <<BLOCK
${domain}:53 {
    template IN A ${domain} {
        answer "{{ .Name }} 60 IN A {HOST_GATEWAY_IP}"
    }
    template IN AAAA ${domain} {
      rcode NOERROR
    }
    log
    errors
}

BLOCK
      done <<< "${host_gateway_domains}"
      cat <<BLOCK
.:53 {
    forward . 127.0.0.11
    log
    errors
}
BLOCK
    } > "${corefile}"
  else
    cp "${SCRIPT_DIR}/dns/Corefile" "${corefile}"
  fi

  # Copy dns entrypoint to build context
  cp "${SCRIPT_DIR}/dns/entrypoint.sh" "${HOLE_TMP_DIR}/dns-entrypoint.sh"

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

  # Read Docker-in-Docker setting (from settings or --with-docker flag)
  local docker_enabled
  docker_enabled=$(echo "${merged_settings}" | jq -r '.container.docker // empty' 2>/dev/null) || true
  if [[ "${with_docker}" == "true" ]]; then
    docker_enabled="true"
  fi

  # Read custom base image setting
  local base_image
  base_image=$(echo "${merged_settings}" | jq -r '.container.baseImage // empty' 2>/dev/null) || true

  # Read setup hook script from merged settings
  local setup_script_path
  setup_script_path=$(echo "${merged_settings}" | jq -r '.hooks.setup.script // empty' 2>/dev/null) || true
  local has_setup_script=false

  if [[ -n "${setup_script_path}" ]]; then
    setup_script_path=$(resolve_host_path "${setup_script_path}" "${project_dir}")
    if [[ -f "${setup_script_path}" ]]; then
      has_setup_script=true
    else
      log_warn "setup hook script '${setup_script_path}' not found, skipping"
    fi
  fi

  # Copy entrypoint.sh to build context
  mkdir -p "${HOLE_TMP_DIR}"
  cp "${SCRIPT_DIR}/agents/entrypoint.sh" "${HOLE_TMP_DIR}/entrypoint.sh"

  # Always create setup-scripts dir in temp (even if empty) so Dockerfile COPY works
  mkdir -p "${HOLE_TMP_DIR}/setup-scripts"
  touch "${HOLE_TMP_DIR}/setup-scripts/.gitkeep"
  if [[ "${has_setup_script}" == true ]]; then
    cp "${setup_script_path}" "${HOLE_TMP_DIR}/setup-scripts/setup.sh"
  fi

  # Copy enabled agents' install scripts to build context
  mkdir -p "${HOLE_TMP_DIR}/agent-installs"
  for enabled_agent in "${enabled_agents_list[@]}"; do
    local agent_src="${SCRIPT_DIR}/agents/${enabled_agent}"
    local agent_dst="${HOLE_TMP_DIR}/agent-installs/${enabled_agent}"
    mkdir -p "${agent_dst}"
    [[ -f "${agent_src}/install-root.sh" ]] && cp "${agent_src}/install-root.sh" "${agent_dst}/"
    [[ -f "${agent_src}/install-user.sh" ]] && cp "${agent_src}/install-user.sh" "${agent_dst}/"
  done

  # Read environment variables from merged settings
  local env_pairs
  env_pairs=$(echo "${merged_settings}" | jq -r '.environment // {} | to_entries[] | "\(.key)\t\(.value)"' 2>/dev/null) || true
  local -a agent_env_vars=()
  while IFS=$'\t' read -r env_key env_value; do
    [[ -z "${env_key}" ]] && continue
    agent_env_vars+=("      - ${env_key}=${env_value}")
  done <<< "${env_pairs}"
  local -a user_env_vars=("${agent_env_vars[@]}")

  # Add Docker-in-Docker env vars for the agent
  if [[ "${docker_enabled}" == "true" ]]; then
    agent_env_vars+=("      - DOCKER_HOST=tcp://docker:2375")
    agent_env_vars+=("      - NO_PROXY=localhost,127.0.0.1,docker")
    agent_env_vars+=("      - no_proxy=localhost,127.0.0.1,docker")
  fi

  local has_agent_args=false
  if [[ ${#agent_args[@]} -gt 0 ]]; then
    has_agent_args=true
  fi

  {
    echo "services:"
    echo "  proxy:"
    echo "    dns:"
    echo "      - ${dns_ip}"
    echo "      - 127.0.0.11"
    echo "    volumes:"
    if [[ "${unrestricted_network}" == true ]]; then
      echo "      - ${SCRIPT_DIR}/proxy/tinyproxy-unrestricted.conf:/etc/tinyproxy/tinyproxy.conf:ro"
    fi
    echo "      - ${HOLE_TMP_DIR}/tinyproxy-domain-whitelist.txt:/etc/tinyproxy/allowed-domains.txt:ro"
    if [[ "${has_host_gateway_domains}" == true ]]; then
      echo "    extra_hosts:"
      while IFS= read -r domain; do
        [[ -z "${domain}" ]] && continue
        echo "      - \"${domain}:host-gateway\""
      done <<< "${host_gateway_domains}"
    fi
    echo "  dns:"
    echo "    build:"
    echo "      context: ${HOLE_TMP_DIR}"
    echo "      dockerfile: ${SCRIPT_DIR}/dns/Dockerfile"
    echo "    volumes:"
    echo "      - ${HOLE_TMP_DIR}/Corefile:/etc/coredns/Corefile.template:ro"
    echo "    networks:"
    echo "      sandbox:"
    echo "        ipv4_address: ${dns_ip}"
    echo "  agent:"
    echo "    dns:"
    echo "      - ${dns_ip}"
    echo "      - 127.0.0.11"
    echo "    build:"
    echo "      context: ${HOLE_TMP_DIR}"
    echo "      dockerfile: ${SCRIPT_DIR}/agents/Dockerfile"
    echo "      args:"
    echo "        AGENT_USERNAME: \"${SANDBOX_USERNAME:-agent}\""
    echo "        AGENT_HOME: \"${SANDBOX_HOME:-/home/agent}\""
    if [[ -n "${extra_packages}" ]]; then
      echo "        EXTRA_PACKAGES: \"${extra_packages}\""
    fi
    if [[ -n "${base_image}" ]]; then
      echo "        BASE_IMAGE: \"${base_image}\""
    fi
    if [[ -n "${agent_mem_limit}" ]]; then
      echo "    mem_limit: ${agent_mem_limit}"
    fi
    if [[ -n "${agent_memswap_limit}" ]]; then
      echo "    memswap_limit: ${agent_memswap_limit}"
    fi
    if [[ ${#agent_env_vars[@]} -gt 0 ]]; then
      echo "    environment:"
      for e in "${agent_env_vars[@]}"; do
        echo "${e}"
      done
    fi
    if [[ "${debug_mode}" == true ]]; then
      echo "    command: [\"bash\"]"
    else
      local -a base_cmd=()
      while IFS= read -r cmd_part; do
        base_cmd+=("${cmd_part}")
      done < <(get_agent_base_command "${agent}")
      local -a full_cmd=("${base_cmd[@]}")
      if [[ "${has_agent_args}" == true ]]; then
        full_cmd+=("${agent_args[@]}")
      fi
      local cmd_str
      cmd_str=$(printf '%s\n' "${full_cmd[@]}" | jq -R . | jq -s -c .)
      echo "    command: ${cmd_str}"
    fi
    if [[ ${#agent_volumes[@]} -gt 0 ]]; then
      echo "    volumes:"
      for v in "${agent_volumes[@]}"; do
        echo "${v}"
      done
    fi
    if [[ "${docker_enabled}" == "true" ]]; then
      echo "    depends_on:"
      echo "      docker:"
      echo "        condition: service_healthy"
    fi

    # Docker-in-Docker sidecar service
    if [[ "${docker_enabled}" == "true" ]]; then
      echo "  docker:"
      echo "    image: docker:dind"
      echo "    privileged: true"
      echo "    entrypoint:"
      echo "      - sh"
      echo "      - -c"
      echo "      - |"
      echo "        rm -rf /var/lib/docker/containerd/daemon/io.containerd.metadata.v1.bolt/meta.db-lock"
      echo "        rm -f /var/run/docker.pid"
      echo '        exec dockerd-entrypoint.sh "$$@"'
      echo "      - --"
      echo "    environment:"
      echo "      - DOCKER_TLS_CERTDIR="
      echo "      - HTTP_PROXY=http://proxy:8888"
      echo "      - HTTPS_PROXY=http://proxy:8888"
      echo "      - http_proxy=http://proxy:8888"
      echo "      - https_proxy=http://proxy:8888"
      echo "      - NO_PROXY=localhost,127.0.0.1"
      echo "      - no_proxy=localhost,127.0.0.1"
      for e in "${user_env_vars[@]}"; do
        echo "${e}"
      done
      echo "    volumes:"
      echo "      - \${PROJECT_DIR:-.}:\${PROJECT_DIR:-.}"
      echo "      - hole-sandbox-docker-data-${instance_name}:/var/lib/docker"
      # Mirror file exclusion volumes on DinD container
      for v in "${agent_volumes[@]}"; do
        echo "${v}"
      done
      echo "    depends_on:"
      echo "      proxy:"
      echo "        condition: service_healthy"
      echo "    networks:"
      echo "      - sandbox"
      echo "    dns:"
      echo "      - ${dns_ip}"
      echo "      - 127.0.0.11"
      echo "    healthcheck:"
      echo "      test: [\"CMD\", \"docker\", \"info\"]"
      echo "      interval: 3s"
      echo "      timeout: 5s"
      echo "      retries: 10"
    fi

    # Top-level volumes declaration for DinD persistent cache
    if [[ "${docker_enabled}" == "true" ]]; then
      echo "volumes:"
      echo "  hole-sandbox-docker-data-${instance_name}:"
      echo "    external: true"
    fi

    # Top-level networks: per-instance sandbox subnet
    echo "networks:"
    echo "  sandbox:"
    echo "    ipam:"
    echo "      config:"
    echo "        - subnet: ${subnet}"
  } > "${compose_file}"

  echo "${compose_file}"
}

# Build the docker compose command array, including agent and optional override file
create_compose_cmd() {
  local instance_name="${1}"
  local project_compose_file="${2}"

  local proxy_compose_file="${SCRIPT_DIR}/proxy/docker-compose.yml"
  local dns_compose_file="${SCRIPT_DIR}/dns/docker-compose.yml"
  local agent_compose_file="${SCRIPT_DIR}/agents/docker-compose.yml"

  COMPOSE_CMD=("${CONTAINER_RUNTIME}" compose -p "${instance_name}" -f "${COMPOSE_FILE}" -f "${proxy_compose_file}" -f "${dns_compose_file}" -f "${agent_compose_file}")
  if [[ -f "${project_compose_file}" ]]; then
    COMPOSE_CMD+=(-f "${project_compose_file}")
  fi
}

# Ensure the persistent Docker cache volume exists (seed for per-instance DinD volumes)
ensure_docker_cache_volume() {
  local volume_name="hole-sandbox-docker-cache"
  if ! "${CONTAINER_RUNTIME}" volume inspect "${volume_name}" >/dev/null 2>&1; then
    log_info "Creating persistent volume: ${volume_name}"
    "${CONTAINER_RUNTIME}" volume create "${volume_name}"
  fi
}

# Create a per-instance Docker data volume and seed it from the project cache
seed_docker_instance_volume() {
  local instance_name="${1}"
  local cache_vol="hole-sandbox-docker-cache"
  local instance_vol="hole-sandbox-docker-data-${instance_name}"

  log_info "Creating instance volume: ${instance_vol}"
  "${CONTAINER_RUNTIME}" volume create "${instance_vol}" >/dev/null

  log_info "Seeding instance volume from cache..."
  "${CONTAINER_RUNTIME}" run --rm \
    -v "${cache_vol}:/src:ro" \
    -v "${instance_vol}:/dst" \
    alpine sh -c 'cp -a /src/. /dst/ 2>/dev/null || true' >/dev/null 2>&1 || true
}

# Sync instance Docker data back to cache, then remove instance volume
sync_and_remove_docker_instance_volume() {
  local instance_name="${1}"
  local cache_vol="hole-sandbox-docker-cache"
  local instance_vol="hole-sandbox-docker-data-${instance_name}"
  local lock_file="${TMPDIR:-/tmp}/hole-docker-cache.lock"

  # Best-effort sync: serialize concurrent syncs with flock
  if command -v flock >/dev/null 2>&1; then
    (
      if flock -w 120 9 2>/dev/null; then
        # Clear cache and copy instance data into it
        "${CONTAINER_RUNTIME}" run --rm \
          -v "${cache_vol}:/dst" \
          alpine sh -c 'rm -rf /dst/* /dst/..?* /dst/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true
        "${CONTAINER_RUNTIME}" run --rm \
          -v "${instance_vol}:/src:ro" \
          -v "${cache_vol}:/dst" \
          alpine sh -c 'cp -a /src/. /dst/ 2>/dev/null || true' >/dev/null 2>&1 || true
      else
        log_warn "Could not acquire cache lock, skipping cache sync"
      fi
    ) 9>"${lock_file}" || true
  else
    log_warn "flock not available, skipping cache sync"
  fi

  # Remove the ephemeral instance volume
  "${CONTAINER_RUNTIME}" volume rm "${instance_vol}" >/dev/null 2>&1 || true
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
  local unrestricted_network="${9:-false}"
  local with_docker="${10:-false}"
  shift 10
  local agent_args=("$@")
  detect_container_runtime

  local build_flag=()
  if [[ "${rebuild}" == true ]]; then
    build_flag=(--build)
  fi

  HOLE_TMP_DIR="$(mktemp -d)"
  # clean up temp folder after the script exits
  trap "rm -rf '${HOLE_TMP_DIR}'" EXIT

  # Validate settings files if present
  validate_settings "${GLOBAL_SETTINGS_FILE}" "global settings (~/.hole/settings.json)"
  validate_settings "${project_dir}/.hole/settings.json" "project settings (.hole/settings.json)"

  # Merge global and project settings
  local merged_settings
  merged_settings=$(merge_settings "${GLOBAL_SETTINGS_FILE}" "${project_dir}/.hole/settings.json")

  # Validate startup agent is in the enabled agents list
  local agent_is_enabled=false
  while IFS= read -r ea; do
    [[ "${ea}" == "${agent}" ]] && agent_is_enabled=true
  done < <(get_enabled_agents "${merged_settings}")
  if [[ "${agent_is_enabled}" == false ]]; then
    log_error "agent '${agent}' is not in the enabled agents list"
    log_info "Enabled agents: $(get_enabled_agents "${merged_settings}" | tr '\n' ' ')"
    log_info "Configure enabled agents via container.enabledAgents in settings.json"
    exit 1
  fi

  # Expose runtime variables for docker-compose.yml
  export PROJECT_NAME="${project_name}"
  export PROJECT_DIR="${project_dir}"
  if [ "$(uname -s)" = "Linux" ]; then # only needed on Linux, Docker Desktop (Windows, macOS)/Orbstack should solve the id mismatches automatically
    local sandbox_uid
    sandbox_uid=$(id -u)
    export SANDBOX_UID="${sandbox_uid}"
    local sandbox_gid
    sandbox_gid=$(id -g)
    export SANDBOX_GID="${sandbox_gid}"
  fi
  # Export host username and home path for container user creation (all platforms)
  local sandbox_username="${USER:-agent}"
  export SANDBOX_USERNAME="${sandbox_username}"
  local sandbox_home="${HOME:-/home/agent}"
  export SANDBOX_HOME="${sandbox_home}"
  # Export trigger to reset docker image cache
  if [[ "${rebuild}" == true ]]; then
    export SANDBOX_REBUILD="true"

    local cachebust
    cachebust="$(date +%s)"
    export CACHEBUST="${cachebust}"
  fi

  if [[ "${debug_mode}" == true ]]; then
    log_warn "Debug mode: opening bash shell instead of agent CLI"
    log_line
  fi
  log_info "Launching sandbox for: ${project_dir}"
  log_info "Project name: ${project_name}"
  log_info "Instance ID: ${instance_id}"
  log_line

  check_for_update

  # Ensure persistent Docker cache volume and seed per-instance volume
  local docker_enabled_vol
  docker_enabled_vol=$(echo "${merged_settings}" | jq -r '.container.docker // empty' 2>/dev/null) || true
  if [[ "${with_docker}" == "true" || "${docker_enabled_vol}" == "true" ]]; then
    ensure_docker_cache_volume
    seed_docker_instance_volume "${instance_name}"
  fi

  # Allocate a unique subnet, generate compose, and start DNS under lock
  # to prevent parallel sandboxes from picking the same subnet.
  local subnet_lock_file="${TMPDIR:-/tmp}/hole-subnet-alloc.lock"
  exec 9>"${subnet_lock_file}"
  flock -w 30 9 || { log_error "Could not acquire subnet allocation lock"; exit 1; }

  local subnet_index
  subnet_index=$(find_available_subnet_index)
  local subnet="172.28.${subnet_index}.0/24"
  local dns_ip="172.28.${subnet_index}.53"

  # Generate per-project compose override from merged settings
  local project_compose_file
  project_compose_file=$(generate_instance_compose "${agent}" "${project_dir}" "${instance_name}" "${merged_settings}" "${debug_mode}" "${unrestricted_network}" "${with_docker}" "${subnet}" "${dns_ip}" ${agent_args[@]+"${agent_args[@]}"})
  create_compose_cmd "${instance_name}" "${project_compose_file}"

  # Start DNS service first (creates the sandbox network, securing our subnet)
  log_info "Starting DNS..."
  "${COMPOSE_CMD[@]}" up -d ${build_flag[@]+"${build_flag[@]}"} dns

  # Release subnet lock — network is created, subnet is secured
  flock -u 9
  exec 9>&-

  # Start proxy in detached mode with health check wait
  log_info "Starting proxy..."
  "${COMPOSE_CMD[@]}" up -d ${build_flag[@]+"${build_flag[@]}"} proxy

  # Start DinD sidecar if Docker is enabled
  local docker_enabled_start
  docker_enabled_start=$(echo "${merged_settings}" | jq -r '.container.docker // empty' 2>/dev/null) || true
  if [[ "${with_docker}" == "true" ]]; then
    docker_enabled_start="true"
  fi
  if [[ "${docker_enabled_start}" == "true" ]]; then
    log_info "Starting Docker-in-Docker sidecar..."
    "${COMPOSE_CMD[@]}" up -d ${build_flag[@]+"${build_flag[@]}"} docker
  fi

  # Start agent service
  log_info "Starting ${agent} agent..."
  log_line
  "${COMPOSE_CMD[@]}" up -d ${build_flag[@]+"${build_flag[@]}"} agent

  # Attach terminal to the running agent
  log_info "Attaching to ${agent} agent..."
  log_line
  "${CONTAINER_RUNTIME}" attach "${instance_name}-agent-1"

  # Dump network access log if requested
  if [[ "${dump_network_access}" == true ]]; then
    local log_dir="${project_dir}/.hole/logs"
    mkdir -p "${log_dir}"
    local log_file="${log_dir}/network-access-${agent}-${instance_id}.log"
    local proxy_container="${instance_name}-proxy-1"
    local tmp_proxy_log
    tmp_proxy_log="$(mktemp "${TMPDIR:-/tmp}/hole-sandbox-proxy-log.XXXXXX")"

    # Stop proxy gracefully so tinyproxy exit() flushes stdio buffers to log file
    "${CONTAINER_RUNTIME}" stop "${proxy_container}" >/dev/null 2>&1 || true

    # Copy the log file from the stopped container and extract domains
    if "${CONTAINER_RUNTIME}" cp "${proxy_container}:/var/log/tinyproxy/tinyproxy.log" "${tmp_proxy_log}" 2>/dev/null; then
      grep -oE 'Established connection to host "[a-zA-Z0-9._-]+"|Proxying refused on filtered (url|domain) "[^"]+"' "${tmp_proxy_log}" | \
        sed 's/Established connection to host "/ALLOWED /; s/Proxying refused on filtered [a-z]* "/DENIED /; s/"$//' | \
        sort -u > "${log_file}" || true
      log_line
      log_info "Network access log written to: ${log_file}"
    else
      log_line
      log_warn "Could not retrieve proxy log from container"
    fi

    rm -f "${tmp_proxy_log}"
  fi

  # Destroy the sandbox after user exits
  "${COMPOSE_CMD[@]}" down --remove-orphans

  # Sync Docker instance volume back to cache and remove it
  if [[ "${with_docker}" == "true" || "${docker_enabled_vol}" == "true" ]]; then
    log_info "Syncing Docker data to cache..."
    sync_and_remove_docker_instance_volume "${instance_name}"
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
  detect_container_runtime

  # Stop running containers for this project
  local running_containers
  running_containers=$("${CONTAINER_RUNTIME}" ps -q --filter "name=hole-sandbox-${project_name}-") || true
  if [[ -n "${running_containers}" ]]; then
    log_info "Stopping running containers..."
    "${CONTAINER_RUNTIME}" stop ${running_containers} || log_warn "Failed to stop some containers"
  else
    log_info "No running containers found"
  fi

  # Remove all containers (running or stopped) for this project
  local all_containers
  all_containers=$("${CONTAINER_RUNTIME}" ps -aq --filter "name=hole-sandbox-${project_name}-") || true
  if [[ -n "${all_containers}" ]]; then
    log_info "Removing containers..."
    "${CONTAINER_RUNTIME}" rm -f ${all_containers} || log_warn "Failed to remove some containers"
  else
    log_info "No containers found"
  fi

  # Remove networks for this project
  local networks
  networks=$("${CONTAINER_RUNTIME}" network ls -q --filter "name=hole-sandbox-${project_name}-") || true
  if [[ -n "${networks}" ]]; then
    log_info "Removing networks..."
    "${CONTAINER_RUNTIME}" network rm ${networks} || log_warn "Failed to remove some networks"
  else
    log_info "No networks found"
  fi

  # Remove cached agent image (unified image for all agents)
  local agent_image="hole-sandbox/agent-${project_name}:latest"
  if "${CONTAINER_RUNTIME}" image inspect "${agent_image}" >/dev/null 2>&1; then
    log_info "Removing agent image: ${agent_image}"
    "${CONTAINER_RUNTIME}" rmi "${agent_image}" || log_warn "Failed to remove image ${agent_image}"
  else
    log_info "No cached agent image found"
  fi

  # Remove cached proxy image
  local proxy_image="hole-sandbox/proxy-${project_name}:latest"
  if "${CONTAINER_RUNTIME}" image inspect "${proxy_image}" >/dev/null 2>&1; then
    log_info "Removing proxy image: ${proxy_image}"
    "${CONTAINER_RUNTIME}" rmi "${proxy_image}" || log_warn "Failed to remove image ${proxy_image}"
  else
    log_info "No cached proxy image found"
  fi

  # Remove cached dns image
  local dns_image="hole-sandbox/dns-${project_name}:latest"
  if "${CONTAINER_RUNTIME}" image inspect "${dns_image}" >/dev/null 2>&1; then
    log_info "Removing DNS image: ${dns_image}"
    "${CONTAINER_RUNTIME}" rmi "${dns_image}" || log_warn "Failed to remove image ${dns_image}"
  else
    log_info "No cached DNS image found"
  fi

  # Remove orphaned Docker instance volumes for this project
  local orphaned_instance_vols
  orphaned_instance_vols=$("${CONTAINER_RUNTIME}" volume ls -q --filter "name=hole-sandbox-docker-data-hole-sandbox-${project_name}-" 2>/dev/null) || true
  if [[ -n "${orphaned_instance_vols}" ]]; then
    log_info "Removing orphaned Docker instance volumes..."
    echo "${orphaned_instance_vols}" | while IFS= read -r vol; do
      "${CONTAINER_RUNTIME}" volume rm "${vol}" 2>/dev/null || log_warn "Failed to remove ${vol}"
    done
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
bash "${tmp_uninstall}" --soft-wipe
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
  local unrestricted_network=false
  local with_docker=false
  local positional=()
  local agent_args=()
  local parsing_hole_args=true

  for arg in "$@"; do
    if [[ "${parsing_hole_args}" == true ]]; then
      case "${arg}" in
        -d|--debug) debug_mode=true ;;
        -n|--dump-network-access) dump_network_access=true ;;
        -r|--rebuild) rebuild=true ;;
        -u|--unrestricted-network) unrestricted_network=true ;;
        --with-docker) with_docker=true ;;
        --) parsing_hole_args=false ;;
        *) positional+=("${arg}") ;;
      esac
    else
      agent_args+=("${arg}")
    fi
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
  instance_name="hole-sandbox-${project_name}-${instance_id}"

  # Handle start command (no agent argument required)
  if [[ "${command}" == "start" ]]; then
    validate_agent "${agent}"

    if [[ "${debug_mode}" == true && ${#agent_args[@]} -gt 0 ]]; then
      log_error "--debug and agent arguments (after --) cannot be used together"
      exit 1
    fi

    cmd_start "${agent}" "${project_dir}" "${project_name}" "${instance_id}" "${instance_name}" "${dump_network_access}" "${debug_mode}" "${rebuild}" "${unrestricted_network}" "${with_docker}" ${agent_args[@]+"${agent_args[@]}"}
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
