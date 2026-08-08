# Changelog

All notable changes to the *papio* daemon and CLI are documented here, keyed
to `v*` release tags. The browser extension is versioned and released
independently (`ext-v*` tags): from extension 0.3.1 onward its changes live in
`extension/CHANGELOG.md`. Through `[0.3.0]` the two shared a version stream,
so older sections below include extension entries. The initial release entry
is synthesized from the complete `papio` and `zotio` Git histories and the
execution records kept during the initial build.


## [Unreleased]

### Added

- **Grab an open PDF straight from a browser tab (ADR-0020).** A tab
  rendered in Chrome's own PDF viewer has no DOM to scan, so the selection
  workspace used to report "no recognizable identifiers" for a paper the
  operator was already looking at. Scanning a PDF tab now offers a one-click
  "grab this PDF" row when the tab's own URL carries no identifier; accepting
  it allocates a `pdf_grab_v1` capture slot (`pdf_grab_request`/
  `pdf_grab_result`), steers `chrome.downloads.download` to
  `papio/grabs/<grab-id>/` — no PDF bytes ever cross native messaging — and
  the daemon's grab sweeper quarantines the settled file, structurally
  validates it, and extracts a front-matter DOI (`internal/pdf`'s
  `documentDOIs`). A found identifier creates the ordinary identifier-keyed
  job (ADR-0010's ledger dedupe applies naturally: an already-owned work
  reports "already in your library" rather than a duplicate job); no
  identifier parks a `pdf_identifier_needed` human action instead of ever
  submitting on the title alone (ADR-0019). Firefox has no download-steering
  API, so the row renders disabled there with honest copy. New migration
  0025 adds the `pdf_grabs` table.

