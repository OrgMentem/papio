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

Arbitration policy: **first hello holds**. A hello from a different session is
denied with a `session_busy` error frame and parked as *pending* (old
extensions log the error frame and idle — fail-visible, not fail-broken). A
holder silent past 10 s (5× the 2 s host poll) yields to a live pending
session, which receives its withheld `hello_ack` on promotion. `goodbye`
releases immediately. An empty `session_id` marks a legacy host and keeps
last-hello-wins in both directions (a legacy host cannot be arbitrated).

Switching is explicit: `papio browser sessions` / `papio browser use
<id>|--latest` (RPCs `browser.sessions` / `browser.claim`). Stateless
request/response frames (`page_acquire`, triage, review preview) pass from
ANY session — "Acquire this page" in the non-holder browser must keep
working; only the offer/handoff flow is holder-exclusive.

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
