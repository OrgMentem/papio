# Changelog

All notable changes to the *papio* daemon and CLI are documented here, keyed
to `v*` release tags. The browser extension is versioned and released
independently (`ext-v*` tags): from extension 0.3.1 onward its changes live in
`extension/CHANGELOG.md`. Through `[0.3.0]` the two shared a version stream,
so older sections below include extension entries. The initial release entry
is synthesized from the complete `papio` and `zotio` Git histories and the
execution records in `notes/acquisition-stack-plan.md`.

## [Unreleased]

### Added

- **Backfill watches**: `papio watch add --kind backfill` schedules the
  existing `acquire --from-zotio` queue on a cadence, so a growing Zotero
  library steadily self-completes its missing PDFs. Runs are bounded by
  `--limit-per-run` and idempotent (deterministic per-item request IDs), and
  the watch is force-runnable with `papio watch run` like any other.
- **Alert-only watches**: `papio watch add --mode alert` runs the scheduled
  discovery search and library-ownership filter but *reports* new works
  instead of acquiring them. New finds are recorded once per watch (re-runs
  never re-report) and browsable with the new `papio watch digest <id>`
  command; notifications point at the digest.
- **Webhook notifications**: a new `notify.webhook_url` config field delivers
  every daemon notification (watch results, human-action handoffs, imports) as
  a JSON POST — Slack/Discord/ntfy-style receivers work out of the box — in
  addition to the local desktop channel. Optional `notify.webhook_secret` is
  sent as a bearer token. Delivery is best-effort and never fails the work
  that triggered it.
- **Semantic Scholar discovery backend**: discovery is now pluggable behind a
  source seam. `discovery.sources = ["openalex", "semanticscholar"]` in config
  fans searches (and watches) across both backends with DOI/title
  deduplication in preference order; `papio search --source` selects one
  explicitly. Citation snowball (`--cites`, `--cited-by`) is supported on both;
  arXiv-only Semantic Scholar results now carry their identifier through to
  acquisition. API key (optional) lives at `sources.semanticscholar.api_key`.

### Changed

- The MCP read resources (`papio://jobs`, `papio://artifacts`,
  `papio://exports`, …) now return `{"<name>": [...], "truncated": bool}`
  envelopes instead of bare arrays, making the 100-row cap honest. Filtered
  and paginated access remains available through the command facade
  (`jobs list --state --limit`).

## [0.5.0] - 2026-07-19

### Added

- Broader browser reach for the native-messaging connector. `papio native-host
  install` now registers the host with every installed Chromium browser it
  detects — Chrome, Edge, Vivaldi, Brave, Opera (Chromium too) — plus Firefox,
  each at its own per-user location (directory on macOS/Linux, registry key on
  Windows), so the same extension works across them. A new `browser.extension_ids`
  config field lists additional Chrome-family extension IDs (e.g. an Edge
  Add-ons build) alongside `extension_id`; the daemon accepts any of them and the
  manifest's `allowed_origins` lists them all.

### Fixed

A triaged glean audit pass (33 confirmed findings fixed, each with a
regression test where behavior changed):

- OA resolver identity verification: CORE, Europe PMC, and OpenAlex title
  searches now verify the normalized title — plus publication year and the
  full author list whenever the request supplies them — before trusting a
  result; Unpaywall requires the returned DOI to match the requested one;
  arXiv compares exact version-stripped IDs instead of substring matching.
  Cuts wrong-paper acquisition risk across the discovery plane.
- PDF identity matching scopes DOI and supplementary-material signals to the
  document's front matter, so a bibliography citing other DOIs — or a body
  mention of "supplementary material" — no longer rejects a correct article.
- Download safety: caller-supplied headers (Authorization, API keys) are
  stripped on cross-origin and HTTPS→HTTP redirects, and the body-reader
  goroutine no longer leaks when a response ignores cancellation.
- Storage integrity: bundle export and Zotero plan staging copy artifacts
  instead of hard-linking the immutable store (a consumer mutating the copy
  could corrupt it); concurrent same-hash promotions converge atomically;
  failed exports roll back the files they created; failed SQLite backups no
  longer strand a partial destination file; promotion and backup fall back
  gracefully on filesystems without hard-link support.
- Concurrency: RPC calls on separate IPC connections no longer serialize
  daemon-wide behind one slow call; the browser bridge releases its session
  lock during PDF validation on download adoption; the serial auto-importer
  releases its lock during retry backoff; concurrent Zotio plan applies of the
  same plan are now mutually exclusive, and a claim abandoned by a crash or
  cancellation heals after a 15-minute lease instead of wedging the plan.
