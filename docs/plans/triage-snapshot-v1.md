# Triage snapshot schema v1 (contract draft)

Payload contract for `triage_snapshot_response` (protocol `papio-browser/1`,
feature `triage_snapshot_v1`) and the shared `internal/triage` read model
consumed by `papio inbox --json`. Once ext-v0.5.0 ships, **schema 1 is
immutable**; changes require schema 2 negotiated via `schema_versions`.

All strings are untrusted display text (rendered via `textContent`); links are
daemon-derived from validated identifiers only. Everything fail-closed on both
validators; frames bounded well under `MaxBrowserMessageBytes` (256 KiB).

## Envelope

```jsonc
{
  "schema": 1,
  "generated_at": "RFC3339",
  "counts": {                     // complete, never truncated
    "pending_total": 0,           // = watch_hits + actions + retractions
    "watch_hits": 0,
    "actions": 0,
    "retractions": 0,
    "jobs_working": 0,
    "jobs_needs_review": 0,
    "failure_groups_7d": 0
  },
  "items": [],                    // bounded: default 50, max 100 per frame
  "cursor": "opaque",             // omitted when no more
  "has_more": false,
  "unsupported_items_count": 0    // items whose kind postdates this schema
}
```

## Item core (every kind)

```jsonc
{
  "kind": "watch_hit" | "human_action" | "retraction",
  "id": "hit:<watch_id>:<work_key>" | "action:<id>" | "retraction:<doi>",
  "rank": 0,                      // daemon-assigned ascending display order;
                                  // class order: retraction < human_action <
                                  // watch_hit; UI never invents order
  "title": "≤500 chars",
  "facts": [ {"label": "≤40", "text": "≤400"} ],   // ≤8; display-only growth
                                                   // area (citation contexts,
                                                   // failure reasons, …)
  "links": [ {"rel": "doi"|"arxiv"|"openalex"|"landing"|"preview", "url": "https://…"} ],
  "ops": ["acquire"|"dismiss"|"accept"|"reject"|"open"|"retry"]  // allowed now
}
```

Newer daemons never send kinds the negotiated schema lacks — they count them
in `unsupported_items_count` instead; unknown kinds in a frame fail closed.

## Kind extras

`watch_hit` (grouped by work identity per D2; consumption stays per-watch):
```jsonc
{
  "work": {"doi": "", "title": "", "authors": "≤200", "year": 0, "is_oa": false},
  "abstract": "≤2000 chars",           // from migration 0009
  "watches": [ {"id": 1, "label": ""} ],  // every watch that surfaced it
  "first_seen_at": "RFC3339"
}
```
`acquire` consumes the hit on all listed watches (D2); `dismiss` takes an
explicit watch scope or `all`.

`human_action` (verify_identity, manual_download, validation_error, …):
```jsonc
{
  "action_id": 1,
  "job_id": "…",
  "action_kind": "verify_identity",
  "job_state": "needs_review",
  "revision": 1,                        // from migration 0010
  "sha256": "hex64",                    // quarantine binding; verify_identity only
  "size_bytes": 0
}
```

`retraction`:
```jsonc
{
  "doi": "…",
  "nature": "retraction" | "correction" | "concern",
  "noticed_at": "RFC3339",
  "notice_doi": "…"                     // the update-to record, when present
}
```

## Requests

`triage_snapshot_request`: `{ request_id, schema_versions: [1], limit?, cursor? }`
— daemon answers in exactly one requested schema or errors.

`triage_counts_request` → counts object only (badge refresh on the alarm).

`triage_decide`: `{ request_id, item_id, op: "acquire"|"dismiss", watch_scope?: "all"|[ids] }`
→ `triage_decide_result`: `{ request_id, outcome: "applied"|"already_applied"|"conflict"|"error", detail? }`.

`human_action_resolve`: `{ request_id, action_id, verdict: "accept"|"reject",
expected_revision, expected_sha256? /* accept: required */ }` → same result
shape. Daemon CASes kind + open status + revision (+ SHA for accept) in one
IMMEDIATE transaction (CasAudit: default deferred tx can surface SQLITE_BUSY
instead of a clean conflict).

`review_preview_request`: `{ request_id, action_id }` →
`{ request_id, url: "http://127.0.0.1:<port>/<capability>", sha256, size_bytes, expires_at }`.
Capability bound to (action_id, sha256), short-lived, GET/HEAD+ranges only
(ADR-0002). QA watch item: Chrome 142 Local Network Access prompt on loopback.

## Correlation & mutation rules (restating ADR-0001, binding here)

- Every request carries `request_id` (msg-id charset, ≤64); every result
  echoes it. Never FIFO, never wire `seq` as idempotency.
- Mutations are never replayed after disconnect; ambiguity → re-snapshot.
- Old parsers reject unknown keys: additions to schema 1 are PROHIBITED after
  release, including "optional" fields.

## Spike outcomes folded in (Phase 0)

- ADR-0002 Option A FAILED both browsers: Chrome 118+ blocks extension
  `tabs.create/update` to `file://` without the default-off per-extension
  toggle; Firefox forbids `file://` in `tabs.create/update` outright
  (Bugzilla 1266960 open, 1617594 REOPENED as of 2026-07-10). Firefox 153
  (released 2026-07-21) adds an off-by-default "Access local files" user
  permission, but it is content-script-scoped only (Bug 2034168 comment #1) —
  it does not enable extension-page or tabs.* file navigation. Option B
  stands.
- D5 resolved as migration 0010 (`human_actions`: candidate_id,
  quarantine_path, quarantine_sha256, revision); the SHA computed at fetch
  time was never persisted, and candidate linkage was inference-only via the
  latest validate attempt.