- **An agent skill (`SKILL.md`) that drives the CLI directly.** MCP was the only
  documented way to hand *papio* to a coding agent, which put a server between
  the agent and a CLI it can already run. The repo now ships a single root
  `SKILL.md` — installable with `npx skills add OrgMentem/papio` or by copying
  it into an agent's skill store — covering the acquisition loop, the `--json`
  contract, and the rules that keep an autonomous agent inside papio's design:
  a human gate is an outcome rather than an error, bare `actions open` drains
  the whole handoff queue and is never an agent's to run, `actions resolve
  --accept` records a *human* verdict, `zotio apply` needs the plan's exact
  digest, and everything a publisher hands back is data rather than
  instruction. `papio mcp` is unchanged and stays the path for hosts that
  cannot run commands.
  `TestSkillInvocationsResolve` and `TestSkillFlagMentionsResolve` pin every
  command the skill runs and every flag it names — including the ones discussed
  in prose rather than on a command line — to the live cobra tree, so it cannot
  drift into telling an agent to run something that no longer exists.
- **`page_bulk_status_request` accepts OpenAlex work identifiers.** The
  `identifiers.kind` vocabulary gains `openalex` alongside `doi`/`pmid`/`arxiv`
  — mirrored across `internal/protocol`, the browser extension's TS
  validator, and `protocol/browser-v1.schema.json` so the three stay
  provably in agreement. The bridge normalizes a scanned W-id through the
  same `work.NormalizeOpenAlex` the CLI and MCP already use and resolves it
  through the existing OpenAlex source, so ownership answers come from
  papio's own ledger. zotio's matcher (DOI/ArXiv/PMID only) is never
  consulted for an OpenAlex-only row — it follows the same not-yet-checked
  presentation as any other identifier no configured source covers, never a
  false `eligible` claim.

### Changed

- **Dependency: `golang.org/x/net` 0.54.0 -> 0.55.0** (GHSA-5cv4-jp36-h3mw,
  medium: HTML-parser denial of service). papio parses provider landing
  pages with this parser; exposure was already bounded by the 512-byte
  landing-page read cap, and govulncheck reports no reachable vulnerable
  call after the bump.

## [0.19.1] - 2026-08-08

### Fixed

- **Workspace ownership checks no longer trigger a Zotero-account sync.** A
  triaged documentation review caught the privacy page promising "no request
  leaves your machine" for page-bulk ownership marks while the underlying
  zotio lookup unconditionally refreshed its mirror first — a network
  operation against the user's Zotero account, once per status poll. The
  page-bulk path now asks zotio in local-only mode (the on-disk mirror
  answers; nothing is synced), which makes the published claim true and
  removes per-poll cloud traffic. Pre-acquisition dedupe for batch submit
  and watches keeps the refresh-first behavior deliberately.

## [0.19.0] - 2026-08-08

### Added

- **A blocked Downloads folder is now a visible action, not a silent stall.**
  When macOS privacy consent (TCC) leaves the daemon unable to read the
  download-adoption root, a completed browser download opens one
  `downloads_access_required` human action on its job — surfaced in the
  extension inbox with required attention and the exact grant remedy — and
  resolves itself the moment adoption succeeds. Poll and sweep passes open
  at most one per job; dismissing it never cancels the job.
- **Adoption sweeps can no longer wedge on a hung filesystem.** Both sweeper
  passes route the adoption-root listing through the bounded, latch-aware
  reader (previously only per-job scans were protected), a single-flight
  gate guarantees at most one outstanding hung syscall bridge-wide (the
  suspend/resume log pair now fires exactly once per episode), and the
  terminal sweep removes empty stray directories from prior database eras —
  a store hiccup is never treated as evidence a job is unknown.

- **Page-bulk status now knows what you own.** `zotio.Service.LookupWorks` —
  already serving batch submit's ownership classification — is wired into
  `page_bulk_status`: a scanned page's identifiers merge zotio's
  `owned_with_pdf`/`owned_missing_pdf`/staleness verdicts with the daemon's
  own ledger, under a fixed precedence — papio's own ready bundle first,
  then zotio's `owned_with_pdf`, then zotio's `owned_missing_pdf` (carrying
  the Zotero item key), then a live queued job, then a complete negative
  lookup (`eligible`). `LookupWork` gains PMID (the facade previously
  carried only DOI/arXiv id; zotio itself already resolved PMID). A stale
  or failed zotio round reports the new `ownership_unknown` status rather
  than a false `eligible` or `ownership_incomplete` mark — page-bulk must
  never claim a work is unowned when it could not check. A `nil` zotio
  client (unconfigured) preserves the prior behavior byte-for-byte.
- **`papio stats page-bulk` measures the feature honestly, denominator and
  all.** Migration 0023 adds `rendered_record_count_hint` to
  `page_bulk_runs`: the extension's scanner counts visible result records
  for structurally-recognized page shapes only — definition-list rows, a
  reference/citation list, or a repeated card grouping, each requiring at
  least two matching siblings before it counts as a "list" at all — without
  reading a single title, URL, or docid, and reports `null` rather than a
  guess when no shape is recognized. The new `stats page-bulk` command
  reports, per source-origin class, total scan sessions, useful-scan rate,
  bulk leverage, submit conversion, and `identifier_yield` — now backed by
  that rendered-count denominator instead of standing without one.
- **`papio bench` compares acquisition coverage, not absolute results.** A
  new hermetic harness reads a `papio-bench-cohort/1` file — a work request
  plus its expected outcome class (`autonomous_ready`,
  `ready_after_human_boundary`, `honest_unavailable`, `identity_review`;
  never an expected provider or route) — and runs it twice against an
  ephemeral database, an empty artifact cache, and injected resolver
  fixtures: once with a baseline overlay (Semantic Scholar, OpenAIRE, and
  typed-relations resolution disabled) and once with the current build.
  `fixture_missing` is an explicit reported state, never a silent skip. The
  headline is `incremental_autonomous_ready` — how many more works the
  current build resolves without a human boundary than the baseline does —
  measured over a frozen field cohort seeded from the 2026-07-21 report,
  answering the question the unmeasured resolver work had left open.
  `papio bench --cohort <path>` never talks to the daemon; a manual live
  mode is deferred and does not block this release.
- **ILLiad requests now poll their own status, and a stuck poll can be
  recovered.** Migration 0024 adds poll-health bookkeeping
  (`provider_status_raw`, display status, `last_successful_poll_at`,
  consecutive-failure count, error class) to `delivery_requests`. The poll
  executor's state map is fail-closed: any successful nonterminal read
  resets the failure count and settles `pending`; `Delivered to Web`
  becomes `fulfilled`; `Cancelled by Customer`/`Cancelled by ILL Staff`
  become `cancelled`/`declined`; an unmapped custom status — ILLiad's
  statuses are institution-customizable, so there is no exhaustive enum —
  stays `pending` with the raw string persisted; and `Request Finished`
  classifies from prior recorded observations, never a guess. **A failed
  poll never becomes `unknown_outcome`.** Transient, credential, and schema
  failures leave the request's recorded state untouched and degrade
  integration health instead: three consecutive failures mark a row
  degraded, and 24 hours with no successful poll raises an advisory that
  *papio* cannot *observe* the request — never that it failed.
  `unknown_outcome` is reserved for genuine provider-side uncertainty after
  successful communication and exhausted reconciliation (a 404 following a
  prior successful lookup, reconciled first against ILLiad's `UserRequests`
  and papio's own idempotency reference). Every poll persists its state as
  a compare-and-swap on the row's originally-read state and next-check
  time, in the same transaction as its event insert, so two pollers racing
  the same row cannot double-append a `fulfilled` event or regress a
  settled state — a lost race is a plain no-op. `papio doctor` reports
  observation health per profile (`poll_health`) without ever claiming a
  request failed, and its remedy for a contract-drift park now names the
  actual recovery command: **`papio delivery resume <request-id>`** clears
  a live request's poll-failure bookkeeping (a terminal row is refused with
  a structured reason) so the next scheduled poll is no longer a no-op —
  pair it with `papio jobs retry <job-id>` to poll immediately rather than
  waiting for the schedule.
- **A fulfilled ILLiad request can retrieve its document through the
  ordinary browser handoff (ADR-0017 Decision 6).**
  `document_delivery.patron_web_base_url` is new, `illiad`-only
  configuration for the patron-facing ILLiadWeb portal — distinct from
  `base_url`'s Web Platform API, and never derived from it, because
  shared-server deployments and customized directories make that guess
  unreliable. When configured, it compiles a new gate-profile capability,
  `fulfillment_channel = "patron_web"`, orthogonal to submission
  auto-capability: a profile can auto-submit requests and still have no
  fulfillment channel, in which case every fulfilled request still lands on
  the existing manual reconciliation action rather than claiming automation
  *papio* cannot back. On `fulfilled`, *papio* builds the form-75 "View
  PDF" URL (`patron_web_base_url` plus `?Action=10&Form=75&Value=<provider
  transaction reference>`) and carries it through the same
  `openurl_handoff` human-action machinery every other browser-driven
  candidate already uses — delegated drives it, assisted opens it,
  conservative only records that retrieval was discovered — so a
  downloaded file lands in the same adoption, quarantine, structural, and
  identity pipeline as any other browser capture. A custom, non-inline-PDF
  landing page is not heuristically scanned for a download link; that stays
  a recorded human action pending a fixture-backed adapter. **`fulfilled`
  still means "the provider supplied the document," never that *papio*
  holds trusted bytes** — only a file that clears validation moves the job
  to `ready`. Live acceptance needs a real ILLiad deployment and is not
  reproducible in CI; everything else ships now.
- **Triage snapshot schema 3 adds `attention`, routing detail, and delivery
  reconciliation.** Every item now carries a closed `attention` field —
  `working` (papio is proceeding on its own), `required` (a human decision
  is needed), or `advisory` (informational, e.g. a retraction notice) —
  replacing any UI inference from `action_kind` or `requires_auth`.
  Human-action items add `route_class` (a fixed enum formalizing the
  existing action-kind vocabulary, now including `document_delivery`) and a
  tri-state `auth_requirement` (`"true"`/`"false"`/`"unknown"`, wired
  separately from the existing boolean `requires_auth`, which keeps its
  narrow execution-gate meaning unchanged). `blocked_by`'s vocabulary grows
  to a v3 superset — adding `login`, `terms`, `delivery_outcome`,
  `identity_review`, `unknown` — without reinterpreting any v2 value;
  schema 2 emission stays byte-identical. A `document_delivery`
  human-action item carries a `delivery` sub-object (provider, provider
  reference, state) and three new operations —
  `open_request_history`, `confirm_request_exists`, and
  `confirm_request_absent` — wired end-to-end through a new
  `delivery_reconcile_request`/`delivery_reconcile_result` message pair. A
  routine store error while assembling an item's delivery detail now
  degrades that one field to absent with a logged line, rather than tearing
  down the whole native-messaging session over a database hiccup. Go's
  `confirm_request_absent` validation now matches the TypeScript and
  JSON-schema sides: an explicit empty `provider_reference` is rejected,
  not silently accepted. Store schema version 24.

- **Document delivery and ILL become a durable, configured route (ADR-0017).**
  A dead end is fixed: a work only obtainable through interlibrary loan used
  to make *papio* observe that fact and stop. A new `delivery_requests`
  ledger — idempotency-keyed on institution profile + canonical work
  identity + provider + request type, store migration 0021 — now records
  the request instead. `[browser.document_delivery]` and
  `[browser.resolvers.<name>.document_delivery]` declare a `kind`
  (`openurl` | `libkey` | `illiad` | `custom` — `oclc` and `rapido` are
  named as intended providers but rejected until their adapters ship) plus
  the gate-profile facts *papio* cannot discover on its own (`legal_basis`,
  `patron_attestation`, `patron_fee_policy`, `monthly_request_cap`,
  `submit_policy`). Those declarations compile at config time into one
  institution-profile × provider × request-class gate profile —
  `auto_capable` | `prefill_only` | `invalid`, with a closed 13-code
  blocker vocabulary and recorded evidence — and a per-request seven-point
  gate (delegated access mode, `auto_if_unconditional`, a supported and
  configured request class, complete metadata, zero required human steps,
  the zero-fee policy, and cap headroom) decides submit vs. prefill for
  every request the profile is asked to place; any condition false or
  *unknown* routes to prefill, never a guess. `illiad` is v1's only
  auto-capable provider: an institution-issued API key that is never sent
  to, stored in, or observable from the extension or the browser wire,
  zero-patron-fee digital journal articles only, and one recorded live
  acceptance — a supervised submit-and-reconcile against the real
  deployment — before the compile can ever read `auto_capable`. Profiles
  declaring Australian `legal_basis = "copyright_act_s49"` compile
  `prefill_only` permanently, by statute rather than caution: the ceiling
  there is automatic prefill followed by one human declaration, and *papio*
  must never tick, script, or represent that declaration itself. New
  `papio delivery get|submit|cancel|history|confirm-exists|confirm-absent`
  commands (MCP-exposed automatically, per the existing command-derived
  facade); `jobs.get_v3` adds a delivery section additively, keeping
  `jobs.get_v2` ratified and byte-identical. A pending request parks its
  job in the existing `retry_wait` state under a new
  `document_delivery_pending` reason — it is not an open action, so
  `actions list` never misrepresents a self-driving poll as one — and only
  once four rounds of deterministic reconciliation (provider reference,
  *papio*'s own idempotency key, a patron request-list search, one delayed
  re-check) are exhausted does it become an `awaiting_human` action of the
  new `document_delivery` kind, offering `open_request_history` /
  `confirm_request_exists` / `confirm_request_absent` and **never**
  resubmission: *papio* must not submit a second request while an earlier
  one's outcome is unknown. `papio init` prints the compiled gate verdict
  before saving (`AUTO-CAPABLE` with its evidence, or `PREFILL ONLY` with
  the specific blocker), and `papio doctor` keeps `DECLARED` configuration
  and `PASS`/`OBSERVED` verified facts strictly separate — it never creates
  a probe request and never prints `PASS` for a policy it merely read from
  config.

- **The daemon side of on-page bulk acquisition ships:
  `page_bulk_acquire_v1` (ADR-0019).** A newly negotiated feature carries
  two strict request/reply families — `page_bulk_status_request` /
  `page_bulk_status_result` and `page_bulk_submit_request` /
  `page_bulk_submit_result` — as thin transport adapters over the existing
  ownership/holdings lookup and `internal/batch` services, adding no
  browser-specific acquisition policy: a page-bulk job enters the same
  waterfall as any CLI- or MCP-submitted job, including LibKey-routed
  institutional handoff where configured. Status resolves up to 200 scanned
  identifiers through *papio*'s own canonicalizers and existing
  live-job/terminal lookups into a closed vocabulary (`eligible`,
  `owned_with_pdf`, `owned_missing_pdf`, `queued`, `previously_unavailable`,
  `ownership_incomplete`, `invalid`); a complete holdings lookup finding
  nothing is `eligible`, and a failed or partial one is
  `ownership_incomplete`, never a negative ownership fact. Submit creates
  one ordinary batch of up to 50 canonical keys through the same
  app-service entry `acquire.submit_v3` uses, with the daemon — never the
  extension — assigning `consumer = "browser-page"`; only a fresh, explicit
  `owned_with_pdf` claim is suppressed server-side, matching the extension's
  own row states. Every scan additionally writes one local, URL-free
  `page_bulk_runs` measurement row (store migration 0022; origin only, no
  path, query, or title — nothing leaves the machine) feeding the feature's
  own bulk-leverage metric.

- **`papio export` writes normalized CSL-JSON, RIS, and BibTeX.** Four
  scopes: `export job <id>...` (argument order, any state — citation
  metadata stays useful when retrieval failed), `export batch <batch-id>`
  (manifest order, including skipped and unavailable works — citation
  export is not an acquisition-success report), `export watch <watch-id>`
  (pending digest entries), and `export ledger` (one record per canonical
  work; ready acquisitions by default, `--state any`, `--since`,
  `--consumer`). Duplicates collapse by canonical identity — DOI, then
  PMID, then arXiv id, then normalized title/first-author/year — unless
  `--include-duplicates`; BibTeX keys are stable
  (`firstauthor-year-titleword-<identity-hash>`), so a small title
  correction never renames an entry. The projections are normalized, never
  a round-trip: only known values are exported, author names stay literal
  rather than being split into family/given by guesswork, and type is
  identifier-based only (ISBN without DOI or container = book). `--format`
  wins, the `-o` extension infers (`.json`/`.ris`/`.bib`), CSL-JSON is the
  default; with the global `--json`, `-o` is required and stdout carries a
  receipt (format, record count, duplicates collapsed, SHA-256, path). The
  MCP facade exposes the new command automatically. Projected values pass
  through the repo's one terminal-control filter, so a third-party title
  cannot inject escape sequences into the no-`-o` stdout path; PMID rides
  CSL 1.0.2's standard `PMID` variable (only arXiv uses the `custom`
  object). `export watch --include-consumed` from the consult design is
  deferred: consumed digest entries never leave the daemon today, so
  including them needs a `watch.digest` v2 method first.

- **OpenAIRE joins the open-access resolver waterfall.** The Graph API's
  research-product lookup by DOI (else PMID) contributes up to three
  candidates per work from the record's licensed or explicitly OPEN
  instances — European institutional repositories being the marginal
  coverage. OpenAIRE marks no URL as the file itself, so every candidate is
  a landing observation and the existing landing-expansion step derives the
  PDF when the page advertises one; a record that is not OPEN, an instance
  without a license or OPEN access right, and an echoed identifier naming a
  different work are all skipped. Keyless access is paced at OpenAIRE's
  public 60 requests/hour (`rate_per_sec = 0.016`); a personal-token
  `api_key` raises the ceiling. OpenAIRE Graph metadata is CC-BY and is
  acknowledged in candidate provenance and the privacy table. Verified
  live: a PLoS DOI returns its licensed publisher instances, query strings
  redacted in evidence.

- **The version hop follows Crossref's typed relations before any fuzzy
  search.** When a DOI's own candidates are exhausted, papio now asks
  Crossref for the registrant-asserted `has-preprint` / `is-preprint-of` /
  `has-version` / `is-version-of` edges (depth one, capped at three, through
  the existing `crossref_metadata` source and its budget) and resolves those
  sibling DOIs through the enabled open-access resolvers — a typed edge was
  asserted by the registrant, so it outranks the OpenAlex title-match hop,
  which now runs only when no typed edge produced a candidate. Typed sibling
  candidates are open-access only (routing a *different* DOI to an
  institutional resolver would sign the operator into the wrong work's
  paywall), are identity-checked against the sibling DOI they were resolved
  from, and still cross PDF semantic-identity validation against the
  requested work before acceptance. A rate-limited relation lookup parks the
  job like any other temporary source failure; it never settles it
  unavailable. Verified live: a paywalled Nature article resolves its typed
  medRxiv preprint in one hop.
  The extension derives each job's institution origin from the offer's
  provider hosts when LibKey fronts the route (the offer origin is
  `libkey.io`, which the daemon's resolver origins never contain), so
  first-sign-in queue release, `auth_returned` session evidence, and
  Alma/Primo resolver auto-routing keep working under link mode; and the
  default profile refuses `link` mode without an OpenURL base — the handoff
  gates key on the base, so a base-less LibKey config would sit validated
  but unreachable.

- **A browser adoption whose session was freshly evidenced as authenticated now
  publishes its entitlement (ADR-0018).** `acquisition-bundle/2`'s
  `operator_browser_session` acquisition mode has had no producer since it was
  reserved; the delivery context added in 0.17.0 supplies one. An exported
  bundle now carries `candidate.entitlement` when the accepted candidate's own
  row records `session_evidence = fresh_auth` on an institutional route, naming
  the page origin the extension observed at adoption. The claim is precisely
  that the bytes arrived through a browser session evidenced as authenticated at
  that origin — not that a login happened during the acquisition, and not that
  the work was paywalled. The gate is the recorded evidence, never the access
  basis: `warm` means the evidence aged past its TTL with nothing confirming the
  session since, a resolver-produced `institutional` candidate has no browser
  session behind it at all, and an adoption that carried no context (a
  directory-scan adoption always does) has an empty binding. All three stay
  entitlement-less, because a false positive here invents rights evidence while
  a false negative costs only a field (ADR-0007). Nothing is backfilled. An
  `oa`-route adoption also gains a route it previously lacked, still as
  `open_access` with no session claim. The bundle shape, schema, and IPC surface
  are unchanged — the mode was already in the v2 enum and the route is already a
  bare origin — so no consumer change is required.

- **Institutional handoffs can route through LibKey's institution link
  (ADR-0016, keyless `link` mode).** A resolver profile — the default
  `[browser]` institution or any `[browser.resolvers.*]` entry — gains
  `libkey_mode` and `libkey_library_id`; with `libkey_mode = "link"`, a
  handoff for a work with a DOI or PMID opens the documented
  `libkey.io/libraries/<id>/<doi-or-pmid>` institution link instead of the
  bare OpenURL resolver, which routes through the library's LibKey
  configuration (direct PDF, resolver, or request path) with no credential
  involved. LibKey augments institutional routing, never replaces it: works
  without either identifier, profiles without a library id, and every
  failure fall through to the plain OpenURL route, and both the CLI's
  `actions open` and the extension offer take the same route from the same
  builder. The offer keeps the resolver host beside `libkey.io` on its host
  list, so the extension stays sighted across the redirect. Config is
  fail-closed per this repo's strict-mode rules: `libkey_mode = "api"` is
  rejected as unimplemented, `link` without a positive `libkey_library_id`
  is rejected, and a library id without `link` mode is rejected rather than
  left silently dead. `papio init` collects the library id — a bare number
  or a pasted BrowZine/LibKey.io URL — interactively after the resolver
  step, or via `--libkey-library-id`; an explicit blank clears the pair.

- **Semantic Scholar now contributes open-access acquisition candidates, not
  just search results.** The provider papio already queried for discovery joins
  the resolver waterfall between OpenAlex and CORE: an exact-identifier lookup
  (DOI, arXiv id, or PMID — never a title search, because a weak title match
  must not become an automatic candidate) that emits one candidate only when
  the record carries a usable `openAccessPdf` URL. `isOpenAccess` without a
  PDF location is treated as metadata, an echoed identifier that names a
  different work rejects the record, the stated license is carried through
  (`unknown` otherwise — a reachable PDF is not a redistribution right), and
  the version stays `unknown` because the API supplies no typed version
  evidence. `[sources.semanticscholar].enabled` gains the documented meaning
  "may also contribute OA acquisition candidates" (default on, paced at the
  keyless 1 req/s public limit; discovery backend selection remains separate
  in `[discovery]`). The privacy table now lists `api.semanticscholar.org`
  under resolving, and 429s defer the source by the server's `Retry-After`
  like every other resolver.

- **`papio actions open` takes a row selector, so a consumer can open the
  handoff it chose.** The command opened the head of the queue and nothing else,
  which is the wrong shape for a caller that ranks its own routes — one
  consumer ranking 164 queued institutional routes by script reach could only
  ever reach the newest. `--job <job-id>` and `--action <id>` open exactly that
  row, and a job holding several open actions is refused with their ids rather
  than resolved by picking one. A selector naming no open action is a clean
  error naming where to look; it does not claim to know whether the row was
  resolved or never existed, because the open-action list it reads cannot tell
  those apart. What it never does is fall back to the head of the queue, which
  would open an unrelated institution's handoff and report success.
- **Acquisitions can carry a consumer name, and listings can be partitioned by
  it.** `work_requests.requester` records the transport principal (`cli`,
  `mcp`, `unknown`), which answers "how did this arrive" and never "who asked
  for it", so a daemon shared between people produced one undifferentiated
  total. `papio acquire --consumer <name>` (also with `--batch`) records the
  submitter through the new `acquire.submit_v3`; `papio jobs list --consumer`,
  `papio actions list --consumer`, and the new `jobs.list_v3` /
  `actions.list_v3` / `jobs.get_v2` methods return and filter it. Attribution is
  nullable with no backfill and no default: a request that named no consumer has
  none, the key is absent rather than empty, and a submission matching an
  in-flight job never rewrites the attribution that job was queued with. It
  binds to the job rather than the work request, so resubmitting a request id
  whose earlier jobs are terminal attributes the new acquisition to whoever
  resubmitted it. A `--consumer` filter against a daemon that predates the
  column is refused rather than answered with every consumer's rows. It is a
  caller's own accounting label: *papio* authenticates nobody, so it is not an
  identity and must never be read as a rights input (ADR-0014). Schema
  version 20. On landing, `acquire.submit_v3` was ratified alongside the
  readers (ADR-0014 Decision 4 addendum): its consumer asked, which was the
  condition Decision 4 set.
- **`papio artifacts validation <job-id>` returns the complete validation
  report.** Every stage's evidence was computed and then discarded: only the
  projections that fit the artifact row survived (page count, text characters,
  OCR use, encryption, active content, identity result), so the payload gate's
  reason, the structural rejection reason, and the identity and capability
  evidence were unrecoverable, and a consumer making a rights or quality call
  had to re-derive them from fragments. Each validation now persists a versioned
  `validation-report/1` document keyed to the job and candidate — for rejected
  candidates too, which is the set "why not this one?" is asked about. The
  extracted text excerpt is deliberately not persisted, and every reason and
  evidence line is bounded and stripped of control characters: several are a
  third-party parser's stderr produced while reading a publisher-supplied file.
  `artifacts.get` is unchanged: it returns the shared, content-addressed artifact
  row, and ADR-0007 forbids projecting one job's identity decision through it.
  Jobs validated before this release list no reports; that is an absence, not an
  empty verdict. This reverses one clause of ADR-0007, which had withdrawn
  structured validation evidence from the external surface; ADR-0014 records why
  the reasoning behind that withdrawal — no per-job identity through a shared
  artifact — is satisfied rather than evaded by keying the evidence to the job
  and candidate. The accepted candidate's bundle remains the only success
  provenance document.
- **`papio actions open` now leaves an audit trail.** Each opened handoff records
  a `handoff.opened` event carrying the owning consumer, the transport principal,
  and the batch size. ADR-0009 does not ratify autonomous drain — a background
  consumer must not open human work on its own — and ADR-0014 Decision 6 declines
  to enforce that with a gate, because a script passes any flag a human passes and
  an agent driving the CLI is meant to get exactly what a human gets. That trade
  is only honest if the boundary is observable: "consumer X opened N human actions
  in M minutes" is now answerable from the event stream, and a drain reads as an
  anomaly rather than as normal traffic. The event names the handoff's owner,
  recorded at submit, rather than an unverifiable label supplied by the caller
  under audit.
- **An open human action that has waited too long is reported stale.** A
  handoff queued weeks ago sat in the queue indistinguishable from one queued
  this morning. `papio actions list` now reports `stale` and `age_seconds` per
  row (a trailing marker in the text listing) against the new
  `actions.stale_after_seconds` config key, default 7 days. Nothing expires as
  a result: *papio* does not cancel, sweep, or close a handoff on a timer,
  because abandoning an acquisition is a person's decision. The threshold is
  separate from `browser.action_expiry_seconds`, which is a 30-minute reminder
  cadence and would have called a handoff queued over lunch abandoned.

### Changed

- **A paper now has to print the requested title, not merely contain its
  vocabulary.** Title identity was unordered token membership — stopwords and
  every word under five runes discarded, 60% of the remainder enough to pass —
  and the corpus harness showed all 52 of its wrong accepts came through that one
  gate, in three families. "How to do a meta-analysis" reduces to the single
  token `analysis`, which matched eight unrelated papers by an author of the same
  surname. "Final Report - Volume 3, Impacts" discards the `3` for being one rune
  long, so eighteen pairs of a seventeen-volume government report matched each
  other on the words they share. And a set cannot tell "Core reporting practices
  in structural equation modeling" from "Update to core reporting practices in
  structural equation modeling", which matched 5/5.

  A pass now also requires the title to be printed as a delimited unit in the
  front matter: the whole requested title, every short word and stopword
  included, beginning where a line begins or a label of at most three words ends
  ("Original Article:", "1."), ending where the line ends or punctuation does,
  and spanning consecutive lines where it wraps. A hyphen broken across the line
  break rejoins first, and a running head glued on with a run of spaces is split
  off, so the shapes a text layer actually produces still match.

  `titleThreshold` therefore means something different: it is the floor below
  which a candidate is rejected, not a level at which a partial title match
  passes. A 3/5 match with a matching author and year is now review where it used
  to be a pass — that combination was the single largest wrong-accept family.
  Measured over 632 documents and 398,786 mismatched pairs: wrong accepts 52 → 2,
  correct passes 586 → 565. The title is compared as one character run rather
  than word by word, because a text layer decides where words begin and decides
  badly — "PsychologicalSafety and LearningBehavior in WorkTeams" is one real
  paper's printed title, and a footnote digit welded to the last word is another
  — and a digit-only run inside a wrap is stepped over, because a submitted
  manuscript numbers every line. Only a colon, full stop, or rule ends the short
  label the start edge allows: a citing sentence reaches for a quote, and
  accepting one let `We cite "…" for guidance` pass as a printed title.

  The 21 documents that moved to review are the ones token overlap can never
  separate, and every one is accounted for in `dev/identity-corpus.md`: eight
  numbered-series covers that print the catalogue's words in another order, five
  text layers shredded by column interleaving, three catalogue records that
  disagree with the printed title, two that concatenate a teaser or drop a
  publisher prefix, one mojibake dash, and two differing subtitles. papio parking
  a correct paper costs a moment where filing the wrong one costs a library its
  trust.
- **`make identity-corpus` no longer grades the identity rules on papio's own
  output.** 47 of the 679 documents it scored resolved, through Zotero
  linked-file attachments, into papio's own artifact, bundle, and staging
  directories — 9 of them artifacts papio itself delivered and had already
  scored. Every one of the 47 passed, which is what recycled output looks like,
  and independence from it was the whole reason the corpus reads Zotero rather
  than papio's store. They are now excluded by name and counted in the report's
  new skip summary, alongside the long documents the 1 MiB extraction cap drops:
  24 of 54 books against 11 of 637 journal articles, so the corpus is the
  library minus most of its books and a book-shaped rule change cannot be judged
  on it. The report says so rather than leaving the reader to assume otherwise.
  A relative linked-file path is resolved against Zotero's own
  `extensions.zotero.baseAttachmentPath` instead of a directory that was never
  the base, and says so when the pref is unset rather than reporting a missing
  file. The identifier-placement histogram asks the real matcher where an
  identifier is printed, so the six documents whose DOI is stored as an EZproxy
  URL are now reported as unusable metadata instead of as a window that needs
  retuning. The library snapshot prefers an atomic `VACUUM INTO`, falls back to
  a byte copy while Zotero holds the database open, retries a copy the WAL grew
  under, and names which path it took. The extracted-text cache — the front
  matter of every paper in the library — now defaults to a per-user cache
  directory rather than shared `/tmp`, refuses a directory it does not own,
  writes each entry atomically, and is documented as retained until deleted.
  Skip lines name an attachment key and a class, never a path or a filename.
- **The privacy policy now says what the daemon actually sends.** It claimed
  "*papio* collects no personal data. Nothing is sent to OrgMentem or any third
  party" while scoping itself to the daemon *and* the extension. That is true of
  the extension, which has no backend, and false of the daemon: finding a paper
  means asking the services that index papers, so every acquisition sends the
  DOI, PMID, arXiv id or title to Unpaywall, Crossref, Europe PMC, arXiv and now
  doi.org — and, where those services require it, the `email` from the config.
  The daily retraction sweep additionally sends the DOIs of papers already in
  the library. `docs/privacy.md` now carries a destination-by-destination table
  naming what is sent, when, and whether it is on by default; the
  `email` row in the config reference names its recipients; and the "no server,
  no account, no telemetry, no analytics" claim is kept, because it is true, but
  stated separately from the claim it was hiding. The once-a-day GitHub release
  check is disclosed rather than glossed: it sends nothing, and
  `updates.check = false` turns it off.

### Fixed

- **One blocked syscall no longer freezes every daemon RPC.** macOS
  Files-and-Folders consent (TCC) can leave `open(2)` on the
  download-adoption root blocked in-kernel for a background daemon — and the
  consent resets every time the binary is rebuilt or upgraded. Adoption
  scanning previously ran that syscall while holding the browser-bridge lock,
  so a single consent wall hung every RPC (`papio daemon status` included)
  until a force-kill. Scans now run through a bounded reader with a
  bridge-wide latch: the daemon stays fully responsive, offers and sessions
  keep flowing, and scanning resumes on its own the moment the grant lands.
  `papio doctor` gains an adoption-root check — bounded the same way, so
  doctor itself can never hang on the wall — that names the folder and the
  System Settings remediation instead of leaving a silent stall.
- **An open-access paper whose cached PDF link has rotted is now recovered from
  the publisher's own landing page instead of being sent to your institution.**
  Aggregators cache a direct PDF URL, and those URLs expire: for one Frontiers
  paper both Unpaywall and OpenAlex returned the same Azure blob link whose
  signature ran out on 2021-02-16, so both candidates 403'd and the job fell
  through to an institutional sign-in — for a paper anyone can download without
  one. Both candidate rows already carried the publisher landing page, and that
  page advertises the working file in `citation_pdf_url`; papio had the recovery
  route in hand and never looked at it. When every open-access candidate fails
  permanently, the landing page is now read (once per pass, before the sibling
  hop and before any retry park, so a slow unrelated resolver cannot delay a
  deterministic recovery) and the file it advertises becomes a derived
  candidate. The derivation inherits its parent's source, access basis and
  policy — it is the same observation reached a second way, not a new source, so
  no source needs enabling and no configuration changes. The reader sends no
  caller credentials, because carrying a signed candidate URL's headers to a
  different origin would leak them.
- **A browser handoff that cannot succeed now stops asking.** The only brake on
  automatic offers was the seven-day `QuiesceAfter` fence, so a handoff that
  could never complete kept costing a tab: one was offered 38 times across three
  days with no outcome of any kind, pinning both browser drive slots and
  starving every other job of them. Offers are now also bounded by evidence —
  three fruitless drive epochs and papio stops offering unprompted, recording a
  single `browser.handoff_quiesced` audit event. Reconnects are not evidence: a
  service-worker restart re-acknowledging the same live drive collapses into the
  epoch already running, so recovering transport is never mistaken for failure.
  Both fences apply, the action stays open and visible, and an explicit
  `papio actions open` always gets its drive.
- **A parked job no longer reports a cause it does not know.** Every retry park
  was labelled `candidate_temporarily_unavailable`, including passes where the
  candidates had failed permanently and something else entirely — a temporary
  resolver, a closed source gate — was holding the job. The transition now
  reports what was actually observed, counting retryable candidates, temporary
  resolvers and closed source gates separately. Scheduling is unchanged; only
  the explanation is. The former reason string still classifies, because events
  already written carry it.
- **An open-access page behind a public anti-bot gate keeps its browser route
  after the first pass.** papio detects that such a candidate is worth one
  ordinary-browser attempt, but the finding lived in a variable scoped to the
  pass that made it. Once the candidate was marked failed, later passes found an
  empty queue, never re-evaluated it, and routed the job to an institutional
  sign-in instead — papio noticed the right answer and then forgot it. The
  finding now survives as a job event recording only the candidate's key, never
  the URL, and the live URL is recovered from the current pass's own candidates.
- **A DOI that does not exist no longer parks the job on an institutional
  sign-in.** A well-formed but unregistered DOI — one transposed digit in
  `10.1016/j.cedpsych.2020.101816` for `…101860` was the reported case —
  survived every upstream check, because Crossref, OpenAlex, EuropePMC and
  Unpaywall all report "I have no record of this" and "this work exists but I
  hold no open copy" through the same empty result. The job therefore reached
  the institutional handoff, the link resolver had nothing to match, and the
  user landed on doi.org's "DOI NOT FOUND" page. That action can never be
  completed, so it was re-offered on every session-live tick — roughly sixty
  tabs over three days — and escalated seven reminders. Before offering a
  handoff for a work whose only fetchable identifier is a DOI, *papio* now
  asks the DOI system itself whether that handle is registered; an
  unregistered one settles `unavailable` with the new terminal reason
  `doi_not_registered`, which names the typo and says plainly that signing in
  will not help. The probe is skipped when a PMID, arXiv id or OpenAlex id is
  also present (each is its own route) and fails open when the registry is
  unreachable, so an outage cannot terminate fetchable work. Like
  `no_identifier`, the new reason is exempt from the zotio backfill cool-down,
  because correcting the DOI makes the item fetchable immediately.
  The same gate now runs in the maintenance repair pass, so a job that was
  *already* parked when this shipped is reclaimed rather than nagging forever;
  the existing "contradicted park" rule could never reach it, because that one
  waits on a `browser.no_entitlement_requeue` event and a dead DOI never gets
  as far as the institutional resolver. Registry answers are memoized so the
  once-a-minute sweep does not become a request per parked job per tick.
  Two hardening notes on the probe itself, both found in review: the request
  path is built by concatenation rather than `path.Join`, because Join Cleans —
  which collapsed the repeated slash that makes `10.48612//monograph-2025-2` a
  different registered work from `10.48612/monograph-2025-2`, and let a `..`
  segment (`doiCoreRE` admits any non-whitespace suffix) escape `/api/handles/`
  into doi.org's own resolver root; dot segments are now rejected outright. And
  a missing HTTP client is an error rather than a plain `http.Client`, which
  would have silently dropped the SSRF guard, redirect cap and body bound the
  daemon's shared metadata client provides.
