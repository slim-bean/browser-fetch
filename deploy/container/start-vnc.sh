#!/usr/bin/env bash
# x11vnc on the virtual display, for guacd (or any VNC client) to attach to.
set -euo pipefail

args=(
  -display "${DISPLAY:-:0}"
  -rfbport "${VNC_PORT:-5900}"
  -forever          # keep serving after a client disconnects
  -shared           # allow more than one viewer
  -noxdamage        # Xvfb + XDAMAGE is unreliable; this trades CPU for correctness
  -repeat
  -xkb
  -nolookup
  -quiet
)

if [[ -f /profile/vnc/passwd ]]; then
  args+=(-rfbauth /profile/vnc/passwd)
else
  args+=(-nopw)
fi

exec x11vnc "${args[@]}"
