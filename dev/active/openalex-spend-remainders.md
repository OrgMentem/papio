# OpenAlex spend remainders: a credit fuse, an egress invariant, and an identity audit

Salvaged from the 2026-08-14/15 OpenAlex spend work (shipped and deployed:
charging mixed passes, the record memo, `select=`, the header floor, the
anonymous fallback, the fuzzy-search boundary/basis gate, and three fail-closed
guards). Those changes are complete; git history and `CHANGELOG.md` hold the
record.

**Seven adversarial reviews, six rewrites.** Three early rounds sharpened a
two-part plan (a per-job spend guard plus a per-identity credit ceiling); a
fourth, given no prior-round context and a wide brief, rejected the shape of both;
rounds five to seven reviewed the shipped code and found twelve defects in it,
including four in fixes made minutes earlier. The seventh judged the architecture
converged — "do one more plan edit, not another redesign" — and this is that edit.
Every finding was verified against the tree before being accepted. Reviews are
preserved under `dev/scratch/oracle/20260815T0*`.

**Rejected designs** at the bottom records every dead end with its reason —
twenty-three of the thirty-one from earlier drafts of this file. Read it before
proposing a simplification; most of the obvious ones are in it, with the sequence
that breaks them.

The recurring shape of every defect found so far, mine included: **a decision made
from information that is not durable yet, or is not what it claims to be.** An
in-memory charge a crash erases; a fail-closed guard that becomes a permission
when a second caller reads it; a best-effort note treated as an authority; a cache
treated as a fact; a durable fact read by only one of its two readers.

## Context: the incident, and what production says now

*papio* burned 10,000 OpenAlex credits in 25 minutes because a job whose
candidates are all dead re-ran the resolver chain every cadence forever: the pass
was classified `source_gate` (uncharged) whenever any unrelated source was gated,
so the bounded retry budget never bound.

That is fixed, and the first post-fix reset confirms it: the cohort ran ten
minutes, decayed `83 → 26 → 0` attempts/minute, and settled with zero jobs left
in `retry_wait` and no future `retry_at`. **The liveness problem is solved.**

What remains is economics, and the measurements are decisive:

- OpenAlex prices `GET /works/{doi}` at **1 credit** and `filter=title.search:`
  at **10** (`x-ratelimit-credits-used`, measured live).
- That post-fix run: 664 attempts, **3,393 credits**. 304 were title searches and
  280 found nothing.
- Lifetime: **73 of 3,165 title searches (2.3%) returned anything at all.**
- Keyed and keyless are separately metered pools (`limit: 10000` vs `1000`).

So the remaining exposure is not an unbounded loop; it is a legal, terminating
workload buying a 10-credit operation that fails 97.7% of the time, with no
aggregate ceiling of any kind.

## 0. In parallel: estimate the yield of the thing we are budgeting

This **does not gate items 1–2** — a post-commit review of the shipped code
established that the egress boundary and the fuse are the missing crash and
storage-failure safety boundary, not merely economics for a low-yield
operation. Measure alongside, and let the result decide whether the fuzzy search
stays enabled or gets narrowed, never whether *papio* needs a hard OpenAlex
egress ceiling.

Estimate, for both the fuzzy sibling hop and metadata enrichment:

```
accepted artifacts attributable to OpenAlex title.search
--------------------------------------------------------
              title.search credits spent
```

The 2.3% is a *result* rate; what matters is the *acquisition* rate — how many
of those 73 hits produced a validated, imported PDF nothing else would have
found.

**Treat the number as a lower-bound estimate, not a computation.** Two reasons,
both structural: `job.sibling_search` is written post-wire and is lossy under
the crash window described in item 2, and `FinishAttempt` is best-effort
everywhere. And "attributable" needs winner/candidate provenance — if the
ledger does not record which candidate won, do not manufacture causation from a
title-search attempt happening to precede a ready job.

## 1+2. One egress authority: commit the credit at the wire, or do not go

These were two items until a post-commit review showed they are one boundary.
A reusable admission marker minted back in `AcquireAny` does not solve the
problem, because it is still separated from egress by a step that can block —
and it could authorize more than one `Do`.

**The defect this closes, in the shipped code.** Three live holes, all found in
review:

1. **The quota floor is not an egress barrier.** `Observer.observe` logs and
   continues when `Defer` fails, so valid low-quota headers followed by a failed
   write leave no durable `_quota` row and the next job spends again. On a
   full disk that runs straight through the supposed 5% stop until some later
   write succeeds or the provider itself refuses.
2. **TOCTOU between admission and the wire.** `AcquireAny` consults `quotaGate`
   *before* an ordinary `Acquire` that may itself wait up to `MaxInlineWait`.
   Workers A…N all pass the quota precheck while it is open, queue in `Acquire`,
   another in-flight response installs the floor, and the queued workers then
   send anyway. The code's comment promises "no new admission once the gate is
   visible"; the implementation does not deliver it. With a synchronized cohort
   the 5% headroom is covering not just requests already on the wire but every
   worker that passed the precheck.
