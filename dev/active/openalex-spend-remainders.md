# OpenAlex spend remainders: a credit fuse, an egress invariant, and an identity audit

Salvaged from the 2026-08-14/15 OpenAlex spend work (shipped and deployed:
charging mixed passes, the record memo, `select=`, the header floor, the
anonymous fallback, the fuzzy-search boundary/basis gate, and three fail-closed
guards). Those changes are complete; git history and `CHANGELOG.md` hold the
record.

**Ten adversarial reviews, nine rewrites.** Three early rounds sharpened a
two-part plan (a per-job spend guard plus a per-identity credit ceiling); a fourth,
given no prior-round context and a wide brief, rejected the shape of both; three
more reviewed the shipped code and found live defects in it; the eighth falsified a
premise this file had just been rebuilt around; the ninth, given the deployed code
and no prior context, found three more live defects — two of them created by the
previous rounds' own fixes — and named the two places where this plan promises what
its mechanism cannot deliver. Anything a review asserted about the provider or the
language was confirmed independently before acceptance. Every finding is verified
against the tree or the live provider before being written down. Reviews are
preserved under `dev/scratch/oracle/20260815T*`. The tenth found no defect in shipped
code — every finding was a plan claim the mechanism could not deliver, which is the
first round that produced no code change.

**Rejected designs** at the bottom records every dead end with its reason —
twenty-three of the forty-four from earlier drafts of this file. Read it before
proposing a simplification; most of the obvious ones are in it, with the sequence
that breaks them.

The recurring shape of every defect found so far, mine included: **a decision made
from information that is not durable yet, or is not what it claims to be.** An
in-memory charge a crash erases; a fail-closed guard that becomes a permission
when a second caller reads it; a best-effort note treated as an authority; a cache
treated as a fact; a durable fact read by only one of its two readers.

## Audience: heterogeneous tiers, not one keyed machine

Every number in the original incident came from one keyed identity on the author's
own machine. That is not the shape of a shared install, and designing against it
produced a mechanism that would have shipped permanently inactive.

- `SourceOpenAlex` defaults to `{Enabled: false}`
  (`internal/config/config.go:579`), so enabling it is deliberate.
- **An earlier draft of this section claimed API keys are "granted rather than
  self-serve", and therefore that keyless is the common install. That is false**
  and is recorded here rather than quietly deleted, because the conclusion it was
  used to justify survived while its premise did not. OpenAlex's current
  documentation (verified live: `help.openalex.org/api/authentication`) says a key
  is free, self-serve, and about thirty seconds' work — an account plus a copy from
  a settings page — and raises the daily budget 10×. So no claim about keyless
  predominance is established, in either direction.
- What *is* established is that capacities differ by an order of magnitude per
  tier, measured live: keyless `X-RateLimit-Limit: 1000`, free keyed `10000`, paid
  plans higher, plus a prepaid balance beyond the daily budget. Relative policy is
  right because of that heterogeneity, not because of a population guess.
- **A search costs 10 credits and a singleton costs 1** — measured, and note the
  provider's own pricing page says singleton retrieval is *free*, which the header
  contradicts. On the keyless tier one search is **1% of the entire day**.
- **The best answer to a keyless user is not a smaller budget, it is a key.** A
  10× increase for thirty seconds of work beats any rationing *papio* can implement,
  so `papio doctor` should say so when OpenAlex is enabled without a key. That is a
  one-check change and it dominates the tuning question.
- Consequences for this plan, applied above but recorded here because they change
  priority order, not just numbers:
  1. **Policy is expressed relative to the provider-reported limit**, never as an
     absolute credit count derived from one key — subject to the frozen, monotone
     denominator in item 1+2, since a provider-reported number now has authority
     over a safety limit.
  2. **The fuzzy sibling search is disproportionately expensive on the low tier.**
     The measurement item — is a 10-credit search worth buying, given 73 of 3,165
     ever returned anything — outranks *optimising or rationing* it. It does not
     outrank the egress fuse, which closes crash, storage-failure, cross-caller and
     pricing-drift holes regardless of whether the search is worth buying at all.
  3. **Visibility ships with enforcement, not after it.** A stranger who hits a
     ceiling sees "papio stopped working"; `spent_usd` currently reads `0.00`
     against a real 3,393 credits. `papio doctor` must name today's credits, the
     limit, and the identity in the same change that can refuse work.
  4. **Safety numbers validated only against the author's Zotero library are
     unfalsifiable elsewhere.** `make identity-corpus` reads a private library, so
     a committed shareable corpus with published wrong-accept counts is what lets
     anyone else tune item 5's thresholds. **Not a blocker for item 5**, though: the
     structural work — the immutable submitted-identity anchor, the promotion-path
     remediation, deterministic adversarial regressions — closes the
     self-confirmation defect without tuning any probability. The corpus gates
     *changing a threshold*, not shipping the fix.
  5. **Re-derive item 6's deferral, don't inherit it.** "The fuse bounds the
     damage" was argued on a 10,000-credit day, where one hopeless work burning 8
     passes at 10 credits is 0.8% of the budget. On the keyless tier the same work
     is **8%**, and a bulk import holds many. Still deferrable — the fuse caps the
     aggregate either way — but the justification changes with the tier, so state
     which one it is being argued on.

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

