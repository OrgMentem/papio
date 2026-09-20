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

- **ScienceDirect entitled download (Unverified).** A 2026-09-20 live probe
  reached the requested paper in Chrome's PDF viewer after explicit institutional
  navigation, but re-fetching its signed delivery URL returned HTML. No validated
  artifact reached `ready`. Automatic viewer adoption now follows the existing
  Send PDF restriction on signed URLs and offers the manual Send PDF / viewer
  Download sequence. That is an operator action, not unattended acquisition.
  Prove a retained, validated artifact before promoting this route. The current
  adapter activates the rendered PDF control; `citation_pdf_url` fetching is not
  its download path. A purchase control beside institutional sign-in remains
  login evidence, not proof of no entitlement.
- **Springer subscription and book acceptance.** Adapter 0.1.2 now selects the
  article header's PDF control rather than also matching the sticky banner.
  A fresh live probe retained the correct 16-page open-access article after one
  explicit Open on 2026-09-20. Subscription sign-in remains pending, and this
  article result does not establish access to a full book or entitled chapter.
- **EBSCO remaining routes.** The institutional resolver reached its PDF viewer
  on 2026-09-20. Adapter 0.3.0 retained the correct validated 10-page PDF after
  one explicit Open with a warmed sign-in session. That closes viewer acquisition
  re-verification. Record-only pages, title-only requests, unattended queue
  progress, and Zotero attachment remain unverified; they are not implied by
  this operator-assisted acquisition.
