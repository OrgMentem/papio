# ADR-0018: `operator_browser_session` is emitted from freshly evidenced sessions only

Status: **Accepted** (2026-08-07). Gives `acquisition-bundle/2`'s
`operator_browser_session` mode its first producer, four months after the enum
value was reserved without one.

Revised the same day, after review, on three points of fact: what the
extension's `fresh_auth`/`warm` values actually mean (Decision 2), what
migration 0019 did to the legacy rows (Decision 4), and two consequences the
first draft missed (cache-completed inheritance and the adopt-then-apply
ordering). The decisions did not change; the reasoning behind Decision 2 and
several stated facts did. The corrections are marked inline rather than quietly
applied, because a durable record that reads as confidently wrong is worse than
one that shows where it was corrected.

Governs `entitlementFor`/`acquisitionModeFor` in `internal/bundle/export.go` and
`BrowserSessionFreshlyEvidenced` in `internal/job`. Applies ADR-0007's asymmetry
to a rights claim; does not amend ADR-0009's or ADR-0014's ratified method
surface, because nothing about the wire contract changes.

## Context

`internal/bundle/export.go` returned no acquisition mode for
`access_basis = institutional`, unconditionally. The comment above it named its
own unblocking condition:

> Recording the true basis and route at adoption time is the fix that gives this
> mode a producer; until then the enum value stays reserved.

Two facts made the omission right when it was written. Browser adoption marked
*every* adoption institutional, including an open-access PDF handed to the
browser only to get past an anti-bot wall; and the adopted candidate's URL is
the synthetic `browser://adopted-download`, so there was no observed origin to
name. Reconstructing one from the current OpenURL config would have been worse
than omitting: config is mutable, so a re-export after an operator edit would
silently rewrite an already-published provenance record.

0.17.0 (`delivery_context_v1`, migration 0019) removed both. It added
`candidates.browser_route` and `candidates.session_evidence` under CHECK
constraints, and `job.BrowserAccessBasis` now derives the basis from that pair
instead of asserting it — an `oa` route with any evidence but `none` is a hard
error, and missing context derives `manual`, never `institutional`.

The consequence for the consumer was measured. Inscribi's 2026-08-06 collection
pass over the `wk1_` cohort offered 112 ready jobs and committed **zero new
works**: 41 open-access papers re-registered idempotently and all 68
browser-adopted papers were refused `no_entitlement` by its per-work retention
gate. That gate was correct — no entitlement is no rights basis — and the
refusal was papio's omission reaching its consumer exactly as designed.

## Decision 1: the gate is recorded evidence, never the basis

Emit `operator_browser_session` when, and only when, the accepted candidate's
own row carries `session_evidence = fresh_auth` on a route
`job.BrowserAccessBasis` admits as institutional.

