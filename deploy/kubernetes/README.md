# Running browser-fetch in Kubernetes

A single pod containing Xvfb, fluxbox, Chrome, x11vnc and the gateway, with the
Chrome profile on a PVC so logins and challenge cookies survive restarts.

```
        ┌─ pod ─────────────────────────────────────────────┐
 :8377 ─┤ browser-fetch ──CDP:9222(loopback)── Chrome        │
 :5900 ─┤ x11vnc ── Xvfb :0 (1920x1080) ── fluxbox           │
        │ supervisord (PID 1, root) → all programs as uid 1000│
        └──────────────── /profile (PVC) ────────────────────┘
```

## Why it must be one pod, one replica

Chrome locks its `user-data-dir`. Two pods on one RWO PVC either fail to start
or corrupt the profile, so this is a `StatefulSet` with `replicas: 1`. Chrome and
the gateway share a pod (not separate Deployments) because DevTools is bound to
loopback: anyone who can reach :9222 can drive a browser holding your sessions.

## Deploy

```bash
kubectl -n browser-test create secret generic browser-fetch \
  --from-literal=token=$(openssl rand -hex 24) \
  --from-literal=vnc-password=$(openssl rand -base64 12 | tr -d /=+ | cut -c1-8)

kubectl apply -k deploy/kubernetes
```

The VNC password is capped at 8 characters by the RFB protocol.

Build and push (zot rejects buildx provenance attestations, and wants OCI media
types, hence the flags):

```bash
docker buildx build --builder <container-driver-builder> --platform linux/amd64 \
  --provenance=false --sbom=false \
  --output "type=image,name=registry.edjusted.com/browser-fetch/browser-fetch:$TAG,push=true,oci-mediatypes=true" .
```

## Connecting a client

```bash
PI_SEARCH_FETCH_PROXY=browser
PI_SEARCH_BROWSER_URL=http://browser-fetch.browser-test.svc.cluster.local:8377
PI_SEARCH_BROWSER_TOKEN=<token from the secret>
```

From outside the cluster: `kubectl -n browser-test port-forward svc/browser-fetch 8377:8377`.

## Watching and driving the browser (Guacamole)

Add a VNC connection pointing at:

| Field | Value |
|---|---|
| Hostname | `browser-fetch.browser-test.svc.cluster.local` |
| Port | `5900` |
| Password | `vnc-password` from the secret |

x11vnc offers standard VNC auth (security type 2), which guacd handles natively —
no RFB auth-type filter needed (that exists for macOS hosts, not this).

Use the headless service (`browser-fetch-headless`, `publishNotReadyAddresses:
true`) if you want the display reachable even when the pod is unready — useful
precisely when Chrome is wedged and you want to look at it.

**This is how you clear captchas.** When the gateway meets a challenge it can't
resolve, it logs

```
WARN challenge needs a human: solve it in the browser window  vendor=reCAPTCHA interactive=true tab=1
```

and holds the tab open for `BROWSER_FETCH_ASSIST_TIMEOUT` (120s here). Connect,
click it, and the fetch completes. The resulting cookie lands in the profile on
the PVC, so subsequent fetches of that site need no further help.

Set the client's timeout above the assist window, or the client gives up first
and you get a 504 while the tab is still waiting for you.

## Verified on k3s (2026-09-11)

Home cluster, residential egress, `browser-test` namespace, Chrome 153:

| Target | Result |
|---|---|
| `example.com` | 200, 0.3 s |
| `grafana.com/docs/…` | 200, 245 KB, 2.1 s |
| a publisher behind a WAF | 200, 182 KB, 5.3 s — 403s every plain HTTP client |
| a discussion site listing | 200, 480 KB, 1.0 s |
| a discussion thread | 200, via Readability, 3.6 s |
| after `kubectl delete pod` | that site still 200 in 2.4 s — challenge cookies persisted on the PVC |

One site exercised two challenge tiers, and both were seen here:

- a **JS challenge**, which real Chrome solves by itself (the final URL comes
  back carrying a solution token);
- a **captcha checkbox**, which needs assist mode and a human at the VNC
  session.

## Choices worth knowing about

**`seccompProfile: Unconfined`.** Chrome's sandbox needs unprivileged user
namespaces; containerd's default profile blocks `CLONE_NEWUSER` and Chrome dies
with *"Zygote process exited prematurely"*. The alternatives are Unconfined
seccomp with Chrome's sandbox intact (chosen; what desktop-in-a-container images
do) or `RuntimeDefault` plus `CHROME_NO_SANDBOX=true`. Keeping Chrome's sandbox
preserves the boundary that actually contains page content.

**`/dev/shm`.** Kubernetes gives it 64 MiB and has no `--shm-size`; Chrome
renderers crash without more. A memory-backed `emptyDir` provides 1 GiB, and it
counts against the pod's memory limit — hence 5 GiB there.

**amd64 only.** Google ships Chrome for linux/amd64; this cluster's Pi nodes are
arm64, so there's a `nodeSelector`.

**`local-path` storage** pins the pod to the node that first ran it. Switch the
`volumeClaimTemplate` to `truenas-iscsi` if you want the profile — and its
logins — to follow the pod across nodes.

**Root PID 1.** supervisord starts as root to prepare `/tmp/.X11-unix` and chown
the PVC, then drops every program to uid 1000. Chrome refuses to run as root
with a sandbox.

## Operating

| Task | Command |
|---|---|
| Health | `kubectl -n browser-test exec browser-fetch-0 -- curl -s localhost:8377/healthz` |
| Live state | port-forward, then `/debug` in a browser (`?format=json` for scripts) |
| Watch fetches | `kubectl -n browser-test logs -f browser-fetch-0` |
| Restart Chrome only | `kubectl -n browser-test exec browser-fetch-0 -- supervisorctl restart chrome` |
| Back up logins | `kubectl -n browser-test exec browser-fetch-0 -- tar cz -C /profile chrome > profile.tgz` |

Known-benign log noise: `Failed to adjust OOM score`, `Failed to connect to the
bus` (no dbus daemon), `DEPRECATED_ENDPOINT` from GCM registration.

## Security posture

Deliberately modest for a personal cluster, in rough order of what to fix first
if this ever faces anything hostile:

1. No NetworkPolicy — anything in the cluster can reach :8377 and :5900.
2. VNC is password-only and unencrypted; keep it cluster-internal and reach it
   through Guacamole, never an Ingress.
3. `seccompProfile: Unconfined` (see above).
4. Egress is unrestricted — the browser can reach cluster-internal services. The
   gateway's URL guard blocks private/loopback/link-local *fetch targets*, but a
   page's own JavaScript is not similarly constrained.
