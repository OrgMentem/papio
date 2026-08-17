# Adapter lifecycle at scale — capture, repair, release, contribute

Status: **Active implementation plan; subordinate to ADR-0021**. ADR-0021
(`Accepted`, 2026-08-10) is the authority for packaged behaviour,
daemon-first URL repair, and restrictive-only control. ADR-0015 remains the
positive-runtime companion only where ADR-0021 does not supersede it. This
document records implementation order, evidence, and acceptance gates; it is
not a second authority decision and no Phase-0 ADR is pending.

The sections below preserve the useful pre-ADR design history. Assertions
labelled **implemented Phase-0 authority** describe the code that exists now;
later phase sections remain targets and must not be read as shipped behaviour.

## Audit 2026-08-16 — verified against the tree, not against this document

- **SHIPPED** — Phase-0 authority, packaged adapters, fixture-backed classification — `extension/src/adapters/types.ts` (packaged registry), `extension/src/plan.ts` (Plan, expected-work binding, revalidation, planExecution/planGeneric)
- **SHIPPED** — generic E0/E1 planning and tuple correlation — `extension/src/plan.ts` planGeneric; `extension/src/background.ts` (planner injection, provider-drive epoch execution/correlation); `extension/src/state.ts` (opaque drive epoch, bounded restart bookkeeping)
- **SHIPPED** — provider-drive epoch authority and access-mode enforcement — `internal/browser/bridge.go`; contracts in `internal/protocol/protocol.go`
- **SHIPPED** — captures and local incident lifecycle with safety latching — `internal/captures/captures.go` (storage, pin/release, retention), `internal/incident/incident.go` (grouping, bounded failure-shape evidence)
- **SHIPPED** — adapter-try offline diagnostic — `extension/tools/adapter-try.ts`, which invokes the production planner
- **SHIPPED** — proposal-only repair scaffold — `internal/cli/adapter_repair.go` (daemon-listed/sanitized capture validation; proposals only)
- **OPEN** — Firefox transmission consent; redacted observation reporting; URL-template expansion; generator/release automation; signed remote control plane; contribution intake; staged rollout; broadened Zotero/generator phases — none present in the tree: no signed control plane, no hosted reporting pipeline, no automatic patch/release generator

Trim candidate: Phase-0 material shipped; later phases still live.

### Implemented Phase-0 authority (2026-08-10)

- The daemon mints and durably records direct and generic provider-drive
  tuples: `drive_attempt_id + ordinal + strategy + revision`. The extension
  persists only the opaque tuple/bookkeeping needed across an MV3 restart;
  direct route URLs and generic candidate URLs are not persisted as authority.
  Daemon restart reconstructs tuple state from the durable event history; an
  extension restart reconstructs only bounded browser-download correlation.
- Direct provider routes travel as daemon-minted
  `provider_direct_get_request` tuples. They are not inferred from
  `job_offer` URLs, hosts, or URL heuristics. The daemon accepts results only
  when the complete tuple is the currently offered, started tuple.
- `plan.ts`'s complete `Plan` is the sole page-side authority for a positive
  adapter effect. It carries `expected_work`, `effect_graph`, `route_origin`,
  and `revalidation` alongside the target and consequence. A packaged
  `DownloadRule.workTarget` is evaluated into the selected target's explicit
  `work_binding`; the background revalidates that complete plan in the live
  document before executing it.
- Generic candidates are page-derived and must have the page's exact HTTPS
  origin; a registrable-domain or sibling-subdomain match is not authority.
  Generic restart safety is correlation-first, not URL replay: candidate URLs
  and ordering are worker-local, and a restart cannot authorize an arbitrary
  stored URL. A fresh candidate requires fresh planning/revalidation and a
  current daemon epoch; stale, duplicate, or unstarted tuples remain assisted.
- ISBN catalogue/ebook handoffs are forced to `assisted`, even when the
  daemon-wide access mode is `delegated`, because the bridge cannot
  automatically validate a book PDF.
- OpenAlex title lookup is strict corroboration: exact normalized title,
  positive requested-year equality when a year is supplied, and one-to-one
  author-list equality by family plus given-name initial. Identifier lookup
  uses a canonical unique DOI/OpenAlex ID (a URL alone is insufficient), and
  the selected location must be explicitly open access.
- Repair-fixture sanitization and independent observational provenance remain
  future generator/intake obligations; Phase 0 local capture is not a hosted
  repair pipeline. Hosted incident retention and deletion are future work.
  Local capture pinning already implements role-scoped first/latest retention
  and release; hosted intake lifecycle remains future.

## Trimmed 2026-08-17

Sections describing shipped work were removed. The pre-trim text is recoverable in
full at `git show 2d29e7a:dev/active/adapter-release-latency-plan.md`. Cut: Why the
pre-Phase-0 loop did not scale (historical baseline), Options, 0. Daemon URL
intelligence — the fastest repair path.

## Decision (2026-08-10, operator-ratified; subordinate to ADR-0021)

Build, in this order of leverage:

1. **Daemon-side provider URL intelligence** — provider knowledge that is
   URL-shaped (direct PDF endpoint templates, resolver and route quirks)
   lives in the daemon. Direct provider routes are emitted only as
   daemon-minted `provider_direct_get_request` tuples; `job_offer` is not a
   direct-route authority and no URL heuristic may synthesize one. The
   extension release gates the feature and `access_mode`; after that,
   daemon route repairs deploy outside browser stores in **hours**. Generic
   page candidates remain page-side, but their positive attempts are admitted
   only by the daemon-minted provider-drive epoch.
2. **An automated adapter patch generator** (capture → candidate source change,
   fixtures, tests, revision bump, changelog, tag) feeding the existing
   dual-store release flow. Store review is "most extensions within a few
   days" and clients auto-update within hours, so mechanically generated
   DOM-level repairs land in **days**, not weeks.
3. **A small signed restrictive control plane** — suspend exact packaged
   revisions immediately; positive activation machinery is deferred until the
   registry work it needs is justified by measured rollout risk.

Do not distribute positive adapter behaviour as a runtime catalog in the
store-listed extension **for now**. This is an evidence-graded risk decision,
not a categorical policy fact — see "Store policy: what is actually
established" below. The remote-catalog pilot (Firefox-first), the Chrome
userScripts channel, and fast-lane distribution were evaluated and explicitly
deferred; their evidence and revisit triggers are recorded in "Deferred
alternatives".

The release-class split (designed to fit documented store policy; store review
of each class remains unproven until exercised):

| release class | remote control | positive repair path |
|---|---|---|
| operational suspension | durably disable an exact packaged revision or safety domain | immediate online-signed control update |
| permanent safety revocation (deferred with offline root) | monotonically revoke an exact packaged revision or domain | offline-root-signed control update |
| packaged activation (deferred) | select an exact installed revision already permitted by its packaged rollout eligibility, safety domain, and effect contract | signed selection — built only when measured rollout risk justifies the registry, after a policy pilot |
| daemon route-template repair | daemon-side; extension executes packaged primitives on offered URLs | daemon release/config update — no store involvement |
| DOM discovery/locator repair | none until the repaired revision is packaged | generated source change → existing Chrome/AMO release flow |
| click, follow-up, login, terms, account navigation | none until packaged | generated source change → store review → same-contract auto-release or effect-contract-delta review |
| new host, method, endpoint family, permission, protocol, or engine logic | none | normal extension feature release |

This preserves the user experience the catalog was meant to provide:

- an unsafe or drifting adapter can stop immediately without waiting for a
  browser-store review;
- capture, diagnosis, patch generation, tests, versioning, and submission are
  automated;
- once the multi-revision registry exists (deferred), repaired code can ship
  inactive and progress incident test → cohort → stable without another
  extension release;
- delegated jobs retry automatically after a valid transition; and
- a browser profile that granted the required automatic-reporting tier is not
  prompted again for every failure.

The unavoidable store latency is review of **new packaged DOM behaviour**.
That is a verified distribution constraint, not a reason to add
human friction elsewhere.

## Measured failure classes (this installation, 2026-08-10)

From the live dev database (552 jobs; single-operator, includes dev noise):

