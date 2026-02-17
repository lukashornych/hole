#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="$HOME/.local/share/hole"
BIN_DIR="$HOME/.local/bin"
BIN_PATH="$BIN_DIR/hole"
DATA_DIR="$HOME/.hole"

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

    warn "If you have running sandboxes, destroy them first:"
    warn "  hole <agent> destroy /path/to/project"
    warn "Proceeding will remove hole files but leave Docker containers/images intact."

    if [ ! -d "$INSTALL_DIR" ] && [ ! -f "$BIN_PATH" ] && [ ! -d "$DATA_DIR" ]; then
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

    if [ -d "$DATA_DIR" ]; then
        info "Removing $DATA_DIR..."
        rm -rf "$DATA_DIR"
        success "Removed $DATA_DIR"
    fi

    print_success
}

main