3. **The credit fuse is a DIFFERENT boundary from the provider floor, and does
   not repair it.** A source-wide daily credit counter answers "how much may this
   instance commit today"; the floor answers "is this identity's provider balance
   spent". A committed credit satisfies the first and says nothing about the
   second, so unless the final egress authority **revalidates the outgoing
   identity's provider gate at the same point it commits the credit**, holes 1
   and 2 survive the fuse entirely. Treat identity-gate revalidation as part of
   the egress authority's contract, not as something the counter subsumes.

**The shape that fixes all three.** Do all cheap validation and ordinary local
gating first — token bucket, advisory throttle, applicability. Then, at the final
OpenAlex HTTP boundary, **one atomic transaction makes two independent decisions
and then the network call happens immediately**:

1. the outgoing credential's `"<source>_quota"` signal is not currently gated; and
2. the source-wide daily credit debit fits the configured allowance.

Both are revalidated *there*, not inherited from an earlier decision. That
transaction *is* the egress authority: no blocking wait may occur after it, a
failed commit means no wire, and a request reaching the transport without one
must fail loudly. Do not fix the race with a manager-wide mutex — a decision
taken before a blocking step cannot be authoritative for egress.

**Requirement: a boundary with provably no automatic replay — `RoundTrip` is not
enough.** The wrapper accepts an `*http.Client`, and `Client.Do` follows
redirects, so one commit could authorize several physical requests. But moving to
a wrapper around `http.Transport.RoundTrip` is *still* insufficient: the standard
`Transport` may internally retry an idempotent request after certain network
failures on a reused connection, and every OpenAlex call is a GET. Sequence:
commit 10 → guarded wrapper calls the transport once → the GET is written on a
reused connection → a qualifying failure → the transport replays it → two
physical requests under one debit.

So: require a transport configuration or implementation with **provably no
automatic physical replay**, or arrange for every replay to re-enter the egress
authority. Disabling redirects remains necessary and is not sufficient. Pin it
with a regression that fails a reused connection in the standard retry condition
and asserts either one send, or a second debit for the second send. **Until this
is decided, item 1+2 is not implementable as specified.**

**Requirement: latch on the parsed header, not on the failed write.** When a
valid low-quota header is parsed, set the in-process fail-closed latch for that
credential **immediately, before attempting persistence**, and clear it only once
the durable gate exists or its reset passes. Waiting for `Defer` to fail leaves a
window — the durable write can block for up to five seconds — in which the
process already knows the fact while new egress transactions still see no gate. A
transient SQLite busy can also drop the floor while the credit write succeeds, so
"both writes fail together because the disk is full" is too narrow an assumption.

**Be honest about the floor's limit.** A fact learned from a response cannot be
made perfectly crash-durable if the machine dies before any write succeeds. The
**daily fuse**, not the header floor, is the crash-hard monetary invariant; the
plan must not claim more for the floor than that.

**Requirement: a capability boundary, not just a passing test.** "Every current
callsite reaches the fuse" is worth asserting, but code outside the egress
wrapper should not *possess* a client able to reach OpenAlex without the commit.
Make the unguarded transport unconstructible from outside, then the test is a
backstop rather than the guarantee.

**Requirement: define the refusal taxonomy now, and make it survive `fetch`.**
Moving refusal to HTTP egress moves it *behind* a layer that currently destroys
its meaning: `openalex.fetch` turns **every** client error into a fresh generic
`resolver.TemporaryError`, discarding the original. A late keyed-quota refusal
then cannot tell acquisition to try the anonymous identity, and a daily-credit
refusal cannot tell item 3 to park until UTC midnight. So the *type* contract
belongs in this item, not in item 3:

| refusal | carries | caller behaviour |
| --- | --- | --- |
| provider-quota / identity gated | that credential's `Until` | acquisition may retry under the next identity with fresh admission; a fixed-policy caller defers |
| source-wide daily credit exhausted | next UTC boundary | park; **never** triggers identity fallback |
| pricing-drift closure | remaining local window | park; never falls back |

Each must be preserved — or deliberately translated — through `fetch` rather than
flattened. The scheduler wiring can still land as item 3, but the taxonomy cannot
wait for the egress implementation, because both sides encode it independently
otherwise.

**Debit placement matters for availability.** Committing before ordinary local
admission would let a reset cohort burn a large slice of the daily allowance on
requests that never leave the machine. After local admission, immediately before
egress: `ErrNoSearchBasis` and `ErrNotApplicable` then cost zero with no
settlement machinery at all.

### The fuse itself

**Not** a reservation ledger. The goal is that *papio* provably stops making
requests, not that its books balance to the credit.

- **One durable counter, `credits_committed`, per source per UTC day**, committed
  atomically at the egress boundary by the request shape's conservative cost (1
  for a singleton, 10 for a search), refused when the debit would exceed the
  configured daily allowance. **Never refunded within the window.**
