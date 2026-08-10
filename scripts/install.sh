#!/bin/sh
# The documented usage pipes this into sh, which ignores the shebang and runs
# whatever the user piped to. On Debian and Ubuntu that is dash, so the script
# stays POSIX: pipefail is unavailable before dash 0.5.12.
set -eu

# Kipper CLI installer
# Usage: curl -sL https://getkipper.com/install | sh
#
# This script detects your OS and architecture, downloads the
# appropriate kip binary from GitHub Releases, and installs it to
# /usr/local/bin.

REPO="getkipper/kipper"
VERSION="${KIP_VERSION:-latest}"

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       echo "unsupported" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64)  echo "amd64" ;;
    aarch64) echo "arm64" ;;
    arm64)   echo "arm64" ;;
    *)       echo "unsupported" ;;
  esac
}

OS=$(detect_os)
ARCH=$(detect_arch)

if [ "$OS" = "unsupported" ] || [ "$ARCH" = "unsupported" ]; then
  echo "Error: unsupported platform $(uname -s)/$(uname -m)"
  exit 1
fi

BINARY="kip-${OS}-${ARCH}"
INSTALL_DIR="/usr/local/bin"

echo ""
echo "  Kipper CLI Installer"
echo ""
echo "  OS:      $OS"
echo "  Arch:    $ARCH"
echo "  Binary:  $BINARY"
echo ""

if [ "$VERSION" = "latest" ]; then
  echo "  Fetching latest version..."
  # The API pretty-prints its JSON, so the pattern has to tolerate whitespace
  # around the colon. sed also keeps this POSIX; grep -o is a GNU extension.
  VERSION=$(curl -sL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  if [ -z "$VERSION" ]; then
    echo "  Error: could not determine latest version."
    echo "  Set KIP_VERSION=v0.1.0 to install a specific version."
    exit 1
  fi
fi

echo "  Version: $VERSION"
echo ""

RELEASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
DOWNLOAD_URL="${RELEASE_URL}/${BINARY}"

# A fixed path in a world-writable directory lets a local user pre-create
# /tmp/kip as a symlink and redirect what this script writes as root.
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM HUP

# sha256sum is coreutils, shasum ships with macOS. The script installs a binary
# and then runs it as root against your servers, so it stops rather than install
# something it cannot check.
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  echo "  Error: neither sha256sum nor shasum is available, so the download"
  echo "  cannot be verified. Install one of them, or download the binary and"
  echo "  its checksum by hand from"
  echo "  https://github.com/${REPO}/releases/tag/${VERSION}"
  exit 1
fi

echo "  Downloading..."
if ! curl -sL --fail "$DOWNLOAD_URL" -o "$TMP_DIR/kip"; then
  echo "  Error: download failed. Check that version $VERSION exists at"
  echo "  https://github.com/${REPO}/releases"
  exit 1
fi

if ! curl -sL --fail "${RELEASE_URL}/checksums.txt" -o "$TMP_DIR/checksums.txt"; then
  echo "  Error: could not fetch checksums.txt for $VERSION, so the download"
  echo "  cannot be verified. Every release publishes one; if it is genuinely"
  echo "  missing, report it at https://github.com/${REPO}/issues"
  exit 1
fi

echo "  Verifying..."
# goreleaser writes "<sha256>  <filename>" per line. Match the whole name so
# kip-linux-amd64 cannot be satisfied by a line for kip-linux-arm64.
MATCHES=$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]][[:space:]]*${BINARY}\$/\1/p" "$TMP_DIR/checksums.txt")
MATCH_COUNT=$(printf '%s' "$MATCHES" | grep -c . || true)
if [ "$MATCH_COUNT" -ne 1 ]; then
  if [ "$MATCH_COUNT" -eq 0 ]; then
    echo "  Error: checksums.txt for $VERSION has no entry for ${BINARY}."
  else
    # Two entries for one name mean the manifest disagrees with itself, and
    # taking either would be a guess about which one is authentic.
    echo "  Error: checksums.txt for $VERSION has $MATCH_COUNT entries for"
    echo "  ${BINARY} and does not agree with itself."
  fi
  echo "  Refusing to install an unverified binary."
  exit 1
fi
EXPECTED="$MATCHES"

ACTUAL=$(sha256_of "$TMP_DIR/kip")
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "  Error: checksum mismatch for ${BINARY}."
  echo "    expected: $EXPECTED"
  echo "    actual:   $ACTUAL"
  echo "  The download was corrupted or tampered with. Nothing was installed."
  exit 1
fi

chmod 755 "$TMP_DIR/kip"

if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP_DIR/kip" "$INSTALL_DIR/kip"
else
  echo "  Installing to $INSTALL_DIR (requires sudo)..."
  sudo mv "$TMP_DIR/kip" "$INSTALL_DIR/kip"
fi

echo ""
echo "  ✔  kip $VERSION installed to $INSTALL_DIR/kip"
echo ""
echo "  Get started:"
echo "    kip install --host <your-server-ip>"
echo ""
