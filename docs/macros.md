# Macros: human-recorded, agent-replayed site flows

## Model

The agent never operates a site. A human (Ed) performs a login/flow once in the
VNC-visible Chrome while the gateway records interaction events; the recording
becomes a **macro** that the agent may replay. Replay executes the same typed
primitives as a live session (click/type/screenshot/assert/…) but the action
sequence came from a human, not from an LLM.

Trust chain: human interaction → recorded macro → **human review/approval** →
deterministic replay with drift detection → evidence log per step.

Approval authority lives on a **separate admin band** the agent cannot reach:

- a second HTTP listener (`-admin-addr` / `BROWSER_FETCH_ADMIN_ADDR`, default
  off) serving a token-gated, server-rendered UI (no JavaScript) at which the
  human reviews each step's recorded context and then approves, revokes,
  edits (drop/reorder steps), or deletes macros;
- its own bearer token (`-admin-token` / `BROWSER_FETCH_ADMIN_TOKEN`) which
  must differ from every agent-facing token — the gateway refuses to start
  the band otherwise;
- the agent-facing `POST /macro/approve` is **removed** (410 Gone): approval
  the agent could grant or forge would certify nothing. There is no agent
  edit endpoint at all. The agent keeps read access (macros carry no values),
  record start/stop (drafts are inert until approved), and replay of approved
  macros;
- any edit of an approved macro clears its approval — an approval certifies
  the exact step list;
- failed draft stores park the recording as `<id>.orphan.json` (0600) so the
  human's clicks are never lost; the admin band can recover or discard parked
  drafts, and refuses to promote an orphan over an approved macro;
- the agent-facing replay handler reports outcomes into an admin-band
  dashboard ring so the human sees every replay (and every abort) that ran.

## Macro file format

```json
{
  "id": "chase-login-and-OFX",
  "site": "chase.com",
  "created": "2026-09-29T12:00:00Z",
  "approved": { "by": "ed", "at": "2026-09-29T13:00:00Z" },
  "profile_hash": "sha256:…",   // browser profile the macro was recorded on
  "steps": [
    { "action": { "kind": "navigate", "url": "https://chase.com" } },
    { "action": { "kind": "assert", "expect": "url", "pattern": "…\\.com/auth$" } },
    { "action": { "kind": "type", "field": "password",
                  "secret": "op://Finance/Chase/login" },   // by reference, never a value
      "recorded": { "element": { "role": "textbox", "text": "Password",
                                 "candidates": ["#password", "input[name=password]"] },
                    "screenshot": "sha256:…", "url": "https://chase.com/auth" } },
    { "action": { "kind": "click" },
      "recorded": { "element": { "role": "button", "text": "Sign in",
                                 "candidates": ["button#login", "…"] } } },
    { "action": { "kind": "wait", "url_regexp": "accounts" } },
    { "action": { "kind": "pause", "reason": "2FA/OTP: human completes" },
      "resume_assert": { "expect": "url", "pattern": "…accounts.*" } }
  ]
}
```

Key properties:
- **Secrets by reference only.** `secret` is an `op://` URI resolved at replay
  time via 1Password; the macro file and evidence log never contain values.
- **Element descriptors, not single selectors.** `candidates` is an ordered
  selector ladder recorded at capture time; replay tries them in order.
- **pause steps** mark human-in-the-loop points (OTP, CAPTCHA). Replay waits
  for a gateway-notified human, then checks `resume_assert` before continuing.
- `profile_hash` pins the macro to the browser profile it was recorded on.

## Drift detection (deterministic, bounded)

Replay tries, per interaction step:
1. Exact selector hit from the ladder (highest confidence).
2. Fuzzy element match: role + text/aria similarity ≥ threshold.
3. Visual check: current screenshot vs recorded baseline — controls-region
   diff score below threshold.

Below the acceptance threshold → **replay aborts at that step** and reports
"macro needs re-recording". Replay never improvises. Intelligence lives at
*maintenance time*: an LLM may propose macro patches (new ladders) as diffs
that Ed approves; runtime stays deterministic.

## API surface (implemented on this branch)

- `POST /macro/record/start` / `POST /macro/record/stop` — driver token; the
  gateway injects its own capture script into the leased tab (gateway-authored
  JS only — the no-caller-JS rule is unchanged). The recording session is
  held open (not idle-reaped) while the human drives.
- `GET /macros` — all stored macros with approval status (driver token).
- `GET /macro?id=…` — full macro JSON for human review (driver token).
- `POST /macro/approve` — **removed (410)**. Approval happens only in the
  human-only admin band (`-admin-addr`): dashboard at `/`, per-macro step
  review at `/macro?id=…`, actions by HTML form post.
- `POST /macro/replay` — driver token; runs an approved macro in a session,
  producing the same per-step evidence log. Response includes `evidence`
  (the session action log) and, on abort, `aborted_at_step`, `step_kind`,
  `cause`, `drift`.
- `POST /macro/resume` — signals a paused replay to continue (human completed
  the OTP/CAPTCHA step).

Macro endpoints require `-macro-store DIR` / `BROWSER_FETCH_MACRO_STORE`
(0600 files; macro store disabled when unset). Replay of `type` steps needs a
secret resolver (1Password bridge, Phase 2); a macro with type steps aborts
at that step with "no secret resolver configured" until then.

## Replay semantics (implemented)

- **Secrets resolved at replay time** via the injected resolver; the value
  exists only inside the replay call frame. The default configuration has no
  resolver, so unresolvable macros abort — they never run with missing auth.
- **Ladder matching**: a click tries recorded candidates first, then generated
  text XPaths from the recorded role/text (tier-2). Tier-3 visual diff is
  deferred. Misses are recorded in the evidence log as drift.
- **Abort on failure**: the first failing step stops the replay with a
  step-addressed error; later steps never execute. Replay never improvises.