**The 2.3% is the yield of this query shape, not of OpenAlex — measure the shape
before condemning the feature.** The plan and the code both call this a "title
search", and it is not one. `lookupURL` sends `?search=<title>&per_page=10`.
OpenAlex documents `search=` as matching title **plus abstract and full text**,
ranked by relevance and citation count, and permits `per_page` up to 100 (a
title-scoped `title.search` filter also exists, currently marked deprecated).
*papio* then applies its own exact-normalized-title test to whichever ten records
that broad relevance ranking happened to return — so an exactly-titled record
ranked eleventh is a ten-credit "no result". Three cheap variants to compare on
the same sample before spending engineering on either rationing *or* removal:
`search=` with `per_page=100`, `title.search=` scoped, and the current shape.
Same cost per call in every case, since pricing is per request and not per row.
**Do not "improve" yield by loosening the acceptance predicate** — that trades the
worst outcome *papio* has (item 5) for a metric.

**Consequence for item 7:** item 0 gates nothing for the fuse or for item 5, but
it *does* gate item 7. Item 7's ternary-basis and re-earn protocol exists to make
the sibling hop correctly available and memoized; if the measurement says the hop
should be deleted, building that protocol first is pure sunk cost. Sequence item 7
after item 0 reports, not before.

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

**Requirement: a boundary with provably no automatic replay. There are *three*
replay mechanisms, and `DisableKeepAlives` closes only one.** Verified in the tree
and against Go 1.26.6:

1. ***papio*'s own redirect loop.** The metadata client is
   `fetch.NewSecureHTTPClient(..., http.DefaultTransport)`, whose `Do` runs its
   own hop loop (`internal/fetch`: `validateURL` → `roundTrip` → `isRedirect` →
   repeat, bounded by `policy.MaxRedirects`). So one `Do` already issues **N
   physical requests** under one admission today. This is not `net/http`'s
   transparent following — *papio* replaced that to get an SSRF guard per hop — but
   the accounting consequence is identical.
2. **HTTP/2's internal retry.** `http.DefaultTransport` sets
   `ForceAttemptHTTP2: true` (verified: `Protocols == nil`,
   `ForceAttemptHTTP2 == true`), and Go's HTTP/2 transport has its own retry loop
   around `RoundTrip` that will re-send a bodyless GET after a retryable
   GOAWAY/stream failure. `DisableKeepAlives` bounds connection *reuse*; it makes
   no promise about attempts per `RoundTrip`. So it does **not** close this.
3. **HTTP/1 retry on a reused connection**, the one `DisableKeepAlives` does close.

**Decision: an OpenAlex-specific transport that is HTTP/1-only, with
`DisableKeepAlives`, no automatic redirect following, and every redirect hop
re-entering the egress authority as a fresh guarded request.** Go 1.26 exposes
`Transport.Protocols`, so `SetHTTP1(true)` / `SetHTTP2(false)` is available and
testable (verified locally). Not offered as a config knob: correctness must not
depend on a stranger's network quality, and "*papio*'s credit accounting is
slightly wrong under packet loss" is undebuggable remotely. Rejected alternative:
wrapping the dialer so replays re-enter the authority — correct and faster, but it
puts the money invariant in the least-inspectable layer in the stack.

**The redirect hop is not merely an accounting problem — it currently corrupts
identity and attribution.** OpenAlex answers a merged entity with `301` whose
`Location` carries only the new entity URL, and the credential travels in the
`api_key` **query parameter**. So: request `Wold?api_key=K` → `301` → papio's hop
loop re-requests a Location with **no `api_key`** → that physical request is
anonymous → `Observer.observe` inspects the *original* request, sees `K`, and files
the anonymous pool's headers against the keyed identity, corrupting the very floor
this plan relies on. Then `echoesOpenAlex` compares `Wnew` against `Wold` and
correctly rejects a legitimate merge. So the redirect work must: recognise a
same-origin OpenAlex entity-merge `301`, validate the `Location`, re-admit and
re-debit it as a new guarded request under a re-derived identity, bound the depth,
and carry the redirect forward as **authoritative alias evidence** so `Wold → Wnew`
passes exact identity verification instead of being discarded.

Regressions: the production OpenAlex transport cannot negotiate HTTP/2; a
merge-`301` yields one debit per physical hop, correct per-hop identity
attribution, and an accepted merged record.

