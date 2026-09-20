#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SOURCE="$REPO_ROOT/.opencode/tools/jev.ts"
DEST_DIR="$HOME/.config/opencode/tools"
DEST="$DEST_DIR/jev.ts"

if [[ ! -f "$SOURCE" ]]; then
  echo "Jev tool not found at: $SOURCE" >&2
  exit 1
fi

mkdir -p "$DEST_DIR"
cp "$SOURCE" "$DEST"

echo "Installed Jev OpenCode tool:"
echo "  $DEST"
echo
echo "Restart OpenCode or restart Glowbom OSS with:"
echo "  Ctrl+C"
echo "  glowbom start"
