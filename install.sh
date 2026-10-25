#!/usr/bin/env bash
# samvnc-server official installer for Linux/macOS — GitHub Releases based
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/samaidev/samvnc/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/samaidev/samvnc/main/install.sh | bash -s -- --version v1.2.0
#
# Downloads the samvnc-server binary for the current platform from the
# samaidev/samvnc GitHub Releases and installs it to /usr/local/bin
# (with root) or ~/.local/bin (without root).
#
# Package downloads are hosted on GitHub Releases:
#   https://github.com/samaidev/samvnc/releases

set -euo pipefail

REPO="samaidev/samvnc"
BIN_NAME="samvnc-server"
INSTALL_DIR=""
REQUESTED_VERSION=""

# Fallback: the most recent release known to ship every platform binary.
# If the latest release does not include your platform, we retry from here
# so the one-line install keeps working between releases.
FALLBACK_TAG="v1.2.0"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)
            REQUESTED_VERSION="$2"
            [[ "$REQUESTED_VERSION" == v* ]] || REQUESTED_VERSION="v$REQUESTED_VERSION"
            shift 2
            ;;
        --prefix)
            INSTALL_DIR="$2"
            shift 2
            ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# --- platform detection -------------------------------------------------
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
    Linux*)  PLATFORM_OS="linux" ;;
    Darwin*) PLATFORM_OS="darwin" ;;
    *) echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac
case "$ARCH" in
    x86_64|amd64)        PLATFORM_ARCH="amd64" ;;
    aarch64|arm64)       PLATFORM_ARCH="arm64" ;;
    armv7l|armv6l|arm)   PLATFORM_ARCH="arm" ;;
    *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
REMOTE_FILE="samvnc-server-${PLATFORM_OS}-${PLATFORM_ARCH}"
echo "Detected platform: ${PLATFORM_OS}-${PLATFORM_ARCH}"

# --- install dir --------------------------------------------------------
if [[ -z "$INSTALL_DIR" ]]; then
    if [[ "$(id -u)" == "0" ]]; then
        INSTALL_DIR="/usr/local/bin"
    elif [[ -w "/usr/local/bin" ]]; then
        INSTALL_DIR="/usr/local/bin"
    else
        INSTALL_DIR="$HOME/.local/bin"
        mkdir -p "$INSTALL_DIR"
    fi
else
    mkdir -p "$INSTALL_DIR"
fi
echo "Install directory: $INSTALL_DIR"

# --- download helper ----------------------------------------------------
fetch() { # fetch <url> <dest>
    if command -v curl >/dev/null 2>&1; then
        curl -fSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        echo "Neither curl nor wget found. Please install one and retry." >&2
        exit 1
    fi
}

TMP_FILE="$(mktemp)"
trap 'rm -f "$TMP_FILE"' EXIT

try_download() { # try_download <tag-or-latest> ; returns 0 on success
    local tag="$1"
    local url
    if [[ "$tag" == "latest" ]]; then
        url="https://github.com/${REPO}/releases/latest/download/${REMOTE_FILE}"
    else
        url="https://github.com/${REPO}/releases/download/${tag}/${REMOTE_FILE}"
    fi
    echo "Downloading ${url}"
    if fetch "$url" "$TMP_FILE" 2>/dev/null; then
        [[ -s "$TMP_FILE" ]] && return 0
    fi
    return 1
}

# --- download with fallbacks --------------------------------------------
SOURCE_TAG=""
if [[ -n "$REQUESTED_VERSION" ]]; then
    try_download "$REQUESTED_VERSION" || {
        echo "ERROR: asset ${REMOTE_FILE} not found in release ${REQUESTED_VERSION}." >&2
        echo "Check https://github.com/${REPO}/releases for available assets." >&2
        exit 1
    }
    SOURCE_TAG="$REQUESTED_VERSION"
else
    if try_download latest; then
        SOURCE_TAG="latest"
    elif try_download "$FALLBACK_TAG"; then
        SOURCE_TAG="$FALLBACK_TAG"
        echo "NOTE: latest release does not ship ${REMOTE_FILE} (Windows-only latest)." >&2
        echo "      Fell back to ${FALLBACK_TAG}, the last release with all-platform binaries." >&2
    else
        echo "ERROR: could not download ${REMOTE_FILE} from GitHub Releases." >&2
        echo "Please grab it manually from: https://github.com/${REPO}/releases" >&2
        exit 1
    fi
fi

# --- install ------------------------------------------------------------
chmod +x "$TMP_FILE"
if [[ -w "$INSTALL_DIR" ]] || [[ "$(id -u)" == "0" ]]; then
    mv -f "$TMP_FILE" "${INSTALL_DIR}/${BIN_NAME}"
    trap - EXIT
else
    echo "Need write permission for ${INSTALL_DIR}, using sudo..."
    sudo mv -f "$TMP_FILE" "${INSTALL_DIR}/${BIN_NAME}"
    trap - EXIT
fi

echo ""
echo "Installed: ${INSTALL_DIR}/${BIN_NAME} (${SOURCE_TAG})"
echo "Run 'samvnc-server --verbose' to start — it prints your public access URL."
echo "Re-run this installer anytime to update."