- **Source-wide, not per identity.** A limit enforced per `(source, identity)`
  means a configured 4,000 actually authorizes 4,000 keyed *plus* 4,000 keyless —
  an operator misreading that weakens the cap's whole meaning. The
  provider-reported floor already protects each real pool separately and
  per-identity; *papio*'s own budget should answer the different question: **how
  many OpenAlex credits may this instance commit today, across all identities?**
  Then `AcquireAny` can still fall through to the keyless identity when the keyed
  one is provider-gated, while the instance-wide allowance keeps bounding the
  total.
- **Availability is the safe failure mode.** A crash leaves the debit. A
  transport error leaves the debit. A path that declines after admission
  (`ErrNoSearchBasis`, `ErrNotApplicable`) may leave the debit. Each costs a
  little availability inside one UTC day and clears at rollover; none can
  overspend. This is what removes the reservation handle, one-shot
  `Confirm`/`Release`, aggregate-reservation correlation, the settlement
  transaction, adapter preflight, and nearly all of the cross-midnight problem.
- **Call it `committed`, not `spent`,** because that is what it is.
- **State the invariant truthfully.** The fuse bounds credits *papio*
  **authorizes**; it cannot bound what the provider actually bills, because a
  request admitted at 23:59:59 may be accounted in either window, and a shape
  that costs more than its conservative charge has already happened by the time
  anyone knows. If observed `credits-used` ever exceeds the committed shape cost,
  that is provider-contract drift — and gating only the *credential* is too
  narrow, because pricing is a property of the **operation**, not the identity.
  Otherwise keyed detects a 10→100 change, is gated, and the next call falls
  through to keyless and repeats the same undercharged shape.

  **Decision: on any positive cost drift, durably close all OpenAlex egress
  until the next UTC boundary**, and set the process latch immediately while that
  write is being established. A process-only mark is not enough — commit 10,
  learn the shape really cost 100, mark it closed, crash, restart, and only the
  under-sized 10-credit debit survives, so the same operation is authorized
  again. Closing the source rather than the individual shape avoids building a
  second per-shape state machine for an event that should be rare and loud. Do
  not build reconciliation to recover a few conservatively overcounted credits.
- **Credits are a new unit, not the existing `estimatedCost`.** That argument
  flows to `reserve(ctx, source, identity, policy.MaxCostUSD, estimatedCost)` and
  into `spent_usd`: it is dollars. Using it for credits implements the
  credits-as-dollars design this document rejects.
- **Atomic in SQL across insert AND update.** Hundreds of jobs target the same
  hot row, so it must be one conditional mutation — and the condition must cover
  the *first* write of a day, not only the conflict-update arm. A naive
  `INSERT … ON CONFLICT DO UPDATE` that puts the limit test only in the update
  arm admits a first 10-credit request against an allowance of 5. Test the
  empty-row-over-limit case explicitly.
- **Config, with the number decided: `daily_credit_limit`, default `4000` for
  OpenAlex.** The blast radius is policy, not an implementation detail, so it
  belongs here rather than being invented by whoever writes the code. 4,000 of a
  keyed 10,000-credit day leaves the provider floor a wide margin, is ~1.2× the
  3,393 credits the worst observed post-fix day actually spent, and still lets a
  large backlog make real progress. Revisit from the first few days of
  `credits_committed` telemetry, not from intuition. `0` means unmetered, matching
  the existing convention, so `papio init` and the shipped defaults must write the
  non-zero value — with a test on the defaults, or the fuse ships inert. Document
  it in the hand-authored `docs/reference/config-reference.md`. Strict config
  decoding means the field and the binary deploy together.
- **`0` disables the ceiling, never the commit.** Every OpenAlex request still
  executes the durable commit and increments `credits_committed`; otherwise the
  configuration that turns off enforcement also bypasses the egress invariant and
  its accounting, which is precisely when an operator most needs the numbers.
- One migration; three hardcoded `user_version` assertions
  (`internal/cli/clean_install_test.go` twice, `internal/doctor/doctor_test.go`,
  `internal/store/migrate_forward_test.go`).

## 3. Park truthfully when a local budget is exhausted

`resolve` converts `budget.ErrExceeded` into `continue` without recording any
gate fact (app.go:539-542), unlike `ErrDeferred`; enrichment does the same. So
with every source exceeded and no candidates, resolution reaches ordinary
`no_legal_candidates` — reporting "no copies exist" when the truth is "out of
budget until the window resets". Dormant today only because monetary reservations
are all `0.0`; the fuse makes it live.

**A local policy budget being exhausted is not evidence that no legal candidate
exists.** It is a *timed* gate, so the disposition must carry the reset that
actually applies — **UTC midnight for the credit fuse, the monthly boundary for
the existing USD budget** — and become a durable park. Two budgets with different
windows must not collapse into one reset, and the pre-existing monetary
`ErrExceeded` path must be fixed in the same change rather than leaving two
dispositions for one condition.

**Decision on representation: one typed local-budget refusal carrying unit,
window and reset**, not two sibling error types. `budget.ErrExceeded` today is
implicitly monthly and carries neither an `Until` nor a budget kind, so extending
it is required either way; a single type with `{unit: credits|usd, window:
utc-day|month, until: time.Time}` keeps one park path and makes a third budget
cheap. This is the same contract as the egress taxonomy in item 1+2 — define it
once, there.