- **An open human action stops nagging after a week.** The reminder backoff
  capped the *interval* at 24h but never the *count*, and the browser bridge
  re-offered every open handoff whenever a session went live, so one handoff
  nobody could complete produced a tab per session and a notification per day
  indefinitely — sixty tabs and seven reminders over three days in the reported
  case. Some of those causes are now caught at the source, but most never will
  be: a title the library does not hold, a provider that changed its login, a
  job the user has decided to ignore. Past `job.QuiesceAfter` (seven days) an
  action goes quiet — no automatic offer, including on the institutional
  re-offer sweep that a fresh sign-in triggers, and no further reminder.
  This is deliberately **not** expiry: the action stays open, stays listed, and
  `papio actions open` still drives it, because an explicit command is user
  intent in a way a session-live tick is not. `papio doctor` gains a
  `quiesced_actions` check so a quiet queue does not become an invisible one —
  out of band on purpose, since the IPC layer decodes strictly and widening an
  existing result shape would make an older CLI reject every response from a
  newer daemon.
- **A paper whose catalogue record names one author no longer goes to review
  over a superscript.** Identity accepts an identifier printed anywhere in the
  document, but only once the byline agrees on an author — and where a
  superscript affiliation marker is glued to the surname ("Keith D. Ciani1∗")
  that agreement needs *two* marked surnames, because the two-character
  tolerance cannot tell a marker from a different name. A record naming a
  single author can never show two, so the rule was unsatisfiable: a Wiley PDF
  matching 7/7 title tokens, the year, and printing the requested DOI verbatim
  below its abstract — past the front-matter DOI window, which stays narrow so
  a reference-list DOI is never read as the document's own — was parked for
  human verification. A *numbered* marker on the requested surname now carries
  the author check by itself where the record names one author, because no
  surname ends in a digit: "Ciani1" is Ciani, whereas "Clarke" is Clark plus a
  letter or a different person, and nothing in the text says which. The
  identifier must also be printed on the document's own page one. Both bounds
  answer the same document — a comment, reply, or erratum on the requested paper
  carries its title and prints its DOI, so on its own the identifier does not
  establish that a file is the paper rather than a note about it. A lettered
  near-miss surname still parks, as does a numbered one whose only identifier
  match is a citation past page one.
- **A page that announces itself as a correction can no longer be filed as the
  paper it corrects.** An identifier printed in the first kilobyte of page one
  passed identity outright, with no title or author check — correct for the 40%
  of real papers that print their own DOI in the masthead, wrong for an erratum,
  corrigendum, retraction notice, or comment article, which is a different work
  that reprints the requested paper's DOI at the top of its own first page. A
  1508-byte correction notice was accepted as the paper. A page-one line
  beginning with one of those markers now caps the verdict at review rather than
  rejecting it, because the operator may have asked for the erratum and a park
  can be undone where a discard cannot, and the marker is named in the evidence
  on every verdict a human sees. Detection reads the 2 KiB byline window, not
  the kilobyte the DOI rule reads, and tests each line in the segments pdftotext
  leaves when it glues a running header or a page number onto the line that
  carries the marker — `"J Sensor Syst 2025;12:1  Erratum: …"` escaped a
  line-prefix test entirely, as did a leading byte-order mark. Runs of two or
  more spaces are what segment a line, so a Bonferroni correction mid-sentence
  is still a real paper, and the list carries "retraction of" rather than
  "retraction" and omits "response to". A pointer to a correction published
  elsewhere — Springer prints "Erratum to this chapter is available at …" on the
  corrected chapter itself — is excluded by name, so the corrected work does not
  park itself.
- **A PDF that opens with a blank cover leaf is read instead of half-read.** The
  windows every identity rule reads were cut at the first form feed, so a
  leading one handed all three of them an empty page one: the DOI rule saw no
  DOI, the title gate saw no byline, and a paper that matched on all four axes
  parked with "title tokens matched only outside the front matter". Leading
  whitespace, form feeds, and byte-order marks are now trimmed before the cut.
  Two documents in a 632-document real library were parked by this and now pass.

- **`papio jobs list` and `papio actions list` state truncation on the human
  surface too.** `--json` has carried a proven `truncated` since 0.16.0, while
  the text listing stopped at the limit and looked complete. Both now say how
  many rows they showed and that more exist behind the page. The `--json`
  envelope is unchanged at exactly two keys.
- **`papio acquire --batch --request-id` no longer points at a mechanism that
  does not exist.** The flag was refused with "put per-work values in JSONL",
  but the JSONL work decoder is strict and has no `request_id` field, so
  following that advice failed with `unknown field "request_id"` and left no
  working option. One flag cannot key many works: batch works get deterministic
  per-work request ids derived from the batch identity and the work identity, so
  resubmitting the same works on the same day reproduces the same keys — the
  batch identity mixes in the calendar date. The refusal now names that instead,
  and calls `request_id` what ADR-0010 calls it: a live-job convergence key, not
  an idempotency key.

## [0.18.0] - 2026-08-06

### Added

- **`page_capture_terms_v1` capability.** The `terms` capture scenario was
  appended to the *existing* `page_capture` scenario enum, so a daemon that
  predates it rejects the whole frame during validation — and a browser
  protocol decode failure tears down the entire native-messaging session, not
  just that request. With two papio binaries on one machine a routine
  occurrence, the daemon now advertises a dedicated capability and the
  extension withholds the option until it sees it. `terms` remains a valid
  value in all three validators; only the producer is gated.
- **`page_capture` carries an optional `request_id`.** A requested capture now
  echoes the `request_id` of the `page_capture_request` it answers, which is
  the only thing that ties the content frame to a specific request. An
  unsolicited capture — the developer capture panel's own button — omits it.
  This is a breaking wire change to an existing message type, landed under the
  pre-1.0 compatibility floor now stated in `AGENTS.md`: daemon, extension, and
  JSON schema move together and the extension must be rebuilt and reloaded
  alongside the daemon. Correlation needs an extension of 0.10.0 or newer;
  below that `papio adapter capture` now refuses up front, naming the connected
  and required versions, rather than capturing the page and then reporting a
  failure it did not have. Only adapter capture is version-coupled, so the
  daemon's own extension floor does not move and every offer and handoff keeps
  working with an older extension.
- **The native host mirrors its diagnostics to `<DataDir>/native-host.log`.**
  Browsers forward a native-messaging host's stderr nowhere — not even into
  `chrome_debug.log` with logging enabled — so a host that rejected a frame and
  tore the session down left no trace, and the operator saw only a downstream
  `nav_failed` or a session that would not connect. Every diagnostic the host
  writes, including the exact reason it exited, is now appended to that file.
  Past 1 MiB it rotates to `native-host.log.1` rather than truncating, so a
  second host process — a service-worker reconnect, or Chrome and Firefox
  connected at once — cannot discard the trace a live sibling is still writing.

### Fixed

- **The quarantine preview re-verifies the file on every serve.** The digest
  was checked once and the result cached on the capability, so any later GET
  of the same URL re-opened the file and served it with no hash check at all —
  a different read from the one that was verified. A quarantined file is
  untrusted by definition and its path is known to the process that wrote it,
  so a swap between the operator's first look and a reload served bytes nobody
  had checked. Verification now runs per serve, from the same handle the
  response is written from, and a mismatch revokes the capability. The hash
  runs outside the server mutex: a PDF viewer issues a range request per chunk,
  and holding the lock across a full file read would serialise every preview in
  the process behind the largest document.
- **`papio activity --json`, and every other `--json` payload, can no longer
  carry a terminal escape.** Go's JSON encoder escapes only bytes below 0x20
  plus quote and backslash, so DEL and the entire C1 block reached the writer
  raw — and a UTF-8 terminal reads U+009B and U+009D as the CSI and OSC
  introducers, the same injection primitive as ESC without an ESC byte in the
  input. A provider-supplied download filename or a third-party bibliographic
  title could therefore inject escape sequences into the terminal of anyone
  piping or eyeballing the output, even though the human-readable rows were
  already stripped. Those code points are now emitted as `\uXXXX`, which every
  conformant JSON parser decodes back to the original value: `--json` stays
  byte-exact for tooling, which stripping would have broken.
- **A page capture can no longer be bound to the wrong request.** The daemon
  matched an incoming capture to a pending `papio adapter capture` on provider
  and scenario alone, so an unsolicited capture of the same pair on the same
  browser session could overwrite the pending request's path and return the
  wrong file to its caller, with no error surfaced. Correlation is now the
  echoed `request_id`, the same key the capture result already used, with no
  provider/scenario fallback.
- **A capture that arrives as its deadline expires is reported as a success.**
  The result channel and the timeout became ready together and Go's select
  chose between them pseudo-randomly, so a capture that had already been
  delivered and stored could still be reported as a timeout — leaving the file
  on disk with nothing pointing at it. The timeout path now re-checks the
  channel under the same lock a deliverer takes, so whichever side arrives
  first wins outright.
- **`papio batch report --markdown` strips terminal control bytes too.** The
  markdown flag renders through its own `describe` helper rather than the
  plain-text path that was hardened, so the same third-party manifest titles
  still reached the terminal raw through a sibling flag of a command already
  listed as covered.
- **Every command that prints third-party text strips terminal control
  bytes.** Bibliographic strings are stored with only `TrimSpace`, so a
  record whose title or DOI carries an OSC sequence could rewrite the
  operator's terminal. `papio search` is the most exposed — anyone able to
  register a work in a discovery backend controls those bytes, and any
  keyword search renders them — alongside `watch digest`, `inbox` (both the
  watch-hit and retraction rows), `status`, `jobs list`/`jobs get`, `batch
  report` and the activity feed. Note that a DOI is not self-sanitising:
  `doiCoreRE`'s `\S` excludes only the five ASCII whitespace bytes in RE2, so
  ESC, BEL, DEL and C1 all survive normalisation. `--json` output is
  deliberately unchanged and still carries exact bytes, since it is the
  authoritative machine-readable form.
- **A page capture that redirects is no longer reported as a failure.** The
  previous release matched a pending capture against the host of the URL it
  requested, but the extension reports the host the page actually *landed* on,
  so an ordinary `www` canonicalization, CDN swap or SSO round-trip left the
  capture uncorrelated and downgraded a stored, successful capture to
  `nav_failed` with no path back to the file. That guard is reverted and the
  episode recorded in the code, because the residual it was aimed at is much
  rarer than the failure it caused.
