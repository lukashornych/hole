#!/bin/bash
set -euo pipefail

# Install additional dependencies if a deps file was mounted
DEPS_FILE="/tmp/hole-dependencies.txt"
if [[ -f "$DEPS_FILE" ]]; then
    mapfile -t packages < "$DEPS_FILE"
    if [[ ${#packages[@]} -gt 0 ]]; then
        echo "Installing dependencies: ${packages[*]}"
        sudo -E apt-get update -q && sudo -E apt-get install -q --no-install-recommends -y "${packages[@]}"
    fi
fi

exec "$@"