## 4. Jitter the budget-reset wake

Every job parked on a quota or local-budget gate becomes runnable at the same
instant, because the park time is the reset instant exactly. The token bucket
protects OpenAlex from the burst, but nothing protects the scheduler and SQLite
from a cohort waking together — and a synchronized cohort is what produced the
original 25-minute burn shape. Wake at `reset + stable_jitter(jobID)`:
deterministic, no new state, one line at the park site. **Window: 0–120 seconds**,
chosen so a several-hundred-job cohort spreads to a handful of wakes per second
while the added latency stays invisible against a day-long gate. Give the constant
a name and let tests reference it rather than picking their own.

**Jitter the job's wake time, never the underlying gate.** The gate instant is
the provider's own fact and other logic reads it; smearing the gate would corrupt
that meaning, while smearing the wake preserves it. **Scope it to quota and
local-budget reset parks**, not to every source-gate wake: an ordinary short gate
is not a synchronized cohort, and smearing those adds latency for no benefit.

## 5. Evidence authority over canonical identity (highest consequence here)

*papio*'s worst outcome is not a spent quota; it is the wrong PDF filed under the
right citation. This is **audit plus mandatory remediation of every violating
promotion path**, not an audit that might conclude nothing.

**The one invariant, phrased around evidence authority rather than any particular
path:**

> Search and routing evidence may create candidates. Only evidence independently
> **verified as describing the same submitted canonical work** may mutate
> canonical identity metadata before artifact validation.

"Tied to" was too weak, and the difference matters: a typed sibling relation *is*
independently tied to work X while describing work Y. Verified-as-describing
excludes it, along with search hits, version edges, and routing evidence — all of
which may create candidates but may never promote their own work metadata. An
exact-identity-echo-verified canonical record (DOI **or** OpenAlex ID) is a
*different authority class* and may enrich.

**Name the anchor, or an implementer has to choose one.** Validation and cache
attestations must consume a **durable, immutable snapshot of the submitted
identity**, captured at submit and never rewritten — not the mutable `row.Work`.
Without that named explicitly, "compare against the submitted identity" silently
becomes "compare against whatever the job now believes", which is the
self-confirming loop this item exists to break.

Known violating shapes to remediate:

- **A weak title match can be accepted on title alone.** When the submitted work
  has no year and no authors, `requested.Year == 0` skips the year check and an
  empty requested-author list makes `sameAuthorLists` return true — so a record
  sharing only a normalized title is accepted at 0.75 confidence, carrying its own
  strong identifiers.
- **Every accepted candidate then flows through the same pre-validation merge**
  (`rank → fillMissing(row.Work, c.ResolvedWork) → FillWorkMetadata`) *before*
  fetching or semantic validation, so identifiers that originated solely in a
  fuzzy match become canonical job metadata a later pass then trusts.
- **`fillMissing` accumulates across ranked candidates**, each conflict-checked
  only against the *pre-merge* `row.Work` — so unless it enforces cross-candidate
  consistency, candidate A can contribute identifier X and candidate B identifier
  Y while A and B disagree with each other.
- **Enrichment adopting an identifier from a fuzzy title match** is the
  self-confirming case: wrong title match → wrong DOI adopted → resolvers fetch
  that DOI → the PDF "agrees". Safe only if the *originally submitted* identity
  remains an immutable validation anchor.
- **Amplifier:** a DOI cache hit verifies the artifact hash and transitions
  straight to `ready` with no visible semantic revalidation, so one bad acceptance
  is reusable forever. Cache reuse must consume a durable identity attestation,
  not merely `DOI → SHA256`.

Audit and remediate `conflicts`, `fillMissing`, `sameWork`, `matchesTitleSearch`,
`validateCandidate`, and `FindArtifactByDOI` against the invariant.
`make identity-corpus` measures wrong-accepts against the real library and must be
run before and after any change here.

## 6. A live defect now; deferrable only once the fuse is deployed

`Process` crash recovery rewinds to `resolving`, and a pass can fail on durable
post-wire state updates (`FillWorkMetadata`, `InsertCandidates`,
`ResetCandidates`) before any `retry_wait` transition exists — so
wire → credits spent → post-wire failure → no retry event → recovery → the same
provider call authorized again. `retryBudgetExhausted` cannot contain it,
because its count comes from persisted event history that this sequence never
writes. The charging information is only an in-memory `retryPlan` until the job
reaches its durable retry transition.

**This is a current shipped spend-safety defect, not a future fairness
concern** — the post-commit review was explicit about that, and storage failure
is not hypothetical on this machine. It is listed sixth because the egress
authority (item 1+2) is the structural fix for its *consequence*: once every
OpenAlex wire attempt requires a fail-closed credit commit, an uncharged repeat
pass can no longer produce unbounded provider spend.