**Requirement: the new authority derives identity from the OUTGOING request, and
must not inherit `sourcegate.Client`'s construction-time policy.** The two live
wirings differ, and the difference is easy to get wrong when the wrapper is added.
`resolverEntries` (`bootstrap.go`) injects a bare `sourcegate.Observer` and is
deliberately *not* wrapped in `sourcegate.Client`, because admission for that path
already happens per call at the `app.go` `AcquireAny` sites — which is correct, and
is why the keyless fallback accounts honestly today: the identity admitted is the
identity `anonymousIfFallback` then puts on the wire. Discovery, DOI-only
enrichment, watch digests and MCP go through `sourcegate.Client`, which holds one
`config.Source` captured at construction and calls `Acquire` with it regardless of
what the request carries. Those paths never strip the key, so nothing is
mis-accounted **yet** — but a fixed-policy wrapper placed at the wire, on a client
whose context can carry `resolver.WithAnonymousCredentials`, produces one physical
request with two different identities in the two authorities. Decide explicitly
whether the existing OpenAlex `sourcegate.Client` reservation is removed or
narrowed to identity-agnostic pacing when the new authority lands; leaving both
means the correct authority can still be pre-empted by the wrong-identity gate.

**Option worth taking while touching this: move the credential to a header.**
OpenAlex accepts bearer authentication as equivalent to `api_key=`. Sending it as
`Authorization` removes the *query-stripping* half of the merge-redirect defect
outright — a `Location` without the parameter no longer downgrades the hop to
anonymous — and takes the key out of URLs that get logged. It does **not** remove
the need for per-hop re-admission and re-debit, and `servedIdentity` must then read
the header rather than the query, so the change is not a substitute for the
redirect work.

**Requirement (SHIPPED — see the fixed list): latch on the parsed header, not on
the failed write.** The in-process fail-closed latch is set for that credential
immediately, before persistence is attempted, and survives until the provider's own
reset. Waiting for `Defer` to fail leaves a window — the durable write can block for
up to five seconds — in which the process already knows the fact while new egress
still sees no gate. A transient SQLite busy can also drop the floor while the credit
write succeeds, so "both writes fail together because the disk is full" is too
narrow an assumption. What remains for this item is the *reader* side: the new
egress authority must consult the latch, since today only `sourcegate.Observer.Do`
enforces it, which covers every OpenAlex client in the tree but is a different layer
from the credit commit.

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
| pricing-drift closure | **sticky, no expiry** — see below | park; never falls back; needs an explicit acknowledgement to reopen |

Each must be preserved — or deliberately translated — through `fetch` rather than
flattened. The scheduler wiring can still land as item 3, but the taxonomy cannot
wait for the egress implementation, because both sides encode it independently
otherwise.

**Requirement: the final transaction revalidates EVERY durable no-egress
authority, not two.** Describing it as "provider quota + daily credit" reintroduces
the exact race the shipped `reserve` already had to fix twice. Sequence: worker A
clears `AcquireAny` with the ordinary source gate open; worker B takes a plain 429
and durably `Defer`s the bare source name; A reaches the final transaction, finds
its identity quota open and its debit fitting, and sends **after a durable ordinary
gate already exists**. The predicate set is therefore: ordinary `next_allowed_at`
for the bare source, the identity's `<source>_quota` floor, the source-wide
pricing-drift/prepaid closure, and the daily credit allowance — all read inside the
committing transaction.

**The in-process latch must move.** `latched` currently lives on an `Observer`
*instance* (`sourcegate.go`), which is correct for the layer it guards but is not a
source-wide authority: the new egress authority cannot merely "consult the latch"
across a package boundary, or a failed durable write recreates the same TOCTOU one
layer down. Move the latch into — or share it with — the egress authority, and
define the linearization point between setting it and admitting a request.

**Requirement: do not promise crash-durability the mechanism cannot give.** This
file already concedes it for the quota floor (latch first, persist best-effort);
the same concession is mandatory for every other response-derived fact the plan
wants to act on. Sequence: the persisted denominator is 10,000; a response
establishes 1,000; the lowering write fails on a busy disk; the daemon crashes. The
counter survived, the "monotone" denominator did not. So: **the debit plus an
absolute hard maximum are crash-hard; provider-relative denominators and
response-derived pricing/prepaid brakes are not.** Set their process latch
immediately, and define restart behaviour explicitly — for keyed installs OpenAlex
exposes `/rate-limit` (credits used/remaining, endpoint costs, prepaid state), so a
restart preflight is available; where no authoritative preflight exists, fall back
to the crash-hard absolute cap rather than assuming the relative guarantee survived.

**Requirement: positive pricing drift is sticky, not a daily gate.** Giving it "the
remaining local window" makes an unattended daemon repeat the unsafe request every
day: search moves 10 -> 100 credits, *papio* debits 10 at 14:00, observes 100,
closes until midnight, and at 00:00 the closure expires while the classifier still
says 10. It closes the source until an explicit software or operator
acknowledgement establishes a new conservative cost schedule, or an authoritative
provider cost description is validated and adopted. Item 3 cannot model it as
`{until: next UTC}`.

