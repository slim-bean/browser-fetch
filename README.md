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
| `GET /runtime` | yes | service identity + configured `capabilities: {cdp,history}` (no secrets) |
| `/cdp/…` | yes | optional Chrome HTTP discovery + WebSocket proxy, full browser control |
| `POST /history/search` | yes | optional pi-browser history query (same parameters as `browser_history`) |
| `GET /history/sources` | yes | configured remote source ids/labels, not filesystem paths |
| `GET /debug` | yes | live HTML dashboard; `?format=json` for machines |
| `GET /debug/pprof/…` | yes | Go profiling |
| `GET /metrics` | optional | Prometheus (`-metrics-auth` to require the token) |
| `POST /session/open` | driver | `{"host":…}` → `{session_id,…}`; leases a tab that holds page state |
| `POST /session/action` | driver | `{"session_id":…, "action":{kind,…}}` → action result; see below |
| `POST /session/close` | driver | `{"session_id":…}` — parks the tab and frees the lease |
| `GET /sessions` | driver | live sessions with idle time and credential-typing eligibility |
| `GET /session/log?session_id=…` | driver | the session's per-action evidence log |
| `POST /macro/record/start` / `stop` | driver | human records a site flow; see Macros below |
| `GET /macros`, `GET /macro?id=…` | driver | list / fetch stored macros for review |
| `POST /macro/approve` | driver | mark a reviewed macro replayable |
| `POST /macro/replay` | driver | run an approved macro in a session |
| `POST /macro/resume` | driver | signal a paused replay to continue |

### Session actions

Actions are a typed vocabulary — **no caller-supplied JavaScript is ever
evaluated**. `kind` selects the action; unknown fields are rejected.

| kind | fields | notes |
|---|---|---|
| `navigate` | `url` | gated by `-allow-hosts` when set, plus the usual URL guard |
| `click` | `selector`, `candidates` | first visible match; `candidates` is the internal selector ladder used by macro replay — the agent API refuses `click`/`type` entirely |
| `type` | `text`, `field`, `submit` | `field=password` refused unless an exact `assert` passed within `-assert-ttl` (default 30s) |
| `wait` | `selector` or `url_regexp`, `timeout_ms` | blocks until visible/matched |
| `screenshot` | `full_page` | PNG, base64; result includes `sha256` |
| `content` | — | current `{url,title,html}` snapshot |
| `download` | `click_selector`, `filename_regexp`, `timeout_ms`, `max_bytes` | captures the file via CDP, returns base64 + `sha256` |
| `assert` | `expect` (`url`,`title`,`landmark`,`landmarks`), `pattern`, `landmarks`, `min_match` | layered screen identity; only exact `url`/`title` matches enable credential typing |

Every action is recorded in the session's evidence log with outcome and
duration; password values are recorded as length only, never text.

### Macros (human-recorded site flows)

Interactive actions are not agent-reachable: the agent API rejects `click`
and `type`. A human records flows instead — the gateway injects its own
capture script (which reports fingerprints, never secret values) while the
human drives the browser, stores the draft unapproved, and only a human
`/macro/approve` makes it replayable. Replay is deterministic: recorded
selector ladders plus generated text XPaths, aborting at the first step that
doesn't match — it never improvises. `pause` steps wait for a human (OTP,
CAPTCHA) and verify a `resume_assert` before continuing. Secrets are stored
as `op://` references and resolved only at replay time. See
[`docs/macros.md`](docs/macros.md) for the macro format and trust model.
Macro endpoints need `-macro-store DIR` (disabled when unset).

### Token classes

The root token ( `-token` ) grants everything. Two optional extra tokens
narrow what a caller can do:

- `-driver-token` — sessions and fetch. This is what the finance flow engine uses.
- `-reader-token` — `/fetch` only; can never open a session.

Error responses carry a `code`: `challenge`, `nav_error`, `rejected_url`,
`timeout`, `bad_request`, `chrome_unavailable`. Clients should treat the first
three as "the target refused" and the rest as "the gateway is broken" -- pi-search
does, so its agent gets an accurate explanation either way.

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  -X POST localhost:8377/fetch \
  -d '{"url":"https://example.com/"}' | jq '.title, (.html|length)'
