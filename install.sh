#!/usr/bin/env bash
# SamVNC official installer for Linux/macOS — public mirror based
#
# Usage:
#   curl -fsSL https://samai.cc/samvnc/install.sh | bash
#   curl -fsSL https://samai.cc/samvnc/install.sh | bash -s -- --version v1.4.1
#
# Downloads the samvnc-server AND samvnc-tray binaries for the current
# platform and installs them to /usr/local/bin (with root) or
# ~/.local/bin (without root).
#
# Default source is the PUBLIC mirror on samai.cc (the GitHub repo is
# private — raw.githubusercontent and release downloads 404 for
# anonymous users). --version <tag> pulls from GitHub Releases instead
# and requires GitHub access to that repo.
#
# `samvnc-tray` is the recommended start command: it brings up the native
# system tray (one unified icon on Windows/macOS/Linux) and launches the
# server automatically. On headless machines (SSH, no desktop) the tray
# binary detects the missing GUI and falls back to server-only mode, so
# the same command works everywhere.

set -euo pipefail

REPO="samaidev/samvnc"
BIN_NAME="samvnc-server"
TRAY_NAME="samvnc-tray"
INSTALL_DIR=""
REQUESTED_VERSION=""

# Public mirror (works for EVERYONE — the GitHub repo is private, so
# raw.githubusercontent and release downloads 404 for anonymous users).
# wf3 mirrors every release's binaries here, flat, latest release only.
MIRROR="https://samai.cc/samvnc/downloads"

# Fallback: the most recent release known to ship every platform binary.
# If the latest release does not include your platform, we retry from here
# so the one-line install keeps working between releases.
FALLBACK_TAG="v1.4.1"

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
TRAY_REMOTE_FILE="samvnc-tray-${PLATFORM_OS}-${PLATFORM_ARCH}"
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

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

try_download() { # try_download <remote-file> <tag-or-latest-or-mirror> <dest> ; returns 0 on success
    local file="$1" tag="$2" dest="$3"
    local url
    if [[ "$tag" == "mirror" ]]; then
        url="${MIRROR}/${file}"
    elif [[ "$tag" == "latest" ]]; then
        url="https://github.com/${REPO}/releases/latest/download/${file}"
    else
        url="https://github.com/${REPO}/releases/download/${tag}/${file}"
    fi
    echo "Downloading ${url}"
    if fetch "$url" "$dest" 2>/dev/null; then
        [[ -s "$dest" ]] && return 0
    fi
    return 1
}

install_asset() { # install_asset <remote-file> <dest-name> <required:yes/no>
    local file="$1" name="$2" required="$3"
    local dest="${TMP_DIR}/${file}"
    local tag=""

    if [[ -n "$REQUESTED_VERSION" ]]; then
        try_download "$file" "$REQUESTED_VERSION" "$dest" || {
            if [[ "$required" == "yes" ]]; then
                echo "ERROR: asset ${file} not found in release ${REQUESTED_VERSION}." >&2
                echo "Check https://github.com/${REPO}/releases for available assets." >&2
                exit 1
            fi
            return 1
        }
        tag="$REQUESTED_VERSION"
    else
        if try_download "$file" mirror "$dest"; then
            tag="mirror (latest release)"
        elif try_download "$file" latest "$dest"; then
            tag="latest"
        elif try_download "$file" "$FALLBACK_TAG" "$dest"; then
            tag="$FALLBACK_TAG"
            echo "NOTE: mirror and latest release do not ship ${file}." >&2
            echo "      Fell back to ${FALLBACK_TAG}." >&2
        else
            if [[ "$required" == "yes" ]]; then
                echo "ERROR: could not download ${file} from ${MIRROR} or GitHub Releases." >&2
                echo "Please grab it manually from: ${MIRROR}" >&2
                exit 1
            fi
            return 1
        fi
    fi

    chmod +x "$dest"
    if [[ -w "$INSTALL_DIR" ]] || [[ "$(id -u)" == "0" ]]; then
        mv -f "$dest" "${INSTALL_DIR}/${name}"
    else
        echo "Need write permission for ${INSTALL_DIR}, using sudo..."
        sudo mv -f "$dest" "${INSTALL_DIR}/${name}"
    fi
    echo "Installed: ${INSTALL_DIR}/${name} (${tag})"
}

# --- install: server (required) + tray (best effort) ---------------------
install_asset "$REMOTE_FILE" "$BIN_NAME" yes
if install_asset "$TRAY_REMOTE_FILE" "$TRAY_NAME" no; then
    TRAY_OK=1
else
    TRAY_OK=0
    echo "NOTE: no ${TRAY_REMOTE_FILE} asset in this release — skipped." >&2
    echo "      The tray UI is not available for this platform yet;" >&2
    echo "      start the server directly with 'samvnc-server --verbose'." >&2
fi

echo ""
if [[ "$TRAY_OK" == "1" ]]; then
    echo "Run 'samvnc-tray' to start — it shows the system-tray icon and"
    echo "launches the server automatically (on headless machines it runs"
    echo "the server in the foreground instead and prints the public URL)."
else
    echo "Run 'samvnc-server --verbose' to start — it prints your public access URL."
fi
echo "Re-run this installer anytime to update."
