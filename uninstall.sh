#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="$HOME/.local/share/hole"
BIN_DIR="$HOME/.local/bin"
BIN_PATH="$BIN_DIR/hole"

info()    { echo "[hole] $*"; }
success() { echo "[hole] OK: $*"; }
warn()    { echo "[hole] WARN: $*"; }
error()   { echo "[hole] ERROR: $*" >&2; exit 1; }

print_success() {
    echo ""
    echo "  hole uninstalled successfully."
    echo ""
}

main() {
    info "Starting hole uninstallation..."

    warn "If you have running sandboxes, exit them first (they auto-destroy on exit)."
    warn "Proceeding will remove hole files and agent home volumes."

    if [ ! -d "$INSTALL_DIR" ] && [ ! -f "$BIN_PATH" ]; then
        info "No hole installation found. Nothing to do."
        exit 0
    fi

    if [ -d "$INSTALL_DIR" ]; then
        info "Removing $INSTALL_DIR..."
        rm -rf "$INSTALL_DIR"
        success "Removed $INSTALL_DIR"
    fi

    if [ -f "$BIN_PATH" ]; then
        info "Removing $BIN_PATH..."
        rm -f "$BIN_PATH"
        success "Removed $BIN_PATH"
    fi

    local agents=("claude" "gemini")
    for agent in "${agents[@]}"; do
        local volume_name="hole-agent-home-$agent"
        if docker volume inspect "$volume_name" >/dev/null 2>&1; then
            info "Removing Docker volume: $volume_name..."
            docker volume rm "$volume_name"
            success "Removed $volume_name"
        fi
    done

    print_success
}

main
