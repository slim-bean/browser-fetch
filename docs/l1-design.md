# L1 interaction primitives — design note

Phase 1 of the finance-fetch plan (see edw-fin `/workspace/finance-fetch/PLAN.md`)
extends this gateway from a stateless reader ("render this URL") into a safe
interaction surface. The constraints that shaped it:

1. **No caller-supplied JavaScript on the session API.** It is a vocabulary of typed
   actions, not an interpreter. A prompt-injected page can at worst lie about
   what it showed; it cannot make the driver execute attacker JS against the
   logged-in profile.
2. **Session primitives route through the scheduler.** Every primitive that touches a
   page goes through the same per-host pacing as `/fetch`. A flow that clicks
   three times is paced like a human clicking three times.
3. **Every action is recorded.** Actions append to a per-session evidence log
   (the existing `reqlog` ring gains an action ring). Failed assertions
   automatically capture a screenshot as evidence.
4. **Credentials are never typed blind.** The `type` primitive refuses fields
   typed as passwords unless a `assert` action has passed within the last
   `assert_ttl` (default 30s) — enforcement lives here, in the gateway, so no
   client bug can bypass it.

The separate optional raw-CDP interface is a **root-token-only** administrative
capability, not part of this restricted driver surface. It deliberately permits
arbitrary page JavaScript and bypasses session pacing/assertions. Do not grant the
root token to restricted drivers; reader/driver tokens are denied CDP and history.

## API

A **session** is a leased tab that stays on one page between actions (it is not
parked to about:blank until closed). Sessions are explicitly opened, keyed by
ID, and expire if idle.

```
POST /session/open    {"host"}              -> {session_id, page_id}
POST /session/action  {"session_id","action":{...}}  -> action result
POST /session/close   {"session_id"}
GET  /sessions        (debug listing)
```

Actions (all fields typed; unknown fields rejected):

- `navigate {url}` — same URL guard as /fetch
- `click {selector}` — Playwright-style selector, resolved via CDP, no eval
- `type {text, field:"password"|"text", submit:bool}` — refuses password fields
  without a recent passing assertion
- `wait {selector|url_pattern, timeout_ms}` — block until visible/matched
- `screenshot {}` — PNG bytes, base64; never mid-typing (type flushes first)
- `content {}` — the current snapshot (url/title/html), reuse of settle's read
- `download {}` — trigger + capture the next CDP `Browser.downloadWillBegin`
  stream; returns the file bytes with a sha256
- `assert {kind:"url"|"title"|"landmark"|"screenshot", ...}` — layered screen
  identity; only `exact` kinds gate credential typing

## What is deliberately NOT here

- No JS eval route (this is the whole point).
- No multi-tab sessions (one tab per session keeps blast radius small).
- No headless mode (the existing browser is headed; screen identity depends on
  real rendering).

## Sequencing

This commit lands the session/action/trace surface with assertions and the
evidence log; the download capture and driver-vs-reader token split follow in
subsequent commits once the core is exercised against the live Chrome.