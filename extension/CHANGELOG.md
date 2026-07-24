# Changelog — browser extension

All notable changes to the *papio* browser extension (Chrome/Firefox MV3,
`extension/`) are documented here. The extension is versioned and released
independently of the daemon: its version lives in `extension/manifest.json`
(and must match `extension/package.json`), and a release is cut with an
`ext-v<version>` tag. Daemon/CLI changes live in the root `CHANGELOG.md`.

A change that spans the native-messaging protocol appears in **both**
changelogs — each file describes the behavior change visible to users of that
artifact.

History before 0.3.1 was recorded in the root `CHANGELOG.md` (the extension
and daemon shared a version stream through 0.3.0); see its `[0.3.0]` section
for the full pre-split extension history.

## [Unreleased]

### Fixed

- **The background work window no longer accumulates.** papio's dedicated
  handoff window is now closed automatically once no handoff owns a tab in it,
  instead of lingering (or multiplying) across acquisitions. A pinned keepalive
  session tab keeps the window alive; a stale window id left by a manual close
  is dropped so the next handoff opens exactly one fresh window.

## [0.5.1] - 2026-07-23

### Fixed

- **ACM Digital Library articles now download autonomously and paywalled ACM
  pages stay assisted.** The adapter keyed the `article` verdict (and its PDF
  href) on the bottom-of-document `a#downloadPdfUrl` anchor, but ACM emits that
  anchor even on non-entitled "Get Access" pages — so an accessible free/entitled
  article was often left as a manual-download handoff, while a paywalled one
  risked fetching an HTML access page. The adapter now keys on the "PDF/eReader"
  toolbar control (the real entitlement signal, present only when this session
  can read the PDF) and builds the deterministic `/doi/pdf/<doi>?download=true`
  endpoint from the DOI in the page URL, fetched through the session cookie jar.

- **The sticky inbox footer is decluttered to two slim rows.** The access
  legend is gone — every item already states its access requirement inline —
  and the keyboard help shrank from a paragraph to key-chip pairs
  (`j`/`k` select · `a` act · `d` dismiss · `o` open), sharing its row with
  the generated-at stamp. The status-glyph legend stays.
- **Dismissing a human action from the inbox works again.** The inbox and the
  native protocol both speak verdict `dismiss`, but the background broker's
  request guard only accepted `accept`/`reject`, so every dismiss died as
  "Invalid action resolution request".
- **A structured broker rejection no longer masquerades as a daemon
  disconnect.** It renders inline on the affected row; only a genuinely
  failed runtime call flips the connection banner.
- **Inbox browser handoffs now open the broker-owned tab**, rather than opening
  the paper's canonical DOI in an untracked tab. The background service keeps
  the resolver or OA URL private, releases queued handoffs through the existing
  work-window choreography, and focuses the exact tab already correlated with
  the job.
- **Explicit zero-electronic-holdings resolver results now stop the handoff**:
  Alma “No full text available” and Primo NDE “No links are available for this
  record” pages report the existing `no_entitlement` outcome once. Inconclusive
  empty or slow resolver pages remain assisted, and no page text or URL leaves
  the browser.

## [0.5.0] - 2026-07-22

### Added

- **Stale-SSO detection and recovery on handoff tabs**: when a tracked
  institutional handoff lands on an identity-provider failure page
  (OpenAthens/Shibboleth stale or expired session), the extension reports a
  `handoff_outcome` to the daemon for the job's audit trail and re-drives the
  tab through the resolver once, minting a fresh sign-in exchange — no page
  content leaves the browser, only the outcome and the IdP hostname.
- `job_offer` now carries `requires_auth`, so the extension can distinguish
  "open access — just render it" handoffs from ones needing an institutional
  sign-in (groundwork for surfacing this in the popup).
- **Inbox access guidance**: triage actions now say “open access — no login needed” or “sign in to your institution first” when the daemon has classified their access requirements.
- **Citation-style rendering in the inbox**: each item now shows a
  reference-style line (authors, year, hyperlinked DOI) in a user-selected
  citation style — APA, MLA, or Chicago — persisted across visits. The DOI
  link is the citation's locator, replacing the separate "Open DOI" row.
- **Status glyph column in the inbox**: every row leads with a colored glyph
  for its kind (manual download ↓, browser handoff ↗, verify identity ?,
  watch hit ✶, retraction !); unknown kinds from a newer daemon degrade to a
  neutral dot. This replaces the action-kind pill. Hovering a glyph shows
  its meaning instantly (no native-tooltip delay), and the footer legend
  spells out all five. The footer (legend + keyboard help) sticks to the
  viewport bottom, so both stay visible without scrolling.
- **Collapsible backend details per inbox row**: a quiet "⋯" chip at the end
  of the actionable status line reveals item id, job id, and revision as
  three compact columns. Its meaning remains available on hover and as
  "Backend details" to screen readers.

### Changed

