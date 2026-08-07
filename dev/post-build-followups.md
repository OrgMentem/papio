# Follow-ups after the ADR-0016/0017/0019 build (2026-08-07)

Triaged from the first live smoke-test session, then arbitrated by the r5
oracle round (`dev/scratch/oracle/papio-integrations-r5.1.md`) — its zotio
claims were verified against `internal/zotio/service.go` before acceptance.
`page_bulk_runs` is already recording the yields the evidence gates below
read (ADR-0019 Decision 10).

## Execution order (settled, r5-arbitrated)

1. **Diagnose/fix the stuck "Institution session — Checking session…"
   probes.** Shipped-path bug, not product work; observed rows that never
   resolve to signed-in/out. Diagnose before touching — may be probe
   lifecycle, may be Primo-NDE-specific.
2. **Wire zotio ownership into page-bulk status.** The scout question is
   already answered in-tree: `zotio.Service.LookupWorks` (1–50 works,
   `owned_with_pdf`/`owned_missing_pdf`, staleness warning, unconfigured
   degrades to not-owned) already serves batch submit — the browser bridge
   just never calls it. Inject it into `page_bulk_status`, merge with
   `canonicalJobStatus` under the precedence: papio ready bundle →
   zotio owned_with_pdf → zotio owned_missing_pdf (+ item key) → live job
   queued → complete negative = eligible → zotio unavailable/stale =
   ownership_unknown (never painted as unowned). Extend `LookupWork` with
   PMID (zotio supports it; the facade carries only DOI/arXiv). Measure
   lookup latency before asking zotio for a bulk command. This repairs the
   most visible false limitation in every fresh workspace.
3. **Silent-UI docs + `papio stats page-bulk`.** Bulk import already exists
   (`papio acquire --batch`: JSONL/RIS/BibTeX/CSL-JSON/NBIB since the
   ADR-0008 work; live-verified — cache dedupe applies, title-only entries
   submit through enrichment). The work is discoverability: user-guide
   section for the OneSearch/Primo/Scholar "Export RIS → --batch" flow,
   pointers from `acquire --help` and the workspace's collapsed-note copy.
   NO `papio import` alias (r5: cut — duplicates a clear verb to
   compensate for documentation). Stats surface is a `stats` command (not
   doctor), through the conformance registry + printPage like everything
   else: sessions, useful-scan rate, bulk leverage, submit conversion,
   per-origin-class yield. Add ONE nullable aggregate field to
   `page_bulk_runs` — `rendered_record_count_hint` (count of visible result
   cards for known page classes; no titles/URLs/queries/docids) — so
   identifier_yield gets an honest denominator.
4. **`papio bench` — comparative, not absolute.** Cohort file
   (`papio-bench-cohort/1`: work request + expected class from
   {autonomous_ready, ready_after_human_boundary, honest_unavailable,
   identity_review}; never an expected provider/route). Hermetic v1:
   ephemeral DB, empty artifact cache, hooks/notifications/Zotero mutation
   disabled, injected resolver fixtures, run twice — baseline overlay
   (S2/OpenAIRE/typed-relations disabled) vs current. Headline number:
   **incremental_autonomous_ready** ("+2 / 9 works") on the frozen
   nine-work field cohort — the question the unmeasured resolver work left
   open. Manual live mode later; never blocks v1.
5. **ILLiad poll executor (2A — fixtures now).** GetTransaction on wake.
   State map: any successful nonterminal read → `pending` (reset failure
   count); `Delivered to Web` → `fulfilled` (stop polling, start
   retrieval); `Cancelled by Customer` → `cancelled`; `Cancelled by ILL
   Staff` → `declined`; `Request Finished` classifies from prior
   observations, `unknown_outcome` only after one delayed reconciliation;
   unknown custom statuses → `pending` + `provider_status_unmapped` (ILLiad
   statuses are customizable — no exhaustive enum; persist raw). **A failed
   poll NEVER becomes unknown_outcome**: transient/auth/schema failures
   leave request state unchanged and degrade integration health instead
   (3 consecutive → degraded; 24 h without success → operator advisory
   saying papio cannot OBSERVE the request, never that it failed).
   `unknown_outcome` is reserved for provider-side uncertainty after
   successful communication + exhausted reconciliation (404 after a prior
   successful lookup → UserRequests + idempotency-reference
   reconciliation first). Persist: provider_status_raw, display status,
   last_successful_poll_at, consecutive_poll_failures, error class.