- **A concurrent review submission is no longer told the verdict was recorded
  before it was.** A second POST arriving while the first was still inside
  `ResolveReviewCAS` was answered with the recorded shell — buttons disabled,
  no retry path — even though nothing had committed yet. If that first
  resolution then failed, the review was silently lost. The handler now tracks
  a real pending/recorded state, answers a genuinely in-flight submission with
  retry guidance, and returns a failed resolution to a retryable state. The
  page decides whether to re-enable its buttons from the response content type
  rather than the status code, which had conflated the two conflicts, and it
  now also re-enables them when the request fails outright.
- **The failure-summary example job is chosen chronologically.** The most
  recently updated job in a group was selected by comparing RFC3339 timestamps
  byte-wise, and `store.Now()` omits the fractional part entirely when the
  nanoseconds are zero. Since `Z` sorts above `.`, a whole-second timestamp beat
  a genuinely later fractional one. Timestamps are parsed before comparison.
- **An out-of-enum candidate value errors instead of vanishing.** Migration
  0019 added CHECK constraints on `browser_route` and `session_evidence`, but
  `InsertCandidates` uses `INSERT OR IGNORE`, and SQLite treats a CHECK
  violation under OR IGNORE as "skip this row" — so a bad value would have
  dropped the candidate silently while the dedupe the clause exists for went on
  working. The enums are validated in Go before the insert. No writer sets
  either field today, so this closes a latent footgun rather than a live bug.
- **`page_host` and `origin_hint` validate identically in all three places.**
  The published schema accepted `page_host` values both parsers rejected
  (leading, trailing and doubled dots), and `origin_hint` disagreed three ways
  at once: Go accepted an uppercase host that the TypeScript URL round-trip
  rejected, while the schema alone rejected a single-label host both parsers
  allowed. One rule now applies everywhere: an https origin, with no userinfo,
  path, query or fragment, and a host that is already lowercase. Label count
  and host length are deliberately NOT constrained — a single-label intranet
  resolver such as `https://library` is a legitimate configuration, since
  `browser.openurl_base_url` requires only an https URL with a host, and the
  wire validator must never be stricter than a valid config can produce.
  Corpus fixtures pin each previously divergent value.
- **`papio adapter capture` reports a daemon upgrade instead of a raw RPC
  error.** Against an older daemon it surfaced the bare `unknown_method`
  failure rather than the actionable message every other versioned command
  renders. With two papio binaries on one machine documented as routine, that
  skew is an ordinary outcome.
- **A page capture can no longer be satisfied by an unrelated capture of a
  different host.** The pending-request match now includes the requested host
  alongside session, provider and scenario. A residual remains and is
  deliberately deferred: nothing on the wire distinguishes a solicited capture
  from one the operator triggers from the developer panel, so a same-host,
  same-provider, same-scenario capture can still bind. Closing that needs a
  negotiated `request_id` on `page_capture`, which is an existing message type
  and therefore cannot take a new field without capability negotiation.
- **A rate-limit gate set mid-flight can no longer be raced.** `budget.Acquire`
  checked the durable gate up front, then called `takeToken`, which may itself
  sleep for up to `MaxInlineWait` waiting on the in-memory bucket. `reserve`'s
  transaction never re-read `next_allowed_at` — it only ever *cleared* an
  already-expired gate — so a worker sleeping in that window committed its
  reservation and sent its request against a gate another worker's 429 had just
  persisted, defeating the guarantee that one 429 stops every caller of that
  source and credential. The gate is now re-read inside the same transaction
  that moves the counters, and a live gate defers the reservation instead.
- **The quarantined-PDF review shell refuses to be framed.** Its only controls
  apply an irreversible, CAS-guarded verdict, so any page holding the
  capability URL could have overlaid the shell and harvested a confirmation for
  a file the operator never looked at. The shell now sends
  `frame-ancestors 'none'` and an `X-Frame-Options: DENY` fallback; the
  extension only ever opens it as a top-level tab, so no legitimate embed is
  lost.
- **`download_id` must be at least 1 on the wire.** It is half of the
  `{job, download}` key the daemon correlates delivery provenance on, and all
  three validators accepted `0` — so two downloads reported as `0` for one job
  collided, letting a `delivery_context` for the first apply its access basis to
  the second, unrelated candidate. Chrome allocates download ids from 1, so no
  genuine client is affected; this closes the hole against a buggy or
  compromised one. Tightened in the Go validators, the TypeScript mirror, and
  the JSON schema together.
- **Session evidence with no origin no longer releases another institution's
  parked handoffs.** `origin_hint` is optional on `session_evidence`, and an
  absent one was read as "match any profile": the fallback scan took the first
  institutional handoff in list order and reoffered that institution's whole
  queue. A sign-in at one institution could therefore reopen tabs and drive
  OpenURL navigations for a second one whose session was never verified — the
  opposite of the isolation per-institution resolver profiles exist to provide,
  and looser than an *unresolvable* hint, which already failed closed. An
  unattributable frame now releases only the default profile. Single-institution
  setups leave the job's resolver profile empty, which *is* the default profile,
  so their behaviour is unchanged. An unattributable frame also may not retire
  the sticky reoffer pin: demoting a named institution's pin to the default
  profile made that institution's own next `auth_returned` a no-op, starving
  its queue for the rest of the session. Only an attributable hint retires a
  pin, and an unhinted frame arriving while a named pin is live now does
  nothing at all.
- **`browser.resolvers.default` is rejected instead of silently ignored.** The
  resolver-name rule accepted the literal name `default`, but `InstitutionFor`
  resolves that name to the implicit top-level institution before ever
  consulting the map — so the profile was unreachable. A job meant for that
  institution routed to the top-level OpenURL base, `ResolverNames()` returned
  a duplicated `default`, and with no top-level base configured
  `browser.default_resolver = "default"` failed with the self-contradictory
  message `not configured (configured profiles: default)`. The name is now
  refused at load with an error naming the reserved key, matching how
  `[sources.*]` already whitelists its own names. No existing config breaks:
  nothing in `papio init` or `config save` ever writes that key.
- **`papio activity` cannot be used to inject terminal escape sequences.** The
  summary sanitizer stripped only NUL, while the browser-reported download
  filename it interpolates is bounded by a regex that forbids just the path
  separators — so ESC and every other C0 byte reached the operator's terminal
  verbatim. A provider page, a spoofed `Content-Disposition`, or a compromised
  browser session could rewrite the terminal on the next `activity` run. Every
  C0 byte, DEL, and the C1 block are now stripped at one exported choke point
  shared by the CLI row, the friendly summary, and the browser activity feed.
  C1 matters because a UTF-8 terminal decodes U+009B and U+009D as CSI and OSC,
  reconstituting an escape introducer with no ESC byte in sight. The job title
  printed beside the summary is covered too: it comes from third-party
  bibliographic metadata and was stored with only whitespace trimming, so it
  bypassed the sanitizer on the very same output row. The `--json` output was
  never affected.

## [0.17.0] - 2026-08-04

### Added

- **`papio doctor` reports native-host version skew.** One papio binary is CLI,
  daemon, and native host, but the native-messaging manifest can point at a
  different copy than the daemon that is running (a packaged install beside a
  local build, or a symlink an upgrade left behind). A stale host enforces its
  own older transport rules, so browser work keeps failing while the daemon
  looks healthy. Doctor now runs the executable the manifest names and fails
  when its version differs from the daemon's, naming the path and the fix.
  `papio-native-host --version` now answers directly, so the probe works whichever
  copy the symlink resolves to.
- **Quarantined PDF previews now include the decision at the point of
  inspection.** The loopback capability URL renders a citation-aware review
  bar above the PDF and records accept or reject through the same durable,
  revision-and-SHA-bound review transition used by the browser inbox. PDF
  bytes remain capability-bound at the `/file` sibling, and each preview
  capability permits only one decisive verdict.
  The review bar carries papio branding, follows the system light/dark
  scheme, and a recorded verdict closes its tab automatically (with a
  graceful "you can close this tab" fallback when the browser blocks it).
- **`papio adapter capture <url>` captures a provider page through the live
  browser session.** The new `adapter.capture_v1` RPC queues one solicited
  capture directive per browser session; the extension opens a governed tab,
  captures the sanitized DOM through the existing pipeline, and the CLI
  returns the structured outcome and stored path. Fixture collection for
  adapter work is now fully agent-drivable — no screenshots, no human clicks.
  The capture vocabulary gains the `informit` provider and a `terms` scenario
  for consent-wall fixtures, validated on both sides of the wire.
- **`papio failures` aggregates terminal and parked failure reasons** by
  reason (default) or provider (`--by-provider`) from each job's most recent
  decisive events, via the new `failures.list_v1` RPC with the usual `--json`
  envelope.
- **The CLI explains itself at three former dead ends.** `papio actions open`
  without a connected browser session prints what is missing and how to check
  (`papio doctor`) instead of a bare exit code; `papio doctor` names the exact
  remedy for a stopped daemon rather than only diagnosing it; and `papio jobs
  show <id>` is an exact alias of `jobs get` instead of a silent no-op.
- **`papio activity` exposes the daemon's recent operator activity.** The new
  `activity.list` RPC and CLI command show a bounded, newest-first view of the
  durable events table, with `--limit`, optional `--job` filtering, and the
  usual `--json` page envelope.
- **The browser activity feed is feature-gated and solicited.** The
  `activity_request` / `activity_response` messages and `activity_feed_v1`
  feature carry bounded display-only entries to an open inbox; this is a pull
  view, not daemon push.
- **Early browser delivery can adopt a PDF for a live job.** The extension
  steers the browser download into `papio/<job-id>/`, and the daemon adopts it
  through existing legal job transitions and the ordinary validation pipeline.
  Queued, resolving, and fetching jobs no longer dead-end when the operator
  already has the PDF in the browser; download races remain structured,
  retryable outcomes.
- **Activity summaries read the same everywhere.** `store.ActivityText` renders
  one friendly line per event kind for both `papio activity` and the browser
  activity feed — raw kinds like `action.reminder` or `browser.error` no
  longer leak to either surface, and the CLI line view drops the raw detail
  JSON (still available under `--json`).
- **Two institutions no longer share one session.** Browser session evidence
  carries the resolver origin that produced it. The extension releases only
  queued handoffs for that origin, and the daemon maps the origin to a
  configured resolver profile before re-offering only that profile's
  store-backed siblings — a second-institution sign-in never releases the default institution's queue on either
  side, and vice versa. Named `[browser.resolvers.*]` profiles now send their
  own `shibboleth_entity_id` / `proquest_account_id` on handoffs (previously
  default-profile only), and a new `browser.default_resolver` key selects
  which profile bare acquisitions use.
- **Contextless browser adoptions are recorded as `manual` access**, matching
  migration 0019's reclassification, and delivery context binds to the exact
  candidate its download created instead of whichever candidate existed.
- **Offer pacing is strictly oldest-first** across ordinary offers (not only
  re-offers), and the two-second browser poll uses one paginated joined query
  instead of one query per open handoff.
- **A verified institution session now unblocks parked institutional work.**
  The extension reports timing-only session evidence (`session_evidence_v1`),
  and the daemon re-offers parked institutional handoffs from *any* prior
  session — discovery is store-backed rather than limited to the current
  connection's offers. Manual-download actions are never auto-driven.
- **Keep-warm follows daemon demand.** Negotiated triage counts
  (`triage_counts_schema_v2`) carry the number of open actions requiring
  institution sign-in, so the browser keeps the session warm while parked
  auth work exists — not only while a handoff tab is open.
- **Adopted downloads record their true access route.** The extension attests
  route (resolver/direct/open-access), a sanitized page host, and session
  evidence per delivery (`delivery_context_v1`); candidates persist
  `browser_route`/`session_evidence`, and `access_basis` is derived from
  evidence — an uncontexted adoption is never marked institutional. This
  gives ADR-0010's reserved `operator_browser_session` basis its producer.
  Schema version 19.
- **Institutional handoffs are paced.** The bridge keeps at most four
  unsettled handoffs in flight per browser session regardless of backlog,
  re-offers at most four parked jobs per cycle (oldest first), and records a
  single `browser.offers_paced` event carrying how many are held back —
  bulk-releasing a hundred parked jobs now drains as a trickle, not a flood.

### Changed

- **Unsupported provider pages now become actionable instead of occupying a
  browser-drive slot indefinitely.** A stable resolver landing outside the
  capped provider list enters the same bounded evidence path. The extension
  can send its existing sanitized diagnostic and `ui_changed` outcome; the
  daemon creates a manual-download action that explicitly names the missing
  adapter and whether local evidence was retained. A missing browser grant is
  not terminal: the governed tab stays live and resumes after the operator
  allows that exact provider origin.
- **Provider-adapter evidence now has a documented contribution path.** The
  guide starts with automatic local gap captures, `papio adapter captures`, and
  `papio adapter diagnose`; it requires review and minimization before any
  fixture is shared, then identifies the declarative spec and focused checks
  needed for a code contribution.

### Fixed

- **Large page captures no longer kill the browser session.** The IPC request
  cap (64 KiB) was smaller than a legal 256 KiB browser frame, so any
  `page_capture` over ~63 KiB failed the native host's relay mid-sync; the
  host treated that as a fatal transport error and said goodbye, tearing down
  the live session and aborting the capture (`nav_failed: browser session
  disconnected during page capture`) and re-parking every in-flight handoff.
  The cap is now 512 KiB and a nativehost regression test pins the invariant.
- **A large focus batch can no longer overflow one sync response.** Handoff
  focus requests now drain in bounded batches, with the remainder riding the
  next ordinary poll, so a caller-supplied job-id list cannot push a
  `browser.sync` response past the IPC result cap — the same class of fatal
  transport failure as the request-side bug above, in the other direction.
  `TestSyncResponseFitsResultCap` pins the response-side invariant.
- **A failed adopted download no longer mislabels paywalled work as needing no
  sign-in.** The replacement manual-download action inherits the resolved
  handoff's authentication classification instead of a blank one.

- **A rate-limit gate or request count earned under one set of credentials no
  longer applies to another.** Providers meter by credential, not by source
  name: measured against OpenAlex from one machine in the same second, an
  anonymous read reported 0 of 1,000 requests remaining while the same source
  carrying an API key reported 8,792 of 10,000. *papio* kept a single
  `source_budgets` row per source, so the two shared one budget. A 429 taken
  anonymously wrote a `Retry-After` lasting until the next UTC midnight; adding
  an API key opened a fresh 10,000-credit allowance, but the row still said
  closed, and 95 jobs parked against a quota that had nothing to do with them
  until it was cleared by editing the database by hand. Rate-limit state is now
  keyed by source *and* a non-secret fingerprint of the credential in use
  (`anonymous`, or `key-` followed by a truncated SHA-256 — never the
  credential itself), so every account carries its own gate, request counter
  and spend figure. Existing rows are preserved under the identity `legacy`:
  which credential earned them is unknowable, and a `legacy` row can never gate
  live traffic. Schema version 18.

## [0.16.0] - 2026-08-02

### Added

- **The six-method consumer IPC contract is ratified and mechanically pinned.**
  External tools can build against `jobs.list_v2`, `actions.list_v2`,
  `actions.open`, `jobs.receipt`, `jobs.add_component`, and
  `jobs.repair_awaiting_human` knowing their names and result shapes will not
  drift. The receipt records typed job outcomes and component inventory where a
  bundle cannot exist; an accepted main component's bundle remains the success
  provenance document. `truncated` on the two paged methods is a proven fact
  from reading one row past the page limit, not a hint that more may exist.
- **`acquire.submit_v2` is ratified, so an external tool can now start an
  acquisition.** The six ratified methods all read or act on a job that already
  exists, so a consumer had no ratified way to ask *papio* for anything and
  *papio*'s acquisition success rate was unmeasurable from the only side
  counting it. One work per call — bulk submission, a generic reopen verb,
  method aliases, and autonomous drain all remain unratified. Frozen: the
  method name, the params `request` / `auto_import` / `force`, the result
  `job_id` / `existing`, and the work-request identity subset plus
  `access_mode_override`. Policy fields stay served but unratified so a
  consumer cannot pin *papio*'s policy vocabulary. Note that `request_id` is a
  live-job convergence key and not an idempotency key: a terminal job plus the
  same `request_id` creates a new job, and `existing` means "a live job already
  owns this work", so a consumer resuming a run must persist the returned
  `job_id`. See ADR-0010.