**Be precise about what the fuse actually buys, though.** It does not make a
pathological job's spend bounded over its lifetime — that job can consume the
allowance again tomorrow, and the day after, starving unrelated jobs each time.
What the fuse gives is an operator-defined **per-day blast radius**. If that is
the accepted safety invariant then deferring the per-job guard is reasonable, but
what remains deferred is a **cross-day starvation and liveness problem**, not
cosmetic fairness.

Reasons it is deferred rather than built now:

1. The fuse bounds the per-day spend consequence, which was the urgent property.
   What the per-job guard adds is that the offending job eventually dies rather
   than merely being prevented from draining today's pool.
2. It never actually delivers a hard per-job bound anyway: novel candidate
   insertion and material metadata changes reset the episode, so a source
   producing continuing low-value novelty keeps a job alive indefinitely.
3. It carries a migration, a new terminal reason, transactionally coupled reset
   semantics, adapter preflight changes, and grace-gate provenance — a large
   surface for a fairness property.
4. **Lease fencing is unresolved.** The store has `lease_owner` /
   `lease_expires_at`, but `InsertCandidates` takes only a job ID and performs no
   generation check. If a resolver call can outlive its lease, a stale pass can
   insert a genuinely novel candidate and rearm the *current* holder's authority —
   or consume it. "Same transaction as progress" is insufficient; it must be
   *progress from the currently authorized pass*. Either prove lease expiry
   cannot overlap live `Process` execution, or carry a pass token in every
   authoritative charge and reset.

Revisit when "one job may not occupy acquisition indefinitely" is adopted as a
product invariant, or on an observable concentration signal — one job's share of a
day's `credits_committed`, or repeated same-work passes visible in external logs.
**Do not rely on in-database telemetry of post-wire/pre-park failures as the
trigger**: storage failure is precisely the condition that produces those failures
*and* erases their record, so that signal is quietest exactly when it matters.
Then frame it as exactly that: a job-liveness fuse, not the remainder of the
OpenAlex incident fix. Its worked design (conditional SQL charge, progress-only
reset via `InsertCandidates`' inserted-row count, a separate
`retryProgressBasis`, prospective checking, a dedicated terminal reason, and the
mandatory regression that crossing the ceiling must **not** make `atBoundary`
true) is in git history at the third rewrite of this file.

## 7. Derive the effective basis explicitly, before novelty gating

Two problems, one abstraction. The first is availability: a DOI-only job whose own
metadata carries no usable basis cannot search for siblings when the memo has
expired or been evicted, and silently discovers nothing. The second is worse and
is live today: **the durable marker can name a different question from the one
searched.** The app hashes `row.Work` *before* calling the adapter, and the
adapter may then substitute a positive memoized canonical record and search on
*its* title instead.

A sentinel-triggered recovery does **not** fix that, which is what an earlier
draft of this item got wrong. The damaging sequence never produces a sentinel at
all:

1. `row.Work` already has a usable title, so no sentinel is returned.
2. A positive canonical memo also exists.
3. The app hashes `row.Work` as basis A and finds no marker for A.
4. The adapter silently searches memoized basis B.
5. Basis B's search completes; the marker records **A**.
6. Later enrichment changes `row.Work` to A′; the memo still holds B.
7. A′ looks novel, and buys the identical B search again.

**The fix is to make the effective basis explicit and authoritative.** A
side-effect-free `SiblingSearchBasis(requested) (work.Work, bool)` returns the
basis the adapter *would* use — positive memo metadata or caller metadata,
whichever wins, subject to one **explicit precedence rule: a positive memo wins
only if it yields a usable basis; otherwise a usable caller basis remains
eligible.** (That rule is now shipped in the adapter — wholesale replacement let a
positive-but-incomplete canonical record suppress caller authors, which was the
negative-memo defect arriving by the opposite route — so item 7 must preserve it
rather than reintroduce a plain override.) Usable means title plus a
canonicalizable author surname; a negative DOI memo suppresses only the
memo-derived basis, never the caller's own. The app hashes **that returned basis**
for the novelty marker, and the subsequent search is forced to use exactly it — no
hidden substitution after the marker check.

Only when no usable local basis exists does the caller, under its *own* separate
admission and credit commit, ask for a one-credit `SiblingBasis` network lookup.
That keeps three properties: each paid request is separately admitted, so a floor
installed by the singleton's own response stops the search; `ErrNoSearchBasis`
keeps meaning "no request happened"; and the marker names the question actually
asked. This item must be designed against the *current* basis semantics, which
changed after the earlier draft was written.

## 8. Merged into item 5

This was "do not enrich the canonical work from an unvalidated *fuzzy sibling*",
with the sequence: citation is DOI X with a sparse bibliography; canonical X
establishes a common title; fuzzy result Y shares that normalized title, a
surname, and a nearby year but is another work; Y's title, year and authors are
persisted alongside DOI X before Y's PDF proves anything.

