# ADR-0021: Packaged behaviour, daemon-first repair, restrictive-only control

Status: Accepted (2026-08-10). Governs how adapter behaviour is distributed
and repaired at scale. Companion to ADR-0015, which stands unchanged for
positive runtime behaviour: this ADR is the suppression-only mechanism its
"Out of scope" section anticipated ("a separate proposal and a much easier
one, because it can only reduce automation"), plus the distribution decision
for the repair pipeline around it. The acceptance tests below are normative.

Amended by [ADR-0029](0029-agent-acquisition-decisions.md): optional agent
acquisition may select observed controls without a provider adapter. The
executor remains packaged; source-repair distribution and restrictive control
keep the rules below. Store acceptance of the agent path remains unverified.

## Context

The shipped repair loop — capture, hand-written adapter edit, extension
release, two store reviews, browser auto-update — does not scale to hundreds
of provider families, and most users will not wait through it: they try the
extension once, watch it miss, and write it off.

Measured on this installation (552 jobs, 2026-08-10): 206 succeeded, half of
those without any browser handoff; the dominant browser-side failure is
`ui_changed` at 119 of 139 provider outcomes (86%) — page drift, exactly the
class repair latency governs. The largest overall failure class
(`no_identifier`, 126) is daemon metadata work no adapter mechanism touches.

Store policy was checked against primary sources rather than assumed:

- Chrome MV3 permits remote data/configuration "for A/B testing or
  determining enabled features" when all logic is packaged, prohibits
  "an interpreter to run complex commands fetched from a remote source, even
  if those commands are fetched as data", and sanctions remote logic only
  through the Debugger and userScripts APIs.
- Enforcement practice draws the line at remote data that parameterizes
  page-level execution: AdGuard's MV3 extension went through five Chrome Web
  Store rejections (2024–25) over remotely updated rules and even packaged
  scriptlets taking remote parameters; Chrome's offered resolution was a
  fast-track for remote DNR-only "safe rules".
- Zotero Connector — the closest academic precedent — still ships MV2 on
  Chrome; its MV3 design evals remotely served translator code in a sandbox
  page and is an open, unresolved bet, not an approved precedent. AMO
  tolerates remote rule data (uBlock Origin cosmetic filters) and Zotero's
  remote translator code in practice.

So a remotely updated selector/action catalog for authenticated page actions
is gray leaning adverse on Chrome, plausible on Firefox, and not flatly
prohibited as data. It is resolvable only empirically, and a rejection risks
the sole distribution channel.

## Decision

1. **Positive adapter behaviour stays packaged and store-reviewed.** Every
   guard, selector, and action class that can drive the user's authenticated
   browser ships inside the reviewed extension bundle. The remote catalog
   pilot (Firefox-first), the Chrome userScripts channel, and fast-lane
   distribution are recorded as deferred alternatives with explicit revisit
   triggers — a documented risk decision, not a categorical policy claim.
2. **Daemon URL intelligence is the first repair path.** Provider knowledge
   that is URL-shaped — direct PDF endpoint templates, resolver and account
   routing — lives in the daemon and reaches the browser as ordered candidate
   navigations through existing packaged primitives. The daemon updates
   outside the stores, so these repairs deploy in hours. The extension
   receives URLs to navigate and observe, never selectors or action
   parameters; identity and PDF validation gates are unchanged.
3. **Source repair is generated, not hand-written.** An adapter patch
   generator turns a reviewed capture into the CSS candidate, fixtures,
   focused tests, revision bump, changelog, and `ext-v*` submission through
   the existing dual-store workflow. Store review remains the external gate;
   the maintainer queue in front of it is removed.
4. **Signed control is restrictive-only in its first version.** One online
   *papio* control key signs a monotonic-sequence document that may suspend
   exact packaged revision IDs and safety domains, and a higher sequence
   replaces the whole set, which lifts a suspension. It names nothing else:
   no hosts, selectors, paths, methods, text, or thresholds. Permanent
   revocation waits for an offline root. The daemon holds canonical verified
   control; the extension persists only
   `{last_sequence, last_verified_suspensions_digest}` and executes no
   positive effect when daemon state is missing or rolled back below that
   sequence. Compromise of the key can only reduce automation. Positive
   activation of packaged-but-inactive revisions is deferred behind a
   reviewer-visible policy pilot on both stores.

   Shipped shape: Ed25519, with the public key embedded in the daemon
   (`internal/adaptercontrol/trust.go`) and the private key held by the
   maintainer's `cmd/papio-control` tool, never in the repository. The signed
   file is `control/adapter-control.json`, fetched by the daemon over HTTPS
   on the update check's cadence and only while that check is on. A 404 means
   nothing is published and is not a failure; a document that fails
   verification, replays, or carries an older sequence is refused and never
   relaxes held state. The extension is gated by version, not by a feature
   flag (the `hello_ack` feature list is full at its cap of 32): the daemon
   adds `adapter_control` to `hello_ack` for extensions at or above 0.16.0.
   A suspended revision, or any browser that has not applied the sequence the
   daemon holds, gets no automatic offer or effect permit and falls back to
   assisted operation.
5. **Existing transmissions get declared before any reporting ships.**
   Mozilla treats data sent to native applications as transmission requiring
   declaration and consent; classifying the extension's current
   `page_capture` flow and adopting Firefox 140+ `data_collection_permissions`
   (custom one-time consent, or disabled capture, on 128–139) is prerequisite
   work, not a reporting-feature afterthought.

## Consequences

- An unsafe or drifting adapter revision can be stopped everywhere without a
  store release; making it work again is a store release plus, for URL-shaped
  breakage, often just a daemon update the same day.
- The extension's trust story is unchanged: everything it can do was in the
  reviewed bundle. No new runtime authority exists for a signature to launder.
- If the deferred alternatives' triggers fire — store-release latency proves
  intolerable in practice, or Zotero's MV3 build survives Chrome review —
  reopening the catalog question starts from the recorded evidence, not from
  scratch.

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
   recovery on the same session; with mandatory control, no provider offer is
   released until the holder browser session reports control `applied` at or
   above the daemon's sequence (non-mandatory control does not need it). Test
   browser sync of control to the extension separately from the daemon's
   HTTPS fetch of the signed document.
3. **Control sequence semantics:** higher-sequence
   `suspended→active` lifts a suspension; a lower or replayed sequence is
   rejected; monotone-revocation tests belong to the deferred offline-root work.
4. **Atomic registry swap (with the deferred positive-activation rollout):** unknown/duplicate/over-cap
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
7. **Report privacy/deletion (deferred automatic reporting):** authorization denial blocks upload;
   rich capture cannot upload under structural consent; delete produces a
   tombstone and clears raw objects, indexes, rows, and logs, with backups
   expiring by the published maximum.
8. **Zotero hermetic replay (deferred daemon-hosted translator candidate source):** attempted network fails, DOM
   reads/outputs are recorded under CPU/time/output limits, old/new pinned
   revisions diff deterministically, and licence exclusion is asserted at the
   SBOM/source-package level.
9. **Fatality containment:** injected failure in every new bridge handler is
   followed by a successful unrelated RPC on the same native session.
10. **Production composition:** the route and observation paths are exercised
    through the real background dispatcher, native host, daemon bridge, and
    planner — not just direct handler calls — because individually tested
    handlers have repeatedly been unreachable or fatal in composition.
