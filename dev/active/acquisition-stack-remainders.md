# Acquisition-stack remainders

Salvaged from `dev/acquisition-stack-plan.md` (deleted 2026-08-07) when that plan
was retired. Its phases 0-4 were complete; git history holds the full 984-line
record and the execution logs.

Every deferred item in that plan was re-checked against the tree before landing
here. Most had shipped since its last dated entry (2026-07-18/23) and is not
repeated: the T&F and PsycNet adapters, per-institution resolver profiles,
zotio stored retro-attachment, and nine of the ten durability gaps from the
2026-07-16 adoption/import review. Only what follows is genuinely open.

## Code

- **Adoption poll hard-caps at 200 jobs with no pagination** (durability review
  gap #6, the one still standing). `poll()` in `internal/browser/bridge.go`
  calls `b.jobs.List(ctx, job.StateAwaitingHuman, 200)` with a fixed cap and no
  cursor, so a settled download older than the 200 most recent `awaiting_human`
  jobs is never scanned for adoption. Deferred at the time as "not biting now —
  queue drained". Needs cursor pagination, or an adoption-root directory scan
  independent of the job list, before the parked backlog can realistically pass
  200.

## Live verification (needs a real session, not code)

These cannot be closed by automation: a warm human browser passes provider
anti-bot checks that no CDP or WebDriver session can, which is the same
constraint that makes the extension QA matrix manual.

- **ScienceDirect entitled download.** The `sciencedirect` adapter's
  `no_entitlement` rule is backed by a real institutional capture (2026-08-06),
  but its entitled-article rule is structural only — `fixtures/sciencedirect/success.html`
  carries a fabricated DOI/PII, not a capture. Confirm the `citation_pdf_url`
  meta fetch reaches a real PDF through the privileged downloads API.
- **Springer supervised acceptance.** The `springer` adapter is still 0.1.0 with
  all six fixture states but no live-verification note, unlike wiley/proquest/
  jstor/acm. One supervised run against an entitled chapter would prove the
  href download rule end to end. Deprioritised because JSTOR and EBSCO had
  harder auth problems, and institutional OpenURL already denies non-OA
  Springer chapters correctly.
- **EBSCO re-verification.** The 2026-07-23 adoption-binding fix (`a4aeab3`)
  landed after EBSCO's only live run, which had stopped at `needs_review`. Re-run
  it end to end to confirm the job now reaches ready/attach, and separately
  confirm the resolver lands on EBSCO's PDF viewer rather than the record page
  — the `api` download method cannot use the record page's `idPattern`.

## Packaging

- **Packed extension-ID re-pin.** `extension/manifest.json` still has no `key`,
  so the extension runs under its unpacked development ID. Pinning means adding
  the preserved manifest key, setting `browser.extension_id`, re-running
  `papio native-host install`, and reloading. Parked deliberately: swapping IDs
  while live `awaiting_human` handoffs reference the unpacked ID would orphan
  them, and the Web Store submission that needs the packed build is gated on
  credentials that do not exist yet.
