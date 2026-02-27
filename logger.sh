#!/usr/bin/env bash

# Check if standard error is connected to a terminal.
# If it is, enable colors. If not (e.g., writing to a file), disable them.
if [[ -t 2 ]]; then
    readonly COLOR_INFO='\033[0;32m'    # Green
    readonly COLOR_WARN='\033[0;33m'    # Yellow
    readonly COLOR_ERROR='\033[0;31m'   # Red
    readonly COLOR_RESET='\033[0m'      # No Color
else
    readonly COLOR_INFO=''
    readonly COLOR_WARN=''
    readonly COLOR_ERROR=''
    readonly COLOR_RESET=''
fi

# Internal helper function for logging
_log() {
    local level="$1"
    local color="$2"
    local message="$3"

    echo -e "${color}[${level}] ${message}${COLOR_RESET}" >&2
}

# Public logging functions
log_info() {
    _log "INFO" "$COLOR_INFO" "$1"
}

log_warn() {
    _log "WARN" "$COLOR_WARN" "$1"
}

log_error() {
    _log "ERROR" "$COLOR_ERROR" "$1"
}

log_line() {
  echo ""
}