- **Inbox visual overhaul**: paper titles are larger, semibold, and clamp to
  two lines instead of truncating at one; the action-kind pill no longer
  stretches into a full-width bar; authors/year render as plain metadata
  prose (labels kept for screen readers); job ids demote to a muted
  monospace line; quarantine file paths collapse to an ellipsized code span
  with the full path in the tooltip. "Open" is styled as the primary action
  on rows where it is the advancing step, while Dismiss/Reject become quiet
  ghost buttons with a danger hover. The header consolidates to two rows,
  the counts line omits zero buckets, link labels capitalize properly
  ("Open DOI"), and rows whose title is just the action kind fall back to
  the paper's DOI styled as a placeholder. Detail text lost its "DETAIL"
  label and reads as plain prose, author lists duplicated into a title's
  " - " suffix are stripped, and the counts line pluralizes correctly.

## [0.4.3] - 2026-07-20

### Fixed

- The options page now requests host access for every registered adapter,
  keeping provider support and Firefox runtime grants in sync.
- Assisted downloads are attributed through the complete adapter registry when
  exactly one tracked job matches the provider host; ambiguous downloads remain
  unowned.
- Firefox now ignores broad native/manual download correlation because it
  cannot steer those files into *papio*'s adoption directory; only exact
  extension-started downloads are acknowledged, so assisted controls remain
  manual while direct extension-API downloads remain automatic.

## [0.4.2] - 2026-07-20

### Fixed

- Repackages the 0.4.1 tracked-provider-host fix after its store workflow
  stopped before either upload when Chrome's publishing client added a required
  API v2 publisher identifier. Extension runtime behavior is unchanged.

## [0.4.1] - 2026-07-20

### Fixed

- Tracked institutional handoffs now classify provider landings from the
  extension's complete adapter registry instead of relying only on the
  protocol-capped `provider_hosts` offer list. Resolver redirects can therefore
  reach every 0.4.0 adapter family while unregistered hosts remain assisted.

## [0.4.0] - 2026-07-20

### Added

- **Acquire this page**: a popup button (shown only when the connected daemon
  advertises the `page_acquire` feature) reads the current tab's
  `citation_doi` metadata under the activeTab grant and asks the daemon to
  acquire the paper; pages without a DOI show "no DOI found on this page"
  and send nothing.
- Adapters can declare `requiresVisible`; their handoff tabs then open in a
  normal, unfocused window instead of the minimized work window (fix path
  for providers that under-render while hidden). No current adapter sets it.
- **14 new fixture-backed provider adapter families**: APA PsycNet, Annual
  Reviews, Taylor & Francis Online, Emerald Insight, Cambridge Core, Thieme
  Connect, Nature, Oxford Academic (Silverchair), MIT Press, BMJ,
  PsychiatryOnline, JAMA Network, Wolters Kluwer/LWW (Ovid journals), and
  HAL — each registered from an authentic captured page (success plus a
  denial capture where one was reachable), doubling adapter coverage of the
  real missing-PDF corpus. Ovid SSO-walled and ISHS Acta Horticulturae
  member-credit pages stay assisted: no authentic entitled capture exists,
  so no adapter is registered for them.
- `scripts/sanitize-fixture.ts`: one-command capture sanitation — reads a raw
  saved page, runs `sanitizeFixture`, verifies the residual-leak guard, and
  writes the committable fixture with its provenance header.

### Fixed

- The developer-only fixture-capture tool no longer leaks its filename
  reservation when Chrome rejects a download; unclaimed reservations expire
  after one minute.
- Fixture sanitization hardened for the new captures: URL-valued provider
  metas (e.g. `citation_pdf_url`, `wkhealth_pdf_url`) keep queryless selector
  evidence instead of being dropped, comments are emptied without merging
  adjacent markup, script/style/SVG bodies are always emptied, and the
  provenance header's provider label is itself guarded against opaque
  observed-host names.

## [0.3.1] - 2026-07-19

First version submitted to Firefox Add-ons (AMO, listed channel). Chrome Web
Store carries 0.3.0 — the cross-store skew is intentional; the listings are
independent.

### Fixed

- The developer-only "Capture fixture" panel in the popup no longer ships to
  store users: it is gated on
  `chrome.management.getSelf().installType === "development"` and appears only
  for unpacked/dev installs.
- The manifest `description` was shortened to fit the Chrome Web Store's
  132-character summary limit (an over-limit summary blocks the store upload;
  `web-ext lint` does not catch it).
- `extension/package.json` version brought back in sync with
  `extension/manifest.json` (the compat preflight in CI enforces they match).

### Changed

- *papio* is now italicised in the extension's own UI, matching the
  product-wide brand convention: a `renderPapio` helper (`src/dom.ts`) wraps
  the wordmark in `<em>` across the popup (daemon status, resolver lede), the
  options page (consent, work-window, and daemon-footer status lines), and the
  static popup/options HTML.

## [0.3.0] - 2026-07-18

First store-submitted version (Chrome Web Store). Shared a version stream with
daemon v0.3.0 — see the root `CHANGELOG.md` `[0.3.0]` section for the complete
extension changes (library-resolver access grants, daemon version-skew
surfaces, Firefox support, background work window, store submission tooling).
