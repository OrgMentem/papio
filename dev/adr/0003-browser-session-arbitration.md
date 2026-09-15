# ADR-0003: Arbitrating concurrent browser sessions on the bridge

Status: Accepted (2026-07-21) — implemented in `e7245ff` (daemon/host phase);
extension takeover UX deferred to a later extension release.

## Context

The daemon bridge is the per-daemon-run browser session. Until this decision,
every `hello` replaced the session state (last-hello-wins). With papio
installed in two browsers — the realistic case being the store extension in a
daily driver plus a dev build in a web-ext profile — each MV3 service-worker
restart respawned a native host whose fresh hello silently stole the session:
job offers landed on an arbitrary browser, `daemon status` version-flapped,
and handoffs stalled with no signal anywhere (P1, observed 2026-07-20).

Two facts constrain the fix:

- The `papio-browser/1` protocol is locked and dual-validated (Go/TS/schema);
  store-published extensions speak it as-is. A fix requiring an extension
  change would leave every released extension still fighting.
- Every browser reaches the daemon through the same native-host binary, which
  ships and deploys with the daemon as one artifact. The daemon↔host IPC
  envelope can therefore change freely without version-skew management.

## Options

### A. Session identity on the IPC envelope (chosen)

The native host mints a per-process `session_id` and carries it — plus a
clean-shutdown `goodbye` — on the `browser.sync` request body. The extension
protocol is untouched; extensions of every version participate immediately.

Arbitration policy: **first hello holds**. A hello from a different identified
session is parked as *pending*. The daemon first sends a role-bearing
`hello_ack` (`role: "pending"`), then a `session_busy` error frame. The
acknowledgement negotiates daemon features without granting the pending
session holder work. A holder silent past 10 s (5× the 2 s host poll) yields
to a live pending session, which receives a role-holder `hello_ack` — unless it
fails the extension-version gate, in which case `Sync` returns
`extension_outdated` rather than seating it in a role it cannot serve.
`goodbye` releases immediately. An empty `session_id` marks
a legacy host and keeps last-hello-wins in both directions (a legacy host
cannot be arbitrated).

Switching is explicit: `papio browser sessions` / `papio browser use
<id>|--latest` (RPCs `browser.sessions` / `browser.claim`). Admission is
independent of holdership for frames whose effects route back to the sending
browser or mutate only daemon-owned state. Holder status gates
daemon-initiated offer and handoff work, plus frames that act on that routed
authority. The exact admitted frame set stays in bridge code and tests because
it changes with the protocol surface.

### B. Rejected: prefer highest extension version

Steals from the browser the user is looking at exactly when versions differ
(development), and version says nothing about which browser the human is in.

### C. Rejected: queue offers to all sessions / most-recent-activity wins

Queueing re-creates the silent-arbitrary-destination bug with a delay.
Activity preference needs new activity signals in the locked protocol — and
its honest form is just the explicit takeover button.

## Decision

Option A. Deterministic default (first holder, usually the daily driver),
explicit one-command switch for the dev workflow, bounded automatic recovery
(goodbye, then the 10 s stale window) for crashes, and full visibility
(`browser sessions`, `ping` pending/denied counts, doctor remediation line).

`dev_reload` also reserves against a holder whose native-host `goodbye` never
arrives. Chrome can kill the host at the same 2 s deadline used by the
best-effort goodbye, while daemon restart reconciliation holds the bridge
lock. A different identified, non-legacy hello inside the live reservation
therefore releases the reserved current holder and takes the holder role
immediately. This rule shares the accepted risk of the time-keyed reservation:
a sibling's fresh hello inside the window is indistinguishable from the
reloaded browser. The CLI already detects that risk by observing which known
session ids disappear instead of treating every holder change as success.

## Constraints on future work

- **Session identity never enters `papio-browser/1` implicitly.** The planned
  extension-side takeover UX adds optional, fail-closed `instance_id` and
  `takeover` fields to `hello` through the full dual-validation path
  (Go + TS + schema + corpus). Anything beyond that — per-session state in
  offers, session routing hints — reopens this ADR.
- **Non-holder deny must stay an ordinary error frame** (`session_busy`), so
  old extensions degrade to visible idling. Hard-failing the sync would send
  released extensions into respawn loops.
- **Firefox correlation limits (AGENTS.md) are untouched**: arbitration is
  per native-host process and never widens download ownership.
