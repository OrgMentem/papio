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

### Added

- **One session row per institution.** The popup's institution card renders a
  row for every config-derived resolver origin advertised in the daemon hello —
  never for a provider offer or a host-permission grant. Each row has its own
  evidence-based verdict, freshness, and a Sign in button that opens *that*
  institution, and keep-warm tracks each origin separately. Warm evidence
  releases only that origin's queued extension handoffs and daemon-side sibling
  re-offers; another institution's queue remains parked.
- **Session verdicts got stricter.** An auth-shaped URL alone never reads as
  signed out (inspection evidence is required), expired identity tokens no
  longer count as signed in, and sign-out markers are read only from real
  controls, links, and forms — never from page scripts, styles, or aggregate
  text.
- **papio owns its surfaces — leftover tabs clean themselves up.** Broker tabs
  papio creates are recorded in a durable ledger that survives extension
  reloads. Shortly after startup, ledger-owned leftovers still sitting in the
  papio tab group or work window are closed automatically; the popup's
  "Leftover papio tabs · Close them" card appears only for ambiguous strays
  outside those surfaces. Only ledger-owned tabs are ever closed: a tab papio
  merely reused, a tab the user is viewing, a pinned keepalive tab, a tab an
  active job tracks, or a stranger tab that merely sits in a papio-titled
  group is never touched.
- **The popup can send the current PDF to *papio*.** It classifies the active tab
  as a PDF, DOI page, or neither; for a PDF it queues or joins the matching
  job, starts a browser-managed download under `papio/<job-id>/`, and reports
  sending, adoption, validation, or a recoverable failure. DOI detection is
  layered — citation/Dublin-Core/PRISM meta tags, canonical and `doi.org`
  links, a bounded page-text scan, and a JSTOR stable-page fallback
  (`10.2307/<id>`). Provider click downloads remain human-assisted on
  Firefox, whose WebExtensions API has no filename-routing hook.
- **Institution sessions are visible at a glance — and the verdict is
  evidence-based.** The popup shows the resolver session as warm, signed out,
  unclear, or keep-warm disabled, always naming its evidence source and time
  ("via your open library tab · 12:38 pm"). The verdict comes from inspecting
  a real resolver tab — the focused tab first — for sign-out affordances
  (including inside closed menus, `href`/`formaction` targets, and ARIA
  labels), with a web-storage JWT identity fallback for Ex Libris-style
  platforms (a bare `sub` claim never counts: anonymous tokens carry one).
  papio never asserts "signed out" without a completed inspection saying so,
  an inspection failure is reported distinctly from a clean miss, and an
  evidence-free probe never latches. A warm session offers no sign-in button;
  "Sign-in unblocked N items" announces once per release event. Options adds
  extension-local keep-warm enable/disable and refresh-interval controls;
  credentials and identity-provider hosts never enter the daemon protocol.
- **Pages with an acquisition already in flight show live progress, not a dead
  button.** The popup renders the job's latest activity with its age, an
  honest "No progress for <time>" prefix when stalled, waiting-on-you wording
  for parked jobs, and Open-inbox-item / Go-to-tab actions.
- **The inbox separates concerns into Actions, Watch hits, and Activity tabs**,
  with actionable items first and keyboard-accessible tab navigation. The
  Activity tab groups a bounded timeline by job, collapses repeats, and
  truncates behind **Show more**; it appears only when the daemon advertises
  `activity_feed_v1`, and older daemons get an explanation rather than an
  implied live push.
- **Every job and resolver tab papio opens lands in the papio tab group** (or
  the work window, matching the existing handoff surface), and repeated opens
  of the same item focus the existing tab instead of spawning duplicates —
  changed re-offers navigate the tab in place.
- **Handoffs are governed, not flooded.** papio drives at most two handoff tabs
  at a time (one until a sign-in is verified), queues the rest, closes tabs
  whose job settled, times out stalled drives after three minutes, and updates
  the papio tab group at most once per five seconds instead of thrashing it
  per job.
- **Security checks get attention instead of silence.** A Cloudflare/Turnstile
  challenge or redirect loop on a driven page pauses that job (never solved
  automatically), keeps its tab, cools the provider host down for ten minutes,
  raises the badge, and appears in the popup and inbox with a Go-to-tab
  action; solving it by hand resumes the drive automatically.

### Changed

- **The popup puts actions where operators look for them.** The current-page
  acquire action is now a paper-plus icon beside Inbox and leaves behind only
  live progress or delivery feedback; idle pages no longer consume a card.
  Papers waiting at institution sign-in sit under the institution session
  rows instead of in a separate amber card, while security challenges retain
  their own attention card. The impact heading and history link now share one
  compact row.