**It was too path-specific.** Typed siblings naming a different DOI or version go
through the same `rank → fillMissing → FillWorkMetadata` stream, and so do
ordinary weak title matches — so a rule written about fuzzy siblings would have
left two equally dangerous routes open while looking complete. Item 5's
evidence-authority invariant covers all of them in one sentence, so this item is
folded there rather than kept as a separate narrower rule.

## Ordering

**`(0 estimate ‖ 5 evidence authority)` alongside `(1+2 egress authority)` → 3
truthful park → 4 jitter → 7 effective basis → 6 (deferrable only once 1+2 is
deployed).**

An earlier draft wrote `… → 4 → 5` while its own prose said item 5 must not queue
behind spend work; the two contradicted each other and the ordering above is the
honest one. Item 0 informs whether the fuzzy search stays enabled but gates
nothing. Items 1+2 are one boundary and one migration, and the newly found
`sourcegate.Client` quota bypass belongs **inside** that item — the shipped
`Acquire` fix closes it at admission, but only the egress authority closes the
TOCTOU window behind it. Item 5 is the only failure mode here that cannot be
undone and must not queue behind spend work. Item 7 needs the effective-basis
correction before implementation and is cheapest once 1+2 has settled how
admission is expressed, because it may need two admissions per hop.

Each migration means daemon and CLI deploy together (`make dev-deploy`), which on
this machine means both *papio* binaries plus the native-host symlink.

Genuinely deferred beyond all of this: wiring the dormant **monthly USD** axis
for real dollars. A billing feature, not a safety mechanism.

## Fixed while reviewing (already shipped)

- **`siblingSearchRecorded` failed open on an unreadable marker.** `Jobs.Events`
  decodes each detail with `_ = json.Unmarshal(...)` and yields a nil map on
  failure, so a corrupt `job.sibling_search` row left the basis `""`, matched
  nothing, and bought another ten-credit search — precisely when storage was
  already misbehaving. A marker of the right kind now counts as proof a search
  happened; only a legible, different basis is evidence the question is new.
  Regression: `TestUnreadableSiblingMarkerFailsClosed`.
- **The `atBoundary` comment over-claimed.** A pass held back only by a durable
  source gate has no temporary failure, so it satisfies the first arm and the
  search may run although that gated source will get another opportunity. The
  behaviour is deliberate (it matches `siblingHop`'s caller and the per-basis
  marker bounds it to one search per question), but the comment now says exactly
  that instead of implying necessity. **Open question for item 0's measurement:**
  whether a pending durable gate should also defer the search.
- **An unreadable retry history was also a positive spend permit.** Exhaustion
  is read in two opposite senses: for liveness "unknown" must mean stop, but
  `resolve` also reads it as one arm of `atBoundary`, which is what *authorizes*
  the ten-credit search. So a single transient `Jobs.Events` failure made
  exhaustion read true, the separate marker read then succeeded and found no
  marker, and the expensive query ran with a temporary retry still pending.
  Split into `retryBudgetExhausted` (fails closed, liveness) and
  `retryBudgetExhaustedProven` (returns the read error; only a proven fact is a
  permit). Regression:
  `TestUnreadableHistoryIsNoPermitForTheExpensiveSearch`.
- **The memo made cache residency part of acquisition correctness — half fixed,
  half reverted.** At the cap, `writeMemo` dropped the entire map, and a DOI-only
  job whose caller metadata has no title depends on that memo for its search
  basis, so unrelated traffic crossing the cap silently cancelled that job's
  sibling discovery. The cap now evicts expired entries first and drops
  wholesale only if that frees nothing (`TestMemoCapEvictsOnlyExpiredEntries`),
  and a fresh negative memo is now consulted instead of being reported like an
  absent one, so a DOI the provider does not know costs nothing to re-ask inside
  the TTL (`TestSiblingHonoursAFreshNegativeMemo`).

  **The one-credit basis re-earn was reverted.** It closed the availability gap
  and opened three worse holes, all found in review: it broke the
  `ErrNoSearchBasis` contract (the caller reads that sentinel as "the adapter
  made no request at all" and skips charging, so a paid singleton followed by
  the sentinel put a false fact in the retry plan — the exact class of defect
  this whole change set exists to remove); it let one `AcquireAny` admission
  cover two HTTP requests, so the ten-credit search could go out *after* the
  singleton's own response had installed the quota floor; and it made the
  durable basis marker name a question that was not the one searched, because
  the app hashes `row.Work` while the adapter searched the canonical record's
  title. **The gap is real and remains open**; the fix belongs to the caller,
  which can admit and charge two calls separately — see item 7 below.
- **A misrouted record could still be accepted and cached.** `echoesDOI` took the
  first parseable identity and stopped, so a record whose `doi` named the
  requested work while `ids.doi` named a different one passed. It now requires at
  least one parseable DOI and demands that *every* parseable echo equal the
  requested one, rejecting internally inconsistent records rather than trusting
  field order — and it is applied before the memo write, so an unverified record
  never becomes a later pass's search basis. Regressions:
  `TestEchoedDOIRejectsInconsistentIdentities`,
  `TestMisroutedRecordIsNeitherPublishedNorMemoized`.
