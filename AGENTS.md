# browser-fetch

Read README.md for architecture, deployment and API. This is the Go page-reading
gateway, not an interactive browser agent. Chrome lifecycle/profile ownership is
external (standalone launcher or pi-assistant).

- `internal/browser`: default-context tab pool, settling/challenge assistance.
  `currentPool` serializes Chrome-generation changes; each pool captures its own
  allocator. In-flight requests may fail across a restart; never replay actions.
- `-background-tabs` allocates a control connection with `chromedp.Targets`, creates
  an explicit background target, then attaches a child context. If no page exists,
  create the first window minimized. Cancelling workers must not close Chrome.
- `internal/server`: authenticated `/fetch`, `/runtime`, stats/debug APIs, opt-in
  `/cdp/` reverse proxy and `/history/search` + `/history/sources` helper bridge.
  `/runtime` reports identity/configured capabilities, never tokens. CDP and history
  require a bearer token even if allow-no-token is set for ordinary local fetches.
- `cdp.go`: fixed Chrome upstream, normalize upstream Host, strip gateway auth,
  rewrite debugger URLs for both HTTP discovery forms, preserve WebSocket hijacking.
  Use explicit public-url behind TLS/path-rewriting proxies, not untrusted forwarded
  headers. Logs cover connections, not CDP frame-level actions.
- `history.go`: execute only configured argv, never caller commands. Bound input,
  output, concurrency and time. pi-browser's Node CLI owns querying/ranking/snapshots;
  only explicit roots, no broad host discovery or raw profile downloads.
- Docker release builds take PI_BROWSER_REF; an explicit PI_BROWSER_SOURCE=local
  uses the ignored archive made by scripts/stage-history.sh. No extra history port.
- `internal/urlguard` and scheduler safeguards stay enabled by default. Test-only
  loopback fixtures may explicitly use `-allow-private`; assistant production may not.
- Logs can contain private URLs. Don't log tokens/cookies or export profiles.

Run `gofmt`, `go test -race ./...`, `go vet ./...`. Build local integration binary
with `go build -o bin/browser-fetch .` (ignored). `../pi-assistant/test/live.ts` tests
shared cookies, multiple processes, restart and desktop focus with a temporary
profile; never use personal browser sessions in tests.