**Requirement: name the migration-day and cold-start seeds.** Two ways the
advertised daily bound is false in practice. *Upgrade day*: the migration creates
`credits_committed = 0` while *papio* may already have authorized thousands of
credits since midnight, so the fuse authorizes its full allowance a second time on
the very day it deploys — either close the remainder of the current UTC day or seed
from an authoritative usage observation, never silently start at zero. *Cold start*:
"a conservative absolute until a limit is observed" is a cohort, not one request —
100 workers can reach the authority before the first response persists a
denominator, and a fallback sized for the keyed tier can overshoot the keyless
tier's whole intended fraction. Name the exact fallback, make it safe against the
lowest supported tier, or permit only a bounded bootstrap egress until the primary
basis is established.

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
- **The provider reports USD as well as credits, so the unit question is settled
  by the wire, not by taste.** Measured live on every response:
  `x-ratelimit-credits-used: 1` **and** `x-ratelimit-cost-usd: 0.0001`;
  `x-ratelimit-limit: 10000` **and** `x-ratelimit-limit-usd: 1`; plus
  `x-ratelimit-remaining-usd` and `x-ratelimit-prepaid-remaining-usd`. Credits are
  simply hundredths of a cent. **Decision: meter in credits internally** — they
  are integers, so the conditional SQL cannot drift on float rounding. Do **not**
  pass credits through `reserve(ctx, source, identity, policy.MaxCostUSD,
  estimatedCost)`: that argument is dollars, and reusing it implements the
  credits-as-dollars design this document rejects.
  - **Do not feed observed `cost-usd` into `spent_usd`.** An earlier draft of this
    bullet said the provider's USD figures let "the dormant axis finally be fed
    truthfully", and that contradicts what `spent_usd` *is*. It is not telemetry:
    `reserve` reads it before the wire, refuses when `spent + cost > limit`, and
    increments it as part of monthly admission — `ErrExceeded` is that refusal. A
    second writer adding provider-reported dollars to the same column therefore
    either double-counts every non-zero estimate or silently activates a monthly
    admission authority this plan explicitly defers. **Decision: observed USD goes
    to a separate diagnostic column with no admission semantics, or is not stored
    at all this tranche.** `spent_usd` is left alone until the monthly-USD feature
    is redesigned deliberately.
- **`prepaid-remaining-usd` changes the stakes: a runaway loop can spend real
  money, not just today's allowance.** A prepaid balance covers usage beyond the
  daily budget, so on any paying installation the pre-fix failure mode was not
  "gated until midnight" but "*papio* charged the user". This is the strongest
  argument in the file for the fuse being safety rather than hygiene, and it was
  invisible while only `credits`/`remaining` were being read.
  - **State the guarantee as what the mechanism can actually deliver.** An earlier
    draft promised the fuse would "refuse to draw down prepaid balance
    implicitly". It cannot: the durable counter knows only what *papio*
    committed, and the provider's balance is learned only from responses *after*
    *papio* sends. An OpenAlex account is shared — the web UI and any other client
    draw on the same budget — so another consumer can exhaust the free allowance
    while *papio*'s own counter sits comfortably under its fraction, and *papio*'s
    next admitted call is the first one charged to prepaid. No arrangement of
    passive response headers closes that, and claiming otherwise is exactly the
    class of error this file keeps catching: an observation treated as an
    authority. **The honest invariant: *papio* bounds the credits it authorizes,
    and stops as soon as it observes impending or actual prepaid use.** A hard
    "*papio* can never spend prepaid dollars" claim requires provider-side
    no-overage enforcement or exclusive account use, neither of which *papio* can
    establish. Note the corollary for the escape hatches: an unmetered
    `daily_credit_fraction = 0`, or an absolute override above the remaining free
    allowance, removes even the bound — so `prepaid-remaining-usd` falling below
    its starting value must close egress regardless of local ceilings.
- **Atomic in SQL across insert AND update.** Hundreds of jobs target the same
  hot row, so it must be one conditional mutation — and the condition must cover
  the *first* write of a day, not only the conflict-update arm. A naive
  `INSERT … ON CONFLICT DO UPDATE` that puts the limit test only in the update
  arm admits a first 10-credit request against an allowance of 5. Test the
  empty-row-over-limit case explicitly.
