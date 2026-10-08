#!/bin/sh
set -eu

if [ "$(uname -s)" != "Darwin" ] || [ "$(uname -m)" != "arm64" ]; then
  echo "Apple Intelligence requires an Apple Silicon Mac." >&2
  exit 1
fi

MAJOR_VERSION="$(sw_vers -productVersion | cut -d. -f1)"
if [ "$MAJOR_VERSION" -lt 26 ]; then
  echo "Apple Intelligence chat requires macOS 26 or later." >&2
  exit 1
fi

# apfel only installs with the Apple Silicon build of Homebrew.
if [ -x /opt/homebrew/bin/brew ]; then
  BREW=/opt/homebrew/bin/brew
elif command -v brew >/dev/null 2>&1; then
  BREW="$(command -v brew)"
  case "$BREW" in
    /usr/local/*)
      echo "This Mac has the Intel version of Homebrew, which cannot install apfel." >&2
      echo "Install the Apple Silicon version from https://brew.sh and try again." >&2
      exit 1
      ;;
  esac
else
  echo "Homebrew is required. Install it from https://brew.sh and try again." >&2
  exit 1
fi

if ! command -v apfel >/dev/null 2>&1 && [ ! -x /opt/homebrew/bin/apfel ]; then
  "$BREW" install apfel
fi

echo "apfel is installed."
echo "Return to Glowbom and turn on Use Apple Intelligence. Glowbom starts apfel on 127.0.0.1:11435 while it runs."
