# ADR-0001: The triage inbox is a full-tab extension page

Status: Accepted (2026-07-21) — `verify_identity` acceptance ships in v1 via
ADR-0002 (quarantined-PDF preview), resolved within the same build.
Review: GPT-5 Pro design review via oracle, 2026-07-21 (verdict:
accept-with-changes; all amendments below adopted). Key factual claims were
verified against the codebase before acceptance.

## Context

papio needs one interactive surface where a researcher triages:

- **watch hits** — alert-mode discoveries persisted per-hit in
  `watch_digest_entries` with a `consumed` flag (acquire / dismiss / open);
- **pending human actions** — `verify_identity` (inspect a quarantined PDF,
  accept/reject), `manual_download`, etc. (`actions.list` / `actions.resolve`);
- **job-state awareness** — counts and failure groups.

Every triage verb already exists as a daemon RPC. The decision is only about
the surface and its transport. Candidates: a TUI (`papio inbox`), the extension
popup, a Chrome side panel, a daemon-served localhost web UI, or a full-tab
extension page.

Forces:

- Two of three item classes terminate in the browser: `verify_identity` means
  *viewing a PDF*; `awaiting_human` handoffs literally are browser tabs in
  papio's work window. Only the extension knows browser-local state (tracked
  handoff tab IDs, work window, granted permissions, connection health).
- Release asymmetry: daemon deploys are same-day; extension releases pass CWS
  review (days) plus a Firefox lane. The protocol (`papio-browser/1`) is
  dual-validated (Go + TS + JSON schema), fail-closed, feature-negotiated via
  `hello_ack.features`, and frame-capped at 256 KiB.
- MV3 SW / Firefox event-page lifecycles; single background-owned native port;
  no browser automation ever (no CDP/WebDriver), so no automated e2e.
- The CLI is the single source of truth for capabilities (cobratree precedent):
  no second surface may grow logic the CLI cannot reach.

## Decision

Build the inbox as a **full-tab extension page** (`inbox.html`), released
standalone as ext-v0.5.0 (click-to-acquire already shipped in ext-v0.4.0, so
there is no batching discount; the inbox carries its own store review).
Rejected: popup as the workflow surface
(focus-loss dismissal), side panel (no cross-browser story), TUI as the primary
surface (no PDF rendering, no handoff-tab ownership; every decision round-trips
to the browser), daemon web UI (new HTTP auth/CSRF/rebinding boundary, second
local API surface, cannot touch handoff tabs or the badge — rejected on cost
comparison, not impossibility).

### Read model: daemon-side, CLI-first

A new `internal/triage` service produces one bounded, transactionally
consistent snapshot (digest hits across watches + open actions + counts +
failures). Exposed as a `triage.snapshot` RPC and a JSON CLI command first; the
browser protocol adapter and any future TUI are consumers. The extension never
composes `watch.list` + N×`watch.digest` + `actions.list` itself (N+1 traffic,
inconsistent reads, duplicated aggregation).

The page merges that snapshot with a **browser-local overlay** (connection
status, permissions, live handoff-tab availability, focus-this-tab) joined by
`job_id`. Tab IDs never enter the daemon snapshot; daemon state never lives in
`chrome.storage` (memory-only snapshot with visible generated-at).

### Protocol: typed, solicited, versioned payloads under `papio-browser/1`

No protocol-string bump (a `/2` would defeat skew tolerance — the parser
rejects mismatches before feature negotiation). Instead, a closed feature-gated
message family: `triage_snapshot_request/response`, `watch_digest_decide(+result)`,
`human_action_resolve(+result)`, `review_preview_request(+result)`; feature flags
`triage_snapshot_v1`, `triage_mutations_v1`, `review_preview_v1`. Rules:

- **Solicited only.** Extensions do not send new requests until the daemon
  advertises the feature; the daemon never pushes new message types unsolicited.
- **Negotiated immutable snapshot schemas.** Requests carry
  `schema_versions: [..]`; the daemon replies in exactly one requested schema.
  Published schemas are frozen; new daemons keep serving old schemas to the
  supported extension floor. (Optional fields only help new parsers reading old
  frames — never the reverse.)
- **Explicit correlation.** Every request carries a `request_id`; every result
  echoes it. The `page_acquire` FIFO-waiter pattern is explicitly not repeated.
  Wire `seq` is never business idempotency.
- **Compare-and-set mutations.** Mutations carry the item identifier plus
  expected revision/open-status; the daemon CASes in one transaction and
  returns `applied` / `already_applied` / `conflict`. Mutations are never
  replayed after a disconnect; ambiguity is resolved by re-snapshotting.
- **Bounded and paginated.** Complete counts, bounded visible items, opaque
  cursor, `has_more`, `unsupported_items_count`, all within the 256 KiB frame cap.