- Job lifecycle: context cancellation during auto-import stays retryable
  instead of recording a permanent failure; crash recovery clears abandoned
  quarantine files and the quarantine sweep continues past individual cleanup
  failures; a validation-persistence failure can no longer orphan a
  just-promoted artifact; watch-discovered works keep their OpenAlex
  identifier; pending notifications flush on a timer instead of waiting for
  the next event; `ping` answers from cache instead of blocking on the daily
  update check.
- Protocol strictness: strict JSON decoding rejects trailing documents; batch
  submissions reject unknown fields; Zotero item keys are validated
  fail-closed at the zotio integration boundary (the published v1 protocol
  contract is unchanged); batch identity hashes widened from 32 to 128 bits,
  with manifests from earlier releases still readable.

## [0.4.0] - 2026-07-19

### Added

- First-class Windows support. The daemon's local RPC runs over a named pipe on
  Windows — restricted to the current user via an explicit SDDL, the analog of
  the Unix socket's `0600` — while macOS and Linux keep their Unix-domain
  socket; the transport is chosen at build time. `papio init`, `papio
  native-host install/uninstall/status`, and `papio doctor` register the browser
  connector through the per-user registry
  (`HKCU\Software\...\NativeMessagingHosts`) instead of a manifest directory,
  and — because Windows has no unprivileged symlinks — install a copy of the
  `papio` binary as the native host (rerun `papio init` after upgrading to
  refresh it). Configuration lives at `%APPDATA%\papio` and data at
  `%LOCALAPPDATA%\papio`, and the update hint recognizes Scoop
  (`scoop update papio`). macOS and Linux behavior is unchanged.

## [0.3.0] - 2026-07-18

### Added

- Automatic, institution-agnostic library-resolver access. When a library's
  OpenURL resolver shows a "full text options" menu instead of direct-linking
  to the provider, *papio* follows the institution's top-ranked electronic
  service link itself — gated on a host permission for that resolver origin.
  The daemon advertises its configured resolver origins in the `hello_ack`
  handshake (new optional `resolver_origins`, backward compatible within
  `papio-browser/1`); the extension requests exactly those origins, so the
  popup surfaces a one-click "Allow library access" prompt (and the toolbar
  badge counts them) whenever a configured resolver isn't granted yet, and the
  options page lists the user's own resolvers under "Your library". Custom
  resolver domains outside the built-in Ex Libris hosts are reached through an
  optional `https://*/*` pattern that is never granted in bulk — only the exact
  configured origin is ever requested. Institution identity lives only in
  `config.toml`, never in extension code.

- Update discovery, without auto-install and without silent network calls.
  Store-delivered extension builds are stamped with the daemon version they
  shipped with, so the popup can show a calm "papio X.Y is available" line
  when the connected daemon is older — *papio* itself performs no network
  activity for this. Separately, an opt-in `[updates] check = true` setting
  (offered by the `papio init` prompt, default yes) has the daemon consult the
  *papio* and zotio GitHub releases APIs independently at most once a day. *papio*
  status appears in daemon status; both targets surface in `papio doctor` and a
  once-daily stderr hint. Configurations without the setting never check.

- Version-skew awareness across every surface. The `hello_ack` handshake now
  carries the daemon's version and a feature list (optional, backward
  compatible within `papio-browser/1`), so the auto-updating extension can
  degrade gracefully against an older daemon instead of failing opaquely. The
  popup reports daemon health directly: a quiet version line when healthy, and
  actionable states for daemon-unreachable, daemon-out-of-date, and
  extension-out-of-date; the toolbar badge shows `!` when attention is needed
  and stays clear otherwise. The options page footer shows extension and
  daemon versions at a glance. The daemon records the connected extension's
  version and rejects extensions below a minimum floor with a clear
  update-the-extension message.

- `papio doctor` now walks the whole integration chain in one report: the
  Phase-1 readiness checks (config paths, database, PDF tooling, credentials)
  followed by integration checks — daemon reachability and version match,
  browser-extension connectivity, native-messaging-host manifests for Chrome
  and Firefox, and the zotio preflight — each failure with a concrete `fix:`
  line. The same diagnostics are exposed to agents as a read-only
  `papio_doctor` MCP tool.

- Every CLI command now warns on stderr (once per invocation, never on
  stdout) when the running daemon's version differs from the CLI binary,
  with the exact recovery command.