| class | count | implication |
|---|---|---|
| succeeded (`ready`+`imported`) | 206 | 106 needed browser handoff, 100 pure OA/daemon — the daemon path already carries half of all successes |
| `unavailable: no_identifier` | 126 | largest failure class; pure daemon metadata-resolution work, untouched by any adapter mechanism |
| user-cancelled/dismissed | 164 | mostly dev noise |
| `awaiting_human` | 27 | parked on human action |
| `browser.provider_outcome: ui_changed` | 83 unique jobs (119 events; 62 jobs saw it once) | adapter drift/unrecognized pages dominate browser-side failures — 83 unique jobs vs 5 `wrong_work` and 6 `no_entitlement` — the repair-latency target |
| `wrong_work` / `no_entitlement` | 5 / 6 unique jobs | identity validation and entitlement walls |

Counted by unique job, not raw events, so repeated re-drives of one broken
page do not inflate the case. Two consequences. First, the highest-leverage
browser-side fix is repair velocity for `ui_changed`, which daemon URL
intelligence and the patch generator both attack. Second, the single biggest
overall win — `no_identifier`, 126 jobs — belongs to daemon metadata
resolution: it is out of this plan's scope and is the explicit **next
priority after Phase 1**, ahead of Phase 2 control and Phase 3 intake, for a
single-installation deployment.

## Store policy: what is actually established (2026-08-10)

