# browser-fetch

Renders web pages with a **real Chrome** and serves the rendered HTML over a
small authenticated HTTP API, so automated clients can read sites that block
non-browser HTTP clients.

Any client can use it. It is also the `browser` reader proxy for
[pi-search](https://github.com/slim-bean/pi-search)'s `web_fetch` tool.

## Why

Bot protection (WAFs, JS challenges, captcha gates) fingerprints TLS and
HTTP/2 behaviour. No header tweaking makes a server-side HTTP client look like a
browser — a Chrome User-Agent from `curl` gets the same 403. Across the sites
we measured, every User-Agent was blocked identically.

A genuine browser on a residential connection passes those checks for the right
reasons, and adds two things no hosted reader can:

- **A profile you can log into by hand.** Where a site gates content behind an
  account or an interactive captcha, do it once in the visible window; the
  cookies persist, and later fetches of that site took ~1-2 s with no
  challenge.
- **Human-in-the-loop.** With `-assist-timeout`, a challenge the automation
  can't clear stays on screen for you to click, and the fetch completes.

## Architecture

```
client (e.g. pi-search) ──HTTP──> browser-fetch ──CDP──> Chrome (headed, your profile)
                                 │
              scheduler: per-host serialisation + jittered gap,
              global tab cap, in-flight dedupe, queue deadlines
```

Chrome locks its `user-data-dir`, so "many processes sharing one profile" is
impossible. Parallelism is **tabs**, which is what we want anyway: tabs in the
default browser context share cookies, so trust earned in one benefits all.

| Package | Role |
|---|---|
| `internal/browser` | CDP attach, tab pool, navigation, challenge settling, assist mode |
| `internal/scheduler` | pacing: host gate → cooldown → global slot; dedupe; stats |
| `internal/challenge` | interstitial detection (JS challenges, humanity gates, reCAPTCHA, hCaptcha, and the common WAF vendors) |
| `internal/urlguard` | rejects non-HTTP schemes and private/loopback/link-local targets, after DNS resolution |
| `internal/server` | HTTP API, auth, request logging, `/debug`, metrics |
| `internal/reqlog` | bounded ring of recent requests for `/debug` |

## Deployment options

| | Where | Notes |
|---|---|---|
| **Kubernetes** | [`deploy/kubernetes/`](deploy/kubernetes/README.md) | Chrome + VNC + gateway in one pod, profile on a PVC, clear captchas over VNC/Guacamole |
| **systemd** | [`deploy/systemd/`](deploy/systemd/browser-fetch.service) | Chrome on your desktop session, gateway alongside it |

Either way the IP matters more than the packaging: bot protection scores
datacenter ranges harshly, so run this where your egress is residential (a home
cluster or workstation). In a cloud VPC you have rebuilt a hosted reader with
extra steps.

## Quick start (local / systemd)

On the VM (Linux with a desktop), or locally for testing:

```bash
# 1. Chrome, headed, dedicated profile, DevTools on loopback
./deploy/chrome-launch.sh

# 2. The gateway
go build -o ~/.local/bin/browser-fetch .
BROWSER_FETCH_TOKEN=$(openssl rand -hex 24) ~/.local/bin/browser-fetch
```

Then from pi:

```bash
export PI_SEARCH_FETCH_PROXY=browser
export PI_SEARCH_BROWSER_URL=http://127.0.0.1:8377     # or the VM's private IP
export PI_SEARCH_BROWSER_TOKEN=…                       # same token
export PI_SEARCH_FETCH_PROXY_HOSTS=host.example,another.example
```

For a persistent setup see `deploy/systemd/browser-fetch.service`.

## API

| Route | Auth | Purpose |
|---|---|---|
| `POST /fetch` | yes | `{"url":…, "timeout_ms":…, "assist_ms":…}` → `{url,title,html,status,page_id,…}` |
| `GET /fetch?url=…` | yes | same, convenient for `curl` |
| `GET /healthz` | no | Chrome connectivity, version, tab pool, pending assists |
| `GET /stats` | yes | scheduler snapshot: queue, per-host cooldowns, totals |
| `GET /debug` | yes | live HTML dashboard; `?format=json` for machines |
| `GET /debug/pprof/…` | yes | Go profiling |
| `GET /metrics` | optional | Prometheus (`-metrics-auth` to require the token) |

Error responses carry a `code`: `challenge`, `nav_error`, `rejected_url`,
`timeout`, `bad_request`, `chrome_unavailable`. Clients should treat the first
three as "the target refused" and the rest as "the gateway is broken" -- pi-search
does, so its agent gets an accurate explanation either way.

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  -X POST localhost:8377/fetch \
  -d '{"url":"https://example.com/"}' | jq '.title, (.html|length)'
```

## Configuration

Every flag has a `BROWSER_FETCH_*` environment variable; flags win.

| Flag | Default | Notes |
|---|---|---|
| `-addr` | `127.0.0.1:8377` | Use the VM's private IP to reach it from the host |
| `-token` | — | **Required** unless `-allow-no-token` |
| `-chrome-url` | `http://127.0.0.1:9222` | DevTools endpoint; `http://` is resolved to the ws URL |
| `-max-tabs` | `4` | Concurrent navigations; ~150–300 MB each |
| `-host-gap` | `1500ms` | Minimum delay between navigations to one host |
| `-host-jitter` | `750ms` | Random extra delay, so pacing isn't metronomic |
| `-request-timeout` | `45s` | Whole `/fetch` call including queue wait |
| `-nav-timeout` | `20s` | One navigation |
| `-challenge-wait` | `15s` | Let an interstitial resolve itself |
| `-challenge-retries` | `1` | Re-navigate after an unresolved challenge |
| `-challenge-retry-delay` | `2s` | Wait before that retry |
| `-assist-timeout` | `0` (off) | Hold an unsolved challenge for a human to click |
| `-block-media` | `false` | Off by default: some scoring notices images never loaded |
| `-allow-private` | `false` | Permit loopback/private targets (SSRF guard off) |
| `-debug` | `true` | `/debug` and `/debug/pprof` |
| `-debug-ring` | `200` | Recent requests retained |
| `-metrics-auth` | `false` | Require the token on `/metrics` |
| `-log-format` / `-log-level` | `text` / `info` | `json` for shipping to Loki |

## Pacing model

Chrome will load a dozen tabs at once; bot vendors score burst rate per
IP+session+host. So the scheduler enforces:

- **per-host concurrency 1**, with `host-gap + rand(host-jitter)` measured from
  the *end* of the previous navigation;
- **global cap** of `max-tabs` concurrent navigations across all hosts;
- **host gate before the global slot**, so a request waiting out a cooldown does
  not occupy a slot another host could use;
- **in-flight dedupe** by URL — agents re-request the same page constantly;
- **context-aware waits**, so a queued request fails on its deadline rather than
  piling up.

`internal/scheduler` has tests covering each of these.

## Operational notes

- **Every request is logged**, with request id, host, outcome, target status,
  bytes, duration and queue wait. `/debug` keeps the last N in memory.
- **Assist mode** logs a `WARN` naming the tab, and pending assists appear in
  `/healthz` and `/metrics` (`browser_fetch_assists_pending`).
- **Tabs are never closed**, only parked on `about:blank`, so Chrome always has
  a target and cookies stay shared. Broken tabs are retired and replaced.
- **Chrome restarts** are the human's business: the gateway reconnects on its
  own. Restart Chrome (not the profile) if memory creeps; back up the profile
  directory to keep your logins.
- **Security.** Bind to loopback or the VM's private interface, never `0.0.0.0`
  on an untrusted network. Anyone who can reach the port can drive a browser
  holding your sessions. The URL guard blocks private/link-local targets so a
  prompt-injected URL can't reach VM-internal services or cloud metadata; the
  gateway never returns cookies and never runs caller-supplied JavaScript.
- **Etiquette.** This fetches one page per explicit request, at human pace. It
  is not a crawler and shouldn't become one: no prefetching, no link walking.
  Plenty of sites disallow crawlers in robots.txt; a person reading a page they
  asked for is not what those policies target, but bulk collection with this
  would be. Respect each site's terms, and don't use it to evade paywalls,
  rate limits or access controls.

## Development

```bash
go test ./...              # scheduler pacing, URL guard, challenge detection
go test -race ./...
go vet ./... && gofmt -l .
```

`internal/challenge/testdata/` holds two real captured pages, with origins
scrubbed: a captcha interstitial (must be detected) and a content page whose
telemetry code happens to contain the token `js_challenge` (must **not** be
detected — that false positive would discard every successful fetch of such a
site).
