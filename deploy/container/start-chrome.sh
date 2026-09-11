#!/usr/bin/env bash
# Launch the Chrome the gateway attaches to.
#
# Headed on a virtual display, not --headless: headless is detectable, and a
# real window is what lets a human take over through VNC to log in or clear a
# captcha. Whatever they do persists in the profile on the PVC.
set -euo pipefail

PROFILE_DIR="${CHROME_PROFILE:-/profile/chrome}"
DEBUG_PORT="${CHROME_DEBUG_PORT:-9222}"

# Wait for the X server; Chrome exits immediately if the display is not up.
for _ in $(seq 1 60); do
  if xdotool getdisplaygeometry >/dev/null 2>&1; then break; fi
  sleep 0.5
done

flags=(
  --user-data-dir="$PROFILE_DIR"
  # DevTools on loopback only: anyone who reaches it drives the browser,
  # including its logged-in sessions. The gateway is in this same pod.
  --remote-debugging-port="$DEBUG_PORT"
  --remote-debugging-address=127.0.0.1
  --no-first-run
  --no-default-browser-check
  --disable-features=Translate
  --restore-last-session=false
  --hide-crash-restore-bubble
  # No keyring in a container; without this Chrome blocks on gnome-keyring.
  --password-store=basic
  --start-maximized
)

# Chrome's sandbox needs unprivileged user namespaces, which some container
# runtimes' default seccomp profiles block. Prefer keeping it on; flip this when
# the runtime says otherwise.
if [[ "${CHROME_NO_SANDBOX:-false}" == "true" ]]; then
  echo "start-chrome: WARNING sandbox disabled (CHROME_NO_SANDBOX=true)" >&2
  flags+=(--no-sandbox)
fi

# Space-separated extra flags, e.g. --lang=en-US --window-size=1920,1080
if [[ -n "${CHROME_EXTRA_FLAGS:-}" ]]; then
  # shellcheck disable=SC2206
  extra=(${CHROME_EXTRA_FLAGS})
  flags+=("${extra[@]}")
fi

echo "start-chrome: launching with ${#flags[@]} flags, profile $PROFILE_DIR" >&2
exec google-chrome-stable "${flags[@]}" about:blank