- **An illegible transition detail silently shrank the retry budget's evidence.**
  `retryBudgetExhaustedProven` type-asserted the detail map and treated a nil
  result as a transition that does not count, so corrupt history read as *more*
  budget remaining. It now returns an error, which the fail-closed liveness
  wrapper turns into "stop" while the permit path correctly declines to treat it
  as proof.
- **The provider floor bound acquisition and ignored everyone else.**
  `sourcegate.Client` — which is how discovery, DOI-only enrichment, watch
  digests and MCP reach OpenAlex — admits through the single-policy
  `budget.Acquire`, and the `"<source>_quota"` signal was consulted only in
  `AcquireAny`. So the observer would write "keyed is spent", acquisition would
  correctly fall back to the keyless identity, and discovery would keep sending
  keyed until some ordinary 429 happened to establish a different gate. That is
  the "enrichment spends independently" class from the original incident, still
  open. The floor now binds inside `Acquire`, where every path passes; a
  fixed-policy caller has no alternative identity, so it defers until the
  provider's own reset rather than falling back. `AcquireAny` keeps its own
  pre-check, which is what distinguishes "this identity is spent, try the next"
  from an ordinary refusal. Regression:
  `TestAcquireHonoursTheProviderQuotaFloor`, including keyed/keyless and
  cross-source isolation.
- **The sibling search basis was wrong in both directions.** A fresh negative DOI
  memo returned `ErrNoSearchBasis` before the caller's own title and authors were
  even considered — but that memo proves only that the provider does not resolve
  *that DOI*, not that there is no basis for finding a sibling indexed under a
  different one, so usable caller metadata was suppressed. In the other
  direction, a bare title was accepted as sufficient to buy the ten-credit
  search even though every result is then required to share an author surname,
  and that check fails whenever either author list is empty — so the search was
  bought on a foregone conclusion. "Usable basis" is now one predicate
  (`usableSiblingBasis`) tied to what the acceptance side needs: a title plus at
  least one canonicalizable author surname. Regressions:
  `TestSiblingRefusesABasisThatCannotAccept` (table),
  `TestNegativeMemoDoesNotSuppressACallerBasis`.
- **Only one of the two exact endpoints verified its identity.** `Resolve` proved
  a DOI lookup came back *about* the requested DOI, but an OpenAlex-ID lookup
  trusted whatever the provider returned — and both publish at
  `IdentityConfidence 1.0`. Submit `W…` with no DOI, receive a valid record for a
  different work, and the resolver asserted an exact canonical match. Now
  `echoesOpenAlex` mirrors `echoesDOI` with the same fail-closed rules (at least
  one parseable echo, every parseable echo must agree, version-preserving
  normalization), applied before the memo write. Regression:
  `TestExactOpenAlexIDLookupRequiresAnEchoedID` — whose first draft was
  *vacuous*, because `work.NormalizeOpenAlex("W1")` is invalid and every case
  returned early; three of four "passed" for the wrong reason until the fixture
  used a real id shape.
- **A whitespace-padded API key defeated the 5% floor entirely.** The resolver
  trims the configured key before putting it on the wire and `budget.identityFor`
  trims it before deriving the identity, but the observer compared the outgoing
  `"key"` against an untrimmed configured `" key "` — matching neither arm of its
  switch, so it silently recorded nothing. A configuration the rest of the stack
  deliberately treats as equivalent turned the floor off. The credential is now
  canonicalized at `NewObserver`. Regression:
  `TestObserverCanonicalizesTheConfiguredCredential`.
- **A positive-but-incomplete memo suppressed a usable caller basis.** The memo
  replaced caller metadata wholesale before the usable-basis test, so a canonical
  record with a title and no legible authors cancelled the hop — the same defect
  as the negative-memo case, arriving by the opposite route. A positive memo now
  wins only if it yields a usable basis. Regression:
  `TestIncompletePositiveMemoDoesNotSuppressACallerBasis`.

## Rejected designs (do not re-derive)

- **Re-earning a sibling search basis inside `ResolveSiblings`.** Shipped and
  reverted within the hour. One admission covered two HTTP requests, so the
  ten-credit search could go out after the singleton's own response had installed
  the quota floor; a paid singleton followed by `ErrNoSearchBasis` put a false
  "no request happened" fact into the retry plan; and the durable marker then
  named a question that was not the one searched. Item 7 is the caller-side fix.
- **`RoundTripper.RoundTrip` as the one-wire boundary.** The standard `Transport`
  may replay an idempotent request after certain connection failures, and every
  OpenAlex call is a GET — so one debit could still cover two physical requests.
  Disabling redirects is necessary and insufficient.
- **Latching the lost floor only when the durable write fails.** The write can
  block for seconds; during that window the process knows the fact while egress
  still sees no gate. Latch on the parsed header instead.
- **A process-only pricing-drift closure.** Commit 10, learn the shape cost 100,
  mark it closed, crash — only the under-sized debit survives and the operation is
  authorized again. The closure must be durable.