- **A duplicate discovered after submission is now recorded rather than
  ignored.** *papio* deduplicates at submit, and a title-only request correctly
  matches nothing because canonical convergence keys on strong identifiers.
  Enrichment can supply a DOI much later, and at that moment the job provably
  names the same work as one already running — two of 309 works on the first
  real cohort. It is written down as a `job.duplicate_work_detected` event and
  **not** merged: `existing` answers a question asked at submit about a handle
  issued at submit, consumers poll that handle, and silently merging would cost
  them a work they believe they are tracking against a duplicate fetch that
  content addressing already collapses to one stored file. See ADR-0010.
- **`bundle.document` and `artifacts.locate` are ratified, so a consumer can
  collect what it acquired.** No ratified method could produce a bundle or
  locate artifact bytes, leaving a consumer to call unratified names or read
  *papio*'s data directory — which would make the on-disk layout a contract
  nobody agreed to. Both are pure reads keyed on `job_id`. The two methods that
  already existed were deliberately **not** the ones frozen: `artifacts.get`
  returns the `job.Artifact` persistence struct including `identity_result`,
  which is last-writer-wins across every job sharing a digest and which ADR-0007
  forbids projecting from an artifact; and `bundle.export_v2` writes a directory
  as a side effect and carries the bundle inline, which — because results reject
  unknown fields recursively — would have frozen the document's shape into the
  RPC contract and forced a new method name for every future bundle schema.
  `bundle.document` returns the document as JSON text instead, byte-identical to
  the exported `bundle.json`, so the bundle keeps evolving under its own
  `schema_version`. Both remain served and unratified for existing callers. See
  ADR-0011.
- **Bundles now emit `acquisition-bundle/2`, adding `candidate.entitlement`.**
  It records the route by which access was obtained — `route`, an optional
  `entitlement_ref`, and `acquisition_mode` — and it is a route, never an
  identity: *papio* never authenticates a human and never holds institutional
  credentials. `acquisition_mode` is derived from the accepted candidate's
  existing `access_basis`, so nothing is inferred, and the whole object is
  omitted whenever *papio* did not observe a route. `daemon_held_credential` is
  not a future mode: CORE and Crossref TDM already acquire with *papio*'s own
  configured API credential, and `entitlement_ref` names which one in cleartext
  (`entitlement:source:crossref_tdm`) rather than hashing a public constant.
  `operator_browser_session` is reserved and has no producer: browser adoption
  records `institutional` for every adopted download, including an open-access
  PDF handed to the browser only because a provider's anti-bot wall refused
  *papio*'s own fetch, so claiming an institutional route would invent
  entitlement evidence for a route nobody walked. v1 decoding is retained
  indefinitely, and a v1 document carrying an entitlement — including an
  explicit `null` — is rejected rather than silently accepted.
- **`bundle.export_v2` carries the exported document; `bundle.export` returns
  the path alone.** IPC results are decoded with unknown fields rejected, and
  that applies to nested objects, so returning a v2 bundle body from the old
  method would make an older CLI reject every export response from a newer
  daemon. The new CLI prefers `bundle.export_v2` and falls back automatically.
- **`papio acquire` accepts a bare arXiv id.** `papio acquire 2301.08745` and
  `papio acquire math/0211159` previously failed with "cannot infer identifier
  type" and needed an `arxiv:` prefix or `--arxiv`. Both forms are unambiguous
  against every other scheme papio accepts — a DOI starts `10.` and contains a
  slash, a PMID has no dot or slash, an ISBN has neither — so they are now
  inferred. A bare ten-digit string is still refused on purpose: it is
  simultaneously a valid ISBN-10 and a valid PMID, so it names two schemes at
  once and the user is asked to disambiguate with a flag.

- **`papio actions dismiss` closes a stale human action without touching its
  job.** An advisory on a terminal job previously had no supported way out:
  cancel refuses a terminal job, resolve handles only identity review, and the
  startup sweep deliberately spares informational advisories so a genuine trace
  survives. Retiring one meant editing the database. `--revision` comes from
  `papio actions list --json` so a stale listing cannot dismiss an action that
  changed underneath it.

- **`papio doctor` reports acquisitions nobody ever collected.** A full text
  papio fetched and no one exported is the one thing a consumer cannot detect
  for itself: a job is stranded exactly when the key naming it stops being
  derivable, so the orphan is the job it can no longer ask about. Counted after
  a grace period, so a freshly acquired work is not mistaken for an abandoned
  one.

### Changed

- **`jobs.receipt`'s `principal` is request-origin classification, not a rights
  input.** It cannot identify whose entitlement obtained the bytes, so consumers
  must not use it as proof of entitlement.
- **A misspelled `[sources.*]` name is now a startup error instead of silence.**
  Source names were never validated, so `[sources.unpaywal]` parsed cleanly and
  did nothing — indistinguishable from "that source found nothing for your
  paper". `papio` now fails closed on an unrecognized source name and lists the
  valid ones. The removed `openalex_content` name is tolerated and dropped on
  load, so a config written by an earlier `papio init`/`papio config save`
  still parses; the next `papio config save` rewrites the file without it.
- **An unrecognized positional identifier gives better guidance.** A bare
  argument beginning with `W` was routed to the OpenAlex parser on the prefix
  alone, so an ordinary word got an OpenAlex-specific complaint instead of the
  actionable list of identifier flags. Scheme inference now checks the full
  shape.

- **`papio doctor` no longer passes cleanly when OpenAlex has no API key.** It
  now warns and names the roughly tenfold difference between the anonymous
  allowance and an account's. Passing read as fully configured, and that is
  exactly how a real operator missed it: they measured the anonymous tier
  against an unkeyed client and recorded multi-day cohort acquisition as a
  property of the design.

- **A negative `rate_per_sec`, `burst` or `max_cost_usd` is now rejected at
  load.** A negative does not throttle harder, it removes the throttle: the
  budget manager reads a rate at or below zero as unlimited and a cost ceiling
  at or below zero as unmetered, so a typed minus sign silently deleted the
  protection it appeared to configure. Zero keeps its documented meaning,
  because no pacing and no ceiling is a choice someone can state.

### Removed

- **`openalex_content` is gone from `[sources]`.** It was reserved end to end —
  constant, default policy row, resolver priority rank, and published config
  reference — but no adapter was ever written, so enabling it produced no
  candidate, no warning, and no `papio doctor` complaint.
- **`work.ClassifyIdentifier`, a second identifier classifier that never ran.**
  It duplicated the CLI's positional-argument inference and had already diverged
  from it, carrying the more inviting name and a passing test suite that asserted
  behaviour `papio acquire` did not have. Its disagreement on the ten-digit case
  was a latent bug — it classified a bare ISBN-10 as a PMID. Bare-string
  inference now lives only in `internal/cli`, where the sole caller is, and its
  accepted shapes and refusals are pinned by tests against the live function.
- **Roughly 570 lines of unreachable code and its false coverage.** Removed
  `store.Backup`/`Checkpoint` and their hard-link fallback subgraph (no command,
  IPC method, or scheduled task ever called them), the orphaned
  `internal/doctor/manifest.go` (a divergent duplicate of the live native-host
  manifest paths in `internal/cli`), six exported alias wrappers in
  `internal/pdf` and `internal/fetch`, `app.RequestForCandidate` and
  `StableResolverNames`, `budget.ClearDefer`, `errcat.WaitGuidance` (superseded
  by `WaitGuidanceWithOpenAction`, whose coverage now sits on the function
  production calls), and `work.FindDOI` (a weaker duplicate of the live
  multi-DOI scanner in `internal/pdf`). No behaviour changes.
- **The dead caller-supplied feature seam in the browser bridge.** `NewBridge`'s
  `features` parameter was always `nil` in production, so its merge, dedupe, and
  32-entry cap could never run, while a test-only synthetic feature made the
  hello-ack assertion describe a wire contract no extension ever received. The
  daemon still advertises exactly the seven mandatory features, and the
  fail-closed 32-feature bound in `internal/protocol` is unchanged. This also
  removes both hardcoded `bridge_test.go` assertions flagged in `AGENTS.md`.

### Fixed

- **A source's daily quota no longer freezes the whole acquisition queue.**
  Providers express an exhausted daily quota as a `Retry-After` pointing at the
  next reset — OpenAlex answers with the next UTC midnight, up to a day out —
  and `budget.Acquire` slept that out inline. The sleeping caller is an
  acquisition worker holding a job claim whose scheduler heartbeat keeps
  renewing the lease, so the claim never expires and the row can never be
  reclaimed: with three workers, three quota-gated jobs stalled a 309-job
  cohort at zero throughput for the rest of the day. `Acquire` now waits out
  only short blips (`budget.MaxInlineWait`, five seconds) and returns
  `*budget.ErrDeferred` for anything longer. Resolution skips the gated source
  and parks the job in `retry_wait` at the gate; a gated fetch candidate stays
  retryable; enrichment is simply skipped. The wait belongs to the scheduler,
  which can run every other job meanwhile.
- **A human action that needs an institutional sign-in now says so.** Whether
  finishing an action requires authenticating is what the conservative access
  mode is checked against, and it was recorded deliberately at only four of the
  twelve places that open one — everywhere else it defaulted to "no sign-in
  needed", and two of those defaults were wrong. The action opened when the
  browser extension has *explicitly reported* an authentication wall claimed the
  opposite, and every landing-page hand-off was recorded the same way including
  paywalled ones, so a work behind an institutional paywall looked freely
  fetchable. Both now record what was observed. A provider's terms-acceptance
  step fails closed, because it can sit behind a sign-in, a free account, or
  nothing at all and *papio* cannot tell which. The value is now a required
  argument, so a future omission is a build failure rather than a silent "no".
- **`papio jobs cancel` no longer reports a cancellation that did not happen.**
  Cancelling is deliberately a no-op once a job has already finished, which is
  right for repeat calls and scripting — but the command printed `Cancelled
  <id>` regardless, so a job that completed a moment before the command ran was
  reported as stopped. It now reads the state back and says what actually
  happened, naming the state the job was already in.
- **Waiting for a closed source gate no longer spends a job's retry budget, and
  never manufactures a terminal verdict about a source that was never called.**
  `retryBudgetExhausted` counted every `retry_wait` transition alike, so once
  gated sources began parking jobs instead of blocking workers, a day-long
  OpenAlex quota gate alongside ordinary thirty-second gates burned all eight
  attempts within minutes and settled the job `temporary source failures did
  not clear` — naming a source that had never been asked. The retry accounting
  now distinguishes the two reasons a pass can end without a verdict: a
  *temporary* failure means a request went out and failed, and costs an
  attempt; a *source gate* means no request was made, and costs nothing.
  Transitions record which, and older events without the discriminator keep
  the original bound. When the attempts genuinely are spent but a gate is still
  closed, the job now parks until it opens — and parks at the *gate*, not at
  the shorter temporary time it would only spin on. A retry time that elapsed
  during the same pass is floored rather than persisted into the past, which
  used to cause an instant re-claim.

- **A rate-limited sibling lookup no longer settles the job `unavailable`.**
  `resolveSiblings` recorded every error as a plain `failed`, never consulting
  `resolver.Temporary`. The version hop runs at the exhaustion boundary, where
  the presence of a retry time is precisely the difference between parking and
  giving up, so a 429 during the hop turned a retryable job into a terminal
  negative claim. Temporary sibling failures now defer the source and schedule
  a retry like any other. A pure source gate also no longer suppresses the hop:
  no request was made, and the sibling source may not be gated at all.

- **A forced resubmission now withdraws the verdict it supersedes.** `Retry`
  out of `unavailable` already cancelled the conservative `openurl_available`
  advisory, because "left open it outlives its own remedy and keeps telling the
  user to do the thing they just did". A `--force` submission withdraws a
  verdict just as much, but created an unlinked fresh job and left the old
  advisory open forever — and that advisory is deliberately exempt from both
  the terminal transition and the startup sweep, so nothing would ever retire
  it. Every resubmission therefore double-counted the work's institutional
  opportunity against a job that no longer represented it. Superseded terminal
  jobs for the same canonical work now have their advisory cancelled and record
  a `job.superseded` event naming the replacement, so a consumer that cached
  the old outcome can learn that a terminal papio record was not final.

- **A trailing full stop no longer hides a work from Crossref title
  enrichment.** APA and several other publishers deposit article titles with a
  closing period; citations and reference managers almost never carry one.
  Title matching normalised case and whitespace but compared for exact
  equality, so a perfect Crossref hit was rejected on one character — and the
  job then settled `no_identifier`, a claim that no identifier could be found
  for a work whose DOI was sitting at rank 0 of the response with matching
  authors and year. Only the trailing period is folded, and on both sides:
  replaying one cohort's twenty-six clean-title failures showed every
  recoverable case was a deposited full stop, so folding `?` or `!` too would
  buy nothing measurable while making `Who?` and `Who!` equal — and the same
  helper normalises author family names, so any widening loosens author
  corroboration as well.

- **The Crossref enricher now identifies itself to the polite pool.** It was
  the one source client that sent no `mailto`, while every other client sends
  one and `papio doctor` reports the contact identity as configured.

- **The per-source token bucket is bounded too, and can no longer busy-spin.**
  `Acquire`'s durable-gate wait was bounded, but the rate-limiter loop beside
  it was not: a slow refill held a leased acquisition worker, contention let a
  waiter lose each refilled token without converging, and at a very low
  configured rate the computed wait overflowed `int64` nanoseconds into a
  non-positive duration, so the sleep returned immediately and the loop spun
  hot. It now shares `MaxInlineWait` and returns `ErrDeferred` past it, so the
  job parks like any other gated source. The wait is never persisted — a token
  is process-local and advisory, and another caller may take it first.

- **A per-request `access_mode_override` now actually governs the acquisition,
  and can only narrow.** The override was validated, snapshotted into the job's
  policy, and printed by `papio diagnose` — while the only code that decides
  whether to open an institutional browser handoff read the daemon-wide
  `access_mode` instead. Submitting one work as `conservative` against an
  `assisted` or `delegated` daemon therefore opened a handoff anyway, and
  reported that it had honoured the override. The job's own snapshot is now
  authoritative at both decision points — the exhaustion gate and the
  `access_mode` sent to the extension in a job offer. An override may now only
  *narrow*: the configured `access_mode` is the operator's standing decision and
  the only brake papio has, so a submitter cannot raise automation above it, and
  the ceiling is re-applied on every read, so tightening `access_mode` restrains
  jobs already queued rather than only new ones. Narrowing is what the override
  is for — a cohort run asking for `conservative` on a delegated daemon records
  `openurl_available` advisories and opens nothing, instead of parking hundreds
  of handoffs that never expire.
- **Widening `access_mode` and retrying a conservative job works again.** The
  conservative advisory tells the operator to switch access mode and retry, but
  a retry preserves the job's policy — so once the policy became authoritative
  the job re-exhausted under its original mode and reopened the same advisory,
  telling the operator to do the thing they had just done. Retry now releases
  the pinned mode in the same step that cancels the advisory.
- **A stale parked job can no longer disconnect the browser extension.** A job
  whose access mode resolves to `conservative` cannot be expressed in a
  `papio-browser/1` job offer, and any error out of the bridge is treated by
  the native host as a dead connection — so one such row would have torn down
  the whole native-messaging session instead of being skipped. It is now
  skipped, and its siblings are still offered.
- **`papio acquire --batch` works again against an older running daemon.** Batch
  moved to `acquire.submit_v2` without the `unknown_method` fallback the
  single-work path has carried since that method shipped, so every work in a
  batch failed against a daemon older than 0.13.0 — a routine state, since one
  binary serves as CLI, daemon, and native host.
- **`update.Checker.Check` no longer returns an error it can never produce.**
  Its failure policy is deliberately soft — a GitHub outage must not make
  `papio doctor` noisy — so every path returned `nil`, leaving unreachable
  error branches in the doctor integration checks and five test assertions that
  could never fail. The signature now matches the contract, and the tests pin
  the behaviour that matters: a transport error, a 304, a non-200, and a
  malformed body each return the previously cached release, or `nil` on a cold
  cache.

- **Asking for more rows no longer returns fewer.** A `--limit` above the
  maximum reset to the *default*, so `--limit 600` yielded 100 rows where
  `--limit 500` yielded 500 — silently, and in the direction of
  under-reporting, which is the worst way to be wrong for the only people who
  pass a large limit: the ones counting. Two separate consumers hit it on the
  same day. Over-large now clamps down to the maximum; unspecified still means
  the default. The same non-monotonic clamp existed in four places, including a
  client-side copy that ran before the request and so decided the answer
  regardless.