- **Badge precedence is explicit and centralized in the background broker.** A
  broken or disconnected bridge shows `!` first, a blocking permission or
  sign-in state takes the next slot, and the pending triage count is shown only
  when higher-priority attention is clear. Popup and options surfaces continue
  to expose the fuller status and version details.

## [0.8.1] - 2026-08-02

### Fixed

- **A file-shaped link is no longer treated as permission to download without
  you.** An offer whose URL looked like a PDF was downloaded straight away, on
  the shape of the link alone, with no reference to whether the work sits behind
  a sign-in. That was safe only by accident: the one kind of offer that carries
  a file URL happens to be marked as needing no sign-in today, and nothing
  prevented a future one from differing. It is also reachable through
  configuration rather than code — an institutional offer carries whatever
  resolver address the operator configured, and *papio* places no restriction on
  its path, so a resolver whose address merely *looks* like a PDF would have
  routed a sign-in-required work straight into a download with nobody present.
  Whether a human must sign in is a property of the work, so it is now checked
  where the download decision is made. Only an explicit "sign-in required"
  refuses; everything else behaves exactly as before.
- **Works behind an institutional paywall are now recognised as needing a
  sign-in.** The daemon recorded several hand-offs as freely fetchable when they
  were not, including ones where this extension had itself reported an
  authentication wall. Those arrive correctly classified now, so such a work is
  held for you to sign in rather than being treated as open access — visible
  here as an amber sign-in count that was previously undercounted, and as work
  correctly queued instead of attempted.
- **A development daemon is no longer told to `brew upgrade`.** The popup's
  version notice assumed every out-of-date daemon came from the cask, so a
  source checkout — whose version carries a `dev` marker — was handed the one
  remediation that cannot work on it, and following it would install a release
  binary over the build under development. A `dev` daemon is now named as a
  development build and pointed at `make dev-deploy`, which is what actually
  rebuilds it, repoints the native-messaging host, and restarts both.
- **The legacy work-window flag is honored in exactly one place.** The
  tri-state handoff-surface setting superseded the older boolean, but the
  migration had been implemented twice — once in the settings getter and once as
  a fallback in the bridge that could never execute. Anyone changing upgrade
  behavior would have edited the unreachable copy and shipped nothing. The
  surface a user gets is unchanged, including the Firefox 128 ESR degradation
  from tab-group mode.

### Changed

- **The wire contract's TypeScript half is now compiler-enforced.** Eighteen
  payload interfaces in `src/protocol.ts` were referenced by nothing, so a field
  added to the daemon struct and the JSON schema but forgotten here produced no
  typecheck error and no test failure. The parser's per-message field lists are
  now typed against those interfaces in both directions: a field the interface
  declares but the parser does not validate, and a field validated under a name
  the interface does not have, are both build errors. No frame that was accepted
  or rejected before changes status.

## [0.8.0] - 2026-07-27

### Added

- **Captures travel over the native-messaging bridge instead of your Downloads
  folder.** Sanitized page HTML is gzipped and sent to the daemon, which stores
  it in its own data directory. This works identically on Chrome and Firefox —
  the old path depended on `downloads.onDeterminingFilename`, which is
  Chrome-only and did not reliably fire even there, so a capture could land as
  `download (1).html` or vanish silently. Gated on the daemon advertising
  `page_capture_v1`. The capture panel is now a build-time developer
  affordance, stripped from shipped Chrome and Firefox bundles rather than
  hidden by a Chrome-only heuristic that left it visible on AMO.

- **Institutional sign-ins now stay visible until you act.** A handoff waiting
  at an identity provider gives the toolbar an amber count, draws attention to
  its restored work window, names its paper while a tab group is expanded, and
  appears in the popup with a **Focus** control. The signal no longer depends
  on catching the first focus grab at the exact moment it happens.
- **Cold institutional handoffs now preflight without disappearing.** An offer
  that needs institutional access waits briefly for a live resolver or provider
  return instead of minting an unattended SAML request. Its amber toolbar count
  remains visible while it waits, and a bounded fallback presents the sign-in
  tab even when session keepalive is disabled; open-access offers still open
  immediately.
- **`papio actions open` can now surface the tab papio already owns.**
  Extension 0.8.0 accepts the job-scoped `handoff_focus` command and restores
  the tracked handoff tab and its minimized work window instead of leaving the
  CLI to open an untracked duplicate. Older extensions are version-gated so
  their strict parser never receives an unknown native-messaging frame.

### Changed

- **The time-saved estimate is now a defensible 5 minutes per paper**, down
  from 20. The old figure implied hours saved after a handful of papers and
  read as marketing; the headline is still a rough estimate the extension
  computes from counts, never a measurement.
