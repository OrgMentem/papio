# Build plan: triage inbox (ext-v0.5.0)

Implements ADR-0001 (full-tab extension inbox) and ADR-0002 (quarantined-PDF
preview, Option B default). Click-to-acquire shipped in ext-v0.4.0; this
release stands alone. Daemon deploys before store submission (release
runbook order); extension submission ends at the human approval gate.

## Ambition posture

Surface count is deliberately conservative; **content and loop-completeness
are not**. v1 must fully retire the CLI for daily triage:

- in-page `verify_identity` accept/reject with bound PDF preview (no CLI punt);
- retraction sentinel item kind at launch (library-integrity story, not just
  alert triage);
- abstracts captured into digest entries so decisions happen in-inbox;
- one keystroke from watch hit to Zotero-imported PDF (AutoImporter chain
  already exists — surface it, don't rebuild it).

Deferred by design (need zero store releases later): citation contexts as
display facts, VoR-upgrade item kind, live push, side panel, TUI.

## Decision points (settle first — frozen into snapshot schema v1)

| # | Decision | Default |
|---|----------|---------|
| D1 | v1 scope | Full loop incl. accept-with-preview (ADR-0002 B) |
| D2 | Cross-watch duplicates | Group by work identity in UI; consumption per-watch; acquiring a work consumes matching hits on all watches |
| D3 | Dismissal undo | None in v1; dismiss is durable (matches `consumed` semantics); revisit only on real complaint |
| D4 | Item ordering | Schema carries a daemon-assigned `rank`; classes ordered retraction > human action > watch hit; UI never invents order |
| D5 | CAS mechanism | No migration if candidate SHA-256 is queryable per open action: CAS on `(action_id, status='open')` + expected SHA; else add revision column (+3 schema-version test bumps) |
| D6 | CLI naming / stats bead | `papio inbox [--json]`; counts+failures come from the triage read model; `papio-stats-cli-refile` narrows to budget snapshots or closes |
| D7 | Snapshot support floor | Daemon serves schema v1 for as long as ext-v0.5.x is the floor; recorded in hello_ack docs |

## Phase 0 — spikes & contracts (parallel, ~1 day)

- **ADR-0002 Option-A spike (timeboxed 1 day)**: `file://` preview on Chrome
  (file-access toggle UX) and Firefox. Any failure → Option B confirmed.
- **CAS audit** (D5): confirm per-action quarantined-candidate SHA-256 is
  queryable; fix D5.
- **Snapshot schema v1 draft**: item kinds `watch_hit`, `human_action`,
  `retraction`; typed core (ids, state, allowed ops, revision/sha, canonical
  links, rank) + bounded `{label, text}` facts; counts, cursor, `has_more`,
  `unsupported_items_count`. Review before any code freezes it.

## Phase 1 — daemon (parallel tracks after Phase 0)

**T1 `internal/triage`**: read-model service — one transactionally consistent
bounded snapshot (digest hits across watches w/ D2 grouping, open actions,
counts, failure groups, ranks). RPCs: `triage.snapshot`, `triage.counts`,
keyed `watch.digest_dismiss` (per-hit; existing `TakeDigest` primitive),
`triage.resolve` (CAS accept/reject per D5, audit fields). CLI `papio inbox`
(+ `--json`), `mcp:read-only` where applicable.

**T2 retraction sentinel**: daily sweep of library DOIs against Crossref
`update-to`; produces `retraction` items + notify event. Budget-tracked under
a source policy entry (free API pattern).

**T3 digest abstracts**: capture `Abstract` into `watch_digest_entries`
(migration NNNN + bump the three hardcoded schema-version assertions:
clean_install_test ×2, doctor_test, migrate_forward_test).

**T4 preview endpoint (ADR-0002 B)**: loopback-only ephemeral port, capability
URL bound to action ID + SHA-256, GET/HEAD+ranges, Host validation, nosniff,
no-store, no CORS, no config field. Issued via native messaging only.

Gate: `go build/vet/test ./...`; commit per track.

## Phase 2 — protocol (after Phase 1 contract, small)

`triage_snapshot_request/response`, `triage_decide(+result)`,
`human_action_resolve(+result)`, `review_preview_request(+result)`; features
`triage_snapshot_v1`, `triage_mutations_v1`, `review_preview_v1`. All three
artifacts (Go, TS, JSON schema) + valid/invalid corpus + round-trip fixtures +
frame-size/pagination boundary tests. Solicited-only; `request_id` echo;
schema_versions negotiation.

Gate: Go + `bun run typecheck` + `bun test`.

## Phase 3 — extension

**T5 inbox page**: `inbox.html` + vanilla TS (options.ts pattern); singleton
tab; grouped items, facts, keyboard (`j/k/a/d/o`, accept opens bound
confirmation — never direct); daemon-down first-class; update-available
banner; memory-only snapshot with generated-at; `textContent` only (bundle
scan test bans `innerHTML`); accessibility acceptance criteria from ADR-0001.

**T6 broker**: inbound processing chain; `ensureConnected`; correlation maps;
resnapshot-on-ambiguity; sender validation (exact extension ID + inbox URL for
mutations); count refresh on existing alarm; badge precedence
health > permissions > pending; notifications deep-link to singleton tab.

**T7 launcher popup rework**: "Acquire this page" (existing flow) +
"Open inbox"; page-aware state (detected DOI, already-owned, job in flight).

**T8 build/manifest**: emit + assert `inbox.html` in both targets; CSP entry;
Firefox event-page checks.

Gate: `bun run typecheck`, `bun test`, `bun run build`, `bun run lint:firefox`.

## Phase 4 — QA & release

- Manual cross-browser checklist (tracked file): SW sleep mid-triage,
  event-page unload, daemon restart, duplicate tabs, stale revision, hostile
  titles, skew matrix (old ext + new daemon, new ext + old daemon).
- Deploy daemon (build → mv → `papio daemon stop` → autostart).
- Tag `ext-v0.5.0` per papio-release runbook; stop at the human approval gate
  before CWS upload; Firefox lane after.

## Fast-follow (daemon deploys only — no store review)

- Citation contexts as facts on cite-watch hits (S2 `contexts`).
- `upgrade_available` (preprint→VoR) item kind.
- Stats absorption per D6.
