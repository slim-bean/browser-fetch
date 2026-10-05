# Paginated visual snapshots (protocol v1)

This is optional on stateless page retrieval; session screenshots and root CDP
remain separate. No caller JavaScript, filesystem paths or session IDs are accepted.
Chrome lifecycle/profile ownership remains external.

## Requests

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -X POST "$GATEWAY/fetch" \
  -d '{"url":"https://example.com/listing","screenshot":true}'

curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -X POST "$GATEWAY/fetch/screenshot" \
  -d '{"capture_id":"<returned ID>","segment":2}'
```

`POST /fetch` retains its existing response with an optional `screenshot` object.
`POST /fetch/screenshot` returns that object directly. Both accept root, driver or
reader credentials. `GET /fetch` remains text-only. Existing fetch URL guards,
pacing, challenge/assist handling and cancellation still apply to initial capture.
Continuation accesses memory only: no Chrome connection, navigation, scrolling,
URL disclosure or URL guard bypass. Unknown fields/trailing JSON are rejected.

## Segment response

```json
{
  "version": 1,
  "capture_id": "opaque-random-id",
  "url": "https://example.com/listing",
  "title": "Listing",
  "expires_at": "2026-10-03T12:00:00Z",
  "segment": 1,
  "segments": 3,
  "viewport_width": 1280,
  "page_width": 1280,
  "page_height": 4000,
  "captured_height": 4000,
  "truncated": false,
  "horizontal_truncated": false,
  "y_start": 0,
  "y_end": 1400,
  "image": {
    "data": "<base64 JPEG>",
    "mime_type": "image/jpeg",
    "width": 1280,
    "height": 1400,
    "bytes": 123456,
    "sha256": "<hex SHA-256 of decoded JPEG bytes>"
  }
}
```

Segment numbers are **one-based**. Coordinates are original CSS pixels with an
exclusive `y_end`. Image dimensions can be smaller after encoding-budget scaling;
coordinates still describe the unscaled source crop. The capture ID is opaque,
not a path or tab ID. `GET /runtime` advertises `screenshotProtocol: 1` and
`capabilities.screenshots: true` (root only); clients may also validate the fetch
response directly. An old server rejects the new field; do not silently retry
without screenshots.

## Capture policy and limits

Policy constants live in `internal/screenshot/image.go` and `store.go`.

| Limit | Value |
|---|---|
| Viewport / device scale | 1280 × 900 CSS pixels / 1 |
| Source clip | x=0, y=0, width=1280, height=min(page height, 12000) |
| Source PNG byte cap | 24 MiB; dimensions checked before decode |
| Segment region | full capture width × at most 1400 CSS pixels |
| Vertical overlap | 100 CSS pixels |
| Encoded segment | JPEG, at most 1280 × 1400 pixels / 384 KiB |
| Segment count | at most 10 |
| Capture/processing concurrency | 2 per browser manager |
| Store | 32 captures, 64 MiB encoded tile bytes total |
| Lifetime | 10 minutes from insertion; never extended by reading |

The screenshot clip bounds Chrome's bitmap allocation **before capture**. A single
source PNG freezes the pixels, then segments are cropped before JPEG encoding.
Encoding tries bounded quality reduction, then area-averaged downsizing if needed.
Only processed tiles are retained; the source bitmap/PNG is discarded. Emulation
is cleared before a healthy worker is reused (a failed reset retires the worker).
Text and visual requests, assist policies and visual credential scopes use distinct
scheduler dedupe keys. Initial responses get separate capture IDs even when work
was deduplicated. Store capacity exhaustion fails explicitly (`screenshot_capacity`,
503), rather than evicting unexpired captures or silently dropping the image.

The gateway waits up to 1.5 seconds for visible images after challenge settling,
then refreshes the HTML snapshot and captures within the same lease. It never
scrolls to force lazy loading, clicks galleries, walks links or loads nested scroll
regions. Dynamic content can change during capture; this is not an atomic DOM +
render transaction. All continuation pixels, however, are immutable. Fixed/sticky
UI is captured according to Chrome's full-page rendering, not repeated per segment.
`-block-media=true` rejects visual requests; it does not return known-empty photos.

## Privacy, authorization and errors

Captures can contain signed-in/private content. They are explicit opt-in, live
only in process memory, disappear on restart, and are pruned on store access plus
the minute reaper. Expired captures are inaccessible immediately; physical memory
retention after idle expiry is at most one reaper interval, excluding Go GC timing.
Never log image bytes or write them to the request/debug evidence ring.

Ownership is the exact configured root/driver/reader credential class. A root caller
cannot retrieve a reader capture by ID; shared credentials intentionally share one
scope. Explicit no-token deployments have one operator-owned scope. IDs are randomly
generated, but possession alone is insufficient: both routes require authorization.
Unknown, expired and wrong-scope IDs all produce the same `screenshot_unavailable`
410 with no page identity. Restart has the same result. Invalid segment numbers
produce `bad_request` 400. Continuation must never trigger implicit recapture.

Screenshot responses set `Cache-Control: no-store`. Image attachments will still
be present in downstream client transcripts/model context, as explicitly requested;
gateway expiration does not erase those copies. Clients should not duplicate
base64 in extra tool metadata and should validate version, dimensions, byte budgets
and checksums before attaching images.

## Tests

`go test -race ./...` covers cropping, overlap, source and output limits, noisy
image downscaling, cancellation, capacity, expiry, concurrent reads and real-route
credential isolation. `TestLivePaginatedScreenshots` is opt-in via
`BROWSER_FETCH_TEST_CHROME_URL`, and must only use an isolated Chrome. It verifies
actual CDP capture, all continuations, truncation, no continuation navigation and
text/visual dedupe separation. From adjacent pi-search, `npm run test:live:screenshot`
provides that temporary Chrome and also checks registered tool image attachments.
