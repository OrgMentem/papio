# ADR-0025: Evidence authority over canonical identity

Status: Accepted (2026-08-17)

## Context

*papio*'s worst outcome is not a spent quota or a failed acquisition; it is **the
wrong PDF filed under the right citation**. Every other failure is visible and
recoverable. This one is silent, durable, and propagates into the researcher's
library and citations.

The primary resolution path manufactured exactly that outcome, and not as a fuzzy
edge case. `matchesTitleSearch` requires an exact normalized title, then skips the
year test when `requested.Year == 0`, and `sameAuthorLists` returns `true` outright
for an empty requested author list. So a submission of `{Title: T}` with no DOI, no
year and no authors was matched on **title alone** — and the accepted record's own
DOI, OpenAlex ID and PMID were then emitted at `IdentityConfidence: 0.75` and merged
into canonical job metadata *before* fetching or semantic validation. Two works
sharing a normalized title (a preprint and an unrelated paper, a common review
title, a translation) are indistinguishable to that predicate.

That produces a self-confirming loop: wrong title match → wrong DOI adopted →
resolvers fetch that DOI → the fetched PDF "agrees" with the metadata that came from
the same bad match. A DOI cache hit then verifies the artifact hash and transitions
straight to `ready`, so one bad acceptance is reusable forever.

ADR-0014 made structured validation evidence durable. This ADR decides **which
evidence has authority over canonical identity**, and when.

## Decisions

### 1. The invariant

> Search and routing evidence may create candidates. Only evidence independently
> **verified as describing the same submitted canonical work** may mutate canonical
> identity metadata before artifact validation.

"Tied to" was considered and is too weak: a typed sibling relation *is*
independently tied to work X while describing work Y. Verified-as-describing
excludes it, along with search hits, version edges and routing evidence — all of
which may create candidates but may never promote their own work metadata. An
exact-identity-echo-verified canonical record (DOI **or** OpenAlex ID) is a
different authority class and may enrich.

This is structural, not threshold tuning. It must not be narrowed to a
fuzzy-sibling rule, and must not be gated on confidence tuning or on a corpus: the
title-only acceptance above survives deleting the sibling hop entirely.

### 2. Name the anchor: a durable, immutable snapshot of the submitted identity

Validation and cache attestations consume `job.SubmittedIdentity` — captured at
submit and never rewritten — not the mutable `row.Work`. Without naming it,
"compare against the submitted identity" silently becomes "compare against whatever
the job now believes", which is the loop this ADR exists to break.

`validationTarget` (`internal/app/identity_promotion.go`) is that read, and
validation compares against it. Enrichment output may still be used **in memory for
the current pass** (`mergeObservedInMemory`) — the prohibition is on durable
promotion, not on using a lead.

### 3. Provenance is per field, not only per identifier

An earlier analysis held that the only missing thing was identifier provenance.
That is wrong in the exact comparison this ADR protects. "Never overwrite a supplied
value" is **not** "preserves the submitted snapshot": a field the user *omitted* is
filled from the accepted record and is thereafter indistinguishable from one they
supplied. A title-only submission is matched on title alone, and the accepted record
then supplies year and authors too — so a later "does this PDF match what was
requested?" check reads a year and an author list that came from the very match it is
meant to validate.

Therefore both:

- `identifiers.provenance`, set at every insert site.
- `work_requests.submitted_fields`, recording which of title/year/authors were
  actually supplied at submission.

### 4. Four provenance states, because legacy quarantine is the mechanism

`unattested | submitted | verified | adopted`. Post-cutover a promotion may only
write `adopted`, and only `submitted`/`verified` may anchor a canonical-identity
comparison.

Four, not three: a three-state domain plus a requirement to backfill legacy rows as
"unattested" is unimplementable — a `CHECK` enforcing three states rejects the
backfill, and a migration permitting anything else has no domain at all. Legacy
quarantine is not ancillary metadata, so it gets a first-class state and no column
default able to manufacture `submitted`.

### 5. Prospectively enforced, with legacy rows quarantined — never claimed retroactive

Every pre-cutover row is backfilled `unattested`: the distinction was never recorded
and must not be manufactured now. Live at cutover: 715 requests, 907 identifier
rows, 98 requests with no identifier at all.

Unattested anchors are barred from canonical-identity promotion **and** from the
`DOI → SHA256` cache fast-path (`SubmittedIdentity.AnchorAllowsDOICache`). Cache
reuse must consume a durable identity attestation, not merely a DOI-to-hash pair.

### 6. An immutable anchor relocates the sparse-input case; it does not close it

Suppose two works genuinely share normalized title T, the submitted snapshot is
`{Title: T}` alone, and the provider returns the wrong one first. The anchor
correctly stops that record from rewriting the submission — and then validation has
nothing left to discriminate with, because every fact the user supplied *does* match
the wrong paper. Validation would either consult candidate-derived author/year/DOI,
which is the self-confirming loop moved one layer down, or have no discriminator at
all.

So there is an explicit **insufficient-authority disposition**: a title-only hit may
create a candidate, but may never become a verified canonical identity, a cache
attestation, or a `ready` artifact unless an *independent* authority supplies further
identifying evidence — a second resolver agreeing, a matching identifier from another
registry, or human confirmation. Otherwise the job stays unresolved and says so
(`insufficient_identity_evidence`, via `InsufficientIdentityAuthority` and
`settleInsufficientIdentity`).

### 7. Never buy yield by loosening the acceptance predicate

This trades the worst outcome *papio* has for a metric. It is prohibited
independently of any measurement, and it is the reason the fuzzy sibling hop was
switched off by measured yield (ADR-0024) rather than by relaxing the match.

### 8. Measure wrong-accepts before and after any change here

`make identity-corpus` scores the operator's own library against these rules and
reports **wrong accepts** — a wrong paper filed under the right citation. Compare
wrong accepts first, then the correct-pair pass rate. Two false accepts previously
survived review in a change that read as obviously safe, because the measurement was
a one-off nobody re-ran. The report names the operator's own papers and must never be
pasted into a commit, an issue, or the changelog.

## Remediation targets (audited against the invariant)

- `matchesTitleSearch` — accepts on title alone when the request carries no year and
  no authors.
- `fillMissing` / `accumulatePromotedIdentity` — accumulate across ranked candidates,
  each conflict-checked only against the *pre-merge* `row.Work`, so candidate A can
  contribute identifier X and candidate B identifier Y while A and B disagree with
  each other. Cross-candidate consistency is required.
- `conflictsIdentity` — must distinguish "contradicts the anchor" from "has nothing
  to add". A single `bool` conflating them discards an agreeing record.
- `validateCandidate` — validates against the anchor, not against mutated `row.Work`.
- `FindArtifactByDOI` — the cache fast-path, gated on an attested anchor.

## Consequences

- A title-only submission can no longer durably adopt the identifiers of whichever
  record a broad relevance ranking returned first. It can still create candidates and
  still acquire, but it must reach `ready` on evidence that is independently
  verified — or park and say why.
- Some acquisitions that previously completed on a fuzzy match now stop as
  insufficient-authority. That is the intended trade: an unresolved job is a
  recoverable state, a wrong filing is not.
- Measured on the operator's library across this change: wrong accepts **2, unchanged
  from the pre-change baseline** — the change closes a class of failure that the
  corpus does not exercise, so it is not a regression, and it is not evidence of
  improvement either.
- A duplicated identifier field must **agree** with itself or be dropped. A response
  contradicting itself has established nothing.
