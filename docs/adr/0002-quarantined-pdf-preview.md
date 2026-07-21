# ADR-0002: Delivering quarantined PDFs to the browser for review

Status: Accepted (2026-07-21) — Option B (preview-only loopback capability
endpoint), unless a one-day timeboxed Option-A spike proves a `file://` route
with acceptable user setup on BOTH Chrome and Firefox. Ships inside the
inbox v1 build (ext-v0.5.0) so in-page `verify_identity` acceptance is not
deferred.

## Context

`verify_identity` review means a human inspects a quarantined PDF that exists
only as a daemon-local file. ADR-0001 puts that review in a browser extension
page, but choosing an extension tab does not make a daemon-local file
browser-readable:

- Sending PDF bytes over native messaging violates the protocol boundary and
  the 256 KiB frame cap.
- `file://` URLs require Chrome's per-extension "Allow access to file URLs"
  user setting, expose absolute paths, and need separate Firefox validation —
  not a dependable default path.

## Options

### A. `file://` spike

Prove, on both Chrome and Firefox, that a `file://` preview works without
unreasonable user setup (per-extension file-access toggle, path exposure,
Firefox behavior). Accept only with evidence.

### B. Preview-only loopback capability endpoint (leading candidate)

A deliberately minimal HTTP listener in the daemon:

- binds a literal loopback address on an **ephemeral** port — no config field
  (strict-mode config stays untouched);
- GET/HEAD only, plus PDF-viewer range requests;
- unguessable short-lived capability URL bound to one action ID + candidate
  SHA-256 — no directory access, no generic file parameter;
- exact `Host` validation (DNS-rebinding), `Content-Type: application/pdf`,
  `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`, no CORS;
- issued via the native-messaging `review_preview_request`; accept/reject still
  travel over native messaging, never HTTP;
- the capability is an intent/TOCTOU safeguard binding the decision to the
  inspected bytes — not a new authentication boundary (native-messaging
  `allowed_extensions` remains that).

## Constraints on whichever option wins

- ADR-0001's accept flow requires: preview occurred for this candidate, accept
  carries action revision + SHA-256, daemon CASes in one transaction.
- Scope creep tripwire: if the endpoint (option B) ever grows JSON APIs,
  sessions, or general UI assets, the daemon-served web UI alternative from
  ADR-0001 must be reopened rather than grown into by accident.

## Decision

Pending: run the option-A spike; adopt option B if the spike fails its
"unreasonable user setup" bar on either browser.