- **Config: express the ceiling as a fraction of the provider-reported limit, not
  as credits. `daily_credit_fraction`, default `0.5`, under a local hard
  maximum.** An absolute default overfits to one machine's key: measured live,
  keyed reports `X-RateLimit-Limit: 10000` and keyless `1000`, so a fixed `4000`
  never fires on the keyless tier at all. A fraction is one policy for a budget
  *papio* cannot know in advance.
  - **Define the denominator, freeze it, and never let it grow.** "Fraction of the
    provider-reported limit" is ambiguous the moment two identities report
    different limits, and the counter is deliberately source-wide: keyed
    establishes 10,000 → allowance 5,000 → fallback to anonymous reports 1,000 →
    the allowance either shrinks below what is already committed, or stops meaning
    what it says. **Decision: the denominator is the configured primary identity's
    reported limit (keyed when configured, else anonymous), captured durably for
    that UTC day; later reports may lower it, never raise it**, and anonymous
    fallback does not rewrite it. Without the monotone rule a malformed
    `X-RateLimit-Limit: 1000000000` would *enlarge* the fuse — an absolute limit
    had no such dependency, and this is the price of relative policy.
  - **Persist the day's basis.** The observer parses `limit` but only stores
    anything when it reaches the floor, so nothing survives a restart today.
  - `daily_credit_limit` (credits) stays as an absolute operator override and as
    the hard maximum the fraction may never exceed.
  - **Before the first response of the day there is no reported limit.** Do not
    gate the first request on an unknown budget: fall back to a conservative
    absolute — itself bounded by the hard maximum — until a limit is observed. Test
    the cold-start path, or the fuse either blocks everything or nothing on a fresh
    daemon.
  - Revisit from `credits_committed` telemetry across real installs, not from one
    library's numbers. `0` means unmetered, matching the existing convention, so
    `papio init` and the shipped defaults must write the non-zero value — with a
    test on the defaults, or the fuse ships inert. Document it in the
    hand-authored `docs/reference/config-reference.md`. Strict config decoding
    means the field and the binary deploy together.
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

## 4. Jitter the budget-reset wake — deferred out of this tranche

**Cut on review, and the reasoning is worth keeping:** once final-egress admission
and the token bucket are correct, a UTC-midnight wake cohort is an operational
smoothing problem, not a monetary invariant. The burn shape was caused by the
uncharged loop, not by the synchronised wake — the wake only made it visible.
Implement when telemetry actually shows scheduler or store contention at a reset
boundary. The design below is settled, so this is a scheduling decision, not
unfinished work.

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

**The primary resolution path already manufactures such a candidate, and it is
not a fuzzy-sibling edge case.** `matchesTitleSearch` (`openalex.go`) requires an
exact normalized title, then skips the year test when `requested.Year == 0`, and
`sameAuthorLists` returns `true` outright for an empty requested author list. So a
submission of `{Title: T}` with no DOI, no year and no authors is matched on title
*alone* — and the accepted record's own DOI, OpenAlex ID and PMID are emitted at
`IdentityConfidence: 0.75`. Two works sharing a normalized title (a preprint and
an unrelated paper, a common review title, a translation) are indistinguishable to
that predicate. This is independent of the sibling hop, survives deleting the
sibling hop entirely, and is sufficient justification for the invariant on its
own: item 5 must not be narrowed to a fuzzy-sibling rule, and must not be gated on
threshold tuning or a corpus.

**The legacy cutover, measured rather than assumed.** A tenth review called this
invariant unachievable retroactively, on the premise that no immutable record of
the submission exists and the anchor would have to be seeded from mutated canonical
state. That premise is half wrong, and the true shape is cheaper. `work_requests`
already preserves the *submitted* title/authors/year: `internal/job/job.go:717-725`
fills each field only when it is empty, so a supplied value is never overwritten by
a resolver. And identifiers are not in `work_requests` at all — they live in
`identifiers(work_request_id, kind, value, raw)`, inserted with a conflict check
(`job.go:744-749`) that *errors* when a resolver reports a different value for an
identifier the request already carried.

So exactly one thing is missing, and it is a column: **`identifiers` has no
provenance**. A DOI the user typed and a DOI adopted from a title-only match are the
same row, and the conflict check cannot fire on the second case because there was
nothing to conflict with. Live counts: 715 requests, 907 identifier rows, 98
requests with no identifier at all.

The cutover is therefore:
- Add `identifiers.provenance` (`submitted` | `verified` | `adopted`), and set it at
 every insert site. Post-cutover, a promotion may only write `adopted`, and only
 `submitted`/`verified` may anchor a canonical-identity comparison.
- Backfill every pre-cutover row as **`unattested`**, not `submitted`: the
 distinction was never recorded and must not be manufactured now.
- Prohibit canonical-identity promotion and the unattested `DOI -> SHA256` cache
 fast-path on `unattested` anchors until independent revalidation or resubmission.
 This is the review's requirement, and it is right; what changes is that the
 remediation is a backfill plus a predicate, not a reconstruction of lost truth.

Do not state the invariant as retroactively enforceable on existing rows. State it
as prospectively enforced, with legacy rows explicitly quarantined from the paths
that could act on an identity nobody attested.

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

