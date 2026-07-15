# Changelog

All notable changes to Papio are documented here. This initial release entry is
synthesized from the complete `papio` and `zotio` Git histories and the execution
records in `notes/acquisition-stack-plan.md`.

## [0.2.0] - 2026-07-15

### Phase 0 — contracts and prerequisite

- Established the Papio Go/Bun workspace, fail-closed shared protocol fixtures,
  and draft work-request, acquisition-bundle, and browser contracts.
- Added Zotio's stored-attachment upload path with reconciliation and retry-safe
  Web API registration, which is the import prerequisite for Papio exports.

### Phase 1 — durable open-access acquisition

- Added private configuration, SQLite migrations, daemon IPC, durable job and
  lease recovery, source budgets, redacted events, quarantine, and content-hash
  artifact storage.
- Added normalized work identity, deterministic candidate ranking, bounded
  HTTPS acquisition, PDF validation, OCR fallback, and review/rejection paths.

### Phase 2 — institutional browser handoff

- Added the native-host bridge, versioned bounded browser protocol, native-host
  install/status commands, and a least-privilege MV3 extension for one requested
  institutional download per job.
- Added adoption confinement and validation for browser downloads, with
  restart-safe daemon and extension lifecycle handling.

### Phase 3 — provider adapters and protocol lock

- Added declarative, permission-gated adapter execution and sanitized fixture
  capture for ProQuest, JSTOR, EBSCO, and Springer flows.
- Locked `work-request/1`, `acquisition-bundle/1`, and `papio-browser/1` with
  strict cross-runtime fixtures; retained Go as the core after the reversal
  review.

### Phase 4 — Zotio, MCP, and human resolution

- Added Zotio capability/version preflight, preview/apply plans, confirmation
  hashes, import-ledger idempotency, missing-PDF intake, and stored attachments.
- Added MCP tools and resources over the same application service, plus bounded
  human identity-review resolution and action lifecycle cleanup.
- Added extension session recovery across daemon restarts and startup wake-up.

### Post-Phase 4 — autonomous acquisition

- Added OpenAlex discovery, batch acquisition, serialized retry-safe auto-import,
  session keepalive, observed-provider fixture capture, library-aware batches,
  OA browser fallback, snowball search, status/reporting, notifications,
  watchlists, MCP loop closure, and first-run onboarding.
- Updated Zotio integration with collection-aware missing-PDF scopes, item-type
  valid container-title mapping, exact-key enrichment, and transactional
  workflow execution.

### Phase 5 — release preparation

- Added local release artifacts for Papio and Zotio binaries, the extension ZIP,
  dependency inventories, license reports, hashes, and a machine-readable
  release manifest.
