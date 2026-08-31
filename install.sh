#!/usr/bin/env bash
#
# Hole installer. Downloads the release binary for this platform, verifies its checksum, and
# installs it to ~/.local/bin/hole.
#
#   curl -fsSL https://raw.githubusercontent.com/lukashornych/hole/main/install.sh | bash
#
# Hole is a single static binary: everything it needs at runtime is embedded, so there is no
# install directory to manage and no jq/jv to install alongside it.
#
# This script defines its own minimal logging because it must run standalone, piped from curl.
set -euo pipefail

GITHUB_REPO="lukashornych/hole"
GITHUB_API="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
BIN_DIR="${HOME}/.local/bin"
BIN_PATH="${BIN_DIR}/hole"
# Where the 1.x tarball installer unpacked the bash implementation.
LEGACY_INSTALL_DIR="${HOME}/.local/share/hole"

log_info()    { echo "[INFO] $*"; }
log_success() { echo "[OK] $*"; }
log_warn()    { echo "[WARN] $*"; }
log_error()   { echo "[ERROR] $*" >&2; exit 1; }

# Prints the "<os>_<arch>" suffix of the release asset for this machine.
detect_platform() {
  local os arch
  case "$(uname -s)" in
    Linux)  os="linux" ;;
    Darwin) os="darwin" ;;
    *) log_error "unsupported OS: $(uname -s). Hole releases cover Linux and macOS." ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) log_error "unsupported architecture: $(uname -m). Hole releases cover amd64 and arm64." ;;
  esac
  echo "${os}_${arch}"
}

check_installer_deps() {
  if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    log_error "curl or wget is required to install hole."
  fi
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    log_error "sha256sum or shasum is required to verify the download."
  fi
}

check_runtime_deps() {
  if ! command -v docker >/dev/null 2>&1 && ! command -v podman >/dev/null 2>&1; then
    log_warn "neither docker nor podman is installed or in PATH."
    log_warn "hole requires docker or podman with the compose plugin to run sandboxes."
  fi
}

fetch() {
  local url="${1}"
  local destination="${2}"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${url}" -o "${destination}"
  else
    wget -qO "${destination}" "${url}"
  fi
}

sha256_of() {
  local file="${1}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
  else
    shasum -a 256 "${file}" | awk '{print $1}'
  fi
}

resolve_tag() {
  local response
  if command -v curl >/dev/null 2>&1; then
    response=$(curl -fsSL "${GITHUB_API}")
  else
    response=$(wget -qO- "${GITHUB_API}")
  fi
  local tag
  tag=$(echo "${response}" | grep '"tag_name"' | head -n1 |
    sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
  if [ -z "${tag}" ]; then
    log_error "failed to resolve the latest release from ${GITHUB_API}"
  fi
  echo "${tag}"
}

remove_legacy_install() {
  if [ -d "${LEGACY_INSTALL_DIR}" ]; then
    log_info "Removing the previous bash installation at ${LEGACY_INSTALL_DIR}..."
    rm -rf "${LEGACY_INSTALL_DIR}"
  fi
}

# Warns when a different hole earlier in PATH keeps answering after this install — a `go install`
# build in $(go env GOPATH)/bin is the usual one. Silence here would look like a failed upgrade:
# the install reports the new version while `hole version` keeps printing the old one.
warn_if_shadowed() {
  local resolved
  resolved="$(command -v hole 2>/dev/null || true)"
  [ -n "${resolved}" ] || return 0
  # -ef compares device and inode, so a symlinked or oddly spelled PATH entry that points at the
  # binary just installed does not raise a false alarm.
  if [ "${resolved}" -ef "${BIN_PATH}" ]; then
    return 0
  fi
  log_warn "another hole comes earlier in your PATH and will keep being used:"
  log_warn "  ${resolved}"
  log_warn "Remove it (a 'go install' build lives in \$(go env GOPATH)/bin) or put ${BIN_DIR} first in PATH,"
  log_warn "then check with: hole version"
}

main() {
  log_info "Starting hole installation..."
  check_installer_deps
  check_runtime_deps

  local platform asset tag work_dir
  platform="$(detect_platform)"
  asset="hole_${platform}"

  log_info "Resolving the latest release..."
  tag="$(resolve_tag)"
  log_success "Latest release: ${tag}"

  work_dir="$(mktemp -d)"
  # shellcheck disable=SC2064  # expand work_dir now, while it is known
  trap "rm -rf '${work_dir}'" EXIT

  local base="https://github.com/${GITHUB_REPO}/releases/download/${tag}"
  log_info "Downloading ${asset}..."
  fetch "${base}/${asset}" "${work_dir}/${asset}" ||
    log_error "could not download ${asset}. Does release ${tag} include a build for ${platform}?"
  fetch "${base}/checksums.txt" "${work_dir}/checksums.txt" ||
    log_error "could not download checksums.txt; refusing to install an unverified binary."

  # An unverified binary is never installed: this is what makes downloading an executable
  # over the network defensible.
  local expected actual
  expected=$(grep -E "[ *]${asset}\$" "${work_dir}/checksums.txt" | awk '{print $1}')
  [ -n "${expected}" ] || log_error "checksums.txt does not list ${asset}."
  actual="$(sha256_of "${work_dir}/${asset}")"
  if [ "${expected}" != "${actual}" ]; then
    log_error "checksum mismatch for ${asset}: expected ${expected}, got ${actual}."
  fi
  log_success "Checksum verified."

  remove_legacy_install

  mkdir -p "${BIN_DIR}"
  chmod +x "${work_dir}/${asset}"
  # One move, so an interrupted install cannot leave a partial binary in place.
  mv -f "${work_dir}/${asset}" "${BIN_PATH}"
  log_success "Installed ${BIN_PATH}"

  case ":${PATH}:" in
    *":${BIN_DIR}:"*) warn_if_shadowed ;;
    *)
      log_warn "${BIN_DIR} is not in your PATH."
      log_warn "Add to ~/.bashrc / ~/.zshrc:"
      log_warn "  export PATH=\"\$HOME/.local/bin:\$PATH\""
      ;;
  esac

  echo ""
  echo "  hole ${tag#v} installed successfully!"
  echo ""
  echo "  Get started:"
  echo "    hole start claude /path/to/project"
  echo "    hole help"
  echo ""
  echo "  Later: 'hole update' upgrades in place, 'hole uninstall' removes everything."
  echo ""
}

main