- **Discovery's provider calls are accounted for and can be paused.** papio
  reached OpenAlex through two clients and only one of them was budgeted: the
  resolvers reserve at the job level, while search, MCP, watch digests and
  DOI-only enrichment held a bare HTTP client. So papio under-reported its own
  consumption by an unknown amount, and a durable gate that paused acquisition
  did not pause discovery at all — it kept calling an API that had already said
  stop. Discovery now reserves per request and reports a rate limit as one.
  It also shares the resolvers' secure HTTP client, so its redirect and size
  policy is the one `papio doctor` reports.

- **A server cannot defer a source for longer than a day.** `Retry-After` was
  honoured unconditionally and an existing gate was never shortened, so a clock
  skew, a malformed header or a provider bug could park every job needing that
  source for as long as it asked — recoverable only by editing the database.
  Waits beyond the horizon are clamped; anything inside it is still honoured to
  the second.

## [0.15.0] - 2026-07-30

### Added

- **`papio jobs receipt`, `papio jobs repair-awaiting-human`, and `papio jobs
  add-component` are now available to operators and MCP agents.** The daemon
  could already perform all three, but only a program dialling the socket could
  ask: neither the CLI nor MCP agents could. The bundle remains the canonical
  provenance document for an accepted main component; the receipt is the
  canonical job outcome and component inventory for every state.
- **`papio inbox decide` acts on a triage inbox item from the terminal.**
  `papio inbox` and `papio inbox counts` could show the inbox, but only the
  browser extension could answer an item — so acquiring or dismissing a watch
  hit, or acknowledging a retraction notice, required the browser even for
  someone working entirely in a terminal. `--op acquire|dismiss` with
  `--watch-scope all` or an explicit watch-id list now does it, reporting the
  daemon's own outcome (including `conflict` and `already_applied`) rather than
  assuming the decision applied.

### Changed

- **Repair audit reasons and request origin no longer overstate who acted.**
  Orphan repair now records `orphan_repair`, rather than naming a caller it cannot
  identify. `principal` is documented as request-origin classification, not proof
  of whose entitlement obtained the bytes; do not use it as a rights input.

### Fixed

- **`attempted_tiers` retains every tier a job genuinely attempted after an
  explicit retry.** It now reads append-only attempt records instead of current
  candidate status, which retry resets can erase.
- **Attaching a supplement now says what to fix.** Every expected refusal from
  `jobs.add_component` — the job holds no main artifact yet, the file sits outside
  the job's adoption root, the file is not a readable PDF, the file carries active
  content — arrived as `internal: operation failed`, because only an unrecognised
  error was meant to collapse that way and none of these were classified. They now
  return the condition and the remedy. The confinement case deliberately does not
  echo the offending path: that belongs in the daemon log, not an RPC message.
  Nothing here loosens a guard; the same files are refused, and now the operator
  can tell which guard refused them. Found by exercising the new command against a
  live daemon rather than a stub.

## [0.14.0] - 2026-07-30

### Added

- **De-duplication against a non-Zotero library** (`[[library.sources]]`, new
  `library.lookup_works` method). Ownership lookup was a zotio capability, so
  without Zotero `papio search` never marked a result `[in library]` and
  `acquire --batch` happily re-acquired papers you already held. Point *papio* at
  BibTeX/RIS/CSL-JSON/NBIB export of what you hold and it stops. Each source
  declares what it asserts — `claim = "pdf_present"` may skip an acquisition,
  `claim = "record_present"` only annotates search — because *papio* refuses to
  infer PDF presence from per-manager attachment conventions and thereby skip a
  paper the source never vouched for. Matching is exact on identifiers supported
  by each format: BibTeX supports DOI, arXiv, and PMID; CSL-JSON and NBIB
  support DOI and PMID; RIS supports DOI only. Titles are never matched and
  ISBN is excluded, since an edited volume shares one ISBN with every chapter
  in it. There is no supported-app list: anything that exports one of those
  four formats works.

  The load-bearing rule is that **a source unreadable to *papio* is not a
  source that holds nothing**. An unreadable or malformed export leaves the
  answer *incomplete*, so `--batch` creates no jobs and names the failing source
  rather than re-downloading the batch. An acquire watch fails the run and
  retries on cadence until the standard fifth-consecutive-failure auto-disable;
  it has no `--include-owned` override. That override is available only for
  `papio acquire --batch`. During a transient provider failure, a fresh
  cached positive may still annotate `papio search` and raw lookup, but batch
  and watch acquisition remain fail-closed. The runtime last-known-good guard
  is not persisted across daemon restarts; restart establishes the current
  export as the new baseline. Because a bibliographic export cannot say
  *which* manifestation it holds, such a source never satisfies an explicit
  `--desired-version published` request. `papio doctor` performs a fresh
  one-shot source probe and reports that read's record count and outcome, not
  daemon cached age, count-collapse, or retained failure state. See ADR-0008.
- **`papio stats`** reports what the pipeline has actually obtained: lifetime
  acquired and failed totals, how many acquisitions needed a browser handoff, the
  split of acquired works by access basis (open access / institutional /
  licensed API / other), and a twelve-week series. The daemon has computed this
  all along for the extension's stats view (`stats_request`), but no RPC or
  command exposed it, so the CLI — and the MCP surface derived from it — was the
  only interface that could not see whether institutional access was buying
  anything. `stats.get` is a passthrough of that read model, not a second
  aggregation: `papio status` still owns the live job board and
  `papio jobs failures` the failure groups.
- **`jobs.receipt`** reports what happened to one acquisition: its state, its
  typed terminal reason, who requested it, which access tiers it actually reached,
  and which components it holds. It exists for the outcomes an acquisition bundle
  cannot describe — a failed job has no bundle — and deliberately does not restate
  what a bundle already carries, so the two can never disagree. Successful jobs get
  `bundle_available: true` and the bundle remains the full provenance record.
- **A job can now hold supplements and appendices beside its main PDF**
  (`jobs.add_component`). A quotation missing from a main PDF may simply live in a
  supplement, so a tool that reports "not in the source" without them is making a
  claim papio's own evidence does not support. Components are validated by the same
  payload and structural gates as a main file; identity is not asserted, because a
  supplement is usually not the article and carries neither its title nor its DOI.
  `html_fulltext` is refused for now with a clear error: raw provider HTML is
  inherently active content, and admitting it needs a sanitization design rather
  than a new role name. No resolver emits components yet.
- **`jobs.list_v2` and `actions.list_v2`, whose `truncated` is a proof.** The
  existing flag is inferred from a full page, so an exactly-full final page is
  indistinguishable from a partial one — fine for a human raising `--limit`, not
  fine for a program reconciling a cohort of hundreds of works. The new methods
  reach one row past the limit and report whether it was there. `papio jobs list`
  and `papio actions list` use them and fall back to the old methods against an
  older daemon; `papio actions list` gains `--limit` to go with it.
- **`jobs.repair_awaiting_human`** returns an *orphaned* parked job — one
  awaiting a human with no open action left to act on — to `resolving`. Nothing
  else could: `jobs.retry` refuses parked jobs by design and `actions open` needs
  an open handoff action, so the row was only recoverable by the daemon's own
  background sweep. Deliberately orphan-only and it takes no action ids, so it
  cannot close an action the caller never read. Handoff offers still never expire.
- **Acquisitions record which principal requested them** (`cli`, `mcp`, or
  `unknown`) instead of a hardcoded `cli`, and terminal reasons are now a closed
  vocabulary rather than free text. Persisted values are unchanged.
- **A retraction notice can be acknowledged.** The sentinel recomputes update
  notices from Crossref for as long as the work stays in the library, so a notice
  never resolved itself: it sat at the top of the inbox — above the actions that
  still need work — for the life of the library copy. Retraction items now carry
  `dismiss` (and `open`), and `triage.decide` routes a `retraction:` item to the
  sentinel instead of looking for a watch digest that will never exist. The
  acknowledgement is bound to the notice identity, so a nature that escalates
  (concern → retraction) or a newly issued notice DOI surfaces the work again, and
  the daily sweep prunes acknowledgements whose notice is no longer current. The
  wire shape is unchanged, so an already-installed extension gains the button.
  Schema version 17.
- **`papio acquire` accepts several identifier flags at once.** A work carries a
  DOI *and* a PMID *and* an arXiv id, and `protocol.Identifiers`, the
  `identifiers` table, and every batch parser have always modelled that — only the
  CLI insisted on one, forcing callers to discard identifiers they already held.
  Flags now compose; mixing them with the positional identifier is still refused,
  because that one really is ambiguous.

### Changed

- **`papio init` accepts `none` for the zotio executable.** An empty answer means
  "keep the default" at every init prompt, and the zotio default is the bare name
  `zotio`, so someone who does not run Zotero had no way to decline it: init wrote
  `zotio.executable = "zotio"` and `papio doctor` then *failed* on a binary that
  was never wanted. Answering `none` clears the key — `doctor` reports zotio as
  `not configured (optional)` — and the attachment-mode question, which only
  describes how zotio files an attachment, is skipped. `--zotio-path ""` already
  did this non-interactively.
- **The README and docs no longer read as if Zotero were the only destination.**
  The `on_ready` hook, standard-format batch input (RIS, BibTeX, CSL-JSON,
  MEDLINE/NBIB), and a zotio-free `doctor` all shipped already, but every entry
  point — README headline, docs landing page, getting-started, user guide, the
  site description, and the pipeline diagram — described the Zotero path
  exclusively, and the hooks page was missing from the landing page's own index.
  Filing is now two paths (zotio → Zotero, or the hook into papis, a plain
  folder, or your own script), getting-started ends in a filing step for
  whichever one you use, and the de-duplication you give up without zotio is
  stated rather than discovered.

### Fixed

- **`papio acquire --batch` no longer reports a decode error after successfully
  creating its jobs.** The batch client samples one field — the job state — out of
  the `jobs.get` result, but declared a struct containing only that field while
  `internal/ipc` rejects unknown fields on the whole envelope. So every batch
  submitted its work, then failed decoding the daemon's reply on the first field
  it did not know about (`job.id`), printed `decode ipc result: json: unknown
  field "id"`, reported every state as `unknown`, and exited non-zero. It now
  decodes the siblings it does not read as raw messages, which is the general rule
  this violated: a caller that reads one field must not be coupled to the shape of
  everything beside it. Found by smoke-testing the batch path end to end.
- **The conservative-mode OpenURL advisory no longer outlives its own remedy.**
  `openurl_available` records that an institutional route existed and
  conservative mode did not take it, and it is deliberately exempt from the
  terminal-action close and the startup sweep so that trace survives on a job
  that stayed unavailable. But its guidance is "switch access mode and retry" —
  and `jobs.retry`, the only edge out of `unavailable`, never touched it. Taking
  the advice therefore left the advisory sitting in the inbox on a job that had
  since reached `ready`, still telling you to do the thing you just did. A retry
  now clears it; re-exhausting in conservative mode raises it again.
- **One job's identity check no longer overwrites another's.** Identity is decided
  against the work *this* job asked for, but it was stored on the shared,
  content-addressed artifact row, so a second acquisition of identical bytes
  rewrote the first job's recorded finding — and with it that job's exported
  validation block. Each acquisition now records its own finding, and bundle
  export reads that rather than the shared row.
- **An acquisition no longer inherits another acquisition's licence.** Artifacts
  are content-addressed and shared, so two jobs can hold identical bytes obtained
  under different terms — an open-access mirror and an institutional copy of the
  same file. Bundle export resolved provenance by content hash alone and picked
  the *earliest* job holding that hash, so an institutionally-acquired job could
  export the earlier job's `access_basis` and `reuse_license`: first-writer-wins
  rights attribution on a digest. Provenance now comes from the job's own
  candidate, and only when that candidate was actually *accepted* — a job can
  carry a rejected selection forward through crash recovery, and reading it would
  have published the licence of a file papio threw away. A job completing from the
  local cache now records the source acquisition's candidate rather than leaving it
  to be reconstructed from the digest later.
  **If you gate retention or redistribution on a bundle's `reuse_license`,
  re-export any bundle written before this release whose PDF you acquired through
  more than one job — including under different identifiers, since the affected
  condition is a shared file, not a shared work record.** A wrong bundle cannot
  self-detect: its `provenance_digest` signs whichever candidate block was written.
- **A browser-adopted download no longer claims a version papio never observed.**
  Adoption labelled the file `published`, or — worse — copied the request's
  `desired_version` *preference* into the obtained *fact*, so asking for an
  accepted manuscript produced a candidate asserting it had received one. papio
  sees bytes arrive from your browser and never learns which version you chose,
  so the adopted version is now always `unknown`. Its access basis
  (`institutional`) and licence (`unknown`) were already honest and are unchanged.
  Adopted candidates written before this release are normalised when the same
  bytes are adopted again or an identity review of them is accepted.
- **The README architecture diagram's `alt` text is no longer truncated
  mid-word.** The `<img>` attribute was cut off after "login, MFA, a" and left
  unterminated, so the tag never closed.

## [0.13.0] - 2026-07-27

### Added

- **Diagnostic page captures no longer land in your Downloads.** They travelled
  via `chrome.downloads` because ordinary extensions have no filesystem access —
  but papio has a native-messaging host and a daemon that already owns its data
  directory. Captures now cross the bridge as a gzip-compressed `page_capture`
  frame (the largest real capture is 40% of the frame budget, so one frame, no
  chunking) and the daemon stores them under `<data dir>/captures/`, bounded to
  10 per host and 14 days. `papio adapter captures` lists them and
  `papio adapter captures purge` removes them. Enabled by default; set
  `[captures] enabled = false` to turn it off. The old
  `~/Downloads/papio-fixtures/` directory is no longer written and is safe to
  delete. Gated behind the `page_capture_v1` feature per ADR-0001, so an
  extension paired with an older daemon simply does not capture.
- **A fixed adapter now retries what the broken one stranded.** Provider
  outcomes record the extension version that produced them, and a job parked on
  a manual download is re-offered once when the live browser reports a newer
  version. Adapters ship inside the extension bundle, so an adapter fix is an
  extension upgrade — precisely the moment a job's failure reason stops
  existing. Bounded to one retry per job per upgrade, only with a live holder
  session, and never touching a leased job, a route already proven empty, or a
  `verify_identity` awaiting your decision.
- **Re-submitting a paper that is already in flight no longer queues a
  duplicate.** Four separate jobs for one DOI had accumulated on one machine,
  because the underlying handoff kept failing and re-submitting was the only
  recourse. `papio acquire` now returns the live job and says so, naming
  `--force` for a genuinely fresh attempt. Terminal jobs never block a new
  submission — `ready`, `failed`, `unavailable` and `no_identifier` are all
  finished, and re-queueing after adding a missing DOI is the documented
  remedy. Works identified only by title are deliberately never merged: a title
  describes rather than identifies, and an erratum or a preprint can share one
  with the paper the user actually wants. Delivered through a new
  `acquire.submit_v2` method with a fallback to the old one, rather than
  widening a result an older CLI would reject.

- **Durable escalating reminders for stranded human actions.** The daemon now
  re-notifies the configured desktop and webhook sinks after
  `[browser] action_expiry_seconds` (30 minutes by default), then doubles each
  action's delay through 24 hours instead of repeating every maintenance pass.
  Notices distinguish an institutional sign-in, an openable no-login handoff,
  and a review decision; each names only actions whose own schedule is due.
  Their per-action schedule is stored as an `action.reminder` event, so daemon
  restarts do not reset it.
- **`papio actions open` now reuses a live extension handoff.** With a
  compatible browser holder, the CLI asks the daemon to focus the job-scoped
  handoff tab instead of launching a second, untracked OpenURL tab through the
  operating system. When that tab is stranded on authentication, reopening it
  re-drives its retained resolver URL; provider pages mid-download are only
  focused. No session, legacy native host, or older extension retains the
  OS-launcher fallback.

### Fixed

- **Guidance for a parked job now comes from its current action, not a frozen
  reason.** A job that parked long ago as `login_required` and later had its
  handoff replaced by a `manual_download` still displayed the original advice:
  sign in, then run `papio actions open` — a command that cannot open a manual
  download. The reason was a historical transition record and never updated
  when the action changed. Where a job has an open action, that action is now
  the source of truth for what the user is told, across `papio status`,
  `papio acquire --wait` and the MCP status surface; jobs with no open action,
  such as terminal `no_identifier` or `no_entitlement`, are explained exactly as
  before. This was the fourth surface to carry the same defect, so the
  conformance guard was extended to the state-driven path as well — each earlier
  guard only covered the surface it was written for, which is precisely how it
  kept reappearing.
