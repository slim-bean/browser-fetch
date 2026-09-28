# browser-fetch

Read README.md and the relevant docs/ design before changes. This gateway owns
browser infrastructure and data; clients own agent-specific behavior/presentation.
Chrome lifecycle/profile ownership is external (launcher, Kubernetes, pi-assistant).

## Layout / boundaries

- `internal/browser`: default-context tab pool, settling, session leases.
  `currentPool` serializes Chrome-generation changes; each pool has its own allocator.
  In-flight requests may fail across restart; never automatically replay actions.
- `internal/session`, `internal/macro`: typed actions, assertions, evidence, human-
  recorded/approved deterministic flows. Preserve these separately from raw CDP.
  Read `docs/l1-design.md` and `docs/macros.md` before modifying them.
- `internal/server`: token classes are route-specific. `/fetch` accepts all three;
  session/macro routes accept driver/root; CDP, history, runtime and authenticated
  admin routes require root. Public health/optional metrics remain public. Never
  replace route-class authorization with "any recognized token" (CDP would bypass
  driver restrictions). Tests must exercise real routes for every token class.
- `cdp.go`: fixed upstream, normalize Host, strip gateway auth, rewrite debugger
  URLs, preserve WebSocket hijacking. public-url is explicit behind TLS/path proxies.
  Root CDP intentionally allows caller JS/cookie access; session guarantees do not
  apply to it. Logs cover connections, not frame-level actions.
- `internal/history`: pure-Go Chromium record reader (`modernc.org/sqlite`, CGO=0).
  Only operator-root profile History/sidecars are copied, boundedly, through os.Root
  into private temporary snapshots. SQLite never opens the live source writable.
  Validate changes/integrity, clean up on failure, surface unreadable/capped data.
  No caller paths, SQL, process commands, query-language parsing, ranking or formatting.
  `docs/history.md` specifies protocol v2. No Node/helper or pi repository build pin.
- Fetch URL guards and scheduler defaults stay enabled. Test fixtures can opt into
  private targets; production assistant launch must not. CDP/history grants remain
  explicit and separate from those safeguards.
- Logs can contain private URLs. Do not log tokens, query contents or history rows.

## Testing

Run gofmt, `go test -race ./...`, `go vet ./...`, and
`CGO_ENABLED=0 go build -o bin/browser-fetch .` (ignored). Preserve the full session/
macro regression suite. `../pi-assistant/test/live.ts` covers shared cookies, clients,
restart, native history and desktop focus with an isolated synthetic profile. Never
use personal histories or logged-in finance accounts in tests.