- Release engineering: `release_metadata.py compat` mechanically verifies the
  cross-artifact compatibility floors (daemon↔extension minimums, zotio
  minimum version, extension manifest/package version agreement) as a
  `release.sh` step and a source-only CI check; `release.sh` now also
  packages the Firefox extension archive alongside the Chrome one. A shared
  release runbook lives at `.agents/skills/papio-release/SKILL.md` and is
  cross-referenced from zotio.

- Extension store submission path for Chrome Web Store and Firefox Add-ons
  (AMO). `extension/scripts/submit-firefox.sh` signs and submits the built
  Firefox package via `web-ext` (AMO API credentials from `extension/.env`),
  and `extension/scripts/submit-chrome.sh` uploads the Chrome package via
  `chrome-webstore-upload-cli`; both are exposed as `bun run submit:firefox`
  and `bun run submit:chrome`. Paste-ready store listing kits (name, summary,
  full description, per-permission rationale, data-collection disclosure, and
  reviewer build instructions for the bundled source) live at
  `extension/docs/amo-listing.md` and
  `extension/docs/chrome-web-store-listing.md`.

- The bundled-zotio compatibility floor (`internal/zotio/client.go`
  `MinimumVersion`) now targets a released zotio line (`0.10.0`) instead of an
  unreleased `1.0.0`; a built zotio 0.10.0 satisfies every capability, operation,
  and write-target *papio*'s preflight requires. `release.sh` now stamps the
  bundled zotio binary with zotio's own version rather than *papio*'s, so the
  cross-artifact compatibility check reflects the real zotio being shipped.

- Documentation: a `Version skew and updates` troubleshooting section (update
  flow, popup states, config-newer-than-binary errors), sister-project
  cross-references between papio and zotio in both READMEs and docs, and
  regenerated command reference.

- The MCP `papio_status` tool now surfaces the same actionable `category` and
  `guidance` as the CLI for parked and no-file jobs (including the config-aware
  `institution_not_configured`), so agents driving *papio* over MCP get the same
  diagnosis and next step as a human. The category catalog moved to a shared
  `internal/errcat` package consumed by both the CLI and the MCP server, so the
  two surfaces cannot drift.

- `papio init` now captures the browser extension IDs during first-run setup, so
  the native messaging host installs on the first run instead of failing with
  `browser.extension_id is not set` and forcing a config hand-edit and re-run.
  The Firefox add-on ID defaults to the built extension's fixed gecko id
  (`papio@orgmentem.com`) so Firefox works out of the box; the Chrome ID is
  prompted (paste the value from `chrome://extensions`). New `--extension-id`
  and `--firefox-extension-id` flags cover non-interactive setup. Unit-tested
  (flag and interactive paths, including that the captured Chrome ID reaches the
  native-host install) and smoke-verified end to end.

- Actionable error categories in `papio status`. Every parked or settled-without-
  a-file job now shows a short, stable category and a one-line next step instead
  of a raw internal reason (or nothing, for failed/unavailable). The catalog is
  config-aware: a job that found no copy under assisted/maximal mode with no
  institution configured surfaces as `institution_not_configured` pointing at
  `papio init`, rather than a silent `unavailable`. Categories/guidance are added
  to the status JSON (`category`, `guidance`) for agents. The same category and
  next-step now print under `papio acquire --wait` when a job parks or settles
  without a file, and the desktop human-action notification tells the user to
  `run papio status to see why` instead of a bare count. Unit-tested; the status
  view and `acquire --wait` guidance are smoke-verified against the live daemon.

- Per-institution access profiles and guided institution onboarding. Named
  resolver profiles under `[browser.resolvers.<name>]` are now full institution
  tables (`openurl_base_url` plus optional `shibboleth_entity_id` and
  `proquest_account_id`), so a multi-institution user routes each job's login to
  the right library. This lifts the earlier "default profile only" limitation on
  federated login-routing and the ProQuest account-id unlock: the daemon now
  wires `login_entity_id`/`proquest_account_id` per selected profile, and a
  named institution never inherits the default institution's identity.
  `papio init` gained an "Institution" step (and `--openurl-base`,
  `--shibboleth-entity-id`, `--proquest-account-id` flags); the ProQuest prompt
  accepts a pasted resolver URL and extracts `accountid=` for users who don't
  know their numeric id. Config validation, per-profile offer wiring, and the
  account-id extractor are unit-tested; the interactive flow is smoke-verified
  end to end.
  Older single-base configs keep loading: a resolver profile may still be a bare
  `name = "https://…"` string (shorthand for `openurl_base_url`), so no config
  migration is required.

