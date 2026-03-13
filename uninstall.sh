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

# Detect container runtime (docker or podman)
detect_container_runtime() {
    if [[ -n "${HOLE_RUNTIME:-}" ]]; then
        if ! command -v "${HOLE_RUNTIME}" >/dev/null 2>&1; then
            log_warn "HOLE_RUNTIME is set to '${HOLE_RUNTIME}' but it is not installed"
            CONTAINER_RUNTIME=""
            return
        fi
        CONTAINER_RUNTIME="${HOLE_RUNTIME}"
    elif command -v docker >/dev/null 2>&1; then
        CONTAINER_RUNTIME="docker"
    elif command -v podman >/dev/null 2>&1; then
        CONTAINER_RUNTIME="podman"
    else
        log_warn "neither docker nor podman found, skipping container resource cleanup"
        CONTAINER_RUNTIME=""
    fi
}

print_success() {
    echo ""
    echo "  hole uninstalled successfully."
    echo ""
}

remove_container_resources() {
    local soft_wipe="${1:-false}"

    if [[ -z "${CONTAINER_RUNTIME}" ]]; then
        log_warn "No container runtime available, skipping resource cleanup"
        return
    fi

    # Stop and remove all containers with name prefix "hole-sandbox-"
    local containers
    containers=$("${CONTAINER_RUNTIME}" ps -aq --filter "name=hole-sandbox-") || true
    if [[ -n "${containers}" ]]; then
        log_info "Stopping and removing containers..."
        "${CONTAINER_RUNTIME}" stop ${containers} 2>/dev/null || true
        "${CONTAINER_RUNTIME}" rm -f ${containers} || log_warn "Failed to remove some containers"
        log_success "Removed containers"
    else
        log_info "No containers found"
    fi

    # Remove all images matching "hole-sandbox/*"
    local images
    images=$("${CONTAINER_RUNTIME}" images --filter "reference=hole-sandbox/*" -q) || true
    if [[ -n "${images}" ]]; then
        log_info "Removing images..."
        "${CONTAINER_RUNTIME}" rmi ${images} || log_warn "Failed to remove some images"
        log_success "Removed images"
    else
        log_info "No images found"
    fi

    # Remove all networks matching "hole-sandbox-"
    local networks
    networks=$("${CONTAINER_RUNTIME}" network ls --filter "name=hole-sandbox-" -q) || true
    if [[ -n "${networks}" ]]; then
        log_info "Removing networks..."
        "${CONTAINER_RUNTIME}" network rm ${networks} || log_warn "Failed to remove some networks"
        log_success "Removed networks"
    else
        log_info "No networks found"
    fi

    # Remove all volumes matching "hole-sandbox-"
    local volumes
    volumes=$("${CONTAINER_RUNTIME}" volume ls --filter "name=hole-sandbox-" -q) || true
    if [[ "${soft_wipe}" == "true" ]]; then
        volumes=$(echo "${volumes}" | grep -v "^hole-sandbox-agent-home-\|^hole-sandbox-docker-cache\|^hole-sandbox-docker-data-" || true)
    fi
    if [[ -n "${volumes}" ]]; then
        log_info "Removing volumes..."
        "${CONTAINER_RUNTIME}" volume rm ${volumes} || log_warn "Failed to remove some volumes"
        log_success "Removed volumes"
    else
        log_info "No volumes found"
    fi
}

main() {
    local soft_wipe="false"
    for arg in "$@"; do
        case "${arg}" in
            --soft-wipe) soft_wipe="true" ;;
        esac
    done

    log_info "Starting hole uninstallation..."

    if [[ "${soft_wipe}" == "true" ]]; then
        log_warn "Proceeding will remove hole files and container resources (preserving agent home and Docker cache volumes)."
    else
        log_warn "Proceeding will remove hole files, container resources and agent home volumes."
    fi

    detect_container_runtime

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

    remove_container_resources "${soft_wipe}"

    print_success
}

# Self-cleanup when run from a temp copy (hole uninstall)
[[ "${0}" == "${TMPDIR:-/tmp}/hole-uninstall."* ]] && rm -f "${0}" 2>/dev/null || true

main "$@"