```

## Single-port assistant gateway

The same HTTP listener can serve page retrieval, interactive CDP and browser history.
Chrome stays on loopback; clients do **not** need a raw CDP port or a profile mount.

```bash
export BROWSER_FETCH_TOKEN=…
export PI_BROWSER_HISTORY_CHROMIUM_ROOTS='[{"browser":"assistant","dir":"/profile/chrome"}]'
export BROWSER_FETCH_HISTORY_COMMAND='["node","/opt/pi-browser/bin/history.ts"]'
browser-fetch -enable-cdp -background-tabs -addr 0.0.0.0:8377 -chrome-url http://127.0.0.1:9222
```

CDP and history are opt-in for the Go binary, and require a token even with
`-allow-no-token`. The container includes Node + pi-browser and configures the history
helper; its entrypoint defaults the history root to `CHROME_PROFILE` (override
`PI_BROWSER_HISTORY_CHROMIUM_ROOTS` to select other explicit roots). The Kubernetes
manifest also enables CDP and background worker tabs. The same bearer token authorizes
all three interfaces. This is intended for your trusted assistant workloads—not
per-tab/per-action isolation or protection against hostile authorized agents.

`/cdp/json/version` and `/cdp/json/list` rewrite `webSocketDebuggerUrl` back through
the gateway. All HTTP discovery and WebSocket upgrades authenticate. Only fixed
Chrome upstream paths are proxied; gateway credentials are not forwarded to Chrome.
For TLS termination or a stripped ingress prefix, set e.g.
`-public-url=https://browser.example/assistant`. Forwarded headers aren't trusted
implicitly. The proxy handles WebSockets; ensure any outer proxy does too.

The history helper is **pi-browser's own implementation**, called over bounded JSON
stdin/stdout (no extra listener). It only searches operator-configured Chromium roots,
never host-wide browser discovery, and accepts query parameters—not paths, SQL or
commands—from clients. Two helpers may run concurrently; excess requests return 429.
Queries have a 15-second deadline, 64-KiB input limit and 2-MiB output limit. Missing
helpers return an explicit error, not a fallback. `/runtime` advertises configuration,
not proof that a helper's files/data are currently readable.

CDP intentionally grants more authority than `/fetch`: it can read cookies, execute
page JavaScript and navigate anywhere Chrome can reach. Fetch URL guards and pacing
do not apply to raw CDP. Logs observe HTTP requests / WebSocket connection lifetime,
**not individual CDP commands or every browser-originated network request**. Keep the
endpoint on a trusted network or behind an authenticated TLS boundary. VNC remains an
optional separate human-access path; all agent traffic uses just the HTTP port.

### Container source dependency

Release builds require `--build-arg PI_BROWSER_REF=<commit-containing-bin/history.ts>`.
The image fetches that pi-browser ref; pin a commit rather than a moving branch.
For uncommitted local development without publishing another repository first:

```bash
scripts/stage-history.sh ../pi-browser
# Add --build-arg PI_BROWSER_SOURCE=local to your normal image build.
```

This creates an ignored source archive, used **only** with the explicit `local`
build argument. Default builds ignore it and require `PI_BROWSER_REF`. There is no
npm dependency tree or second server for history; the CLI needs Node >=22.19.

## Configuration

Every flag has a `BROWSER_FETCH_*` environment variable; flags win.

| Flag | Default | Notes |
|---|---|---|
| `-addr` | `127.0.0.1:8377` | Use the VM's private IP to reach it from the host |
| `-token` | — | **Required** unless `-allow-no-token` |
| `-chrome-url` | `http://127.0.0.1:9222` | DevTools endpoint; use HTTP discovery for restart recovery and the CDP proxy |
| `-enable-cdp` | `false` | Enable authenticated `/cdp/` HTTP/WebSocket proxy |
| `-public-url` | request scheme/host | External gateway base URL for discovery behind TLS/path-rewriting proxies |
| `-history-command` | disabled | JSON array of executable + argv; e.g. `["node","/opt/pi-browser/bin/history.ts"]` |
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
| `-background-tabs` | `false` | Create worker tabs without activating Chrome (`BROWSER_FETCH_BACKGROUND_TABS`); enabled by pi-assistant |
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
- **Chrome lifecycle** remains external: launch it yourself or use pi-assistant.
  With an HTTP CDP endpoint, the gateway checks Chrome's websocket identity before
  each fetch and replaces its allocator/tab pool after a restart. In-flight work
  can fail; it is not replayed. Reuse the profile directory to retain cookies/history.
  A fixed `ws://...` endpoint must be updated when its browser generation changes.
- **Background work.** `-background-tabs` allocates browser connections without
  creating foreground tabs, then creates each worker with CDP `background: true`.
  It does not activate the browser for assist mode; a human can bring the window
  forward deliberately. Initial desktop launch behavior belongs to the launcher.
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