**The hashed value must include the canonical DOI, and must not be `work.Work`.**
Sibling acceptance does not depend only on title/year/authors: `ResolveSiblings`
also *excludes* the canonical DOI from its result set (`resolved.DOI ==
canonicalDOI`, `openalex.go`), so the canonical identifier is part of the question
being asked. Hashing only the textual basis suppresses a search that genuinely
changed: basis `{DOI: X, title: T, authors: A}` is marked done; canonical identity
is later legitimately corrected to DOI Z with the text unchanged; under Z the
excluded row is different and X is itself a candidate sibling, yet the hash matches
and the needed search never runs. Hashing the whole `work.Work` makes the opposite
error — an unrelated PMID or OpenAlex-ID enrichment buys a fresh 10-credit search.
So the effective basis is a dedicated immutable value carrying exactly the fields
that affect the request and the acceptance predicate: normalized search title, year,
canonicalized author-match keys, and canonical DOI. Hash that value, and pass *that
same value* into the search — do not use `work.Work` as the protocol object.

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
side-effect-free `SiblingSearchBasis(requested)` returns the basis the adapter
*would* use — positive memo metadata or caller metadata, whichever wins, subject to
one **explicit precedence rule: a positive memo wins only if it yields a usable
basis; otherwise a usable caller basis remains eligible.** (That rule is now
shipped in the adapter — wholesale replacement let a positive-but-incomplete
canonical record suppress caller authors, which was the negative-memo defect
arriving by the opposite route — so item 7 must preserve it rather than reintroduce
a plain override.) Usable means title plus a canonicalizable author surname; a
negative DOI memo suppresses only the memo-derived basis, never the caller's own.
The app hashes **that returned basis** for the novelty marker, and the subsequent
search is forced to use exactly it — no hidden substitution after the marker check.

**The result must be ternary, not `(work.Work, bool)`.** `recordFor` deliberately
returns `false` for both "never asked" and "asked, fresh negative memo", and a
two-valued basis result collapses them again — so a pass whose DOI `Resolve` just
got a 404 would immediately buy the *same* singleton back as a basis lookup. Return
`usable` / `known_no_remote_basis` / `lookup_needed` (names immaterial): a fresh
negative memo suppresses the redundant singleton without suppressing usable caller
metadata.

Only in the `lookup_needed` state does the caller, under its *own* separate
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
truthful park → 7 effective basis, *if item 0 says the sibling hop survives*.**
Then, deferred with reasons written down: **4 jitter** (operational smoothing, not
an invariant) and **6 per-job guard** (fairness, capped in aggregate by the fuse).

Item 0 gates nothing else — not the fuse, not item 5 — but it does gate item 7,
which exists solely to make the sibling hop correctly available and memoized.
Measuring the query shape (`search=` vs a title-scoped filter, `per_page` 10 vs
100) is part of item 0, not a separate task: the current 2.3% is the yield of one
truncated broad query, so the hop could be worth keeping *or* worth deleting and
the measurement cannot tell you which until the shape is controlled for.

Within item 1+2 the shortest correct path is: **build the guarded one-hop request
primitive → define, freeze and clamp the relative denominator → express `Wold →
Wnew` as an explicit redirect loop over that primitive.** An earlier draft put the
merge-redirect fix first, on the grounds that it is a live defect corrupting the
floor. As a dependency graph that is backwards: a *correct* merge fix is exactly
"every hop re-admits and re-debits", so it needs the primitive that was scheduled
last. The primitive is: HTTP/1 only, no connection reuse, no redirect following,
full revalidation of every durable no-egress authority, atomic debit — one commit,
one physical request. Then the redirect loop calls it per hop and carries alias
evidence forward. Bearer-header auth may land earlier as a narrow attribution fix
(it keeps the key off redirected query strings), but redirects are not solved until
the guarded-hop primitive exists.

An earlier draft wrote `… → 4 → 5` while its own prose said item 5 must not queue
behind spend work; the two contradicted each other and the ordering above is the
honest one. Item 0 informs whether the fuzzy search stays enabled but gates
nothing. Items 1+2 are one boundary and one migration, and the newly found
`sourcegate.Client` quota bypass belongs **inside** that item — the shipped
`Acquire` fix closes it at admission, but only the egress authority closes the
TOCTOU window behind it. Item 5 is the only failure mode here that cannot be
undone and must not queue behind spend work. Item 7 needs the effective-basis
correction and its ternary result before implementation, and is cheapest once 1+2
has settled how admission is expressed, because it may need two admissions per hop.

Each migration means daemon and CLI deploy together (`make dev-deploy`), which on
this machine means both *papio* binaries plus the native-host symlink.

