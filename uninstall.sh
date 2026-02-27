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

remove_docker_resources() {
    local answer
    echo ""
    read -r -p "Do you also want to remove all hole Docker resources (images, networks, volumes)? (y/N) " answer
    if [[ ! "${answer}" =~ ^[yY]$ ]]; then
        log_info "Skipping Docker resource cleanup."
        return 0
    fi

    # Stop and remove all containers with name prefix "hole-"
    local containers
    containers=$(docker ps -aq --filter "name=hole-") || true
    if [[ -n "${containers}" ]]; then
        log_info "Stopping and removing containers..."
        docker stop ${containers} 2>/dev/null || true
        docker rm -f ${containers} || log_warn "Failed to remove some containers"
        log_success "Removed containers"
    else
        log_info "No containers found"
    fi

    # Remove all images matching "hole/*"
    local images
    images=$(docker images --filter "reference=hole/*" -q) || true
    if [[ -n "${images}" ]]; then
        log_info "Removing images..."
        docker rmi ${images} || log_warn "Failed to remove some images"
        log_success "Removed images"
    else
        log_info "No images found"
    fi

    # Remove all networks matching "hole-"
    local networks
    networks=$(docker network ls --filter "name=hole-" -q) || true
    if [[ -n "${networks}" ]]; then
        log_info "Removing networks..."
        docker network rm ${networks} || log_warn "Failed to remove some networks"
        log_success "Removed networks"
    else
        log_info "No networks found"
    fi

    # Remove all volumes matching "hole-"
    local volumes
    volumes=$(docker volume ls --filter "name=hole-" -q) || true
    if [[ -n "${volumes}" ]]; then
        log_info "Removing volumes..."
        docker volume rm ${volumes} || log_warn "Failed to remove some volumes"
        log_success "Removed volumes"
    else
        log_info "No volumes found"
    fi
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

    remove_docker_resources

    print_success
}

main