The basis alone is insufficient, and not only for the historical reason. A
resolver-produced candidate reaches `institutional` from its own paywall
metadata with no browser session behind it at all (see
`internal/app/app_test.go`'s fixture candidate at `https://paywall.test/landing`).
The export comment claimed browser adoption was this basis's only writer; that
was already untrue. Gating on the basis would therefore have published a login
claim for a candidate no operator ever touched.

`internal/job.BrowserSessionFreshlyEvidenced` answers the narrower question and
**defers to `BrowserAccessBasis` for the route/evidence lattice rather than
restating it**, so the two cannot drift.

The `oa` route cannot satisfy the predicate: `BrowserAccessBasis` requires
evidence `none` for it and derives `open_access`. That is worth stating
precisely, because an earlier draft of this ADR overstated it as making the
open-access-PDF hazard "structurally unreachable". **It does not.** The route is
chosen by the extension from the daemon's pre-fetch `requires_auth` flag
(`deliveryRouteFor`, `extension/src/background.ts`), not from the bytes, so an
open-access file fetched through an institutional handoff — including the exact
anti-bot-wall case the original omission cited — routes `direct` and can carry
this mode. What the lattice closes is only the subset papio had *already*
classified as open access, which is the subset that never needed closing.

That is a limit on what the mode asserts, not a hole in it: see Decision 2.

## Decision 2: `fresh_auth` is the floor, because it means recent confirmation

`BrowserAccessBasis` derives `institutional` from both `fresh_auth` and `warm`,
and that is right for the basis: a session evidenced at some point is a real
one, and the bytes really did come through it.

For this mode only `fresh_auth` qualifies. **Read those two values for what the
extension means by them, not for what they sound like.** An earlier draft of
this ADR justified the split as "fresh_auth witnessed the login, warm inherited
a session it never saw authenticate". That was wrong, and review caught it.
`currentSessionEvidence` (`extension/src/background.ts`) tiers purely on the AGE
of that origin's entry in `authEvidenceByOrigin`: inside the TTL it reports
`fresh_auth`, outside it reports `warm`. A witnessed login is only one producer
of that entry — a keepalive probe committing a verdict of "in" writes it too,
and the extension reports that same observation to the daemon as
`warm_verified`. `extension/test/background.test.ts`'s
"currentSessionEvidence for a job on B is not labelled warm or fresh by A's
evidence" is an executable demonstration: it injects one keepalive probe, no
login anywhere, and the emitted `delivery_context` carries `fresh_auth`.

The split is therefore about **recency of positive confirmation**, and on that
reading it is still the right floor and still fail-closed in the same direction:

- `fresh_auth` — something confirmed this origin's session was authenticated
  within the TTL. Recent, positive, and the strongest signal papio has.
- `warm` — evidence exists but has aged past the TTL with nothing confirming the
  session since. Strictly weaker; a stale SSO cookie sits here.
- `none` — nothing at all.

So the claim `operator_browser_session` makes is exactly: *the bytes arrived
through a browser session that was evidenced as authenticated at that origin.*
It does **not** claim a login happened during this acquisition, and it does
**not** claim the work was paywalled. Inscribi maps it to
`AccessTier.subscription_access`; the bundle also carries `reuse_license` and
`access_basis`, so a consumer that wants to downgrade an open-licensed artifact
has what it needs to do so. Deciding that is the consumer's call, not papio's.

ADR-0007's asymmetry still decides the floor: a false positive invents rights
evidence, a false negative costs nothing but a field.

## Decision 3: the route is the recorded page origin, and there was one all along

The handoff plan for this change assumed there might be no origin to name, and
proposed widening both papio's `validateBareRoute` and Inscribi's
`_SAFE_ROUTE_FORMS` to accept route *classes* (`direct`, `resolver`) as bare
literals, the way `zotero_storage` already is.

**That was unnecessary, and it was checked before being adopted.**
`AdoptDownloadWithContextCandidate` already persists `"https://" +
context.PageHost` as `landing_redacted` (`internal/app/browser_adopt.go`), from
the page host the extension observed at handoff. The cohort's one contexted row
carries `https://journals.sagepub.com`. So the strongest available record — a
real bare origin, quoted from the row — was already in the database, and both
validators already accept it unchanged.

`entitlementRoute` therefore prefers `landing_redacted` for a browser candidate
with recorded evidence. The synthetic `browser://adopted-download` names no
origin and is correctly rejected by the existing https check, so preferring the
recorded landing is what makes the emission possible at all.

It generalises to an `oa`-route adoption too, and the size of that change is
larger than an earlier draft of this ADR recorded. The draft said such a row
"previously emitted `open_access` with no route"; it did not — `entitlementFor`
is all-or-nothing, so a failed route meant no entitlement object at all. So
`oa`-route adoptions go from **no entitlement** to a present one naming the
observed origin. That is correct and desirable, and it is now pinned by its own
case in the derivation table.

The guard is `Source == "browser" && SessionEvidence != ""`. Only one writer can
satisfy that pair today, since `session_evidence`'s sole writer is gated to
`source = 'browser'`, so the source half is currently redundant. It is kept
because it names the invariant actually relied on: for any other candidate
`url_redacted` IS the host papio fetched from — often a CDN — and
`landing_redacted` is a different page, so a future non-browser writer of
`session_evidence` would otherwise silently repoint the route with no other
symptom.

Rejected alternatives:

- **Widening both route vocabularies to carry route classes.** Would have
  required a coordinated two-repo cutover of a ratified contract to publish a
  strictly weaker record than the one already stored. Refused on evidence, not
  taste.
- **`route:sha256:` over the recorded context.** Satisfies the consumer's form
  while conveying nothing; a digest of an enum value is theater.

## Decision 4: no backfill, and the guard stays one-way

An adoption with no recorded `browser_route`/`session_evidence` stays
entitlement-less: `BrowserSessionFreshlyEvidenced("", "")` is false by
construction. Its rescue is a re-drain through the post-0.17.0 pipeline — an
operator decision — never a retroactive stamp. This is the same rule AGENTS.md
records for `WithHumanActionBinding`: a guard applies going forward, and rows
predating it are not evidence that the guard is broken.

**That shape is not only historical, and an earlier draft of this ADR got the
facts wrong here.** The draft said "65 of the first cohort's 66 adopted works
predate migration 0019". Migration 0019 does not leave them alone — it
normalizes exactly that shape (`source='browser' AND access_basis='institutional'
AND browser_route IS NULL AND session_evidence IS NULL`) to `access_basis =
'manual'`, which is why the live store holds 79 browser/`manual` rows. What
remains is different and more interesting: measured on the operator's store,
**29 collectible rows still read institutional with no context, and every one
was created AFTER the migration ran** (all within four minutes, roughly two
hours before the first contexted row).

So this is an ongoing shape, produced by adoption paths that carry no context at
all: a directory-scan adoption (`scanAdoptionDir` in `internal/browser/bridge.go`)
always calls the context-less `adopt`, and a delivery context can be pruned by
`deliveryContextTTL` before the completion frame arrives. The guard is therefore
load-bearing in normal operation, not just against legacy data — which makes it
more important, not less.

## Consequences

- The emitted object is unchanged in shape, so no schema bump, no protocol
  change, no migration, and no consumer change.
  `protocol.BundleEntitlement.validate` already admitted the mode, and
  Inscribi's `_ACCESS_TIER_BY_ACQUISITION_MODE` already maps it.
- Pinned in two places: the derivation table in `internal/bundle/export_test.go`
  (all four institutional shapes) and `TestRatifiedConsumerContract`, which
  asserts against a real `bundle.document` response because that is where a
  consumer actually reads the claim.
- One live row (`job_ea65d899fc848f83add01210d7`, `direct`/`fresh_auth`) becomes
  retainable, out of 189 collectible jobs: 29 institutional rows stay
  entitlement-less, 79 `manual` and 80 `open_access` are unaffected (the store
  holds no browser-sourced open-access row, so the `oa` change above is a code
  path with no current data). Treat one as a **floor, not a fixed number** — see
  the propagation note below.
- **A cache-completed job inherits the entitlement.** `FindArtifactByDOI`
  completes a later request for the same DOI by recording the *original*
  acquisition's candidate, and `CandidateForArtifact`'s digest fallback resolves
  to it as well, so job B's bundle publishes job A's `operator_browser_session`
  though B opened no tab. This is ADR-0007's documented rule — provenance
  follows the bytes, which is why `access_basis`, `reuse_license` and `source`
  already travel this way — and the claim stays true of the bytes. It is
  recorded here because the mode's *wording* is about a session, so a human
  auditing a cache-completed job will read a per-job claim that was never
  earned per-job; the consumer does not, since it maps the mode to a tier for a
  per-work gate and never reads `job_id`. If a non-DOI-keyed cache path is ever
  added, revisit this: the digest fallback is `WHERE j.artifact_sha256 = ?`
  alone, with no work constraint.
- **The context lands after the job is ready.** `AdoptDownloadWithContextCandidate`
  runs the full adoption — including the ready transition, auto-import and the
  ready hook — and applies the delivery context only afterwards. A consumer
  driven by the on-ready hook rather than a later collection pass therefore sees
  an entitlement-less bundle. The direction is fail-safe (the claim is lost,
  never fabricated), but a hook-driven consumer will not observe the row this
  ADR predicts becomes retainable.
- `ApplyBrowserDeliveryContextToCandidate` overwrites route/evidence
  unconditionally while preserving a non-empty `landing_redacted`. That was
  harmless while landing was diagnostic; now that it is the rights route, a
  second delivery of identical bytes in the same job that upgrades the evidence
  but reports no page host would assert the later evidence against the earlier
  origin. Not fixed here, since it needs a missing page host and only one
  ordering is harmful, but it is now a provenance field and should be written
  atomically with the evidence it was observed with.
- The extension follow-up the plan proposed as option (a) is already landed for
  this purpose. What remains genuinely open is whether `PageHost` should also be
  recorded for **uncontexted** adoptions, which is a separate question about the
  legacy path, not about entitlement.