Genuinely deferred beyond all of this: wiring the dormant **monthly USD** axis for
real dollars. Now cheaper than it looks — the provider reports `cost-usd`,
`limit-usd` and `remaining-usd` on every response — but still a billing feature
rather than a safety mechanism, *except* for `prepaid-remaining-usd`, whose refusal
belongs in the fuse because drawing it down spends real money.

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
- **The memo cap still dropped every fresh entry, just at a different trigger.**
  An earlier fix evicted expired entries first and replaced the whole map only "if
  that frees nothing" — which is the same availability cliff, reached by 512
  *simultaneously fresh* entries instead of by any 512. A DOI-only job whose caller
  metadata has no title depends on that memo for its search basis, so unrelated
  traffic between its `Resolve` and its sibling hop could still delete its only
  basis. Now exactly one oldest entry is evicted. Note the residual, stated in the
  test: oldest-first is a *bound*, not a guarantee — a basis that genuinely is the
  oldest live entry can still go, one at a time rather than 512 at once; item 7's
  re-earn is the real repair. Regression:
  `TestMemoCapEvictsOneOldestEntryWhenNothingHasExpired`, whose first draft failed
  because the fixture made the basis itself the oldest entry.
- **`openalex.fetch` destroyed every client error, including admission refusals.**
  It replaced the cause with a fresh generic `resolver.TemporaryError`, so the
  fixed-policy quota floor's `*budget.ErrDeferred{Until: midnight}` reached the
  caller as an undifferentiated transport failure: it could not park until the
  provider's reset and cycled through generic retry instead. Not a spend defect —
  each retry is refused before the wire — but live liveness and diagnostic churn.
  `TemporaryError` already implements `Unwrap`, so wrapping instead of replacing
  restores `errors.As` without coupling the adapter to `internal/budget` or
  changing the retry classification. Regression:
  `TestFetchPreservesTheClientCause`. The app-side park semantics remain item 3's
  typed taxonomy.
- **A failed floor write handed permission back.** `Observer.observe` parsed
  `remaining`/`limit`/`reset`, determined the 5% floor was crossed, attempted the
  durable `Defer`, and on failure logged and returned — so a busy or full SQLite
  converted a fact the process already held into more wire traffic at exactly the
  moment the budget was lowest. This was the plan's own "latch on the parsed
  header" requirement, unimplemented while the plan asserted the floor was
  authoritative. Now: latch first, before persistence, per identity, until the
  provider's own reset, and `Observer.Do` refuses a latched identity before the
  wire. No retry or settlement machinery — losing availability for one reset
  period is the safe side; spending a prepaid balance is not. Regressions:
  `TestFloorLatchesOnTheParsedHeaderNotOnAFailedWrite`,
  `TestFailedFloorWriteLatchesEgressClosed`, `TestLatchIsPerIdentity`,
  `TestLatchExpiresAtReset`, `TestLatchIgnoresUnnameableCredential`.
- **The provider floor was not atomic with the debit, so waiting workers escaped
  it.** `Acquire`'s quota pre-check (added earlier this session) is not the
  authority it was described as: after it passes, `Acquire` can sleep up to
  `MaxInlineWait` in the gate loop or on the token bucket, and another goroutine's
  headers can commit the floor during that sleep. `reserve` re-read
  `next_allowed_at` for the *ordinary* row only, so such a worker committed and
  sent. It had not reached the transport at all, so the old comment's defence —
  "at most one already-in-flight request" — described a different situation than
  the one that occurs. `reserve` now re-reads the quota row for the same identity
  inside the committing transaction, and an unparseable floor fails closed.
  Regression: `TestReserveRefusesAFloorThatLandedDuringTheWait`.
- **Making the floor authoritative silently destroyed the anonymous fallback.**
  With the above, `Acquire` can refuse for provider-quota reasons — and
  `AcquireAny` could not tell that apart from an ordinary gate, so it returned the
  deferral and parked the job with the keyless tier sitting there unspent. That is
  the fallback's entire purpose, defeated by the fix meant to protect it. Now
  `ErrDeferred.Quota` types the refusal and `AcquireAny` advances on it exactly as
  its own pre-check would, while an ordinary gate is still returned unchanged —
  a rate/retry gate is not a credential-switch licence. `budget.QuotaSourceName`
  replaces the `source+"_quota"` string literal that three readers and one writer
  in two packages were agreeing on by hand. Regressions:
  `TestAcquireAnyFallsBackOnAFloorCommittedDuringAnInlineWait` (drives the real
  race: an ordinary gate inside `MaxInlineWait` holds the worker while the floor
  commits; both fixes are required, verified by mutation),
  `TestAcquireAnyDoesNotFallBackOnAnOrdinaryGate`.
- **The round-eight `fetch` comment asserted a wiring that does not exist.** It
  claimed the injected client "is a `sourcegate.Client`". For `resolverEntries` it
  is a bare `Observer` — admission happens upstream at the `AcquireAny` call site,
  by design — and only the discovery wiring wraps a `Client`. The fix itself
  (wrap, never replace) stands and is more useful than stated, since it now also
  preserves `*sourcegate.ErrQuotaLatched`; the rationale was corrected in place.
  Recorded because it is the same failure this file keeps finding: a comment
  describing intended wiring, read later as a fact about the tree.

