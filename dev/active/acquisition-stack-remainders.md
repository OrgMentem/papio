# Acquisition-stack remainders

Salvaged from `dev/acquisition-stack-plan.md` (deleted 2026-08-07) when that plan
was retired. Its phases 0-4 were complete; git history holds the full 984-line
record and the execution logs.

Every deferred item in that plan was re-checked against the tree before landing
here. Most had shipped since its last dated entry (2026-07-18/23) and is not
repeated: the T&F and PsycNet adapters, per-institution resolver profiles,
zotio stored retro-attachment, and nine of the ten durability gaps from the
2026-07-16 adoption/import review. Only what follows is genuinely open.

## Live verification (needs a real session, not code)

These cannot be closed by automation: a warm human browser passes provider
anti-bot checks that no CDP or WebDriver session can, which is the same
constraint that makes the extension QA matrix manual.

- **ScienceDirect entitled download (Unverified).** The live primary access-bar path, viewer adoption, and provider outcome are now repaired/observed, but no retained validated artifact reached `ready`; the exact route is therefore not canary-qualified or **Verified working**. Page capture or provider outcome alone is insufficient to promote it. The purchase-wall capture also offers institutional sign-in, so it no longer qualifies as `no_entitlement` evidence. That combination now stays sign-in pending. The entitled-article rule remains fixture-backed only — `fixtures/sciencedirect/success.html` carries a fabricated DOI/PII, not a capture. Confirm the `citation_pdf_url` meta fetch reaches a real PDF through the privileged downloads API.
- **Springer supervised acceptance.** Adapter 0.1.1 corrects a captured access
  panel that offered institutional sign-in but was classified as no entitlement.
  The rendered PDF control still takes precedence. This repair is fixture-backed;
  a retained, validated PDF from a fresh live run remains necessary to prove the
  download route end to end.
- **EBSCO re-verification.** The 2026-07-23 adoption-binding fix (`a4aeab3`)
  landed after EBSCO's only live run, which had stopped at `needs_review`. Re-run
  it end to end to confirm the job now reaches ready/attach, and separately
  confirm the resolver lands on EBSCO's PDF viewer rather than the record page
  — the `api` download method cannot use the record page's `idPattern`.
