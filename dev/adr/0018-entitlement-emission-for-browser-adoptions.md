# ADR-0018: `operator_browser_session` is emitted from witnessed logins only

Status: **Accepted** (2026-08-07). Gives `acquisition-bundle/2`'s
`operator_browser_session` mode its first producer, four months after the enum
value was reserved without one.

Governs `entitlementFor`/`acquisitionModeFor` in `internal/bundle/export.go` and
the qualifying-evidence predicate in `internal/job`. Applies ADR-0007's
asymmetry to a rights claim; does not amend ADR-0009's or ADR-0014's ratified
method surface, because nothing about the wire contract changes.

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

`internal/job.BrowserSessionWitnessedLogin` answers the narrower question and
**defers to `BrowserAccessBasis` for the route/evidence lattice rather than
restating it**, so the two cannot drift. The `oa` route needs no special case:
that function already requires evidence `none` for it and derives
`open_access`, so `oa` can never satisfy a `fresh_auth` predicate. The
open-access-PDF-through-the-browser hazard that motivated the original omission
is now structurally unreachable rather than avoided by hand.

## Decision 2: `warm` does not qualify. `fresh_auth` is the floor

`BrowserAccessBasis` derives `institutional` from both `fresh_auth` and `warm`,
and that is right for the basis: a pre-existing authenticated session is a real
one, and the bytes really did come through it.

It is not right for this mode. `operator_browser_session` tells a consumer that
the operator's own authenticated session obtained the artifact, and Inscribi
converts it directly into `AccessTier.subscription_access`. A `warm` session is
evidence papio **inherited**: it found the session already authenticated and
never observed the login. That is a weaker claim than the mode makes, and the
gap is not theoretical — a stale-but-not-yet-expired SSO cookie presents as
warm.

So `warm` is refused explicitly rather than defaulted into. The cost is a field
on adoptions that did not re-authenticate; the alternative cost is asserting a
login that may not have happened. ADR-0007's asymmetry decides it: a false
positive invents rights evidence, a false negative costs nothing but a field.

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

`entitlementRoute` therefore prefers `landing_redacted` for any candidate with
recorded evidence. The synthetic `browser://adopted-download` names no origin
and is correctly rejected by the existing https check, so preferring the
recorded landing is what makes the emission possible at all. This generalises
correctly to an `oa`-route adoption too, which previously emitted `open_access`
with no route and now names the origin papio observed.

Rejected alternatives:

- **Widening both route vocabularies to carry route classes.** Would have
  required a coordinated two-repo cutover of a ratified contract to publish a
  strictly weaker record than the one already stored. Refused on evidence, not
  taste.
- **`route:sha256:` over the recorded context.** Satisfies the consumer's form
  while conveying nothing; a digest of an enum value is theater.

## Decision 4: no backfill, and the guard stays one-way

65 of the first cohort's 66 adopted works predate migration 0019. Their
`browser_route`/`session_evidence` binding is empty and stays empty forever;
`BrowserSessionWitnessedLogin("", "")` is false by construction. They remain
entitlement-less, and their rescue is a re-drain through the post-0.17.0
pipeline — an operator decision — never a retroactive stamp.

This is the same rule AGENTS.md already records for `WithHumanActionBinding`: a
guard applies going forward, and rows predating it are not evidence that the
guard is broken.

## Consequences

- The emitted object is unchanged in shape, so no schema bump, no protocol
  change, no migration, and no consumer change.
  `protocol.BundleEntitlement.validate` already admitted the mode, and
  Inscribi's `_ACCESS_TIER_BY_ACQUISITION_MODE` already maps it.
- Pinned in two places: the derivation table in `internal/bundle/export_test.go`
  (all four institutional shapes) and `TestRatifiedConsumerContract`, which
  asserts against a real `bundle.document` response because that is where a
  consumer actually reads the claim.
- One live cohort row (`job_ea65d899fc848f83add01210d7`, `direct`/`fresh_auth`)
  becomes retainable. Exactly one — the honest number, and the reason the
  end-to-end expectation is `imported` rising by 1 rather than by 68.
- The extension follow-up the plan proposed as option (a) is already landed for
  this purpose. What remains genuinely open is whether `PageHost` should also be
  recorded for **uncontexted** adoptions, which is a separate question about the
  legacy path, not about entitlement.
