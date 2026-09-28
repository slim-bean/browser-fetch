#!/usr/bin/env bash
# Explicit local-development source bundle. Release builds should pin PI_BROWSER_REF.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SOURCE="${1:-$ROOT/../pi-browser}"
[[ -f "$SOURCE/bin/history.ts" && -f "$SOURCE/package.json" ]] || {
  echo "usage: scripts/stage-history.sh /path/to/updated/pi-browser" >&2; exit 1;
}
mkdir -p "$ROOT/deploy/history-source"
tar -czf "$ROOT/deploy/history-source/pi-browser.tar.gz" -C "$SOURCE" package.json bin extension
printf 'Staged local source. Build with --build-arg PI_BROWSER_SOURCE=local\n'