- **Leaving the late-refusal error contract to item 3.** `openalex.fetch` flattens
  every client error into a fresh generic `TemporaryError`, so a quota refusal
  raised at egress cannot reach the fallback logic and a credit refusal cannot
  reach the park logic. The taxonomy belongs with the egress design.
- **"Independently tied to the submitted identity" as the enrichment test.** A
  typed sibling relation is genuinely tied to work X while describing work Y. The
  test is *verified as describing the same work*.
- **Comparing validation against the mutable `row.Work`.** That is the
  self-confirming loop: the anchor must be an immutable snapshot of the submitted
  identity.
- **Verifying the echoed identity on the DOI endpoint only.** The OpenAlex-ID
  endpoint publishes at the same 1.0 confidence and had no check at all.
- **Expecting the daily credit fuse to restore the provider floor.** They are
  different boundaries: a source-wide counter answers "how much may this instance
  commit today", the floor answers "is this credential's provider balance spent".
  A committed credit satisfies the first and is silent on the second, so the
  egress authority must revalidate the outgoing identity's gate too.
- **Gating only the credential when observed cost exceeds the committed shape
  cost.** Pricing is a property of the operation, not the identity, so keyed
  detects the drift, gets gated, and the next call falls through to keyless and
  repeats the same undercharged shape.
- **A sentinel-triggered basis recovery as the fix for the marker mismatch.** The
  damaging sequence never returns the sentinel: caller metadata is usable, the app
  hashes basis A, the adapter silently searches memoized basis B, and the marker
  records A.
- **Putting the egress commit at a generic `HTTPClient.Do`.** `http.Client.Do`
  follows redirects, so one commit can authorize several physical requests. Use
  `RoundTripper.RoundTrip`, or disable redirects and treat one as a response
  needing fresh admission.
- **Letting `0 == unmetered` skip the durable commit.** Disabling enforcement
  would then also bypass the egress invariant and its accounting — exactly when an
  operator most needs the numbers.
- **A conditional UPSERT whose limit test lives only in the conflict-update arm.**
  The first write of a day then admits a 10-credit request against an allowance of
  5.
- **Item 8 as a fuzzy-sibling-specific rule.** Typed siblings and ordinary weak
  title matches use the same pre-validation merge, so the narrow rule looked
  complete while leaving two equally dangerous routes open.
- **Accepting a record on the first parseable DOI echo.** A record naming the
  requested work in `doi` and a different one in `ids.doi` passed. Every parseable
  echo must agree.
- **Re-basing `retryBudgetExhausted` on a lifetime `PaidAttempts(jobID)` count.**
  Wrong dimension (58–1,470 attempt rows per settled job, worst 4,949), and it
  inverts the sibling gate: that predicate is what *permits* the ten-credit
  search, so exhausting the budget would unlock it.
- **Treating a NULL attempt `outcome` as proof of spend.** `StartAttempt`
  precedes admission and `FinishAttempt` errors are ignored, so NULL means
  unknown.
- **Counting a generic forward state transition as progress.** `retry_wait ->
  resolving` (332×/day) and `resolving -> fetching` (231×) fire every pass; the
  counter would reset every pass and never bind.
- **A single last-sibling-basis column replacing the event set.** Breaks A→B→A: a
  scalar buys basis A a second ten-credit search.
- **A second per-job `lifetime_pass_charges` ceiling.** No evidence supports a
  defensible N, and a pass count is loosely coupled to spend once passes contain
  differently priced calls.
- **A reservation/settlement ledger with one-shot `Confirm`/`Release`.**
  Over-engineered for a safety ceiling: a never-refunded pre-admission debit
  gives the same guarantee, and treating availability as the safe failure mode
  removes correlation, settlement, cross-midnight, and preflight problems
  wholesale.
- **Retryable settlement against aggregate columns.** Two settlements of one
  handle silently consume a different in-flight reservation.
- **Enforcing the daily allowance per `(source, identity)`.** Doubles the
  configured cap's real meaning across keyed and keyless; the provider floor
  already protects each pool per identity.
- **Charging a per-job episode after the adapter returns** so post-call no-wire
  sentinels can be honoured. That is exactly the wire→crash→uncharged gap the
  guard existed to close.
- **Putting credit shape into `Acquire`'s existing `estimatedCost`.** It is
  dollars, and flows to `spent_usd`.
- **Making OpenAlex's `source_budgets` window daily.** One `window_start` also
  governs the request counter and the monthly USD counter.
- **A rolling time window as an episode reset.** Time alone re-authorizes a
  permanently stuck job — the original defect on a timer.
- **Restricting the fuzzy search to paywalled canonical records.** Premature; the
  hop can rescue an OA record whose advertised locations are unusable. Item 0's
  measurement is the right way to decide its fate.
- **`LatestGate` → earliest gate.** With one post-exhaustion wait available,
  waking at the earliest gate starves the latest-gated source of the call the
  grace rule exists to give it.
- **Merging the two identifier normalizers.** `internal/work` is
  version-preserving (acquisition), `internal/ownership` version-collapsing
  ("same work?").