- **Dismissing an inbox item no longer asks first.** The row leaves the list
  immediately and the daemon call is held for six seconds behind an **Undo**
  bar (keyboard `u`); dismissing several rows in a row batches them into one
  undo. The confirmation dialog it replaces protected nothing — the daemon
  cannot un-cancel a job, so clicking through it committed the same
  irreversible change, one extra click per row. Leaving or hiding the page
  commits whatever is still waiting. Accepting or rejecting a quarantined PDF
  still asks.
- **The inbox says what a dismissal actually costs.** It mirrors the daemon's
  own rule (a dismissal cancels the job only when the job is parked on that
  action) and names the cancelled acquisition only in that case. Advisory
  `openurl_available` rows and actions left behind on a job that moved on are
  reported as what they are: a closed dead row.

### Fixed

- **SAGE article pages had become unreadable.** The adapter keyed on
  `a#downloadPdfUrl[data-doi]`, an element SAGE no longer renders — the page now
  offers a `View PDF/EPUB` link to an eReader route inside a `View Options`
  panel. Six of nine failed handoffs on one machine were SAGE, every one
  reported as "the provider's UI changed" and parked for manual download. The
  adapter now keys on the semantic `section.format--pdf_epub` panel and derives
  the download from the DOI rather than following the eReader link, which is a
  viewer rather than a file. Captured from a real authenticated page and
  committed as its fixture.
- **Only one `papio` tab group again.** Group rediscovery matched the title
  exactly, so once a group was renamed to carry the paper being worked on it
  became invisible to its own lookup and a fresh group was created beside it —
  three had accumulated on one machine. Adoption is now window-aware and
  serialised so concurrent folds converge, a renamed group is still recognised,
  and existing duplicates are merged on startup rather than left for the user
  to tidy up.
- **The options page reported permissions it did not have, and "revoke all"
  silently did nothing.** `https://*/*` is offered as an escape hatch for
  unlisted providers but appeared in no list the page rendered, so once granted
  it made every specific origin report as granted, and revoking a specific
  origin resolved successfully while changing nothing — the toggle snapped
  straight back. Underneath, the UI painted from the return value of
  `permissions.request`/`remove` rather than re-reading the resulting state, so
  it could claim a change that never took effect. The page now paints from a
  fresh `permissions.getAll()` snapshot, shows all-sites coverage as coverage
  rather than as a broken toggle, makes the all-sites grant itself visible and
  revocable, and either removes it as part of "revoke all" or says plainly why
  access remains.
- **papio no longer blames the adapter when it was never allowed to look.**
  Every page read goes through `chrome.scripting`, so a provider whose host
  permission is not granted is unreadable — and that was reported as the same
  "the provider's UI changed" as a stale adapter, with no indication anywhere.
  A blocked host is now named exactly in the outcome detail, surfaced on the
  toolbar and in the popup with a route to the grant control, and deduplicated
  so a standing condition reports once rather than once per tab update.
- **A single handoff no longer reports the same outcome seven times.** The
  classify retry loop re-emitted `ui_changed` on every second unknown verdict,
  up to the retry cap. It was invisible until the daemon began recording
  provider outcomes; the report is now latched once per drive and released when
  a genuinely new drive begins.
- **The inbox "View PDF" control never worked in any shipped version.**
  `requestNative` named `review_preview_result` as the reply it waits for, but
  `onInbound`'s correlation switch enumerated the other five result types and
  omitted that one — so the daemon issued the loopback preview capability, the
  frame fell through to the ignore-echo default, and every click sat until the
  request timed out reporting "The daemon did not respond in time". Correlated
  replies now route from a single `CORRELATED_RESULT_TYPES` list, and
  `requestNative` rejects an unrouted expected reply immediately regardless of
  whether its caller uses a literal, variable, or helper wrapper.
- **Recoverable institutional login pages are no longer navigated away from.**
  A password-expiry or retry error still surfaces its handoff window, but only
  a title-confirmed Shibboleth/OpenAthens **Stale Request** page is re-driven
  through the resolver.
- **Cancelled handoffs no longer leave a paper title on a live tab group.**
  When a keepalive tab retains the collapsed "papio" group after its last
  handoff closes, papio now resets the group title and collapse state first.
- **A handoff stranded on a dead sign-in page now raises the work window.**
  Stale-SSO recovery reported the failure and re-drove the tab through the
  resolver, but did so *before* the code that surfaces the minimized work
  window — so the one moment papio was certain a human was needed was the one
  moment it stayed hidden. Handoffs could sit unnoticed on an expired
  institutional sign-in for hours. The window is now restored and focused
  first, once per job.