6. **Fulfillment retrieval (2B — design + fixtures now, live acceptance
   gated).** The r5 correction that killed the plan's hidden hole: ILLiad's
   API does NOT serve the delivered PDF bytes; electronic delivery posts to
   the patron web UI (form-75 "View PDF" route:
   `illiad.dll?Action=10&Form=75&Value=<txn>`). So `fulfilled` means "the
   provider has supplied the document", not "papio has trusted bytes".
   Config gains a distinct `patron_web_base_url` (never derived from
   `api_base_url`). On fulfilled: route the form-75 URL through the
   existing browser-handoff machinery (`route_class=document_delivery`,
   provider reference carried), drive per access mode
   (delegated/assisted/conservative), adopt through the ordinary
   quarantine → validation → identity pipeline. Custom HTML delivery pages
   require a fixture-backed adapter — no generic "PDF-looking link"
   hunting. New compiled gate-profile capability: `fulfillment_channel =
   patron_web` — a site with email/pickup delivery can't compile
   end-to-end auto-capable even with an auto-capable submission API.
   Unit-correct from fixtures; ACCEPTANCE requires a real ILLiad
   institution (structurally not the operator's own — s49/Alma).
7. **`triage-snapshot/3` — one rev, three riders.** (a) document_delivery
   reconciliation rendering + closed ops; (b) ADR-0016 tri-state
   `auth_requirement` + `route_class` + post-contact auth observations;
   (c) r5's third rider so v4 isn't needed within months: closed
   `attention` field — `working` / `required` / `advisory` (unknown-auth
   LibKey handoff = working; login/MFA boundary = required; delivery
   unknown_outcome = required; integrity notice = advisory). `blocked_by`
   becomes a v3 enum (adds delivery_outcome, identity_review, unknown —
   never overload v2 values). Pending pollable delivery stays
   Activity-side, out of the snapshot.
8. **Extension store release.** Bundle: page-bulk (zero new manifest
   permissions — verified), LibKey origin-derivation fix, snapshot v3
   consumers, auth-observation presentation, any ILLiad fixture work ready
   by cut time. Listing/privacy text gains the scan disclosure per
   ADR-0019 Decision 8. Non-urgent while the verified zero-install window
   holds; re-verify AMO/CWS counts at cut time.
9. **Reassess Primo/Scholar class-2 from run data.** Primo moved to
   Later/conditional (r5): Export-RIS → `--batch` already preserves MORE
   metadata than an extractor could scrape. Evidence gate (all required,
   measured by #3's denominator): ≥5 Primo selection sessions across ≥3
   days; median ≥10 selectable records; exact-identifier yield <25%; ≥3
   sessions abandoned specifically because the RIS route was disruptive;
   operator confirms the friction. If it fires, build the **rendered-row
   PNX reader** first — join rendered cards to already-loaded client-state
   PNX JSON by record ID, exact identifiers out, structured
   title/creator/year as class-2 candidates; NEVER call Primo search APIs,
   replay queries, paginate, or read unrendered records (that crosses the
   privacy line and duplicates RIS). If NDE exposes no stable client-side
   PNX object: stop. Scholar class-2 stays evidence-gated behind the same
   metrics (currently 7/10 post-generalization).

## Title-only asymmetry — deliberate today, not permanent (r5 ruling)

`acquire --batch` stays permissive: structured citation fields deliberately
supplied as acquisition input are identity evidence; the browser's bounded
display label is not, and must never be relabeled as `title` into
enrichment. When a class-2 detector emits exact visible title + author
evidence + year + detector identity + source-record binding, it uses the
SAME batch submission path — no new caution policy. ADR-0019's rationale
should be amended from "browser title-only is inherently too risky" to
"v1's detector does not yet produce structured bibliographic evidence" —
door open, v1 unweakened.

## Later / dormant (unchanged)

- **DataCite version relations** — additive client behind the
  `VersionRelations` seam; build when a real version chain shows up.
- **LibKey remainder** (ADR-0016): `api` mode (blocked on Third Iron),
  doctor link-mode probe, init Library-List discovery. Keyless link route
  is live and sufficient.
- **`citation-record/1` durable model** — revisit when export users exist
  beyond the operator.
- **ILLiad live acceptance** (ADR-0017 3A): wakes if a partner library
  materializes; #5/#6 must land first regardless.
- **oclc / rapido adapters**: config kinds stay rejected until built;
  Rapido must live-verify no auto-set declaration before compiling
  auto-capable.
- **In-panel "Check routes"**: explicit selected-rows-only action, never
  pre-click badges; the privacy line does not move.
