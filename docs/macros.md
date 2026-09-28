# Macros: human-recorded, agent-replayed site flows

## Model

The agent never operates a site. A human (Ed) performs a login/flow once in the
VNC-visible Chrome while the gateway records interaction events; the recording
becomes a **macro** that the agent may replay. Replay executes the same typed
primitives as a live session (click/type/screenshot/assert/…) but the action
sequence came from a human, not from an LLM.

Trust chain: human interaction → recorded macro → **human review/approval** →
deterministic replay with drift detection → evidence log per step.

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
    { "action": { "kind": "assert", "kind_assert": "url", "pattern": "…\\.com/auth$" } },
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
      "resume_assert": { "kind_assert": "url", "pattern": "…accounts.*" } }
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

## API surface (planned)

- `POST /macro/record/start` / `POST /macro/record/stop` — driver token; the
  gateway injects its own capture script into the leased tab (gateway-authored
  JS only — the no-caller-JS rule is unchanged).
- `GET /macro/pending` — raw recording awaiting human review.
- `POST /macro/approve` — human (over VNC/assist channel) approves → stored.
- `POST /macro/replay` — driver token; runs an approved macro in a session,
  producing the same per-step evidence log.