- **No opaque JSON escape hatch.** Strictly typed core (identities, state,
  allowed operations, revisions, links) plus a bounded display-only fact list
  (`{label, text}[]`); unknown actionable kinds fail closed.

### Domain gaps to close (prerequisites in the same change)

- A **keyed per-hit dismiss** RPC (`watch.digest_clear` is watch-wide bulk
  today; the store's keyed `TakeDigest` primitive exists but is not exposed).
  Dismiss-one, clear-watch, and acquire-one are distinct verbs.
- **Landing links are derived, never guessed**: canonical DOI/arXiv/OpenAlex
  URLs from validated identifiers only.
- **Cross-watch duplicates**: digest identity is per-`watch_id`; the inbox
  groups by work identity while consumption stays per-watch. Whether acquiring
  a work consumes matching hits on other watches, and whether durable dismissal
  (consumed survives re-discovery) gets an undo window, are product decisions
  recorded at implementation time — not rendering details.

### Toolbar, badge, entry points

Keep a **minimal launcher popup** ("Acquire this page" / "Open inbox") since
`default_popup` suppresses action-click events and click-to-acquire needs the
popup anyway. The inbox opens as a **singleton tab** (focus existing over
duplicate). Badge precedence, documented: (1) disconnected/broken `!`,
(2) blocking permission state, (3) pending triage count. Count freshness rides
the existing alarm heartbeat with a lightweight count request — never color
alone; tooltip and inbox header carry full state.

### Security posture

This widens the browser plane from acquisition observation into an
administrative mutation surface; the ADR says so plainly.

- **Finite operation mapping** — no `{method, params}` pass-through, ever.
- **Sender validation**: privileged runtime messages require the extension's
  own ID and the exact inbox-page URL; content scripts, provider tabs, options,
  and the launcher popup cannot invoke them.
- **Untrusted text**: titles/authors/details/errors render via `textContent`;
  links validated to intended `https` targets (or the ADR-0002 preview URL).
- **Daemon policy stays authoritative**: kind, open status, job state, and
  revision checked in one transaction; audited with source surface.
- **`verify_identity` accept is bound to the inspected bytes**: accept carries
  action revision + candidate SHA-256, requires the preview step to have
  occurred, and a distinct explicit confirmation (the `a` shortcut opens the
  confirmation; it never directly accepts a quarantined PDF).

### Background broker obligations

Single background-owned native port stays (a second port would fork hello
state). Add: a serialized inbound-processing chain (mirroring the storage-write
chain), `ensureConnected` for user-visible requests (immediate reconnect +
backoff reset + bounded wait for a current `hello_ack`), correlation maps, and
no authoritative pending state in worker memory. First version is one-shot
request/reply runtime messaging (`return true` async pattern); a page runtime
port is a later optimization and must assume Firefox can close it. Live daemon
push is **deferred work** — the current bridge is a pull loop; a subscription
mechanism is a separate decision.

## Consequences

Positive:

- Triage stays in the researcher's authenticated browser; handoff tabs and PDF
  inspection are first-class. No popup focus-lifetime constraint.
- One page app and domain model across Chrome/Firefox despite different
  background lifecycles.
- No general-purpose daemon web app, HTTP mutation API, CSRF, or web sessions.
- Daemon/SQLite remain authoritative; the read model supports a later TUI or
  side panel without re-aggregation (TUI consumes the RPC, not the browser
  protocol).

Negative / obligations:

- All UI iteration inherits CWS/AMO latency; an open inbox tab can additionally
  delay Chrome extension updates → update-available banner + deliberate
  reload flow, and no claim that fixes land immediately after approval.
- Snapshot schemas become supported compatibility contracts for the extension
  version floor; protocol additions stay triple-validated and feature-gated.
- Badge semantics become lossy and precedence-governed.
- Daemon-down is first-class UI state: page always renders, banner, explicit
  reconnect, mutations disabled, no optimistic success, CLI diagnostic hint.
- Testing grows a protocol corpus (Go/TS/schema valid+invalid), frame-size and
  pagination boundaries, skew matrices, concurrent out-of-order replies,
  disconnect-before/after-commit, Firefox event-page unload, stale revisions,
  hostile text, and a manual cross-browser release checklist (automation is
  prohibited by architecture). Both build targets must emit and assert the page.
- Accessibility is acceptance criteria, not polish: semantic controls behind
  every shortcut, no single-letter shortcuts while typing/dialog-open,
  predictable focus after acquire/dismiss, live-region announcements, text not
  color.
- Secure quarantined-PDF preview is a hard prerequisite for `verify_identity`
  acceptance in the page — split to ADR-0002. If its endpoint ever grows JSON
  APIs, sessions, or UI assets, the daemon-web-UI alternative must be reopened.
