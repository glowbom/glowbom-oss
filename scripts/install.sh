#!/bin/sh
set -eu

REPO="${GLOWBOM_REPO:-${GLOWBY_REPO:-glowbom/glowbom-oss}}"
INSTALL_DIR="${GLOWBOM_INSTALL_DIR:-${GLOWBY_INSTALL_DIR:-/usr/local/bin}}"
BASE_URL="https://github.com/${REPO}/releases/latest/download"

detect_os() {
  case "$(uname -s)" in
    Darwin*) echo "darwin" ;;
    Linux*)  echo "linux" ;;
    MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
    *) echo "unsupported"; return 1 ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)   echo "arm64" ;;
    *) echo "unsupported"; return 1 ;;
  esac
}

main() {
  OS="$(detect_os)"
  ARCH="$(detect_arch)"
  ARCHIVE_EXT="tar.gz"
  BIN_NAME="glowbom"

  if [ "$OS" = "unsupported" ] || [ "$ARCH" = "unsupported" ]; then
    echo "error: unsupported platform $(uname -s)/$(uname -m)" >&2
    exit 1
  fi

  if [ "$OS" = "windows" ]; then
    ARCHIVE_EXT="zip"
    BIN_NAME="glowbom.exe"
  fi

  ARCHIVE="glowbom-${OS}-${ARCH}.${ARCHIVE_EXT}"
  URL="${BASE_URL}/${ARCHIVE}"

  echo "Downloading Glowbom OSS for ${OS}/${ARCH}..."
  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  if ! curl -fsSL "$URL" -o "${TMPDIR}/${ARCHIVE}"; then
    echo "error: failed to download ${URL}" >&2
    echo "  Check that a release exists at https://github.com/${REPO}/releases" >&2
    exit 1
  fi

  if [ "$OS" = "windows" ]; then
    unzip -q "${TMPDIR}/${ARCHIVE}" -d "$TMPDIR"
  else
    tar xzf "${TMPDIR}/${ARCHIVE}" -C "$TMPDIR"
  fi

  if [ ! -f "${TMPDIR}/${BIN_NAME}" ]; then
    echo "error: ${BIN_NAME} binary not found in archive" >&2
    exit 1
  fi

  echo "Installing to ${INSTALL_DIR}/${BIN_NAME}..."
  install -d "$INSTALL_DIR"
  install -m 755 "${TMPDIR}/${BIN_NAME}" "${INSTALL_DIR}/${BIN_NAME}"

  echo "Glowbom OSS installed successfully to ${INSTALL_DIR}/${BIN_NAME}"
  echo "Run 'glowbom doctor' to verify your setup."
}

main
