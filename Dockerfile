# browser-fetch: a real Chrome on a virtual display, reachable over VNC, with
# the fetch gateway attached to it via CDP.
#
# amd64 only: Google publishes Chrome for linux/amd64 only. Chromium exists for
# arm64, but reports a different UA brand and is marginally more bot-flagged, so
# we prefer real Chrome and pin scheduling to amd64 nodes.
#
# Layout inside the container:
#   Xvfb        :0        virtual display
#   fluxbox               minimal WM, so Chrome has a window manager to talk to
#   x11vnc      :5900     view/control the display (guacd connects here)
#   chrome      :9222     DevTools on loopback only
#   browser-fetch :8377   the HTTP API
# supervisord runs all five and restarts any that die.

# ---- build the gateway -------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
      -ldflags="-s -w" -o /out/browser-fetch .

# ---- runtime -----------------------------------------------------------------
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Chrome's apt repo, plus the display stack. Fonts matter twice over: pages
# render correctly, and a bare font list is itself a bot signal.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg \
 && curl -fsSL https://dl.google.com/linux/linux_signing_key.pub \
      | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
 && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
      > /etc/apt/sources.list.d/google-chrome.list \
 && apt-get update && apt-get install -y --no-install-recommends \
      google-chrome-stable \
      xvfb x11vnc fluxbox xdotool \
      supervisor tini procps \
      fonts-liberation fonts-dejavu-core fonts-noto-core fonts-noto-color-emoji \
      dbus-x11 \
 && apt-get purge -y gnupg \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

# Chrome refuses to run as root with its sandbox enabled, and we want the
# sandbox (this browser visits arbitrary pages). supervisord stays root as PID 1
# so it can prepare /tmp/.X11-unix and the PVC, but every program drops to this
# unprivileged user.
RUN useradd --uid 1000 --create-home --home-dir /home/chrome --shell /usr/sbin/nologin chrome

COPY --from=build /out/browser-fetch /usr/local/bin/browser-fetch
COPY deploy/container/supervisord.conf /etc/supervisor/supervisord.conf
COPY deploy/container/start-chrome.sh  /usr/local/bin/start-chrome
COPY deploy/container/start-vnc.sh     /usr/local/bin/start-vnc
COPY deploy/container/entrypoint.sh    /usr/local/bin/entrypoint
RUN chmod +x /usr/local/bin/start-chrome /usr/local/bin/start-vnc /usr/local/bin/entrypoint

# Defaults; override in the manifest.
ENV DISPLAY=:0 \
    SCREEN_GEOMETRY=1920x1080x24 \
    CHROME_PROFILE=/profile/chrome \
    CHROME_DEBUG_PORT=9222 \
    CHROME_NO_SANDBOX=false \
    CHROME_UID=1000 \
    HOME=/home/chrome \
    VNC_PORT=5900 \
    BROWSER_FETCH_ADDR=0.0.0.0:8377 \
    BROWSER_FETCH_CHROME_URL=http://127.0.0.1:9222 \
    BROWSER_FETCH_LOG_FORMAT=json

EXPOSE 8377 5900
VOLUME ["/profile"]

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint"]
