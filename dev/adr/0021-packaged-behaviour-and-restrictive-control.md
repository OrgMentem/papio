# ADR-0021: Packaged behaviour, daemon-first repair, restrictive-only control

Status: Accepted (2026-08-10). Governs how adapter behaviour is distributed
and repaired at scale. Companion to ADR-0015, which stands unchanged for
positive runtime behaviour: this ADR is the suppression-only mechanism its
"Out of scope" section anticipated ("a separate proposal and a much easier
one, because it can only reduce automation"), plus the distribution decision
for the repair pipeline around it. Implementation order, evidence tables, and
acceptance tests live in `dev/active/adapter-release-latency-plan.md`; when
that plan ships, this ADR is the durable record.

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
   or revoke exact packaged revision IDs and safety domains — nothing that
   names hosts, selectors, paths, methods, text, or thresholds. The daemon
   holds canonical verified control; the extension persists only
   `{last_sequence, revoked_ids}` and executes no positive effect when daemon
   state is missing or rolled back. Compromise of the key can only reduce
   automation. Positive activation of packaged-but-inactive revisions is
   deferred behind a reviewer-visible policy pilot on both stores.
5. **Existing transmissions get declared before any reporting ships.**
   Mozilla treats data sent to native applications as transmission requiring
   declaration and consent; classifying the extension's current
   `page_capture` flow and adopting Firefox 140+ `data_collection_permissions`
   (custom one-time consent, or disabled capture, on 128–139) is Phase 0
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