Verbatim policy ([Chrome MV3 requirements](https://developer.chrome.com/docs/webstore/program-policies/mv3-requirements)):

- **Allowed:** "The extension may reference and load data and other information
  sources that are external to the extension, but these external resources must
  not contain any logic," and "Fetching a remote configuration file for A/B
  testing or determining enabled features, where all logic for the
  functionality is contained within the extension package."
- **Violation:** "Building an interpreter to run complex commands fetched from
  a remote source, even if those commands are fetched as data."
- **Sanctioned remote logic:** only via documented APIs — Debugger and
  [userScripts](https://developer.chrome.com/docs/extensions/reference/api/userScripts)
  (user enables "Allow User Scripts", Chrome 138+).

Enforcement record (the policy text does not define the config-versus-commands
boundary; these do):

- **AdGuard MV3** went through [five CWS review rejections](https://adguard.info/en/blog/review-issues-in-chrome-web-store.html)
  (2024–25): remote filter code, then remotely downloaded "Quick Fixes" rules,
  then even packaged scriptlets taking remote parameters were all rejected;
  Chrome's offered resolution was fast-track for remote **DNR-only "safe
  rules"**. Enforcement line in practice: remote data may block/hide traffic;
  it may not parameterize page-level execution.
- **uBO Lite** ships all rules in the package and "never makes network requests
  to any remote servers" ([FAQ](https://github.com/uBlockOrigin/uBOL-home/wiki/Frequently-asked-questions-(FAQ)));
  full uBlock Origin on **Firefox** auto-updates remote filter lists including
  cosmetic CSS selectors — AMO tolerates remote DOM-selector data.
- **Stylus** (CWS MV3, min Chrome 128) auto-updates remote user-installed CSS —
  page-affecting remote data is accepted when user-initiated.
- **Zotero Connector**: the shipped Chrome build is still MV2; its MV3 design
  ([offscreenTranslate.js](https://github.com/zotero/zotero-connectors/blob/master/src/browserExt/offscreen/offscreenTranslate.js),
  open PR #632) evals remote translator **code** in a sandbox page precisely to
  avoid store-release cycles ("that is an untenable situation for us").
  Whether CWS review accepts it is unresolved — "Zotero already does this on
  Chrome MV3" is NOT established. On AMO, remote translator JS has shipped for
  years without enforcement.
- Review latency: "for most extensions, review is completed within a few days"
  ([review process](https://developer.chrome.com/docs/webstore/review-process));
  Chrome checks for extension updates on startup and every few hours
  ([update lifecycle](https://developer.chrome.com/docs/extensions/develop/concepts/extensions-update-lifecycle)).

Graded verdict: a remotely updated selector/action catalog for authenticated
page actions is **gray, leaning adverse on Chrome** (INFERENCE from the AdGuard
record), **plausible on Firefox** (uBO/Zotero practice), and **not** flatly
prohibited as data. Mozilla prohibits remote *code* and permits userScripts
only for user-script managers; it does not expressly classify behavior-driving
remote data ([AMO policies](https://extensionworkshop.com/documentation/publish/add-on-policies/)).

## Deferred alternatives (evaluated 2026-08-10, not chosen)

Recorded so the conservative position is a documented decision with revisit
triggers, not a baked-in assumption:

1. **Remote declarative catalog pilot (Firefox-first).** Strongest upside:
   hours-level positive repairs. Revisit when the measured store-release repair
   latency (capture → user-installed fix) exceeds what users tolerate, or when
   Zotero's MV3 build ships and survives CWS review — that outcome is a live
   natural experiment directly on point.
2. **Chrome userScripts channel.** Explicitly policy-sanctioned remote logic;
   costs a one-time per-user browser toggle and explicit, listed, removable
   adapter-pack installs (Tampermonkey model; AMO restricts the API to
   user-script managers, so Chrome-only). Revisit if a catalog pilot is
   rejected on Chrome but remote repair latency remains the binding constraint.
3. **Fast-lane distribution.** Mozilla-signed self-distributed XPI beta channel
   (automated unlisted signing) and documented dev-mode unpacked Chrome for
   power users. Cheap; adopt opportunistically when a user asks.

## Target architecture (forward-looking beyond the implemented Phase-0 slice)

```mermaid
flowchart LR
    P[Provider page] --> A[Packaged adapter or generic strategy]
    A --> S[Safety-domain gate]
    S --> X[Complete page-side Plan]
    X --> B[Browser action executor]
    B --> V[PDF and work-identity validation]
    X --> O[Redacted EffectObservation - future]
    V -->|unknown / ui_changed / rejected| C[Local capture + observation]
    C --> D[Fingerprint + deduplicate]
    D --> R[Deterministic repair generator]
    R --> T[Historical corpus + effect-contract delta]
    T -->|source candidate| E[Extension PR/version/tag]
    E --> W[Chrome + AMO review]
    W --> I[Installed packaged revision]
    I --> K[Signed staged activation - deferred]
    V -->|unsafe effect| Q[Domain circuit breaker]
    Q --> U[Signed suspension or revocation]
```

The architecture has seven boundaries:

1. **Execution planning — implemented Phase 0.** `planExecution` returns the
   complete page-side `Plan` or an assisted result. The plan binds
   `expected_work`, its `effect_graph`, `route_origin`, and bounded
   `revalidation` limits. A packaged `DownloadRule.workTarget` is not merely
   a source hint: it must produce the selected target's explicit
   `work_binding` (or the planner returns assisted). Full URLs and live
   targets
   remain memory-only. `executePlannedPageEffect` is not allowed to infer a
   missing authority field from the adapter or current DOM.
2. **Observation — future.** A separate redacted `EffectObservation` may
   record enough action semantics to reproduce and classify a failure without
   becoming a reusable action. It is not a shipped Phase-0 authority object.
3. **Safety domains** — adapters and generic strategies share explicit packaged
   domains; a failure determines which fallbacks remain eligible.
4. **Source representation** — current store-bundled CSS rules remain initially;
   a locator AST is introduced only from measured need.
5. **Adapter patch generator** — future deterministic generation, corpus replay,
   effect-contract classification, and release preparation.
6. **Control plane** — ADR-0021 permits signed state to suspend exact
   packaged IDs/domains; positive selection remains deferred and control never
   names selectors or actions.
7. **Contribution and coverage inputs** — future minimized reports and
   upstream translator evidence create source candidates, never runtime
   authority.

## Invariants

1. **Wrong-work adoption remains the worst outcome.** Every generic and adapter
   path crosses the PDF identity validator. Autonomous downloads also require
   pre-action expected-work evidence appropriate to their effect class.
2. **A safety failure cannot increase automation.** Wrong-work, validation,
   unexpected-effect, or envelope failures put the job/domain into
   `no_positive_effects`; generic fallback is ineligible. Selector miss or
   ordinary UI drift may fall through only to an explicitly same-or-lower-risk
   packaged generic domain.
3. **Plan is page-side authority.** Tests and reviews consider the complete
   planner result — expected work, effect graph, route origin, revalidation
   limits, target, exact effect contract, and observed result — together.
4. **One planner owns action invariants.** Offline tools invoke the same
   self-contained planning function as live page injection. A third
   reimplementation is a bug.
5. **Execution revalidates the plan.** On mutation between planning and action,
   rerun the planner, discard the old plan, and execute the fresh plan when
   exact work, unique target, effect contract, origin/path envelope, and access
   authority remain unchanged; otherwise fail assisted. A refreshed signed
   query or SPA-replaced DOM node is not itself an authority change.
6. **Recorded access policy gates every effect.** The extension must consume
   negotiated per-job `access_mode`; parsing but ignoring it is not authority.
7. **Generated rules contain no free-form JavaScript.**
8. **Runtime control names only packaged IDs.** It cannot carry hosts,
   selectors, paths, methods, predicates, text, thresholds, or executable data.
9. **Restrictive safety state is durable and fail-closed.** The daemon holds
   canonical verified control; the extension persists only
   `{last_sequence, last_verified_suspensions_digest}` in dedicated
   `storage.local`. A suspension persists until a higher verified sequence
   lifts it; MV3 restart, expiry, offline operation, or pinning cannot relax
   accepted state. Permanent revocation arrives only with the deferred
   offline root. If daemon state disappears or rolls back below the
   extension's sequence, no positive effect executes until current control
   is recovered.
10. **Protocol traffic remains solicited, feature-gated, and non-fatal.** No
    new field widens a strict existing frame, and an inbound handler never
    awaits a request whose reply must traverse the same serialized chain.
    Every new browser handler converts application, store, projection, and
    self-validation failures into a correlated structured result; only framing,
    peer-protocol, or unusable-stream failures return a fatal error. A failed
    observation or control request must be followable by a successful unrelated
    request on the same native session.
11. **The extension makes no independent OrgMentem request.** Update and
    reporting traffic belongs to the daemon and is disclosed separately.
12. **Reporting consent is durable but profile-scoped.** Each Chrome/Firefox
    profile authorizes `structural` and optional `rich_capture` transmission
    tiers once; routine failures do not reopen the decision.

## 1. Split execution from observation

**Superseded pre-Phase-0 diagnosis:** the former model separated
classification from later execution, duplicated URL extraction, and parsed
`job_offer.access_mode` without consuming it. The implemented replacement is
the single complete `Plan` below, injected by `background.ts` and checked
again at execution.

### Memory-only `Plan` (implemented Phase 0)

`planExecution(page, packagedRevision, expectedWork, accessPolicy)` returns a
complete `Plan` or an assisted result:

The `Plan` remains in memory and contains everything the executor must
revalidate:

- adapter/generic strategy and immutable revision IDs;
- `safety_domain_id` and `effect_contract_id`;
- current page origin and route family;
- access mode and any already-recorded consent/configuration relied on;
- complete `expected_work` evidence (requested DOI/title plus page evidence
  fingerprints);
- decisive guard and exact-one target;
- the `effect_graph` (primary/follow-up/terms targets, API result binding,
  consequence, and route);
- `route_origin` and the bounded `revalidation` limits; and
- exact action class and full resolved URL, including query values when the
  request needs them.
The full URL, query values, live target, and credentials never enter an event,
capture, report, control document, or log. Before any adapter effect,
`background.ts` reruns the planner against the live document and requires the
fresh plan's verdict, rule, target, expected work, effect graph, route origin,
access mode, and revalidation limits to match; any authority-relevant
difference stays assisted. Browser APIs such as `chrome.downloads`, tab
creation, and downloads remain in the background executor rather than the
pure planner.

### One injectable implementation

`executeScript({func})` serializes a function without imported closure state.
Therefore `planExecution` is one explicitly injectable function whose required
helpers are nested or passed as serializable data. `background.ts` injects it;
`adapter-try` invokes that exact function against happy-dom. Only the
background-owned executor performs side effects.

### Access-policy prerequisite

Consume the existing `job_offer.access_mode` — it is already negotiated per
job, only-narrowing, re-applied on read, and delivered in the offer. Parsing
but ignoring it is the gap, not a missing protocol field. Semantics:

- `assisted`: may classify and open/focus, but performs no E1–E3 effect;
- `delegated`: may perform contract-authorized effects;
- `conservative`: receives no provider offer under current behaviour.

Add a new protocol field only if these semantics prove genuinely insufficient.

### Effect classes and contracts

Every packaged revision carries a canonical `effect_contract_id` derived from
action class, HTTP method, origin/path envelope, cardinality requirement, expected
consequence, follow-up/consent policy, safety domain, and access-mode floor.

| class | examples | repair automation |
|---|---|---|
| E0 non-mutating discovery | read metadata or inspect a packaged route | deterministic generation and automatic source release |
| E1 fixed authenticated GET/download | exact HTTPS anchor/API/meta URL with exact work identity | automatic source release after deterministic/live evidence |
| E2 page mutation | click a control, open a viewer, wait for a consequence | automatic source release after live evidence; staged activation deferred with the registry |
| E3 chained/consent/auth | follow-up click, terms, login, account navigation, form interaction | automatic source release only with recorded access/consent and live evidence; a contract delta requires review |
| E4 new capability | host authority, endpoint family, action method, permission, protocol, engine logic | normal feature design/release |

For the implemented generic engine, an exact expected DOI corroborated by
page evidence is required before E1; the current `ExpectedWork` contract does
not claim generic PMID/arXiv support. Title similarity remains E0 discovery
evidence only. Adapter E1 plans bind requested DOI/title to page evidence
fingerprints in `expected_work`; a missing, ambiguous, or mismatched binding
returns assisted rather than authorizing a download.

## 2. Source repair representation — do not block on a new DSL
This section is a forward-looking repair-generator design, not a claim that
the generator, locator AST, or generated fixtures ship in Phase 0. The current
authority is the packaged adapter plus the complete `Plan`.

The current CSS-based adapter schema is source-controlled, store-reviewed, and
already exercises the production `interpret` function. A custom locator AST is
not a prerequisite for the adapter patch generator and does not by itself make
a selected action safe.

Start by making the current representation mechanically safe:

- resolved targets pass the complete `Plan` rather than gaining authority from
  selector syntax;
- selector complexity, text, candidate, and wait budgets are bounded;
- text matching never authorizes an action target by itself;
- URL facts normalize before comparison;
- ordered rules that produce different effects are classified as an effect
  change; and
- the duplicate `extractDownloadURL`/`extractMetaURL` logic is folded into
  nested helpers inside the exported, self-contained `planExecution` function;
  `background.ts` injects that function and `adapter-try` calls it directly,
  without imported closure state that serialization would drop.

The deterministic generator may emit a minimal CSS source change immediately;
it is still packaged and store-reviewed. Introduce a finite locator/guard AST
only after measured repair candidates show that it improves generation,
reviewability, or invariant enforcement. If introduced, keep it a
source/build-time representation, define packaged structural regions, disallow
arbitrary regex escape hatches, and migrate incrementally. Do not hold the
capture-to-source fast path behind a tree-wide adapter rewrite.

## 3. Redacted execution observations (future intake design)

Sanitized HTML is useful but loses facts that determine browser consequences.
The persisted/uploadable record is not the `Plan`. A future
`EffectObservation` would be a strictly smaller record constructed from
allowlisted facts after planning and execution. Phase 0 does not claim that
new observation frame or repair-fixture pipeline is shipped.

A future observation may contain:

- packaged revision/strategy, safety-domain, and effect-contract IDs;
- normalized route family with identifiers removed;
- expected-work evidence kind and boolean result, never the identifier/title;
- candidate count and target tag/role class;
- allowlisted attribute **names**, never raw ids/classes/values;
- destination origin class and normalized path shape, never a full URL;
- HTTP/action class and expected consequence;
- whether an expected modal/follow-up existed before or appeared after action;
- tracked navigation/new-tab/download/adoption correlation;
- provider outcome and PDF/work-validation result; and
- keyed local pattern hashes when cross-event comparison needs equality without
  publishing the raw pattern.

It excludes URLs, query keys/values, cookies, credentials, session tokens, form
values, raw IdP routes, titles/authors/DOIs, DOM text, CSS selectors, and target
handles. A pattern hash uses a key local to the installation or incident; a
public hash of a low-entropy class/id string is not anonymization.

The existing `provider_outcome` and `page_capture` frames are strict
contracts. A future `provider_effect_observation` message may be sent only
after the daemon advertises its feature; it must not widen either existing
payload.

### Incident fingerprint (current grouping; future upload deduplication)

The current incident package computes a keyed failure-shape fingerprint from
the bounded `safety_domain`, registrable `host_family`, outcome, and sorted
decisive marker classes. Raw hosts, URLs, and work identifiers do not enter the
fingerprint. The local operator aggregate intentionally exposes those bounded
failure-shape labels (`safety_domain` and `host_family`) alongside the outcome
and evidence window; that output is not raw host/identifier disclosure.
Adapter and extension revisions are evidence facets, not the primary identity,
so one provider redesign can group across releases without publishing article
identity or low-entropy page strings.

A future intake may add a local content digest for exact report deduplication;
that is not a Phase-0 transmission or retention claim.

### Old evidence and retention

The incident package currently treats the first provider outcome as the
immutable decisive boundary for its compatibility helper. For each decisive
outcome, it walks backward only to the nearest compatible `page_capture`
within the same `drive_attempt_id` (when epochs are present), matching
adapter identity/version and stopping at the previous provider outcome.
Older or mismatched captures cannot rewrite that outcome, and labels or
identifiers are never guessed.

Its aggregate exposes `first_seen` and `last_seen` from decisive outcome
timestamps across jobs. Those timestamps are an evidence window, not proof
that raw captures are retained. The local capture store does implement raw
capture pinning: provisional captures are retained as first/latest evidence
by role before ordinary age/count pruning, and `PinIncident` replaces only the
latest marker while preserving the first marker. `ReleaseIncident` and
`ReleaseJob` remove the markers; the next `Sweep` applies normal eviction.
`pins_test.go` covers burst survival, latest replacement, provisional
pre-outcome retention, and release followed by sweep. This local lifecycle is
implemented Phase-0 behavior; hosted retention and deletion remain future
intake work.

Future incident intake must carry the immutable first boundary and latest
compatible evidence into its consented/minimized bundle without allowing later
evidence to rewrite the first boundary, and must define hosted retention and
deletion. Those hosted guarantees are not current Phase-0 claims.

### Safety-domain circuit breaker

Every packaged adapter/generic strategy names a `safety_domain_id`. Map failures
before selecting fallback:

- selector miss, ordinary `unknown`, or UI drift latches locally at
  `(job, revision, route_family, page_shape)` — one selector miss must not
  suspend a whole provider adapter — and permits only explicitly
  same-or-lower-risk packaged generic strategies in that domain; global
  suspension requires signed control or maintainer action;
- wrong-work, unexpected effect, work/PDF validation failure, or envelope
  violation records a daemon-durable `(job, safety_domain)`
  `no_positive_effects` latch — deliberately WITHOUT a page-shape dimension,
  so navigation, SPA mutation, strategy changes, extension restart, or
  registry changes cannot restore positive automation for that job/domain. It
  clears only on an explicit human retry decision or the job's terminal
  outcome. It never falls through to generic automation.

"Assisted" is not a job state; latched failures land in existing transitions:

- a correlated `wrong_work`/unexpected effect follows the existing
  `awaiting_human` + `manual_download` transition;
- a validation failure follows the existing candidate rejection or
  identity-review transition;
- an uncorrelated envelope violation mutates no job and fails the peer session;
- the latch restricts browser effects only — unrelated OA/API candidates for
  the same work continue.

Phase 1 enforces the latch entirely through these existing job transitions; no
control protocol or registry is required. Once latched, that job/domain is
never offered another positive browser drive. Tab reclassification, MV3
restart, or a repeated provider outcome cannot spin it. This local automation
needs no telemetry or hosted service. Signed global suspension arrives in
Phase 2.

## 4. Adapter patch generator (future; local repair scaffold shipped in Phase 0)

Phase 0 ships a local `papio adapter repair <capture-id-or-path>` scaffold. It
accepts only daemon-listed, daemon-sanitized capture input, verifies the
capture's hash and sanitizer metadata, and creates a bounded local
`dev/scratch/repair/<provider>-<timestamp>` workspace containing the exact
fixture bytes, a report, and review/apply instructions (plus local
`adapter-try` analysis when available). The workspace is proposal-only:
review is required before any source or fixture change, and the command does
not provide hosted intake, automatic generator/catalog publication, store
submission, or trust/promotion without review. Those hosted, generator, and
release-automation capabilities remain future work.

The future expanded generator contract uses this same local command surface:

Add:

```text
papio adapter repair <capture-or-incident>
```

The command:

1. freezes and reproduces the old packaged revision’s result;
2. loads every historical capture and independently labelled fixture for the
   provider;
3. deterministically enumerates the smallest bounded selector/source changes;
4. invokes the real `planExecution` function for old and proposed revisions;
5. compares `effect_contract_id`, safety domain, and observed consequence across
   provider and adversarial corpora;
6. assigns E0–E4 and the required evidence/review class;
7. creates only fixtures whose labels follow independently observed outcomes;
8. bumps the immutable adapter revision;
9. prepares the source, tests, changelog, extension patch version, and release
   evidence; and
10. emits a PR-ready diff and exact commands/results.

AI ranks ambiguous candidates and explains failures after deterministic
enumeration. It does not invent fixture labels, widen an effect class, or decide
that a failing gate is harmless.

### Corpus gates

Every positive repair replays:

- all historical success, login, terms, no-entitlement, wrong-work, and drift
  captures for that provider;
- shared references, recommendations, issue lists, purchase controls, cookie
  notices, unrelated PDFs, duplicate targets, stale SPAs, and malicious-link
  fixtures;
- expected-work identity boundaries;
- origin/path and query normalization;
- target uniqueness and execution revalidation;
- unchanged `effect_contract_id` and safety domain for any “same-contract” claim;
  and
- the previous revision, so the report shows exactly what changed.

The adapter under repair never labels its own new oracle. If a real outcome is
unavailable, the candidate stays unpromoted rather than fabricating a fixture.

### Release preparation

Drive the existing `ext-v*` workflow rather than replacing it:

- bounded candidates create a source PR with generated evidence;
- configured repository policy may auto-merge/tag same-contract E0–E3 repairs
  after deterministic gates and required live evidence pass; store review
  remains the external gate, not a maintainer queue. For E2/E3 the
  preconditions are explicit: the **authority-bearing action rule is
  unchanged** — the repair touches only recognition, guards, waits, or
  consequence observation — plus full corpus pass including adversarial
  negatives, live consequence evidence from the reporting installation, and
  a baseline that is already broken (the repair competes with "parked for
  everyone", not with working behaviour), with the circuit breaker and
  signed suspension as the post-release backstop. A repair changing the
  decisive action target, form/control identity, consent control, account
  navigation, purchase/request control, or submitted values is an
  **authority-contract delta** and requires maintainer effect review — one
  institution's live layout cannot prove another layout's control at the
  same structural position is not "Accept terms" or "Purchase". An E2
  download control reducible to an exact GET envelope is reclassified E1
  rather than retained as a click. (The third review pass wanted review for
  all E2/E3; the fourth accepted this narrower authority-rule boundary.)
  Flip trigger: wrong-work adoption OR any unexpected authenticated effect
  OR unintended terms/form/account/purchase/request mutation caused by an
  auto-released repair converts the class to review-required until staged
  rollout ships;
- a release bot prepares the patch version, changelog, tag, and existing
  dual-store submission;
- an effect-contract delta or missing consequence evidence routes to maintainer
  review;
- E4 uses the normal feature process; and
- while review is pending, the broken revision stays locally latched; daemon
  URL candidates and safety-domain-eligible packaged generic strategies run
  before any job parks for a human.

The authorization to auto-merge/tag is a durable repository policy. It does not
ask the end user to approve routine repair mechanics.

## 5. Signed adapter control plane

First control version (Phase 2) is deliberately small: one online *papio*
control key that can **suspend** exact packaged revision IDs and safety
domains and **lift a suspension with a higher sequence** — nothing else. A
compromised online key can deny automation temporarily; it cannot
permanently brick packaged revisions across reinstall and recovery.
Permanent revocation belongs entirely to the deferred offline root; do not
persist a `revoked_ids` set until that design exists. Everything else in
this section — multi-revision registry, staged activation, incident tests,
offline root — is **deferred** until measured rollout risk justifies it.

### Packaged registry (deferred with activation)

Control is safe only after the extension can prove what it already contains.
When activation is built:

```text
byRevision: revision_id → AdapterSpec
compiledDefaultByAdapter: adapter_id → revision_id
activeByAdapter: adapter_id → revision_id | disabled
incidentTestByJob: job_id → revision_id
bundle_id
inventory_digest
```

CI rejects duplicate revision IDs, duplicate `(adapter_id, host matcher,
priority)` tuples, ambiguous default host precedence, invalid defaults, and
bundle/active maps above explicit protocol and storage caps.

Each revision packages:

- adapter id/revision and safety-domain id;
- canonical effect-contract id;
- packaged rollout eligibility (`default`, `cohort`, `fallback`, `revoked_at_build`);
- compatible engine/protocol floors; and
- explicit generic fallback IDs, if any.

Control selects only installed revision/strategy IDs allowed by those packaged
fields. The extension atomically computes a new active-registry version,
validates every reference and cap, persists it, then releases affected jobs.
A tab drive holds the active-registry version it started with; it cannot mix
old classification with a new executor.

`activeByAdapter` is the stable/cohort selection. An incident-scoped
packaged-revision test is a bounded `incidentTestByJob` override created only
after the daemon proves the private incident grant; the published document
carries its one-way commitment, not the job id or bearer receipt. The override
is removed on terminal outcome, expiry, or local circuit break and cannot
affect another job. One incident job is a contained test, not a canary cohort.

The legacy `hello.adapter_versions` map remains optional and capped at 50. It is
never truncated: once the active registry cannot fit, omit it and require the
new inventory/control feature.

### Control document

The signed document may contain only:

- schema version, monotonic sequence, issued/expiry timestamps, key ID, and
  minimum bundle/extension versions;
- reversible suspensions and higher-sequence lifts;
- permanent revocations (deferred with the offline root);
- exact packaged revision/strategy activation for stable or cohort audiences
  (deferred with the registry);
- safety-domain `no_positive_effects` restrictions;
- incident/fixed-version notices; and
- previous digest for audit lineage.

It cannot contain hosts, selectors, locators, paths, methods, predicates, text,
thresholds, waits, URLs, or action parameters. Set explicit caps on directive
count, affected IDs, cohorts, and serialized bytes.

### Signing roles

- Phase 2: one online *papio* control-key signs restrictive directives; clients
  persist anti-replay sequence and the last accepted suspension sequence and
  digest; verification covers
  exact bytes before semantic parsing; PR/store-submission jobs cannot access
  the signing role.
- Deferred: an offline root that authorizes/rotates keys and signs permanent
  revocations, once activation raises the value of a stolen online key.
  Compromise of the online key can only reduce automation (suspend) in
  the first version; after activation exists it can still select only
  packaged, rollout-eligible behaviour.

### Lifecycle and solicited protocol

Per browser session, the bridge keeps:

```text
control_state = pending | applied | failed
bundle_id
inventory_digest
applied_control_sequence
active_registry_version
```

After `hello_ack` advertises `adapter_control_v1`, the extension starts — but
does not await inside the serialized inbound handler — this correlated exchange:

```text
adapter_control_request
  { bundle_id, inventory_digest, last_control_sequence }
adapter_control_response
  { control_sequence, directives, required_adapter_ids }
adapter_control_apply_result
  { control_sequence, active_registry_version, applied, missing,
    active_required_versions, outcome }
```

All messages land in Go, TypeScript, and the schema together. Missing,
incompatible, or stale state returns a structured result; it never raises the
routine error that would kill native messaging.

`hello_ack` and normal poll work can share one `Sync` response today, so the
daemon withholds provider `job_offer`s until the holder reports `applied`.
Control-absent startup has one rule:

```text
last accepted sequence = 0 and bundle minimum = 0
    → packaged defaults may run
last accepted sequence > 0 or bundle minimum > 0
    → missing control means no positive effects
```

A fresh install is never bricked by an unreachable control endpoint, and an
installation that has accepted restrictive state can never roll back past it.
New-daemon/old-extension sessions remain connected but receive no provider
offers once control is mandatory. New-extension/old-daemon sessions surface
`control_required` and stay assisted.

Each Chrome/Firefox session applies control against its own bundle. Pending
sessions never inherit the holder’s registry. Holder switch and MV3 restart
repeat the exchange before positive work.

### Durable state, expiry, and rollback

The daemon persists canonical verified control. The extension persists only
`{last_sequence, last_verified_suspensions_digest}` in dedicated
`chrome.storage.local`, not the session backend. Suspensions and activation
maps come only from the highest verified sequence; a higher online-signed
sequence may lift a suspension. Permanent revocation is deferred with the
offline root and, once built, unions monotonically and can never be lifted by
runtime control. On missing, corrupt, or lower-sequence daemon state, the
extension executes no
positive effect until current control is recovered.

Expiry/stale network state has asymmetric semantics:

- once the deferred offline-root phase exists: revocations persist
  unconditionally; until then, suspensions persist until a higher
  verified sequence lifts them;
- stable activation persists only while still compatible and non-revoked;
- an expired incident test selects the highest eligible, non-suspended
  packaged stable revision; if none exists, the adapter is disabled;
- missing or unknown IDs fail assisted; and
- rollback publishes a higher sequence rather than restoring an old document.

### Transition repair

Do not overload `HandoffRepairer.RepairAdapterUpgrade`. It assumes semantic
version increase and its existing latch keys on `(job, adapter_id,
new_adapter_version)`, so rollback/disable and re-activation can be suppressed
incorrectly.

Add a separate `RepairAdapterTransition` keyed by a unique signed-control event
id plus `(session/bundle, adapter_id, from_revision, to_revision|disabled)`.
It handles activation, suspension, rollback, and holder change; scopes affected
parks by browser session/capability; rolls back all state on transaction error;
and records the latch only after a successful transition. Old events without
adapter identity do not participate automatically. A maintenance command may
retry a failed transition.

Retain `RepairAdapterUpgrade` only for real extension/bundle upgrades.
Automatic downgrade across extension versions remains out of scope: an
operator-installed older bundle fails assisted for unavailable revisions.

### Staged rollout (deferred; designed to fit documented policy, review unproven)

When the registry exists, a repaired extension contains the new revision
inactive:

1. suspend the broken old revision online;
2. let the store-reviewed update install;
3. activate the new revision only for the incident job that supplied live
   evidence (an incident-scoped packaged-revision test);
4. advance to a deterministic local cohort, then stable;
5. retry already-delegated jobs through `RepairAdapterTransition`.

Use a per-install random cohort secret. Upload only a one-way cohort/receipt
commitment; a published control document never contains a raw bearer receipt.
The receipt remains a private credential for status/deletion.

Before any of this ships, run the store **policy pilot**: submit one
reviewer-visible dormant revision, the bounded ID-only control schema, and
complete reviewer instructions — no job-scoped activation — and record the
Chrome and AMO outcomes separately. Multiple packaged revisions are not needed
for source generation, generic fallback, daemon URL candidates, or suspension;
they exist only for this staged rollout.

### Local versus global rollback

- **Local:** the safety-domain circuit breaker immediately applies a restrictive
  transition from this installation’s evidence.
- **Global:** maintainers publish signed suspension based on maintainer
  testing and opted-in reports.

*papio* does not claim a global failure-rate signal without reporting telemetry.

## 6. User contribution intake (future; not shipped in Phase 0)

This section is the planned local/hosted reporting surface. Its consent,
sanitization, retention, fixture-provenance, and deletion statements are
requirements for a later intake implementation, not current behavior.

### Product surface

The planned surface adds **Report provider failure** to the inbox and **Report
this provider page** to the extension popup for a tracked or active tab, plus:

```text
papio adapter reports list
papio adapter reports show <id>
papio adapter reports export <id>
papio adapter reports submit <id>
papio adapter reports delete <id>
```

The one-click path uses the existing `activeTab`/optional-host permission model.
If the user already granted **All sites**, it never asks per provider; otherwise
the browser’s one-time host grant is the only unavoidable permission step.

The preview names the host, failure shape, capture time, adapter/extension
revisions, evidence categories, estimated upload size, destination, retention,
and whether full sanitized HTML is included.

### Durable reporting consent

Reporting is authorized per browser profile and per transmission tier:

- mode `never` — local evidence/repair only;
- mode `ask` — create a ready-to-submit inbox item;
- mode `automatic` — submit the profile’s authorized tier without another
  prompt;
- tier `structural` — redacted `EffectObservation` plus a structural sketch;
- tier `rich_capture` — sanitized HTML or richer page-derived content.

The extension asserts its stored profile authorization to the local daemon
through a feature-gated message, using an opaque random local
`reporting_scope_id` to separate profiles; that stable identifier is never
uploaded — the hosted service sees only a fresh per-report receipt. Daemon
config may narrow it globally or per provider but cannot let one
Chrome/Firefox profile authorize another. Holder switching silently reasserts
the stored choice; it never re-prompts.

Classifying and declaring the existing extension→daemon transmissions
(including `page_capture`) is **Phase 0** work: Mozilla treats data sent to
native applications as transmission requiring declaration and consent. Use
Firefox's built-in `data_collection_permissions` flow on Firefox 140+ and
either a one-time custom consent or disabled capture/reporting on supported
Firefox 128–139. Chrome disclosures, privacy, onboarding, and configuration
must name the same tiers and destination. Hosted reporting itself remains
Phase 3.

Recommend `automatic` + `structural` for fastest repairs. Offer
`rich_capture` once as a durable high-data choice. Routine failures never reopen
an already-recorded decision.

### Structural reports

Keep full sanitized captures local. Automatic structural reports contain:

- the redacted `EffectObservation`;
- a structural sketch of allowlisted tag/role classes, attribute **names**,
  candidate cardinalities, normalized route family, and before/after
  consequence flags;
- packaged revision, engine, browser, and protocol versions; and
- the minimized failure timeline.

They contain no DOM fragment, text, CSS selector, raw id/class/attribute value,
title/author/identifier, full URL, query key/value, or low-entropy public hash.
Structural reports therefore **deduplicate and triage**; they cannot by
themselves generate a replacement selector. Candidate generation requires an
already-held local fixture or a separately authorized `rich_capture`
submission.

### Phase 1: no hosted service

- export a `.papio-adapter-report` bundle;
- open a prefilled issue/support draft containing only reviewed structural
  facts and versions; and
- let the user deliberately share any rich bundle out of band.

No capture is placed in a public issue automatically.

### Phase 2: private hosted intake

Recommended for a fit-for-purpose loop:

1. the daemon submits structural evidence and receives a private bearer receipt;
2. the service deduplicates the keyed failure shape and reports whether richer
   authorized evidence is needed;
3. the daemon uploads only the profile-authorized tier;
4. duplicates attach to one internal incident;
5. the repair worker produces a source candidate and evidence;
6. public issues/PRs contain only reviewed structural facts; and
7. the receipt supports status and deletion, while a one-way commitment — not
   the bearer — scopes any incident-scoped packaged-revision test.

No account is required. Encrypt in transit/at rest. Define deletion deadlines
for raw/rich objects, derived indexes, database rows, operational logs, and
backups; state what de-identified repair artifacts survive. The service can
open a PR but cannot sign control, tag a release, or publish behaviour.

### Contributor feedback

```text
received → deduplicated → reproduced → repair proposed
  → store review → packaged → incident test → fixed
```

The daemon polls on its normal cadence. Once a valid packaged revision is
activated, jobs whose recorded access policy permits retry resume without a new
confirmation.

## 7. Generic acquisition

The packaged generic E0/E1 path is implemented behind the execution,
identity, safety-domain, and access-policy gates. Its current positive
strategies are the two named in `planGeneric`; the remaining strategy ideas
below are future coverage, not shipped claims.

Run generic once on the first settled `unknown` during the handoff. Do not wait
for a terminal `ui_changed` transition. After a packaged revision is locally
latched, generic is eligible only for selector/UI drift and only when its
`safety_domain_id` is explicitly same-or-lower risk. Wrong-work, validation,
unexpected-effect, and envelope failures set `no_positive_effects` and follow
the existing transitions named in the circuit-breaker section.

Current strategies:

- exact citation metadata/canonical identity (E0), and one declared PDF —
  `citation_pdf_url`, JSON-LD `contentUrl`, or
  `link rel=alternate type=application/pdf` — bound to the page's exact
  expected identifier (E1);
- unique article-scoped PDF anchor inside `article`, `[role='article']`, or
  `main`, with a conservative PDF/download path shape (E1).

Future strategy ideas (not Phase-0 claims) include tracked browser viewer
tabs, exact browser download IDs, and Zotero-derived candidates after they
become reviewed source. Control may eventually suspend exact packaged strategy
IDs; it cannot supply their predicates.

E0 discovery may use title/year metadata as evidence. Current `planGeneric`
E1 candidates require an exact expected DOI corroborated by page citation
metadata or JSON-LD, exactly one candidate per strategy, HTTPS with no
userinfo, and the page's **exact origin** (not a sibling subdomain or merely
the same registrable domain). They also require delegated access, execution
revalidation, and final PDF/work validation. The planner enforces the
available expected DOI binding before authorizing a generic download; it does
not claim PMID/arXiv generic support that `ExpectedWork` does not carry.

Generic execution is disciplined, not one-shot-then-park: it records E0
evidence, ranks the two eligible E1 strategies deterministically, and executes
E1 candidates **strictly sequentially** — the next may start only after the
previous has a correlated terminal observation — with at most two positive
candidate attempts per daemon-minted `drive_attempt_id`. The daemon persists
the full tuple (`drive_attempt_id + ordinal + strategy + revision`) and
reconstructs its state from job events; navigation, SPA replacement, redirects,
tab replacement, and MV3 worker restart cannot mint a new tuple locally.
Candidate URLs and ordering remain worker-local and are never replayed from
persisted state.

On MV3 restart, `reconcileGenericDownloads` searches only the persisted
in-flight download ID and restores its opaque tuple correlation. It does not
restore candidate URLs or ordering; a missing download is reported cancelled
and parked. Candidate two therefore requires a daemon-offered successor tuple
plus fresh planning/revalidation, never restart-local URL replay or tuple
minting. Within the live sequential chain, advance to candidate two only when a
clean non-PDF result is acknowledged `applied` for the current tuple. HTML,
login, MFA, CAPTCHA, terms, rate-limit, server-error, unknown, cancelled, and
unacknowledged results retain/park the current candidate. Only an explicit
human retry mints a new drive attempt. Late or duplicate terminal observations
are applied only when the daemon accepts the matching
`(drive_attempt_id, ordinal, strategy, revision)` tuple. An identity,
validation, or unexpected-effect failure sets the `(job, safety_domain)`
`no_positive_effects` latch and stops everything.

Persisted extension job state carries only `generic_evaluated`, bounded
attempt/strategy/evidence bookkeeping, `generic_terminal`, the in-flight
download ID when present, and the opaque `generic_drive_epoch` tuple. The
daemon event history is the authority for offered/started/result/superseded
status; the extension's local fields do not authorize a new candidate on their
own.

E2 generic clicks remain a future graduation requiring the same effect
contract, adversarial corpus, incident-scoped live evidence, and zero
wrong-work evidence.

## 8. Zotero evidence importer

Start a corpus survey alongside Phase 1. Pin each upstream translator revision
and record license/provenance.

Statically import only reviewable evidence:

- `target`, `priority`, and `multiple` as ranking/negative evidence rather than
  proof of support;
- literal selectors and metadata field names;
- expected bibliographic output and upstream tests;
- attachment MIME/URL transforms that map to packaged E0/E1 candidates; and
- explicit host/route hints for source repair.

For behaviour static analysis cannot recover, execute the pinned translator —
never in the browser, and never as browser authority. Two execution homes are
ratified as **live options** (2026-08-10, operator decision):

1. **Repair-time hermetic harness.** Run the translator over a captured
   fixture with the network denied; record attempted requests, DOM reads,
   candidate outputs, and helper usage. Replay old and new pinned revisions
   differentially so an upstream update cannot silently alter generated
   *papio* behaviour. Output feeds the patch generator as evidence for a
   source PR.
2. **Daemon-hosted candidate source.** The daemon (not the store-reviewed
   extension) runs a translator runtime as a separately-installed subprocess
   — the zotio pattern — using Zotero's own open-source server-side stack.
  Translators execute against locally held captures/fixtures only after the
  future sanitizer and independent-provenance boundary (no network) or, later,
  against public endpoints via daemon-controlled HTTP.
  Their output is **metadata and candidate PDF URLs only**, which
   enter the existing direct-offer envelope: delegated-only, identity-gated,
   origin/path-checked, one in flight, and always behind final PDF and
   work-identity validation. Translator code never gains browser authority
   and never ships inside *papio*'s bundle; the corpus and runtime are
   fetched separately at install time (licensing decision required before
   shipping — the stack is not MIT).

Gate for option 2: the **capture-replay yield experiment** — run matching
translators against the stored capture/fixture corpus and count PDF
candidates the generic engine and route table missed. It earns
implementation only if that marginal yield is material.

**Replay yield measured 2026-08-10** (same pinned corpus; 106 pages — 45
live captures, 61 fixtures; network-denied translation-server harness,
report in `dev/scratch/zotero-replay/`): `detectWeb` succeeded on 41% of
regex-matched runs, `doWeb` completed without network on 46% of those,
translators produced PDF candidates on exactly 2 pages — and both were
already covered by the generic citation/anchor checks. **Marginal yield: 0
pages.** The daemon-hosted runtime therefore does NOT clear its gate on
current evidence and stays unimplemented. Standing revisit trigger: re-run
the replay (cheap, offline) whenever the capture corpus gains a meaningfully
different provider mix — the corpus today is biased toward providers *papio*
already handles, which is exactly where translators are redundant.

**Static yield measured 2026-08-10** (pinned corpus
`fbee32689eca0d88105ac518c3b7f53bdbdd2508`, 749 translators): exactly **1**
statically representable E1 candidate; 97% of translators depend on
network/helper calls and every route-adjacent publisher translator computes
`detectWeb`. The build-time static importer is therefore not worth building;
translator value, if any, is reachable only through execution — see the two
live options above.

## Operations and objectives

Track locally:

- capture → incident, incident → candidate, candidate → tag, store review, and
  install → activation intervals;
- blocked-work integral per failure shape;
- generic, packaged-adapter, and assisted completion rates;
- safety-domain trips and suspensions;
- repair recurrence by immutable revision/effect contract;
- target ambiguity and execution-plan rejection;
- wrong-work and effect-contract violations;
- reporting consent by tier, deduplication, and fix-notification rates; and
- static and hermetic-sandbox Zotero evidence yield.

Nothing leaves the machine unless that browser profile authorized the
transmission tier and daemon policy still permits it.

Objectives:

- one acquisition produces one deduplicated local incident;
- unsafe behaviour can be suspended without an extension release;
- a capture produces a PR-ready repair without hand-copying selectors,
  fixtures, version bumps, or tests;
- approved source automatically enters the existing dual-store flow;
- if staged rollout is ever built, a packaged repair progresses incident test
  → cohort → stable without another extension release or prompt;
- rollback needs no rebuild and cannot relax accepted restrictive state;
- duplicate submissions converge on one private incident; and
- stable operation tolerates zero known wrong-work adoptions.

## Implementation order

### Phase 0 — implemented authority slice and remaining obligations

ADR-0021 is accepted; this plan no longer schedules a Phase-0 ADR. The
following authority work is implemented in the listed code:

- `plan.ts` supplies the complete page-side `Plan` with
  `expected_work`, `effect_graph`, `route_origin`, and `revalidation`;
  `DownloadRule.workTarget` is enforced into the selected target's explicit
  `work_binding`; `background.ts` injects it and executes only after
  live-document revalidation.
- `job_offer.access_mode` is consumed before positive effects. Direct routes
  are feature-gated daemon `provider_direct_get_request` tuples, never
  `job_offer` URL heuristics. ISBN catalogue/ebook handoffs are forced
  assisted by the bridge.
- Direct and generic attempts use daemon-minted durable
  `drive_attempt_id + ordinal + strategy + revision` epochs. The bridge
  records tuple lifecycle events and rejects stale/unstarted/duplicate
  results; extension storage carries only opaque tuple and bounded
  bookkeeping. Daemon restart rebuilds tuple state from job events.
- Generic E0/E1 is bounded and sequential. `planGeneric` accepts candidates
  only from the page's exact HTTPS origin. Candidate URLs/order remain
  worker-local; on MV3 restart only the persisted in-flight download ID and
  opaque epoch are reconciled, never a URL or candidate-two continuation.
  Candidate two requires the daemon's successor epoch plus fresh
  planning/revalidation; restart cannot replay a URL or mint a tuple.
- OpenAlex title matching is strict: exact normalized title, positive equal
  year when requested, and exact one-to-one author family/given-initial
  matching; identifier paths use canonical unique IDs and an explicitly
  open-access location. Weak or URL-only corroboration is not authority.
- DOI/title evidence, target uniqueness, exact-origin/path checks, access
  policy, and final PDF/work validation remain authority gates. The current
  generic strategies are only those implemented by `planGeneric`.

Remaining Phase-0 obligations are explicitly not shipped claims: classify and
declare existing extension→daemon transmissions (including `page_capture`) and
complete the required Firefox 140+ / 128–139 consent treatment; add the
redacted `provider_effect_observation` contract only when its feature-gated
structured-failure path is implemented; and keep repair-fixture
sanitization/provenance and hosted incident retention in their later phases.

**Exit for this authority slice:** current adapter and generic effects are
planned, policy-gated, revalidated, and tuple-correlated; stale URLs and
stale plans cannot independently authorize positive work.

### Phase 1 — shortest path from failure to success

The authority prerequisites above are complete. Remaining Phase-1 work is
coverage and automation, not a second authority model:

- extend daemon route templates and `route_revision` coverage; every
  autonomous `provider_direct_get_v1` candidate remains feature-gated,
  delegated-only, one tuple in flight per job;
- measure and broaden the packaged generic E0/E1 strategy set without
  weakening the bounded sequential chain, safety-domain gates, or daemon epoch
  correlation;
- add the future redacted observations, keyed fingerprints, and incident
  evidence intake described above;
- build the adapter patch **scaffolder**: independently evidenced capture →
  sanitized/provenanced fixture and CSS candidate, focused test, revision bump,
  changelog, and patch release through the production planner; and
- drive the existing `ext-v*` path from verified E0/E1 candidates and survey
  Zotero in a hermetic network-denied repair harness.

After Phase 1 lands, the next priority is `no_identifier` metadata
resolution (126 jobs — the largest measured class), ahead of Phase 2 and
Phase 3, for a single-installation deployment. Restrictive global control
becomes urgent with multiple active installations or an observed unsafe
packaged revision, not merely many UI misses.

Do not wait for a locator AST, hosted service, or multi-revision registry.

**Exit:** a real failure either succeeds through a daemon URL candidate or
identity-proven packaged generic logic, or produces a PR-ready store release
without hand plumbing.

### Phase 2 — restrictive control

- publish the minimal signed control schema: one online *papio* control key,
  monotonic sequence, suspension and higher-sequence lift of exact packaged
  revision IDs and safety domains only;
- persist canonical control in the daemon and
  `{last_sequence, last_verified_suspensions_digest}` in extension
  `storage.local`;
- add the feature-gated per-session control exchange without widening `hello`;
- withhold provider offers until each holder reports `applied`; and
- add `RepairAdapterTransition` with exact event latch and transaction rollback.

Deferred from this phase: offline-root signing, permanent runtime revocation,
registry/default/active maps, and any positive activation.

**Exit:** unsafe/drifting revisions stop across Chrome/Firefox holder changes,
restart/offline/stale state cannot relax safety, and normal skew stays connected
but assisted.

### Phase 3 — contribution consent and intake

- ship popup/inbox reporting and local list/show/export/submit/delete;
- add per-profile `never`/`ask`/`automatic` and
  `structural`/`rich_capture` authorization;
- implement hosted-transmission consent on top of the Phase 0 local-consent
  machinery (Firefox 140+ built-in categories, 128–139 custom flow); hosted
  `ask` creates one deduplicated inbox row, never a modal;
- publish matching Chrome/Firefox/privacy/onboarding/config disclosures;
- add structural report intake, private rich uploads, deduplication, receipts,
  complete deletion schedules, and status; and
- route hosted evidence into the same repair worker.

**Exit:** a nontechnical user authorizes once per profile/tier, duplicates merge,
and status/retry adds no per-failure prompt.

### Phase 4 — staged rollout (build only if measured need)

Prerequisite: the store policy pilot (one reviewer-visible dormant revision,
bounded ID-only control schema, reviewer instructions, no job-scoped
activation) accepted on **both** Chrome and AMO, plus measured evidence that
store-release rollout risk justifies the registry machinery.

- package multiple exact revisions with rollout eligibility;
- scope incident-test activation to the reporting job with a one-way
  commitment;
- advance deterministic local cohort → stable through signed configuration;
- transition parks through `RepairAdapterTransition`; and
- auto-promote same-contract E2/E3 only after incident-scoped live evidence.

**Exit:** first positive E2/E3 effects are contained and rollout is automatic
without treating a store-reviewed but untested revision as globally safe.

### Phase 5 — broaden generated coverage

- productionize useful Zotero evidence with pinned differential replay;
- prepackage proven generic/fallback strategies;
- add a source AST only where measured candidates need it; and
- graduate generic action shapes from observed zero-wrong-work evidence.

**Exit:** upstream maintenance and common page shapes expand reach without
shipping translator logic or remotely supplied behaviour.

## Acceptance tests

1. **Execution/observation separation:** seeded secrets, selectors,
   identifiers, and page-derived URL sentinels never surface in events,
   captures, logs, reports, control, or extension storage across success,
   failure, crash, and logging sinks. A daemon route URL may appear only in
   its solicited `provider_direct_get_request` frame and extension memory;
   credentials, signed queries, and page-derived URLs never become authority
   state.
2. **Control gate and skew:** a fresh install (sequence 0, bundle minimum 0)
   runs packaged defaults with no control fetch; once any restrictive state
   is accepted, missing/rolled-back control means no positive effects until
   recovery on the same session; no provider offer is released before
   `applied`.
3. **Control sequence semantics (Phase 2):** higher-sequence
   `suspended→active` lifts a suspension; a lower or replayed sequence is
   rejected; monotone-revocation tests belong to the offline-root phase.
4. **Atomic registry swap (with Phase 4):** unknown/duplicate/over-cap
   directives, and crashes both before and after the durable write, release no
   job and leave the previous active-registry version intact; a tab drive
   retains the version it started with.
5. **Transition matrix:** an explicit state/lease/action table drives the
   assertions — upgrade, rollback, disable, re-enable, holder change,
   repeated event, failed transaction, and old no-ID events each name their
   expected job state, lease owner, and action set afterward; leased,
   adopting, and identity-review jobs remain untouched.
6. **Consent/profile isolation:** automated tests cover profile persistence and
   isolation across reconnect and holder switch with zero prompts; manual
   checklists cover Firefox 140+ built-in and 128–139 custom consent
   acceptance.
7. **Report privacy/deletion (Phase 3):** authorization denial blocks upload;
   rich capture cannot upload under structural consent; delete produces a
   tombstone and clears raw objects, indexes, rows, and logs, with backups
   expiring by the published maximum.
8. **Zotero hermetic replay (Phase 5):** attempted network fails, DOM
   reads/outputs are recorded under CPU/time/output limits, old/new pinned
   revisions diff deterministically, and licence exclusion is asserted at the
   SBOM/source-package level.
9. **Fatality containment:** injected failure in every new bridge handler is
   followed by a successful unrelated RPC on the same native session.
10. **Production composition:** the route and observation paths are exercised
    through the real background dispatcher, native host, daemon bridge, and
    planner — not just direct handler calls — because individually tested
    handlers have repeatedly been unreachable or fatal in composition.

## Release-class verdicts

| class | verdict | mechanism |
|---|---|---|
| daemon route-template repair | **Go now; autonomous use is feature/access/epoch gated** | daemon-minted direct tuple through packaged navigation/download primitives; one candidate in flight |
| local drift/safety latches | **Go now** | daemon-durable latches through existing job transitions |
| reversible suspension | **Go after control protocol** | online-signed suspension + higher-sequence lift, prominently disclosed |
| permanent revocation | **Deferred** | offline-root signing built with staged rollout |
| generic E0/E1 authority | **Implemented bounded slice; broaden after Phase 0** | packaged `planGeneric` strategies, exact identity, fresh revalidation, daemon epoch correlation |
| same-contract E0/E1 repair | **Go automatic** | generated source, deterministic/live gates, store release |
| same-contract E2/E3 repair (authority-bearing action rule unchanged) | **Go automatic (store-released active)**; flip to review on wrong-work adoption OR any unexpected authenticated effect OR unintended terms/form/account/purchase mutation; staged inactive rollout deferred | generated source, live evidence, broken baseline, store release |
| effect-contract delta E2/E3 | **Go with maintainer action review** | generated candidate, live evidence, store release |
| E4 new capability | **Go through normal feature design** | extension source and store review |
| automatic reporting | **Go after per-profile/tier consent** | structural by default; rich only when separately authorized |
| packaged positive activation | **Policy pilot required** | dormant-revision pilot on both stores before any registry build |
| positive runtime rule catalog | **Deferred, not categorical** | revisit per "Deferred alternatives" triggers |
| runtime Zotero translator execution (browser) | **No planned implementation** | translator code never runs in the extension |
| daemon-hosted translator candidate source | **Live option, gated on measured marginal yield** | subprocess runtime over stored captures; candidates through the direct-offer envelope; licensing review before shipping |

## Stop conditions

Stop or narrow a component if:

- control carries behaviour instead of exact packaged IDs/restrictions;
- any provider offer can bypass pending/failed control or access-policy gates;
- execution cannot revalidate after SPA mutation;
- source repair cannot preserve independently labelled negatives;
- generic fallback can follow a safety-domain failure;
- hosted intake cannot meet tiered consent, minimization, retention, and complete
  deletion contracts;
- Zotero evidence yields little accepted coverage; or
- store review dominates blocked-work enough to justify a separately
  distributed enterprise build.

The success criterion is not adapter count. It is low user interruption, short
repair latency, high full-text completion, zero wrong-work adoption, and
visible authority/rollback.

## External constraints checked (2026-08-10)

- [Chrome Manifest V3 requirements](https://developer.chrome.com/docs/webstore/program-policies/mv3-requirements)
  permit remote data/config and feature-flag configuration when all logic is
  packaged; prohibit an interpreter for remotely fetched commands/data;
  sanction remote logic only through the Debugger and
  [userScripts](https://developer.chrome.com/docs/extensions/reference/api/userScripts) APIs.
- [Chrome review process](https://developer.chrome.com/docs/webstore/review-process):
  most reviews complete within a few days;
  [clients check for updates](https://developer.chrome.com/docs/extensions/develop/concepts/extensions-update-lifecycle)
  on startup and every few hours.
- [AdGuard's CWS review episode](https://adguard.info/en/blog/review-issues-in-chrome-web-store.html):
  five rejections over remotely updated rules that parameterize page-level
  execution; remote DNR-only "safe rules" offered a fast-track — the observed
  enforcement boundary.
- [Mozilla add-on policies](https://extensionworkshop.com/documentation/publish/add-on-policies/)
  require self-contained add-ons and prohibit remote code; remote data driving
  behaviour is not expressly classified. In practice AMO tolerates uBlock
  Origin's remote cosmetic-filter lists and Zotero Connector's remote
  translator code.
- [Zotero Connector's MV3 design](https://github.com/zotero/zotero-connectors/blob/master/src/browserExt/offscreen/offscreenTranslate.js)
  keeps remote translator code eval'd in a sandbox page ("untenable" to bundle);
  its shipped Chrome build is still MV2 and the migration PR is open, so it is
  not yet a CWS MV3 precedent.
- [uBO Lite FAQ](https://github.com/uBlockOrigin/uBOL-home/wiki/Frequently-asked-questions-(FAQ)):
  package-only rulesets, no remote requests — the maximal-conservatism pole.
- [Firefox built-in data consent](https://extensionworkshop.com/documentation/develop/firefox-builtin-data-consent/)
  applies on Firefox 140+, treats data sent to native applications as
  transmission requiring declaration and consent, and requires a custom
  consent flow (or disabled collection) on older supported versions.