- **Shibboleth "Stale Request" pages are no longer missed.** Some identity
  providers serve the dead page at the *same URL* as the working login form —
  only the document title differs — and detection ran solely on the tab's
  `complete` event, which a browser can deliver before that title resolves.
  Detection now also runs on title-only updates. It deliberately still
  requires the title: a URL-only rule would reload the page out from under a
  half-typed password on the live login form.
- **The stale re-drive is now bounded across service-worker restarts.** Its
  "once per outcome" latch lived in service-worker memory while the dead tab,
  the parked job, and the user's next sign-in attempt all survived a restart,
  so the resolver loop was effectively unbounded. The re-drive is charged to
  the same durable per-job authentication budget as a normal handoff drive.
  At the cap the tab is deliberately left on the failure page — the user needs
  to see it — and the job is reported `human_auth_required`, which keeps it
  parked daemon-side rather than silently looping.
- **A closed inbox page no longer keeps polling the daemon.** The counts poll
  re-armed itself unconditionally, so a page module that outlived its document
  kept issuing triage requests against whatever page replaced it. It now stops
  when the live document is no longer the one it bootstrapped against.
- **The popup’s Focus control can now surface its institutional handoff.**
  Its narrowly scoped broker permission is limited to that focus operation;
  the popup still cannot make inbox or triage changes.
- **`papio actions open` now refreshes an expired sign-in exchange.** A focus
  request re-drives a tab only while it is on an authentication page or is
  waiting for authentication, and otherwise leaves an active provider download
  alone.
- **One stale sign-in document now consumes one recovery attempt.** Browsers
  can report its title and completion at the same time; duplicate callbacks no
  longer exhaust the retry budget or send repeated resolver navigations.
- **The badge returns to blue after institutional sign-in completes.** Pending
  triage work no longer inherits the amber colour reserved for a sign-in
  blocker.

## [0.7.0] - 2026-07-25

### Added

- **Tab-group handoff on Firefox.** The collapsed "papio" tab group now works
  on Firefox 139+ (which added the `tabGroups` WebExtensions API), not just
  Chrome. Older Firefox still falls back to the background work window
  automatically. The handoff group is now coloured orange.
- **Acquisition history and impact stats.** The popup now shows a compact
  "Your papio impact" summary — papers acquired, estimated time saved (at a
  rough 20 minutes of manual chasing per paper), and success rate — with a
  **View history** control that opens a new full-tab history page. The page
  charts weekly acquisitions over the last 12 weeks and breaks down success
  rate, access routes (open access / institutional / licensed API / other),
  and how often an acquisition needed a human handoff. Needs a daemon with
  the `browser_stats_v1` feature; without one the popup hides the summary and
  the history page shows a muted "stats unavailable" note instead of an error.
- **The inbox keeps itself current.** It now refreshes the moment you return to
  the tab, and checks in on its own periodically while the tab stays open, so a
  new or resolved job doesn't wait for a manual Refresh. An auto-refresh never
  reorders the list while a confirmation dialog is open or an action is still
  in flight — it waits until you are done.

### Fixed

- Reloading the extension no longer creates a second "papio" tab group: the
  existing group is rediscovered by title first. (An extension reload clears the
  in-memory group id but leaves the physical group in the window.)
- The inbox popup now closes after you click **Open inbox** on Firefox, instead
  of staying on "Opening inbox…". Chrome already dismissed the popup when the
  new tab took focus; Firefox does not, so it is now closed explicitly.
- A daemon-side inbox or stats query that fails no longer disconnects the
  extension. The daemon used to treat it as a dead connection and drop the
  whole native-messaging session, taking page acquire, the triage inbox and
  the handoff flow down with it; it now replies with an error the extension
  renders as the muted "unavailable" state. Needs a daemon carrying the fix.

## [0.6.0] - 2026-07-24

### Added

- **Tab-group handoff mode.** A new "Where papio opens tabs" setting lets you
  put handoffs in a collapsed "papio" tab group in your current window instead
  of a separate background window (or inline, as before). The group folds away
  when idle, expands and focuses the tab only when you are needed for sign-in,
  then re-collapses; the keepalive session tab joins the same group. Chrome
  only (adds the `tabGroups` permission); Firefox falls back to the background
  work window. Your existing on/off choice is preserved.
- **Options page redesign.** Per-host library access and publisher-terms
  controls are now toggle switches, and the handoff-tab location is a
  highlighted button group, for a clearer at-a-glance view of what is granted
  and where handoffs open.
- **Popup and settings UX polish.** The inbox popup and options surfaces were
  refreshed for clearer status, actions, and at-a-glance state.

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
