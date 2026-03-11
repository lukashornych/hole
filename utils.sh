#!/usr/bin/env bash

# Check that a command is available, exit with a clear error if not
require_cmd() {
    local cmd="$1"
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        log_error "'${cmd}' is required but not installed. See requirements in the README: https://github.com/lukashornych/hole#installation"
        exit 1
    fi
}
