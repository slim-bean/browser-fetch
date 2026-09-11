#!/usr/bin/env bash
# Launch the Chrome the gateway attaches to.
#
# Headed on purpose: headless Chrome is detectable, and a visible window is what
# lets you log into sites and click captchas by hand — after which the profile's
# cookies carry every later fetch.
#
# Chrome 136+ refuses --remote-debugging-port on the default profile directory,
# so a dedicated --user-data-dir is required (and is what we want anyway: this
# profile is the agent's identity, separate from your personal browsing).
set -euo pipefail

PROFILE="${BROWSER_FETCH_PROFILE:-$HOME/.local/share/pi-browser-profile}"
PORT="${BROWSER_FETCH_DEBUG_PORT:-9222}"
CHROME="${CHROME_BIN:-}"

if [[ -z "$CHROME" ]]; then
  for candidate in google-chrome google-chrome-stable chromium chromium-browser \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
    if command -v "$candidate" >/dev/null 2>&1 || [[ -x "$candidate" ]]; then
      CHROME="$candidate"
      break
    fi
  done
fi
[[ -n "$CHROME" ]] || { echo "no Chrome found; set CHROME_BIN" >&2; exit 1; }

mkdir -p "$PROFILE"

# Bind DevTools to loopback only. Anyone who can reach this port can drive the
# browser, including its logged-in sessions; the gateway is the only thing that
# should talk to it.
exec "$CHROME" \
  --user-data-dir="$PROFILE" \
  --remote-debugging-port="$PORT" \
  --remote-debugging-address=127.0.0.1 \
  --no-first-run \
  --no-default-browser-check \
  --disable-features=Translate \
  --restore-last-session=false \
  "$@" \
  about:blank
