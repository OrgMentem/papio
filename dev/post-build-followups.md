# Follow-ups after the ADR-0016/0017/0019 build (2026-08-07)

Everything below is triaged from the first live smoke-test session (five page
shapes fixed same-day; two identified as structural) and from scope the ADRs
deliberately deferred. Ordered by decision quality: what unblocks the
operator's real workflow first, what needs evidence before building, what is
gated on externals. `page_bulk_runs` is already recording the per-site-class
yields the Next-tier decisions depend on (ADR-0019 Decision 10).

## Now — build next, no further evidence needed

1. **`papio import` (RIS/BibTeX → batch).** The generic bulk bridge for every
   discovery UI the scanner is structurally blind to: Primo/Alma listings
   (1/50 yield — identifiers live in the index, not the DOM), silent Scholar
   rows, ScienceDirect, JSTOR. Every such system already has an "export RIS"
   button; papio has export since v0.18-dev but no import. Daemon/CLI only,
   no scraping fragility, mirrors `internal/cite` in reverse: parse → work
   requests → existing batch submit with `consumer=import:<format>`.
   Fail-closed rules: identifier-bearing entries submit; title-only entries
   are listed-not-submitted (same weak-match stance as ADR-0019) behind an
   explicit `--allow-title-only` that routes through the existing
   title-enrichment path. Acceptance: OneSearch "Export RIS" of a 50-result
   page → `papio import results.ris` → one batch, dedupe against ledger,
   honest per-entry report.
2. **Delivery status poller + fulfillment adoption.** ADR-0017's submitted
   path parks `retry_wait/document_delivery_pending` with a poll schedule,
   but nothing yet executes the poll: an ILLiad `GetTransaction` check on
   wake, state map (submitted→pending→fulfilled/declined), and fulfilled →
   fetch through ordinary quarantine/validation. Without it the auto path is
   submit-and-forget, which the ADR explicitly forbids claiming. Build
   against the existing `internal/illiad` fixtures; no live institution
   needed for correctness, required before anyone's live acceptance run.
3. **`papio bench` (the measurement harness, r2 §3 design).** The original
   roadmap's "rerun the nine-paper cohort" never happened; S2, OpenAIRE, and
   typed relations shipped unmeasured. Smallest cut: cohort file (works +
   expected terminal class), OA-only subset runnable hermetically in CI,
   institutional subset as a manual live run, `autonomous_ready_rate` as the
   headline number. Also becomes the arbiter for resolver tuning ever after.

## Next — cheap, or gated on evidence now being collected

4. **Page-bulk pilot readout.** The expand/stop gates (ADR-0019 Decision 10)
   are undecidable without a surface over `page_bulk_runs`. One `papio stats
   page-bulk` (or doctor section): sessions, useful-scan rate, bulk leverage,
   submit conversion, per-origin-class yield. Trivial; do with #1.
5. **Stuck "Institution session — Checking session…" probes.** Popup rows
   observed never resolving during smoke testing. Diagnose before touching:
   may be probe lifecycle, may be NDE-specific. Bug class, not feature.
6. **Primo/Alma class-2 extractor.** Highest-yield gap for this operator
   (both institutions are Primo NDE), but build ONLY if the RIS-import flow
   (#1) proves insufficient in practice and the pilot readout (#4) shows
   OneSearch scans recurring. ADR-0019 conditions hold: source-controlled,
   fixture-backed (`make`-style captured fixtures), explicit invocation,
   rendered-rows-only.
7. **zotio-backed workspace ownership.** "Owned" currently means
   "papio-acquired"; the pre-papio Zotero collection is invisible to the
   selection sheet (honest, collapsed-note UX shipped). Needs a scout pass
   first: does the zotio CLI expose a bulk identifier-lookup surface? If
   yes, a purpose-scoped ownership provider (ADR-0008's "explicit lookup
   purpose" carve-out) closes the gap; if no, file upstream against zotio
   and leave the note.

## Later — bundled, versioned, or externally gated

8. **`triage-snapshot/3` bundle.** Two declared dependents wait on the next
   negotiated snapshot schema: document_delivery action rendering in the
   extension inbox (ADR-0017) and the tri-state auth presentation carriers
   (ADR-0016 Decision 4, `handoff_access_observation_v1`). Bump the schema
   once, carry both; never two revs.
9. **Extension store release.** The store builds predate LibKey
   origin-derivation, page capture caps, and the entire page-bulk feature.
   Bundle: page-bulk (zero new manifest permissions — verified), the
   origin-derivation fix, adapter updates; store listing/privacy text gains
   the user-facing scan disclosure per ADR-0019 Decision 8. Timing is the
   operator's call; the zero-install window makes it non-urgent until
   there's a second user.
10. **Scholar class-2 extractor.** Only if #4's metrics show Scholar sheets
    recurring AND stuck at partial yield after the DOI-path generalization
    (currently 7/10 on the probe query). Cheaper than Primo's; lower value
    for this operator.
11. **DataCite version relations.** Additive client behind the existing
    `VersionRelations` seam; needs the new-source-name two-edit dance,
    budget row, privacy row. Build when a real DataCite-registered version
    chain shows up in practice (Zenodo/monograph workflows).
12. **LibKey remainder** (ADR-0016): `api` mode with a Third Iron partner
    key (blocked on their reply), doctor link-mode probe, init Library-List
    discovery. The keyless link route is live and sufficient meanwhile.
13. **`citation-record/1` durable model** (r2 §4A): persisted normalized
    records with per-field source attribution, richer exports
    (volume/issue/pages/publisher/abstract). Revisit when export users exist
    beyond the operator.

## Dormant — external dependencies, correct to wait

- **ILLiad live acceptance** (ADR-0017 Decision 3A): requires an
  institution-issued key and a supervised run; structurally unavailable at
  the operator's own (s49, Alma/Primo) institutions. Wakes if any partner
  library materializes — the poller (#2) must land first regardless.
- **oclc / rapido delivery adapters**: config kinds stay rejected until
  built; Rapido additionally needs live verification that no declaration is
  configured before it may ever compile auto-capable.
- **In-panel "Check routes" availability action** (ADR-0019 deferral): an
  explicit selected-rows-only action, never pre-click badges. Revisit only
  with real demand; the privacy line does not move.