- **papio was abandoning handoffs while the provider was still verifying the
  browser.** Nine of ten real handoffs on one machine ended `ui_changed`,
  reported as "the provider's UI changed" and parked as a manual download. The
  pages papio captured at those moments were Cloudflare interstitials — one of
  them reading *"Verification successful. Waiting for journals.sagepub.com to
  respond"*. The challenge was passing; papio's two-unknown-verdicts-5s-apart
  escalation was simply faster than the challenge, so it gave up seconds before
  the article would have loaded and told the user to fetch by hand a PDF that
  was already on its way. A bot check is now recognised as the transient state
  it is: escalation is deferred, the unknown streak cleared, and classification
  retried for up to a minute; only if it never clears is the job reported
  blocked by a bot check rather than mislabelled as adapter rot. A genuinely
  unreadable provider page with no challenge present still escalates exactly as
  before, so real adapter rot stays visible. The detector was validated against
  all 25 pages papio had captured from live failures, matching both Cloudflare
  variants on every affected host and none of the 15 ordinary pages.
- **papio asked users to sign in for items their library had already said it
  does not have.** The institutional resolver answers "No full text available"
  on its own page, but no adapter covered the resolver host, so that terminal
  answer classified as unknown and became a manual-download park — complete
  with advice to sign in first. A resolver adapter now reads that answer as
  `no_entitlement`, which papio already knows how to handle.
- **A reminder told users to run a command that cannot act on their action.**
  `papio actions open` cannot open a `manual_download`, yet the escalating
  reminder named it for every auth-requiring action. This was the third
  appearance of one defect: three separate places derived "what should the user
  do next" from an action's kind and sign-in requirement, each written when only
  handoffs carried that requirement. They now share one authority, and a new
  conformance test asserts that any command named for an action can actually act
  on it — the earlier guard only checked that the command existed, which is why
  it passed all three times.
- **One sign-in now releases the batch.** Jobs parked on the same institutional
  route are re-offered when a session becomes live, instead of each waiting for
  its own handoff and eventually being reported as needing a manual download.
  Bounded by the usual guards: same resolver profile, live holder, never a route
  already proven empty, and never a job an adoption is mid-flight on.
- **Provider outcomes now record why they happened.** `adapter_version` and
  `detail` already crossed the bridge, validated and bounded, and the daemon
  discarded both — so nine failed handoffs left an audit trail that said only
  that they had failed. `papio jobs get` now shows which adapter version saw
  what, which is how the Cloudflare diagnosis above was possible at all.
- **A handoff whose provider page papio could not drive became a dead end.**
  When the extension reported `wrong_work` or `ui_changed`, the daemon resolved
  the job's only open action and parked it in `needs_review` — a state whose
  entire contract is that a human approves or rejects something — without
  opening anything to approve or reject. `papio status` said "see `papio
  actions` and approve or reject it" while `papio actions` was empty, and
  `needs_review` is not retryable, so the job could be neither acted on nor
  re-run. Nine jobs sat like that for nine days on one machine. Such outcomes
  now park in `awaiting_human` with an open `manual_download` action, which is
  actionable, visible to the reminder escalation, and still adoptable if the
  user downloads the PDF themselves. Existing stranded jobs are healed by
  handoff maintenance rather than needing manual cleanup, and the replacement
  action inherits the sign-in requirement of the handoff it replaces so it
  cannot advertise "no login needed" for a paywalled work.
- **The recovery command `papio status` recommended did not exist.** Every
  parked browser handoff, open-access browser fetch, and generic
  `awaiting_human` job was told to run `papio actions --open`, which exits 1
  with `unknown flag: --open`; the real command is the subcommand
  `papio actions open`. So the surface a stuck user actually reads pointed
  them at a dead end — precisely when a stale institutional sign-in had
  already cost them the handoff. The institutional guidance now also states
  that re-running it mints a fresh resolver link, which is the recovery for a
  "stale request" / "expired" sign-in page. A new test parses every backticked
  `papio …` command out of the `internal/errcat` catalog and resolves it
  against the live command tree, so guidance can no longer quote a command or
  flag that does not exist.
- **A daemon started by the browser could not find Poppler, so every PDF was
  staged for human review.** macOS gives a launchd child
  `PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`, and capability detection
  trusted `PATH` alone — so a daemon autostarted by the browser's
  native-messaging host saw no `pdftotext`, failed semantic extraction on
  everything, and marked every document's identity unverifiable. Whether papio
  could read a PDF at all depended on *who happened to start the daemon*: from
  a shell it worked, from the browser nothing did. Detection now falls back to
  the usual package-manager prefixes (Homebrew on both architectures, MacPorts,
  Linuxbrew, snap) when `PATH` comes up empty, preferring `PATH` when it
  resolves. `papio doctor` also states the consequence — "every PDF will be
  staged for human review" — and, when the tool is installed but unreachable,
  says to restart the daemon from a shell rather than telling you to install
  software you already have.
- **PDF identity verification accepted the wrong document roughly one time in
  ten.** Title, author, and year evidence was matched against the *entire*
  extracted text, and a paper's bibliography is several hundred other papers'
  titles and thousands of their authors — so any long document satisfied almost
  any other document's author and year. Measured over 1560 deliberately
  mismatched document/metadata pairs drawn from one real 40-paper library, 155
  (9.9%) were accepted as the wrong work, unlocked by given names such as
  "david" and "john", by an organisational "the", and by any recent year
  appearing in a citation. Identity evidence now comes from the byline window
  at the top of page one, the author test requires a family name rather than
  any name token, and the year is recorded as corroboration instead of gating
  acceptance. The same corpus now yields no wrong-document acceptances, while
  correct acceptances rose from 39/40 to 40/40.
- **A printed DOI, arXiv id, or PMID now corroborates identity wherever it
  appears.** The front-matter scan reads only the top of page one so a
  reference-list DOI is never mistaken for the document's own, which also meant
  it missed an identifier printed in a running footer or below the abstract —
  17 of 40 real papers. Once a document clears the title gate, its own printed
  identifier is accepted as proof, which also fixes reprints: the CACM edition
  of a NIPS paper is catalogued under 2017 and contains no "2017" anywhere in
  its text, so it was staged for human review despite printing the exact
  requested DOI on page one.
- **Identifiers are read even when the publisher letter-spaces them.** ACM
  typesets a DOI as `DOI:10.1145/ 30 6 5 3 8 6`, which no regex could match.
  Identifier corroboration now compares whitespace-insensitively.
- **Line-break hyphenation and ligatures no longer cost title matches.**
  Justified text yields `classifi-\ncation` and some producers keep `ﬁ`/`ﬂ`
  codepoints; neither tokenized as the word the title contains. Superscript
  affiliation markers glued to byline surnames (`Arrietaa`, `Tabikg`) are
  tolerated too — one real 12-author paper had all twelve marked that way.
  Document text is also tokenized once per decision rather than re-walked for
  every candidate token.
- **Work with no fetchable identifier no longer asks for an institutional
  sign-in.** `exhaustedCandidates` asked only whether a resolver base was
  configured, so a book, chapter, report, or thesis with no DOI parked as an
  institutional OpenURL handoff — telling the user to spend an SSO round trip
  on something a library can only return as a catalogue record. On one real
  112-item backlog that was 27% of the queue, and each one parked forever.
  Ordinary exhausted jobs now settle `unavailable` with the reason
  `no_identifier`, with no institutional handoff, and explain the remedy for
  either origin: re-submit a manual request with its DOI, or
  apply Zotero DOI enrichment and re-queue. A metadata-corrected Zotero item
  bypasses the ordinary unavailable cool-down, so that remedy works
  immediately. The gate is a *fetchable* identifier — DOI, PMID, or arXiv id
  — so a chapter with a publisher DOI keeps its handoff, while an ISBN alone
  does not. A title-matched anti-bot page remains available even without an
  identifier. If that OA offer reaches an auth or terms wall, reports no
  entitlement, or is rejected, it settles `no_identifier` rather than falling
  back to an institutional sign-in it cannot complete. Existing parks are healed
  automatically: handoff maintenance now returns an unfetchable park to
  `resolving`, where the same gate reclassifies it.
- **Zotero ISBNs reach the resolver, and monographs are described as books.**
  `--from-zotio` dropped the ISBN, and every OpenURL put the work's title in
  `rft.atitle` — asking the library for an *article* by that name. A work with
  an ISBN and no DOI is now sent as `rft.isbn` plus `rft.btitle` under the book
  metadata format. Multi-edition fields now split on edition separators before
  whitespace, so space-formatted ISBNs do not shatter into invalid digit groups.
- **A completed browser download can no longer be lost to handoff repair.**
  Repair now atomically checks the adoption lease before closing stale actions
  or returning a parked job to resolution, so a just-completed download remains
  on the adoption path instead of becoming eligible for cleanup.
- **Handoff maintenance now rotates through the whole parked queue.** A
  bounded pass no longer revisits the same first 500 awaiting jobs forever, so
  newer stranded handoffs and reminder-eligible actions are eventually reached.
- **Reminder backoff survives a backward wall-clock correction.** A future
  reminder timestamp is rebased while retaining its escalation count, rather
  than suppressing a stranded handoff until wall time catches up.
- **`papio actions open` can focus handoffs outside the ordinary poll page.**
  An explicit focus request now force-offers its parked job before asking the
  extension to surface it, rather than suppressing the CLI's OS-launcher
  fallback for a job the extension was never told about.

## [0.12.0] - 2026-07-25

### Added

- `papio doctor` now warns when an OpenURL resolver base — the default
  `[browser] openurl_base_url` or any named `[browser.resolvers.*]` profile —
  points at a raw Alma link resolver (`…/view/uresolver/…`). That deep link
  sends an unauthenticated patron to the Alma *staff* login (`…/mng/login`),
  which patrons cannot complete; the check recommends the institution's Primo
  OpenURL endpoint (`…/discovery/openurl` or `…/nde/openurl?vid=…`) instead.
- **Acquisition-history stats over the browser bridge** (`browser_stats_v1`).
  The daemon answers a new `stats_request` frame with lifetime acquisition
  aggregates: works acquired, works failed, how many needed a human handoff,
  a breakdown by access route (open access / institutional / licensed API /
  other), and a 12-week weekly series bucketed by when each work was actually
  acquired rather than when its row was last touched. Counts only — the
  daemon reports facts and never a "time saved" figure. Read-only; the one
  schema change is an index, no table or column touched.
- `papio doctor` now reports discovery backend health. A backend that failed
  during a recent search is surfaced as a warning naming the backend and the
  cause, so thin search results can be told apart from a broken backend; a
  backend that has since answered is cleared automatically. Previously a
  backend could fail on every search with nothing said anywhere, because a
  partial failure was discarded whenever another backend answered — the daemon
  now logs it too.

### Changed

- **BREAKING for anyone parsing `--json` as a bare array:** `search`, `jobs
  list`, `actions list`, `actions open`, `watch list`, and `watch digest` now
  return `{"<name>": [...], "truncated": bool}` envelopes — the same shape
  the MCP resources (`papio://jobs` and friends) already used — instead of a
  bare top-level array. `truncated` means the page filled its row cap — the
  default or a `--limit` bound — so more rows may exist; it is not proof that
  they do. An empty result is `[]`, never `null`. `jobs failures` was
  already an object (`{"failures": [...], "since": ...}`), never a bare
  array; its breaking change is narrower — the `since` metadata key is no
  longer re-emitted in the `--json` output (the daemon reply still carries
  it and the CLI still decodes it, so `--since` itself keeps working).
  Commands returning a single structured record (`jobs get`, `doctor`,
  `status`, `batch report`, `zotio plan`, `inbox`) are unchanged.
- `papio init` now points Firefox users at the published add-on listing
  (`https://addons.mozilla.org/firefox/addon/papio/`) instead of the
  temporary `about:debugging` load it recommended while the listing was in
  review. A non-default add-on ID still gets the development-build hint.

### Fixed

- `papio daemon status`, `papio browser sessions`, and `papio browser use` no
  longer fail with `unknown field "pending_browser_sessions"` when more than
  one browser is connected. The CLI's daemon-ping decoder was missing the
  browser-session-arbitration fields (added to the daemon in 0.8.0), so the
  strict version-check pre-hook rejected any ping that reported waiting or
  denied sessions.
- `papio --version` now works alongside the existing `papio version`
  subcommand, printing identical output.
- A failed discovery search no longer risks echoing the request URL — and with
  it the configured contact email and any backend API key — into the error
  message or the daemon log. Backend errors are now redacted to
  scheme/host/path before being reported, keeping search failures inside the
  same rule that already governs stored URLs.
- `papio search` no longer buries an exact-title match behind unrelated
  higher-cited papers. Results are annotated with `match_score` (0..1) and
  `match_kind` (`exact_title`, `title_phrase`, `title_tokens`, `weak`,
  `unscored`) so it's clear why a row ranked where it did; confident title
  matches are promoted to the front, best score first, and every other row
  keeps the order the backend gave it — a discovery backend weighs the
  abstract, concepts, and citation graph better than title comparison can on
  a keyword search, so *papio* only refuses to bury an obvious title match,
  never tries to out-rank the backend generally. Ranking now happens before
  `--limit` is applied, so a match the backend ranked low can still surface.
  A short-token query or a citation-snowball search (`--cites`/`--cited-by`/
  `--related-to` with no query) is reported `unscored` and deliberately left
  unreordered. The human-readable output now says so plainly when nothing in
  the result set matched strongly, instead of silently returning the closest
  unrelated papers.
- A failed inbox-counts or acquisition-stats query no longer disconnects the
  browser extension. Both handlers returned a raw error, and the native host
  treats any error out of a browser-bridge handler as a dead connection, so
  one failed aggregate tore down page acquire, the triage inbox, and the
  handoff flow along with it. They now answer with an ordinary error frame —
  the extension already renders that as a muted "unavailable" state — and log
  the cause for the operator. The stats query that was most likely to fail no
  longer scans `human_actions` in full once per acquired work either.

## [0.11.0] - 2026-07-24

### Added

- When the institutional OpenURL route reports no entitlement (or only
  document delivery), the daemon now re-enters resolving once and lets the
  existing sibling discovery look for an open-access copy of an alternate
  version before giving up. A durable `browser.no_entitlement_requeue` job
  event guards the retry: once the route proved empty, later no-entitlement
  reports — including from a rediscovered open-access handoff — park the job
  `unavailable` with terminal reason `no_entitlement` and the honest
  no-access guidance instead of offering that route again.
- A maintenance pass (run at startup and once a minute) heals handoff parks
  stranded by a crash mid-transition: an `awaiting_human` job with no open
  action, or one still offering an institutional route its own event history
  already disproved, is sent back to resolving to finish honestly.
- DOI-only submissions now get their title, authors, and year backfilled from
  one bounded OpenAlex lookup at resolve time, so inbox rows — especially
  identity-verification prompts — show what the paper is supposed to be
  instead of a bare DOI.

### Fixed

- Open-access works whose only candidate is a landing page (the publisher's
  DOI page rather than a direct PDF) now route to the browser handoff instead
  of a dead-end manual download. The daemon cannot fetch an HTML landing page
  as a file, but the extension's provider adapters resolve the PDF from that
  page — so an open-access landing on a supported provider is handed to the
  browser (no login) and downloaded automatically. Paywalled landing pages
  still park as a manual download or take the institutional handoff.