- ProQuest account-id unlock: on ProQuest's "Find your institution" wall, papio
  appends `?accountid=<id>` to the current URL, which unlocks Example University's
  institutional access with **no sign-in at all** (verified live 2026-07-18 —
  resolves the wall cold, "Access provided by EXAMPLE UNIVERSITY"). New
  per-institution config `[browser] proquest_account_id` (digits); the daemon
  passes it as the optional job-offer field `proquest_account_id` (default
  profile only); the ProQuest adapter gains `accountIdParam: "accountid"`, and
  on a `login` verdict papio appends it (latched, once) — preferred over the
  federated route since it needs no credentials. This is the fix for the
  ProQuest openurl-handler blocker (the Shibboleth-DS route authenticated only
  ProQuest's main context, not the link-resolver handler). Config + protocol +
  adapter + bridge are unit-tested; the full download still needs a live pass on
  a ProQuest-*held* title.

- Institution auto-selection ("login routing"): on a provider login wall, papio
  navigates the handoff tab straight to the institution's federated login,
  skipping the provider's institution picker — selection is deterministic config
  (which institution you're at), not a secret, so only credential entry stays
  with you. New per-institution config `[browser] shibboleth_entity_id` (the
  Shibboleth IdP entityID, e.g. Example University's `https://idp.example.edu/entity`); the
  daemon passes it to the extension as the optional job-offer field
  `login_entity_id` (default resolver profile only, to avoid mis-routing another
  institution's job); and an adapter gains an optional `federatedLogin` template
  (`{entityID}` placeholder). On a `login` verdict papio navigates once (latched)
  to `<federated-login>?entityID=<configured>`. ProQuest ships the route
  (verified live 2026-07-17: Example University's entityID via ProQuest's Shibboleth DS URL
  routes straight to `idp.example.edu` login, skipping the WAYF picker). Config +
  protocol + adapter classify + bridge routing are unit-tested; the full
  post-sign-in download on a ProQuest-held title still needs a live pass.

- ProQuest institution-wall handling (`proquest` adapter v0.2.0): a `login`
  classify rule (ordered before `article`) now recognizes ProQuest's "Find your
  institution" wall (`form#institutionForm` + `input#institutionName`,
  fixture-backed `fixtures/proquest/login-return.html` captured live via CDP).
  *papio* surfaces it as a human sign-in step (`login` → `auth_pending`) instead
  of silently staying assisted/`unknown`. Matters disproportionately because
  Example University's OpenURL resolver routes many titles (incl. SAGE/T&F journals) to
  ProQuest rather than the publisher. Classify verified by fixtures; the full
  post-sign-in download recovery (authenticate ProQuest → re-drive → entitled
  docview → download) still needs a live pass.

- SAGE Journals adapter (`journals.sagepub.com`), fixture-backed
  (`fixtures/sage/success.html`, captured live via CDP from a Example University-authenticated
  article). SAGE emits no Highwire metas; classifies on `publication_doi` + the
  `downloadPdfUrl` anchor (same shape as ACM) and downloads that anchor's
  `/doi/pdf/<doi>?download=true` href. Classify is fixture-verified; the
  end-to-end download is not yet live-exercised because Example University's resolver routed
  the SAGE test title to ProQuest rather than sagepub (the adapter fires when a
  title routes to journals.sagepub.com).

- Wiley Online Library adapter (`onlinelibrary.wiley.com`), fixture-backed
  (`fixtures/wiley/success.html`, captured from a Example University-authenticated article).
  Classifies via the Highwire `citation_pdf_url`/`citation_title` metas, then
  builds and fetches Wiley's direct `/doi/pdfdirect/<doi>?download=true` file
  through the privileged downloads API — `citation_pdf_url` (`/doi/pdf/`) and
  the `/doi/epdf/` link both return an HTML viewer wrapper, only `pdfdirect`
  returns the file (verified live end-to-end: 1.15 MB PDF → `ready`). Closes the
  gap where Wiley pages classified `unknown` and stayed assisted (browser-
  agnostic; affected Chrome too). tandfonline/psycnet remain unimplemented —
  permissioned but not yet fixture-backed (both paywalled in the dev session;
  psycnet also emits no standard metadata).

- Firefox dev loop: `bun run dev` runs `build.ts --watch` (rebuilds `firefox/`
  on any `src/`, `icons/`, or `manifest.json` change) alongside `web-ext run`,
  which hot-reloads the add-on in a dedicated Firefox Developer Edition instance.
  `web-ext-config.mjs` pins an absolute, gitignored dev profile
  (`.ff-dev-profile`) so permissions and institutional logins persist across
  reloads — and, being path-based, boots straight in without Firefox's
  profile-chooser modal. web-ext installs and hot-reloads over the devtools
  RDP (not WebDriver/Marionette), so it does not set `navigator.webdriver` — but
  that live RDP connection makes Firefox show its remote-control indicator and
  is itself an automation surface a bot wall could fingerprint. Two modes:
  `bun run dev` for fast iteration and fixture testing; for real Cloudflare-
  walled providers, `bun run build` then load `firefox/` manually via
  `about:debugging` (one-shot install, no persistent connection, no indicator,
  `navigator.webdriver` false).

- Brand: a papio logo — an oblique lowercase **p** (coral `#E85D4A`) inside a
  broken ink ring (`#2B2D42`); the p's descender becomes a download arrow that
  exits through the ring's bottom gap. Structural sibling of the zotio badge
  with its own palette. Vector sources live in `docs/assets/` (`logo.svg`,
  `logo-dark.svg` for dark surfaces, `logo-tile.svg` for theme-agnostic toolbar
  icons, `logo-wordmark.svg`, `logo-wordmark-dark.svg`) and are used in the
  README wordmark header, the docs site logo/favicon (`mkdocs.yml`), the Chrome
  extension toolbar/action icons (`extension/icons/`, wired in
  `manifest.json`), and the extension popup header.
- Brand: the README header wordmark (`logo-wordmark.svg`,
  `logo-wordmark-dark.svg`) is now an animated SVG. The mark builds in on a calm
  ~10s loop — the broken ring draws on, the coral **p** and download arrow drop
  into place, the wordmark rises in — then a cheeky little papio (baboon) head
  peeks over the wordmark to blink, tilt, and wave before ducking away, leaving a
  long clean hold on the finished logo. Pure CSS (no script/SMIL, self-contained
  for GitHub's `<img>` rendering); the resting state is byte-for-byte the prior
  static logo and `prefers-reduced-motion: reduce` shows it with no animation.
- Background work window: papio now does its browsing in one dedicated
  minimized, unfocused Chrome window instead of the user's tab strip. Every
  broker handoff tab (first, queued, and download-fallback) and the keepalive
  tab route there; provider-spawned viewer tabs inherit it via their opener.
  A tab surfaces (window restored + focused, tab activated) only when the
  human is needed: on the IdP transition (`auth_pending`), on keepalive
  reauth, and from the popup's Focus button — which now also restores a
  minimized window. Opt out anytime via the options page ("Keep papio tabs in
  a background window"); disabling restores the legacy visible-handoff
  behavior, as does any runtime without `chrome.windows`.
- Firefox support, day one: `bun run build` now emits a second complete
  extension at `extension/firefox/` (MV3 event-page background as a classic
  iife bundle, `browser_specific_settings.gecko.id = papio@orgmentem.com`,
  `strict_min_version 128`) generated from the same `manifest.json` source of
  truth. The native-host installer registers a Firefox manifest
  (`allowed_extensions`, Mozilla `NativeMessagingHosts` dir) alongside
  Chrome's when the new `[browser] firefox_extension_id` config is set, and
  the host accepts Firefox's bare-ID invocation with the same exact-match,
  fail-closed validation as Chrome's origin. The options page gained a
  "Library resolver access" grant section because Firefox treats MV3
  `host_permissions` as runtime-optional; on Chrome it simply shows the
  install-time grants. No behavior change for Chrome users. The provider
  section also gained "Grant all providers" / "Revoke all" — one click issues a
  single `permissions.request` for every publisher origin (one Firefox
  doorhanger) instead of ten separate grants.

### Changed

- Rewrote the README on the zotio template: centered wordmark + tagline +
  badges + docs nav, a "Why papio" section with the hard boundaries, a
  hand-drawn two-row serpentine architecture diagram in the brand palette
  (`docs/assets/architecture.svg` + `-dark.svg`, theme-switched via a
  `<picture>` element; replacing the mermaid flowchart, which rendered
  poorly on GitHub) with the
  access-mode table, the research loop, validation/provenance and
  zotio-boundary sections, the MCP tool surface, and install paths (brew,
  scoop, signed releases, source). Brand style: *papio* italic in prose,
  zotio plain.
- Redesigned the wordmark's baboon cameo: the abstract head is now a
  recognizable hamadryas baboon (cape mantle, long muzzle, heavy brow) that
  peeks up holding a stack of papers instead of waving. Light and dark
  wordmark variants stay in sync; in dark mode the paper stack renders navy
  against the cream mantle for contrast, and the face details (eyes, brows,
  muzzle) stay navy on the coral face in both modes.
- Config unknown-field errors now explain that the config was likely written
  for a newer papio and name the offending fields, instead of surfacing a raw
  TOML parse error. zotio preflight failures name the installed version, the
  configured executable path, and the action that fixes the mismatch.
- MCP tool surface now derives from the papio CLI command tree instead of a
  parallel set of hand-maintained typed tools, so the CLI is the single source
  of truth and the two can no longer drift. The default surface is a command
  facade — `papio_command_search` to discover commands and `papio_command_run`
  to execute one (JSON output, command-local flags only, inherited globals
  rejected); `PAPIO_MCP_SURFACE=mirror` instead exposes one `papio_<command>`
  tool apiece. Setup and lifecycle commands (`init`, `config`, `daemon`,
  `native-host`, `mcp`) are hidden via `mcp:hidden` annotations. Two composite
  tools with no single-command equivalent stay first-class — `papio_acquire_batch`
  (bulk work input) and `papio_batch_wait` (bounded polling) — alongside the
  five read resources. Migrated the server library from
  `modelcontextprotocol/go-sdk` to `mark3labs/mcp-go` for parity with zotio.

### Fixed

- Reliability: overlapping extension state writes are now persisted through a
  serialized save chain, so a reordered `chrome.storage` write can no longer
  resurrect a stale snapshot after a service-worker restart.
- Reliability: concurrent queued-handoff fallback timers no longer drop each
  other's forced releases; a single drain loop consumes every pending release,
  so queued jobs can no longer be stranded invisibly with `tab_id -1`.
- Reliability: a failed native-host idle-poll write now tears the bridge down
  instead of leaving the process alive but no longer polling (which starved the
  extension of offers and cancels).
- Reliability: `fetchCandidates` propagates the `OpenHumanAction` write error
  before parking a landing-page-only job, matching `exhaustedCandidates`, so a
  transient write failure can no longer strand a job with no human-action row.
- Concurrency: removed a redundant drain goroutine in `readBodyWithContext`
  that doubled leaked goroutines when a response body read hung.
- MCP `acquire.report` now classifies failures — missing batch as `not_found`,
  malformed batch ID as `invalid_argument`, and other failures as `internal` —
  instead of collapsing every error into `not_found`.
- Batch settlement is now a single source of truth (`batch.Report.Settled`),
  removing a stale duplicate outcome list in `papio_batch_wait` that carried
  legacy outcome spellings.
- Docs/schema for `papio_batch_wait` `timeout_seconds` now state that `0` or an
  omitted value defaults to 300, matching the implementation.

## [0.2.0] - 2026-07-15

### Phase 0 — contracts and prerequisite

- Established the *papio* Go/Bun workspace, fail-closed shared protocol fixtures,
  and draft work-request, acquisition-bundle, and browser contracts.
- Added zotio's stored-attachment upload path with reconciliation and retry-safe
  Web API registration, which is the import prerequisite for *papio* exports.

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

### Phase 4 — zotio, MCP, and human resolution

- Added zotio capability/version preflight, preview/apply plans, confirmation
  hashes, import-ledger idempotency, missing-PDF intake, and stored attachments.
- Added MCP tools and resources over the same application service, plus bounded
  human identity-review resolution and action lifecycle cleanup.
- Added extension session recovery across daemon restarts and startup wake-up.

### Post-Phase 4 — autonomous acquisition

- Added OpenAlex discovery, batch acquisition, serialized retry-safe auto-import,
  session keepalive, observed-provider fixture capture, library-aware batches,
  OA browser fallback, snowball search, status/reporting, notifications,
  watchlists, MCP loop closure, and first-run onboarding.
- Updated zotio integration with collection-aware missing-PDF scopes, item-type
  valid container-title mapping, exact-key enrichment, and transactional
  workflow execution.

### Phase 5 — release preparation

- Added local release artifacts for *papio* and zotio binaries, the extension ZIP,
  dependency inventories, license reports, hashes, and a machine-readable
  release manifest.
