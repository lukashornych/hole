#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="$HOME/.local/share/hole"
BIN_DIR="$HOME/.local/bin"
BIN_PATH="$BIN_DIR/hole"

# We don't use logger library here because this scripts needs to be runnable on its own
log_info()    { echo "[INFO] $*"; }
log_success() { echo "[OK] $*"; }
log_warn()    { echo "[WARN] $*"; }
log_error()   { echo "[ERROR] $*" >&2; exit 1; }

print_success() {
    echo ""
    echo "  hole uninstalled successfully."
    echo ""
}

main() {
    log_info "Starting hole uninstallation..."

    log_warn "If you have running sandboxes, exit them first (they auto-destroy on exit)."
    log_warn "Proceeding will remove hole files and agent home volumes."

    if [ ! -d "$INSTALL_DIR" ] && [ ! -f "$BIN_PATH" ]; then
        log_info "No hole installation found. Nothing to do."
        exit 0
    fi

    if [ -d "$INSTALL_DIR" ]; then
        log_info "Removing $INSTALL_DIR..."
        rm -rf "$INSTALL_DIR"
        log_success "Removed $INSTALL_DIR"
    fi

    if [ -f "$BIN_PATH" ]; then
        log_info "Removing $BIN_PATH..."
        rm -f "$BIN_PATH"
        log_success "Removed $BIN_PATH"
    fi

    local agents=("claude" "gemini")
    for agent in "${agents[@]}"; do
        local volume_name="hole-agent-home-$agent"
        if docker volume inspect "$volume_name" >/dev/null 2>&1; then
            log_info "Removing Docker volume: $volume_name..."
            docker volume rm "$volume_name"
            log_success "Removed $volume_name"
        fi
    done

    print_success
}

main