- `papio native-host install` now pins the browser native-messaging host to the
  invocation path (e.g. Homebrew's stable `/opt/homebrew/bin/papio`) instead of
  the fully symlink-resolved `…/Caskroom/papio/<version>/papio`. A `brew upgrade`
  deletes the old versioned directory, which previously dangled the host symlink
  and disconnected the extension ("could not establish a current daemon
  session"); pinning the stable launcher survives upgrades. `papio doctor` now
  also fails when the host executable a manifest points at is missing, with the
  exact remediation, instead of only validating the manifest JSON.

- Single-word author names (bare family names and mononyms) no longer sink
  metadata corroboration: Europe PMC and OpenAlex title-search matching and
  Crossref enrichment now compare them by name instead of rejecting the whole
  author list, so title-only requests with such authors can resolve.
- BibTeX/BibLaTeX imports now split authors separated by uppercase or
  mixed-case `AND`, matching BibTeX's case-insensitive separator, instead of
  ingesting them as one concatenated name.
- `papio actions open` launches the browser on Windows and Linux (default
  browser via `rundll32`/`xdg-open`) instead of failing on the macOS-only
  `open -b` command; macOS still pins Chrome.
- Development and pre-release builds (`X.Y.Z-dev`) no longer trigger false
  "update available" nudges: version comparison strips pre-release/build
  suffixes instead of parsing the component as 0.
- Adopting a browser download now resolves the handoff action it satisfied.
  Previously the "sign in to your institution" row lingered beside the
  verification prompt for the same paper, and dismissing that stale row
  cancelled the whole job — deleting the quarantined PDF that was waiting for
  human review.
- Dismissing a human action cancels its job only when the job is actually
  parked on that action (`awaiting_human` for handoff/manual-download rows,
  `needs_review` for identity verification). A stale action from an earlier
  state is closed without disturbing the job's live work.
- `papio actions open` no longer fails with a bare "exit status 1" when the
  browser cannot be launched: the error now says the browser handoff could not
  open and points at enabling the extension and `papio doctor`.
- `papio doctor` autostarts the daemon like ordinary daemon-backed commands
  (`daemon status` and `daemon stop` still never autostart) and runs its
  database integrity check through the daemon, so a stopped daemon no longer
  produces a FAIL/WARN/SKIP cascade — a genuinely unstartable daemon is one
  failure line plus one collapsed skip.
- Unknown verbs under command groups (for example `papio jobs show`) now fail
  with the list of valid verbs instead of silently printing nothing, and a
  bare command group still prints its help.
- Accepting an identity review now promotes the exact quarantined PDF you
  verified instead of discarding it, re-resolving, and downloading the work
  again — a flow that could silently drop the acceptance when a resolver did
  not re-offer the same URL. The reviewed file is hash-checked against the
  recorded review binding before promotion; a missing or altered file still
  falls back to a fresh fetch.
- The CORE and Crossref TDM resolvers work again: both rejected the daemon's
  hardened HTTP client as unsafe and had failed every request since that
  client was introduced.
- `papio ping` and triage RPCs no longer stall behind background network work:
  the daily update check and the retraction sweep both released their caches'
  locks only after their HTTP requests finished.
- A single failed DOI lookup no longer aborts the whole retraction sweep; the
  failed DOI keeps its previous notices and the sweep commits partial results.
- A transient `accept` error (such as running out of file descriptors) no
  longer shuts down the daemon IPC server and severs active CLI connections;
  accepts now retry with bounded backoff.
- Coalesced notifications delayed into a later window are no longer dropped:
  the flush timer detached from the request context that scheduled it.

## [0.10.1] - 2026-07-23

### Fixed

- Direct-distribution release manifests now record the bundled zotio version,
  rather than incorrectly repeating the papio release version.

## [0.10.0] - 2026-07-23

### Added

- **Zotero exception-tag ledger** (`zotio.exception_tags`, off by default):
  the daemon now answers "is this item coming, or is it mine now?" inside
  Zotero itself by maintaining two reconciled automatic tags on linked items —
  `papio:needs-action` (acquisition parked on a human action) and
  `papio:unavailable` (OA and institutional routes exhausted, as of the last
  attempt). Desired state combines each item's newest job with a live
  exact-key attachment check, so a PDF attached manually clears the marker.
  Schema v14 rolls the v13 applied-state ledger forward with personal-library
  provenance plus pending/owned/foreign/missing-target state; per-item
  mutations survive partial failures, serialize concurrent passes, and never
  claim or remove a same-name manual tag.
  `papio zotio tags reconcile` runs one pass on demand. Requires zotio ≥
  0.13.0 (`items tags add --automatic`, `items tags remove --automatic-only`);
  personal library only. Turning the feature off cleans up papio-owned tags.
  Design record: zotio `dev/adr/0004`.
- Backfill re-checks `unavailable` items after a cool-down
  (`zotio.unavailable_recheck_days`, default 14) instead of retrying them on
  every watch cadence — availability drifts upward (green-OA deposits,
  holdings changes, new adapters), and a `papio:unavailable` saved search
  doubles as an ILL/manual worklist only if it neither rots nor flaps.

### Changed

- `papio init` autodiscovers more of its own answers: the zotio executable is
  resolved from PATH and labeled with its source, prompt defaults name where
  they came from (`keep current` vs `from Zotero`), the Browser step names
  the supported browsers and lists those detected on the machine, the Chrome
  extension ID prompt and `--extension-id` accept an unpacked extension
  *folder path* and compute the ID from it (macOS/Linux; Windows requires
  the literal ID), and the browser setup instructions are suppressed when
  doctor reports a healthy, current extension connection.

### Fixed

- Upgraded `golang.org/x/text` to 0.39.0, fixing GO-2026-5970 (an infinite
  loop on invalid normalization input reachable through PDF inspection).

## [0.9.2] - 2026-07-22

### Fixed

- `papio doctor` no longer fails against a healthy daemon whose daily update
  check has results: the strict ping decode now accepts the optional
  `update_available`/`latest_version` fields (and their zotio counterparts).

## [0.9.1] - 2026-07-22

### Changed

- Doctor's extension check is floor-aware: a connected extension below the
  daemon's minimum version now WARNs with the exact skew and remediation
  instead of reporting a bare `connected` PASS while every handoff refuses.
- `papio init`'s guided flow was redesigned: numbered step sections with
  one-line explanations, one question per prompt, a single `[Y/n]` grammar,
  middle-ellipsized long defaults, per-browser setup instructions, a doctor
  summary that shows only checks needing attention, and a closing `Next:`
  action. Behavior and flags are unchanged.

## [0.9.0] - 2026-07-22

### Added

- **Institution discovery in `papio init`**: paste a library discovery URL or
  use the resolver configured in Zotero to derive the OpenURL base; scripted
  setup supports `--institution-url`.
- **`papio adapter diagnose <job-id>`** — sanitized, paste-safe support report
  for provider/adapter failures (job policy, actions, redacted event timeline,
  extension connectivity).
- **Library-completeness line in `papio status`**: when zotio is configured,
  status leads with `Library: N item(s) missing PDFs — papio acquire
  --from-zotio fills them` (or `Library: complete`), backed by a new
  `zotio.missing_count` RPC over zotio's local mirror.
- **`papio doctor --start`**: opts doctor into the same daemon autostart every
  ordinary command uses, so a first run can bring the stack up instead of
  reporting it down. Default behavior still diagnoses a stopped daemon.
- **`papio init` defaults the Chrome extension ID to the Chrome Web Store
  package** (`npccengdhjmpojpjmjoeeclpdhcjelhf`) — a store install needs no ID
  copying; only unpacked development builds still paste theirs.
- Triage snapshot schema 2 exposes institutional sign-in/open-access guidance
  only for actions classified by the acquisition waterfall; schema 1 remains
  available unchanged for older browser extensions.
- **Standard bibliographic ingestion for `acquire --batch`**: RIS, BibTeX,
  CSL-JSON, and MEDLINE/NBIB use one robust interchange pipeline for reference
  managers, database exports, and systematic-review tools.
- **Provider compatibility matrix**: a hand-maintained page records the
  individually live-verified provider routes, adapter status, and limitations
  without inventing aggregate success rates.

### Changed

- Doctor's daemon-unreachable report collapses the four dependent SKIP lines
  into one `integrations` line and names the actual problem ("not running or
  unreachable") with the `--start` remediation, instead of a six-line cascade
  for a stopped daemon.
- Opener failures in `papio actions open` now name the failing command even
  when it produced no output (no more bare `exit status 1`).
- Getting-started, README, and troubleshooting now document the Chrome Web
  Store install as the default path; unpacked loading moved under a
  development-install tab. Firefox remains a temporary add-on until the AMO
  listing clears review.
- **BREAKING:** Access mode `maximal` is now `delegated` in the config value,
  `--access-mode`, and wire enum. Existing configs must be updated by hand;
  stored job policies migrate automatically.

## [0.8.0] - 2026-07-21

### Added

- **`[hooks] on_ready`** — a generic post-acquisition hand-off: when a job
  reaches `ready`, the daemon runs a user-configured shell command once with
  the job's metadata in `PAPIO_*` environment variables (`PAPIO_DOI`,
  `PAPIO_PDF`, `PAPIO_TITLE`, …). Fire-and-forget with a durable
  `hook.on_ready` job event as the audit trail; file acquisitions into papis,
  Calibre, a plain folder, or any script without a bespoke integration
  (ADR-0004). zotio remains the deep Zotero path.
- **zotio is now optional**: an empty `zotio.executable` disables the deep
  Zotero integration instead of rejecting the config. Ownership lookup
  degrades to not-owned with a staleness warning so `batch submit` and
  watches keep working, `doctor` reports zotio as "not configured
  (optional)", and `zotio.auto_import = true` still requires the executable.
- **Browser session arbitration** (`papio browser sessions` / `papio browser
  use <id>|--latest`, +`browser.sessions`/`browser.claim` RPCs): with *papio*
  installed in more than one browser, exactly one session now holds the
  offer/handoff flow. Later hellos wait as pending (`session_busy`) instead of
  silently stealing the session; a holder silent for 10 s yields to a live
  pending session; a cleanly closing browser releases immediately. Session
  identity rides the daemon↔native-host envelope, so extensions of every
  version participate. `papio status` and `doctor` report waiting browsers and
  denied hellos, and the extension-version flap between competing browsers is
  gone (ADR-0003).
- **Handoff actions say whether a login is needed**: human actions carry
  `requires_auth` and `blocked_by` (`anti_bot`, `paywall`, `landing_page`),
  `papio actions list` prints "open access — no login needed" vs "sign in to
  your institution first", and batch reports classify handoffs from the
  structured fields (schema v11 migrates and backfills existing parked
  actions). The `job_offer` protocol message carries `requires_auth` for the
  extension.
- **Open-access sibling fallback**: when a DOI's own record yields no legal
  candidate — or every candidate it did yield fails to fetch — the OpenAlex
  resolver searches for open-access sibling versions (preprints or repository
  copies under a different DOI) with strict title/year/author matching and
  tries those before parking the job — a paywalled paper with a free
  SSRN/arXiv copy no longer requires a manual re-search.
- **OpenAlex works without an API key**: the resolver now runs in OpenAlex's
  free polite pool with just the configured contact email — matching the
  search/discovery client — and treats `sources.openalex.api_key` as optional
  premium capacity. `doctor` reports the polite-pool state instead of failing.
- **Stale-SSO handoff recovery**: the daemon records `browser.handoff_offered`
  and `browser.handoff_failed` job events (new `handoff_outcome` protocol
  message), and institutional handoff guidance now says to sign in first and
  simply re-open on a stale-session error — every open mints a fresh link.
- **`imported` terminal job state**: a successful `zotio apply` (manual or
  auto-import) advances the job `ready → imported` with the Zotero item keys
  on the transition, so `jobs list` and `status` stop presenting filed work as
  actionable; batch reports read the keys from the job itself.
- `--label` now works on single `papio acquire` (not just `--batch`), seeding
  the target collection when `--collection` is unset.

### Fixed

- `papio actions open` failures now name the URL and underlying error (with a
  `papio doctor` hint) instead of a bare `exit status 1`, and explain when
  open actions exist but none are openable from the CLI.

## [0.7.2] - 2026-07-20

### Fixed

- Scheduled backfills now scan the complete zotio missing-PDF queue and skip
  already-live request IDs without consuming the per-run cap, preventing one
  persistent entry from starving later work.
- Alert-watch digests now retain consumed identities as a durable seen set,
  merge title, arXiv, OpenAlex, and DOI aliases, preserve author names
  losslessly, recheck zotio ownership before acquisition, and serialize clear
  and acquire operations.
- Long failure reasons remain separate groups until after display truncation,
  so unrelated failures with a common prefix no longer collapse together.
- Semantic Scholar citation and reference snowballs now enforce year and
  open-access filters and reject ambiguous query-plus-snowball requests.

## [0.7.1] - 2026-07-20

### Fixed

- Browser-download adoption now waits for Firefox `.part` writes and their
  zero-byte target placeholders to settle before validating the final file,
  preventing valid PDFs from being quarantined mid-download.
- Accepting an identity review for a browser-adopted PDF now binds the override
  to that file's SHA-256 candidate key, so the scheduler's safe re-resolution
  path reaches `ready` for the same bytes instead of parking the same PDF for
  review again.

## [0.7.0] - 2026-07-20

### Added

- **`papio jobs failures`** (+`jobs.failures` RPC): groups jobs that did not
  complete on their own (failed, unavailable, needs-review, awaiting-human)
  by state, provider host, and terminal reason, with `--since` (Go durations,
  `Nd` days, or RFC3339) — so adapter and resolver work can be prioritized
  from real failure data.
- **`papio acquire --from-digest <watch> [--keys ...]`** (+`watch.digest_acquire`
  RPC): queues pending alert-watch digest entries as acquisition jobs through
  the same submission path watches use, preserving the recorded identifiers so
  deduplication holds; entries are removed only after their manifest is
  persisted and submission succeeded. `papio watch digest clear <id>`
  (+`watch.digest_clear`) empties a digest.
- **Structured webhook notifications**: webhook payloads now carry `event`,
  `watch_id`, `watch_label`, and `count` alongside `message` for routable
  receivers; plain notices keep the exact previous `{source, message,
  sent_at}` shape.
- **Click-to-acquire support**: the daemon accepts the new `page_acquire`
  browser-protocol message (advertised via the `page_acquire` `hello_ack`
  feature) and queues the page's DOI through the normal acquisition pipeline,
  answering with `page_acquire_ack` (job id or duplicate marker).

### Fixed

- `papio acquire --from-digest --keys` now treats every `--keys` value as one
  opaque work key: keys containing commas (title-derived digest keys) are no
  longer split into fragments that miss valid entries. Repeat the flag for
  multiple keys.
- `papio jobs failures --since` rejects negative durations (`-1h`, `-1d`)
  instead of silently computing a future cutoff that hid every failure.

## [0.6.0] - 2026-07-20

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

A triaged audit pass (33 confirmed findings fixed, each with a
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
  releases its lock during retry backoff; concurrent zotio plan applies of the
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
  cross-references between *papio* and zotio in both READMEs and docs, and
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

- ProQuest account-id unlock: on ProQuest's "Find your institution" wall, *papio*
  appends `?accountid=<id>` to the current URL, which unlocks Example University's
  institutional access with **no sign-in at all** (verified live 2026-07-18 —
  resolves the wall cold, "Access provided by EXAMPLE UNIVERSITY"). New
  per-institution config `[browser] proquest_account_id` (digits); the daemon
  passes it as the optional job-offer field `proquest_account_id` (default
  profile only); the ProQuest adapter gains `accountIdParam: "accountid"`, and
  on a `login` verdict *papio* appends it (latched, once) — preferred over the
  federated route since it needs no credentials. This is the fix for the
  ProQuest openurl-handler blocker (the Shibboleth-DS route authenticated only
  ProQuest's main context, not the link-resolver handler). Config + protocol +
  adapter + bridge are unit-tested; the full download still needs a live pass on
  a ProQuest-*held* title.

- Institution auto-selection ("login routing"): on a provider login wall, *papio*
  navigates the handoff tab straight to the institution's federated login,
  skipping the provider's institution picker — selection is deterministic config
  (which institution you're at), not a secret, so only credential entry stays
  with you. New per-institution config `[browser] shibboleth_entity_id` (the
  Shibboleth IdP entityID, e.g. Example University's `https://idp.example.edu/entity`); the
  daemon passes it to the extension as the optional job-offer field
  `login_entity_id` (default resolver profile only, to avoid mis-routing another
  institution's job); and an adapter gains an optional `federatedLogin` template
  (`{entityID}` placeholder). On a `login` verdict *papio* navigates once (latched)
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
  (`fixtures/sage/success.html`, captured live via CDP from an institutionally authenticated
  article). SAGE emits no Highwire metas; classifies on `publication_doi` + the
  `downloadPdfUrl` anchor (same shape as ACM) and downloads that anchor's
  `/doi/pdf/<doi>?download=true` href. Classify is fixture-verified; the
  end-to-end download is not yet live-exercised because Example University's resolver routed
  the SAGE test title to ProQuest rather than sagepub (the adapter fires when a
  title routes to journals.sagepub.com).

- Wiley Online Library adapter (`onlinelibrary.wiley.com`), fixture-backed
  (`fixtures/wiley/success.html`, captured from an institutionally authenticated article).
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

- Brand: a *papio* logo — an oblique lowercase **p** (coral `#E85D4A`) inside a
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
- Background work window: *papio* now does its browsing in one dedicated
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
  for a newer *papio* and name the offending fields, instead of surfacing a raw
  TOML parse error. zotio preflight failures name the installed version, the
  configured executable path, and the action that fixes the mismatch.
- MCP tool surface now derives from the *papio* CLI command tree instead of a
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
