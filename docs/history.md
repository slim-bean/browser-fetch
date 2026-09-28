# Native Chromium history — protocol v2

browser-fetch owns access to its Chrome profile. Clients own query-language parsing,
exact URL-host filtering, deduplication, ranking, grouping and presentation. The
server returns normalized records, not pi tool results or Markdown. It has no
pi-browser source dependency, Node runtime, helper process, or extra listener.

## Enable

```bash
BROWSER_FETCH_TOKEN=… browser-fetch -history-root=/profile/chrome -history-source=assistant
```

`history-root` is an operator-selected Chromium **user-data directory**, containing
`Default/History`, `Profile 1/History`, etc. Empty disables history. The container
entrypoint defaults it to `CHROME_PROFILE`; explicitly set
`BROWSER_FETCH_HISTORY_ROOT=""` to disable. `history-source` defaults to `assistant`
and is a label/id prefix, not a path. Only immediate non-symlink profile directories
with a regular `History` file are discovered, at most 64 profiles. There is no
host-wide browser discovery and no arbitrary path/SQL accepted from clients.

History and raw CDP require the **root token**. Reader tokens grant `/fetch` only;
driver tokens grant fetch and the existing session/macro APIs. Do not give a
restricted finance driver the root token: root CDP deliberately bypasses the typed
session restrictions. History filters are data selection, not authorization scopes.

## HTTP contract (same gateway port)

`GET /history/sources`:

```json
{
  "version": 2,
  "sources": [
    {"id":"assistant/Default","browser":"assistant","profile":"Default","label":"assistant/Default"}
  ]
}
```

IDs are based on profile directory names and remain stable as other profiles are
added. Labels do not read Chrome's unrelated `Local State` file or expose absolute
paths. Treat IDs as opaque; use returned IDs rather than constructing file paths.

`POST /history/query`:

```json
{
  "version": 2,
  "sourceId": "assistant/Default",
  "terms": ["grafana"],
  "excluded": ["advertisement"],
  "hosts": ["grafana.com"],
  "sinceMs": 1750000000000,
  "untilMs": 1760000000000,
  "limit": 20000
}
```

All fields except version/sourceId are optional. Unknown fields are rejected.

- `terms`: each must match URL or title, using parameterized SQLite `LIKE`.
- `excluded`: none may match URL or title. `%`, `_`, and backslashes are literal.
- `hosts`: an OR'ed **coarse URL substring prefilter**, like the local pi-browser
  SQL backend. Clients must enforce exact host/subdomain matching themselves.
- Times: inclusive Unix milliseconds. Chromium microseconds-since-1601 conversion
  happens in SQL; oversized raw timestamps never enter JavaScript.
- Limit: 0/omitted = 20,000; otherwise 1–20,000. At most 64 values per filter array,
  1–4096 bytes per value. Browser-internal URLs are not presentation-filtered here.

Response:

```json
{
  "version": 2,
  "source": {"id":"assistant/Default","browser":"assistant","profile":"Default","label":"assistant/Default"},
  "rows": [{"url":"https://grafana.com/", "title":"Grafana", "visits":3, "ms":1750000000123}],
  "truncated": false
}
```

Rows are ordered by last visit descending, then URL ascending; they are **candidates,
not relevance-ranked search results**. Hidden Chromium rows are omitted. Queries
read aggregate URL history (last visit/count), not a per-visit event log. Unicode
case matching follows SQLite LIKE, as in the local backend.

Each response is capped at 8 MiB and 20,000 rows. Oversized URL/title fields (16 KiB),
unsupported numeric values, row limits or response-size limits set `truncated`;
never present capped counts/rankings as complete. Narrow the query/time window.
No profile/database is downloaded to the client, and no source paths are returned.

## Reading live profiles

The reader uses `modernc.org/sqlite` (pure Go, pinned in go.mod; CGO not required).
Only `History`, `History-wal` and `History-journal` are copied into a private,
per-query temporary directory. The WAL index is rebuilt. Source accesses are
confined by `os.Root`; static symlink profiles/files are excluded. SQLite opens
**only the disposable copy**, so hot-journal recovery cannot write to the live
profile. Copies are checked for concurrent file changes and SQLite integrity;
unstable/corrupt snapshots are retried at most three times, then fail explicitly.
This is a best-effort filesystem snapshot, not an atomic Chrome export.

Total snapshot files are limited to 256 MiB; directories/files are owner-only and
removed after the query. Query/copy work checks a 15-second context deadline. Two
queries may run concurrently; excess requests return 429. Input is capped at 64 KiB.
A source that cannot be read is an error, never an empty successful history.

`/runtime` advertises `capabilities.history` (configured root, not proof of readable
data) and `historyProtocol: 2`. Root-path/database failures return 503, unknown source
IDs 404, invalid queries 400. Query contents and returned records aren't logged.

## Migrating from the temporary Node-helper implementation

- No `PI_BROWSER_REF`, `PI_BROWSER_SOURCE`, staging archive, Node image, or
  pi-browser checkout is needed to build browser-fetch.
- Remove `BROWSER_FETCH_HISTORY_COMMAND` / `-history-command`. A nonempty old env
  setting fails startup with migration advice; the old flag is no longer accepted.
- Set `BROWSER_FETCH_HISTORY_ROOT` instead of the helper's
  `PI_BROWSER_HISTORY_CHROMIUM_ROOTS`. That pi-browser env var still controls its
  **local** sources, but is not read by this server.
- Update remote clients for v2. `/history/search` returns 410 with update advice;
  `/history/sources` now reports v2 directory-based IDs. There is no fallback to
  subprocess execution or v1 formatted responses.
- Update the deployed image **and** manifest; old helper env entries must be removed.
  Existing session/macro APIs and profile contents do not need migration.

Once on v2, client query/ranking/formatting changes do not require rebuilding the
server. The language-neutral HTTP contract is the only cross-project dependency.