## Rejected designs (do not re-derive)

- **Feeding provider-reported `cost-usd` into `spent_usd`.** Written into an
  earlier draft of this file as "the dormant axis can finally be fed truthfully".
  `spent_usd` is not a telemetry column: `reserve` reads it before the wire,
  refuses on `spent + cost > limit`, and increments it as monthly admission. A
  second writer either double-counts every non-zero estimate or activates an
  admission authority this plan defers. Observed USD gets its own diagnostic
  column or none.
- **A fuse that "refuses to draw down prepaid balance implicitly".** Not
  implementable from passive response headers: the local counter knows only what
  *papio* committed, the provider balance is known only after *papio* sends, and an
  OpenAlex account is shared with the web UI and any other client. Another consumer
  can exhaust the free allowance while *papio* sits under its fraction, making
  *papio*'s next admitted call the first charged to prepaid. Replaced by the
  honest bound plus a stop on observed prepaid movement.
- **Retrying or settling a failed floor write to regain availability.** Considered
  when fixing the fail-open. Rejected: a latch held until the provider's own reset
  is the safe failure mode, and retry machinery adds a second state machine whose
  own failure modes need the same analysis. Availability lost for one reset period
  is cheap; the alternative spends real money.
- **Treating the 2.3% search yield as a property of OpenAlex.** It is the yield of
  `?search=<title>&per_page=10` plus a local exact-title filter — a broad
  relevance-ranked query (title, abstract, full text) truncated at ten rows, so an
  exactly-titled record ranked eleventh is a paid miss. Compare query shapes before
  concluding the feature is uneconomic; the per-request price does not change with
  `per_page`.
- **Building item 7 before item 0 reports.** Item 7's ternary basis and re-earn
  protocol exists to make the sibling hop correctly available. If the measurement
  says delete the hop, that protocol is sunk cost. Item 0 gates nothing else, but
  it gates this.
- **Bearer authentication as a substitute for the redirect work.** Moving the
  credential to an `Authorization` header is worth doing — it removes the
  query-stripping half of the merge-`301` identity downgrade and keeps keys out of
  logged URLs — but it does not remove per-hop re-admission or re-debit, so it is
  an addition to item 1+2, not a replacement for any part of it.
- **An absolute credit ceiling as the shipped default (`daily_credit_limit =
  4000`).** Derived from the author's own keyed 10,000-credit day. Measured live,
  the keyless pool reports a limit of 1,000, so the default would have been
  **permanently inert** on the low tier — on which one 10-credit search is 1% of the
  day. A fraction of the provider-reported limit is one policy for every tier. The
  absolute form survives as an operator override and as the hard maximum.
- **"API keys are granted, not self-serve", and the keyless-predominance argument
  built on it.** Factually wrong: OpenAlex documents a free, self-serve key that
  takes about thirty seconds and raises the budget 10×. Relative policy survives on
  tier heterogeneity; the population claim does not. Recorded rather than deleted
  because the conclusion outlived its premise.
- **`DisableKeepAlives` alone as the one-physical-request boundary.** Closes HTTP/1
  connection-reuse retry only. `http.DefaultTransport` sets
  `ForceAttemptHTTP2: true`, and Go's HTTP/2 transport has its own retry loop that
  re-sends a bodyless GET; papio's `fetch` also runs its own redirect loop, so one
  admission already covers N hops today. HTTP/1-only plus no automatic redirects
  plus per-hop re-admission is the actual requirement.
- **A fraction of "the provider-reported limit" without a defined denominator.**
  The counter is source-wide but the reported limit is identity-specific, so a keyed
  10,000 establishing 5,000 followed by anonymous fallback reporting 1,000 either
  shrinks the allowance below what is already committed or stops meaning what it
  says — and a malformed `X-RateLimit-Limit: 1000000000` would *enlarge* the fuse.
  Freeze the primary identity's limit for the UTC day, monotone downward, under a
  local hard maximum.
- **A two-valued `SiblingSearchBasis(work.Work, bool)`.** `recordFor` returns
  `false` for both "never asked" and "fresh negative memo", so a two-valued result
  re-buys the singleton *papio* was just told does not exist. The result is ternary.
- **Treating the provider's published price list as authoritative.** Its pricing
  page says single-entity retrieval is *free*; the live header charges 1 credit
  ($0.0001) for exactly that request. Documented prices are telemetry; the header is
  the fact, and the drift closure stays.
- **Offering the replay-safe transport as a configuration choice.** Three options
  were drafted (disable keep-alives / wrap the dialer / accept slight
  undercounting). Handing a stranger a knob whose wrong setting produces quietly
  wrong money accounting under packet loss is not a choice worth exposing — and the
  failure is unreproducible remotely. Decided in-tree instead.
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
