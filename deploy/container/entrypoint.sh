#!/usr/bin/env bash
# Prepare persistent state, then hand off to supervisord.
set -euo pipefail

PROFILE_DIR="${CHROME_PROFILE:-/profile/chrome}"
CHROME_UID="${CHROME_UID:-1000}"
mkdir -p "$PROFILE_DIR" /profile/vnc /tmp/.X11-unix
chmod 1777 /tmp/.X11-unix || true

# The PVC arrives owned by root (or by fsGroup). Chrome runs unprivileged, so
# hand it the profile. Cheap on an existing profile, and idempotent.
chown -R "$CHROME_UID:$CHROME_UID" /profile 2>/dev/null || \
  echo "entrypoint: could not chown /profile; set fsGroup or run as root" >&2

# Chrome refuses to start on a profile that still holds a lock from an unclean
# shutdown -- which is every pod eviction, OOM kill and node reboot. The lock is
# meaningless here because the PVC is RWO and we run exactly one replica.
for lock in SingletonLock SingletonCookie SingletonSocket; do
  if [[ -e "$PROFILE_DIR/$lock" ]]; then
    echo "entrypoint: clearing stale $lock" >&2
    rm -f "$PROFILE_DIR/$lock"
  fi
done

# Chrome writes an "exited cleanly" flag; if it is missing it shows a session
# restore bubble that covers the page. Rewrite it so fetches start clean.
prefs="$PROFILE_DIR/Default/Preferences"
if [[ -f "$prefs" ]]; then
  sed -i 's/"exit_type":"Crashed"/"exit_type":"Normal"/g; s/"exited_cleanly":false/"exited_cleanly":true/g' "$prefs" || true
fi

# Optional VNC password. Without one x11vnc listens unauthenticated, which is
# fine only on a trusted network behind a NetworkPolicy.
if [[ -n "${VNC_PASSWORD:-}" ]]; then
  x11vnc -storepasswd "$VNC_PASSWORD" /profile/vnc/passwd >/dev/null 2>&1
  chown "$CHROME_UID:$CHROME_UID" /profile/vnc/passwd
  echo "entrypoint: VNC password set" >&2
else
  rm -f /profile/vnc/passwd
  echo "entrypoint: VNC has NO password (relying on network isolation)" >&2
fi

exec /usr/bin/supervisord -c /etc/supervisor/supervisord.conf
