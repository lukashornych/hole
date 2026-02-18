#!/usr/bin/env bash
set -euo pipefail

REPO_URL="https://github.com/lukashornych/hole/archive/refs/heads/main.tar.gz"
INSTALL_DIR="$HOME/.local/share/hole"
BIN_DIR="$HOME/.local/bin"
BIN_PATH="$BIN_DIR/hole"

TMPDIR_WORK=""

info()    { echo "[hole] $*"; }
success() { echo "[hole] OK: $*"; }
warn()    { echo "[hole] WARN: $*"; }
error()   { echo "[hole] ERROR: $*" >&2; exit 1; }

detect_os() {
    local os
    os="$(uname -s)"
    case "$os" in
        Linux|Darwin) ;;
        *) error "Unsupported OS: $os. hole supports Linux and macOS." ;;
    esac
}

check_installer_deps() {
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        error "curl or wget is required to install hole."
    fi
    if ! command -v tar >/dev/null 2>&1; then
        error "tar is required to install hole."
    fi
}

check_runtime_deps() {
    if ! command -v docker >/dev/null 2>&1; then
        warn "docker is not installed or not in PATH."
        warn "hole requires Docker to run sandboxes. Install it from https://docs.docker.com/get-docker/"
    fi
}

check_existing() {
    if [ -d "$INSTALL_DIR" ] || [ -f "$BIN_PATH" ]; then
        info "Existing installation detected. Removing old installation..."
        rm -rf "$INSTALL_DIR"
        rm -f "$BIN_PATH"
        success "Old installation removed."
    fi
}

setup_cleanup() {
    TMPDIR_WORK="$(mktemp -d)"
    trap 'rm -rf "$TMPDIR_WORK"' EXIT
}

download() {
    info "Downloading hole from GitHub..."
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$REPO_URL" -o "$TMPDIR_WORK/hole.tar.gz"
    else
        wget -q "$REPO_URL" -O "$TMPDIR_WORK/hole.tar.gz"
    fi
    success "Download complete."
}

extract_and_install() {
    info "Extracting archive..."
    tar -xzf "$TMPDIR_WORK/hole.tar.gz" -C "$TMPDIR_WORK"

    local src_dir
    src_dir="$(find "$TMPDIR_WORK" -maxdepth 1 -type d -name 'hole-*' | head -1)"
    if [ -z "$src_dir" ]; then
        error "Failed to locate extracted hole directory."
    fi

    mkdir -p "$INSTALL_DIR/agents/claude"
    mkdir -p "$INSTALL_DIR/proxy"

    cp "$src_dir/hole.sh"                       "$INSTALL_DIR/hole.sh"
    cp "$src_dir/docker-compose.yml"            "$INSTALL_DIR/docker-compose.yml"
    cp "$src_dir/agents/claude/Dockerfile"      "$INSTALL_DIR/agents/claude/Dockerfile"
    cp "$src_dir/agents/claude/entrypoint.sh"   "$INSTALL_DIR/agents/claude/entrypoint.sh"
    cp "$src_dir/proxy/Dockerfile"              "$INSTALL_DIR/proxy/Dockerfile"
    cp "$src_dir/proxy/tinyproxy.conf"          "$INSTALL_DIR/proxy/tinyproxy.conf"
    cp "$src_dir/proxy/allowed-domains.txt"     "$INSTALL_DIR/proxy/allowed-domains.txt"

    chmod +x "$INSTALL_DIR/hole.sh"

    success "Files installed."
}

create_wrapper() {
    mkdir -p "$BIN_DIR"
    cat > "$BIN_PATH" <<EOF
#!/usr/bin/env bash
exec "${INSTALL_DIR}/hole.sh" "\$@"
EOF
    chmod +x "$BIN_PATH"
    success "Created wrapper: $BIN_PATH"
}

check_path() {
    case ":${PATH}:" in
        *":${BIN_DIR}:"*) ;;
        *)
            warn "$BIN_DIR is not in your PATH."
            warn "Add to ~/.bashrc / ~/.zshrc:"
            warn "  export PATH=\"\$HOME/.local/bin:\$PATH\""
            ;;
    esac
}

print_success() {
    echo ""
    echo "  hole installed successfully!"
    echo "  Location: $INSTALL_DIR"
    echo "  Command:  $BIN_PATH"
    echo ""
    echo "  Get started:"
    echo "    hole claude start /path/to/project"
    echo "    hole help"
    echo ""
}

main() {
    info "Starting hole installation..."
    detect_os
    check_installer_deps
    check_runtime_deps
    check_existing
    setup_cleanup
    download
    extract_and_install
    create_wrapper
    check_path
    print_success
}

main
