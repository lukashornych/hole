#!/usr/bin/env bash
set -euo pipefail

GITHUB_REPO="lukashornych/hole"
GITHUB_API="https://api.github.com/repos/$GITHUB_REPO/releases/latest"
INSTALL_DIR="$HOME/.local/share/hole"
BIN_DIR="$HOME/.local/bin"
BIN_PATH="$BIN_DIR/hole"

TMPDIR_WORK=""
DOWNLOAD_URL=""

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
    if ! command -v jq >/dev/null 2>&1; then
        warn "jq is not installed or not in PATH."
        warn "hole requires jq to parse project settings. Install it from https://jqlang.github.io/jq/download/"
    fi
    if ! command -v jv >/dev/null 2>&1; then
        warn "jv is not installed or not in PATH."
        warn "hole requires jv to validate project settings. Install it from https://github.com/santhosh-tekuri/jsonschema"
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

resolve_download_url() {
    info "Resolving latest release..."
    local response
    if command -v curl >/dev/null 2>&1; then
        response=$(curl -fsSL "$GITHUB_API")
    else
        response=$(wget -qO- "$GITHUB_API")
    fi

    local tag
    tag=$(echo "$response" | grep '"tag_name"' | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
    if [ -z "$tag" ]; then
        error "Failed to resolve latest release. No releases found at $GITHUB_API"
    fi

    DOWNLOAD_URL="https://github.com/$GITHUB_REPO/releases/download/$tag/hole.tar.gz"
    success "Latest release: $tag"
}

download() {
    info "Downloading hole from GitHub..."
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$DOWNLOAD_URL" -o "$TMPDIR_WORK/hole.tar.gz"
    else
        wget -q "$DOWNLOAD_URL" -O "$TMPDIR_WORK/hole.tar.gz"
    fi
    success "Download complete."
}

extract_and_install() {
    info "Extracting archive..."
    tar -xzf "$TMPDIR_WORK/hole.tar.gz" -C "$TMPDIR_WORK"

    local src_dir="$TMPDIR_WORK/hole"
    if [ ! -d "$src_dir" ]; then
        error "Failed to locate extracted hole directory."
    fi

    mkdir -p $INSTALL_DIR
    cp -r "$src_dir"/* "$INSTALL_DIR"

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
    resolve_download_url
    download
    extract_and_install
    create_wrapper
    check_path
    print_success
}

main
