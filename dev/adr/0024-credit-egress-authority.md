# ADR-0024: Credit egress authority for metered metadata providers

Status: Accepted (2026-08-17)

## Context

*papio* exhausted its entire 10,000-credit OpenAlex daily allowance in 25 minutes
(1,605 HTTP calls between 00:00 and 00:25 UTC), then sat gated until the next UTC
midnight. Nothing was wrong with OpenAlex. A single job whose candidates were all
permanently dead re-ran the full resolver chain roughly every 60 seconds forever,
because a pass in which any *other* source happened to be gated was classified as
`source_gate` and never charged against the 8-attempt retry budget — so the budget
never bound, and each cycle spent real credits. Metadata enrichment had a second,
independently exploitable version of the same defect: it made real budgeted
requests while being entirely disconnected from the retry plan.

Two facts turn this from hygiene into safety. OpenAlex reports
`x-ratelimit-prepaid-remaining-usd`, so on any paying installation the failure mode
is not "gated until midnight" but *papio* **charging the user**. And an OpenAlex
account is shared — the web UI and any other client draw on the same balance — so
another consumer can exhaust the free allowance while *papio*'s own accounting sits
comfortably under its ceiling.

ADR-0012 already covers provider-*derived pacing*: reading rate-limit headers and
slowing down. This ADR is a different mechanism at a different layer — a **debit,
committed durably before the request leaves**, which refuses the request when the
debit cannot be written. Pacing answers "how fast may I go?"; this answers "may I
send this at all?".

## Decisions

### 1. One egress authority: the credit is committed at the wire, or there is no wire

A single transaction commits the credit **and** revalidates every durable
no-egress signal it depends on. Both are revalidated *there*, never inherited from
an earlier decision. That transaction *is* the egress authority:

- No blocking wait may occur after it.
- A failed commit means no request.
- A request reaching the transport without one must fail loudly.

A decision taken before a blocking step cannot be authoritative for egress, so this
is deliberately **not** fixed with a manager-wide mutex. `Manager.CommitEgress`
(`internal/budget/credit_egress.go`) is that transaction.

### 2. The boundary must have provably no automatic replay

Three mechanisms can turn one admission into several physical requests, and
`DisableKeepAlives` closes only the third:

1. *papio*'s **own** redirect loop — `internal/fetch` replaced `net/http`'s
   transparent following to get an SSRF guard per hop, so one `Do` already issues
   N physical requests under one admission.
2. **HTTP/2's internal retry** — Go's HTTP/2 transport re-sends a bodyless GET
   after a retryable GOAWAY. `DisableKeepAlives` bounds connection *reuse* and makes
   no promise about attempts per `RoundTrip`.
3. **HTTP/1 retry on a reused connection.**

Metered metadata therefore travels over a transport that is HTTP/1-only
(`Protocols.SetHTTP1(true)`/`SetHTTP2(false)` **with** the ALPN list pinned to
`http/1.1` — restricting `Protocols` alone leaves `h2` advertised in the handshake
and an h2-capable host fails with "malformed HTTP response"), with keep-alives
disabled and no automatic redirect following. Every redirect hop re-enters the
egress authority as a fresh guarded request under a re-derived identity, bounded by
`maxOpenAlexEntityRedirects`.

This is not a config knob. Correctness must not depend on a stranger's network
quality, and "*papio*'s credit accounting is slightly wrong under packet loss" is
undebuggable remotely. Rejected: wrapping the dialer so replays re-enter the
authority — correct and faster, but it puts the money invariant in the
least-inspectable layer of the stack.

### 3. Identity is a property of the outgoing request, and the credential is a header

The authority derives identity from the **outgoing** request
(`sourcegate.servedIdentity`), never from a client's construction-time policy. A
fixed-policy wrapper at the wire, on a client whose context can carry
`resolver.WithAnonymousCredentials`, otherwise produces one physical request with
two different identities in two authorities.

The credential is sent as an `Authorization` bearer token
(`sourcegate.SetOpenAlexAuthorization`), not as an `api_key=` query parameter. This
removes the query-stripping half of the merge-redirect defect outright — a
`Location` without the parameter no longer downgrades a hop to anonymous — and keeps
keys out of logged URLs. It does **not** remove the need for per-hop re-admission
and re-debit.

### 4. An entity-merge redirect carries authoritative alias evidence

OpenAlex answers a merged entity with `301`. Re-admitting the hop is only half the
fix: the redirect must also be carried forward as alias evidence, so `Wold → Wnew`
passes exact identity verification instead of being rejected as a mismatched
record.

### 5. The fuse is a stop, not a ledger

One durable counter, `credits_committed`, per source per UTC day
(`source_credit_fuse`), committed atomically at the boundary by the request shape's
**conservative** cost, refused when the debit would exceed the day's allowance, and
**never refunded within the window**. The goal is that *papio* provably stops
making requests, not that its books balance.

- **Source-wide, not per identity.** A limit enforced per `(source, identity)`
  means a configured 4,000 actually authorizes 4,000 keyed *plus* 4,000 keyless.
  The provider-reported floor already protects each real pool separately; *papio*'s
  own budget answers the different question — how many credits may this instance
  commit today, across all identities?
- **Availability is the safe failure mode.** A crash leaves the debit. A transport
  error leaves the debit. A path that declines after admission may leave the debit.
  Each costs a little availability inside one UTC day and clears at rollover; none
  can overspend. This is what removes reservation handles, `Confirm`/`Release`,
  settlement transactions, and nearly all of the cross-midnight problem.
- **Atomic in SQL across insert AND update.** Hundreds of jobs target the same hot
  row, and the limit test must cover the *first* write of a day: a naive
  `INSERT … ON CONFLICT DO UPDATE` that tests only in the update arm admits a
  10-credit request against an allowance of 5.
- Called `committed`, not `spent`, because that is what it is.

### 6. The ceiling is a fraction of the provider's reported limit, with a frozen denominator

`daily_credit_fraction` (default `0.5`) under a local hard maximum. An absolute
default overfits one machine's key: keyed reports `X-RateLimit-Limit: 10000` and
keyless `1000`, so a fixed `4000` never fires on the keyless tier at all.

- **The denominator is the configured primary identity's reported limit** (keyed
  when configured, else anonymous), captured durably for that UTC day. Later reports
  may **lower** it, never raise it, and anonymous fallback does not rewrite it.
  Without the monotone rule a malformed `X-RateLimit-Limit: 1000000000` would
  *enlarge* the fuse — the price of relative policy.
- **Before the first response of the day there is no reported limit**, so a
  conservative absolute applies (`BootstrapCreditCap`, `ColdStartCreditCap`) until
  one is observed. Untested, the fuse either blocks everything or nothing on a fresh
  daemon.
- `daily_credit_limit` remains an absolute operator override and the hard maximum
  the fraction may never exceed.
- **`0` disables the ceiling, never the commit.** Every request still executes the
  durable commit and increments `credits_committed`; otherwise the configuration
  that turns off enforcement also bypasses the accounting, which is precisely when
  an operator most needs the numbers. The shipped defaults must therefore write a
  non-zero value, with a test, or the fuse ships inert.

### 7. Meter in credits; observed dollars are not an admission authority

The provider reports both (`x-ratelimit-credits-used: 1` alongside
`x-ratelimit-cost-usd: 0.0001`). Credits are integers, so the conditional SQL
cannot drift on float rounding.

Observed `cost-usd` must **not** be added to `spent_usd`. That column is not
telemetry: `reserve` reads it before the wire, refuses when `spent + cost > limit`,
and increments it as part of monthly admission — `ErrExceeded` is that refusal. A
second writer would either double-count every non-zero estimate or silently
activate a monthly admission authority left deliberately unbuilt.

### 8. Positive cost drift closes the source until acknowledged — no timed reopen

If observed `credits-used` exceeds the committed shape cost, that is provider
contract drift. All egress for that source closes durably
(`Manager.DriftClose`) and stays closed until an explicit acknowledgement
establishes a new conservative cost schedule.

Drift is the one refusal in the taxonomy with **no** timed reopen. A UTC expiry is
wrong here specifically: the classifier still says 10 after midnight, so an
unattended daemon repeats the undercharged request every day, once per reset,
forever. A process-only mark is also insufficient — commit 10, learn the shape cost
100, mark it closed, crash, restart, and only the under-sized debit survives.
Gating the *credential* is likewise too narrow: pricing is a property of the
**operation**, so keyed would be gated and the next call would fall through to
keyless and repeat the same undercharged shape.

### 9. The in-process latch is set on the parsed header, before persistence

When a response says the identity's allowance is nearly gone, the fail-closed latch
for that credential is set **immediately**, before the durable write is attempted,
and survives until the provider's own reset. Waiting for the durable write to fail
leaves a window — it can block for seconds — in which the process already knows the
fact while new egress still sees no gate. A transient SQLite busy can also drop the
floor while the credit write succeeds, so "both writes fail together" is not a safe
assumption.

### 10. Pacing and credential are separate authorities

A 429 with `Retry-After` is a property of the **source and this machine's egress
IP**, not of the credential presented. It is recorded as a source-wide pacing row
(`Manager.DeferSourceWide`, `PacingSourceName`) that binds every identity, while a
provider-reported per-credential exhaustion binds only that identity. Conflating
them either blocks the keyless fallback the design depends on, or lets a credential
switch bypass a real rate limit.

`Manager.AcquireAny` admits the first policy whose own quota signal is not gated,
checked **before** the ordinary per-source admission attempt — a keyed identity's
ordinary gate can be wide open while its daily quota is nearly gone, so deferring
the check until after an ordinary failure means it is never consulted on the common
path. Advancing past a quota-gated identity must not step over that identity's own
ordinary gate, and must not step over the shared pacing row at all.

### 11. A pass that made a request is charged, and enrichment participates

The originating defect was a classification one: a pass that called any source
counts against the retry budget, whatever other sources were gated in the same
pass. Metadata enrichment returns a retry plan for the same reason — an enricher
call is a real, budgeted provider request, so a pass whose only outbound call was
an enrichment is chargeable.

### 12. The fuzzy sibling title-search hop is opt-in, default off — measured, not assumed

Measured 2026-08-16 against the operator's own history: 8 accepted artifacts
attributable to the sibling hop, against 43,810 credits spent on `search=` calls
(3,150 sibling searches at 31,500 credits; 1,231 enrichment searches at 12,310).
The numerator is severely lossy — `job.sibling_search` is written post-wire and so
is lost in exactly the crash window this ADR closes — so the point estimate of
0.018% is not decision-grade. The **denominator** and the 317 accepted artifacts in
the window (all sources) are not lossy, and together they bound the answer: even if
every one of those 317 had come from an OpenAlex title search, the hop would still
have cost ≥138 credits per accepted artifact. Against a 10,000-credit keyed day and
a 1,000-credit keyless day, that is not worth its cost in the current query shape.

`[sources.openalex].sibling_title_search` therefore defaults to `false`.

Note what was measured: the **query shape**, not OpenAlex. The code and the plan
both called this a "title search" and it is not one — `lookupURL` sends
`?search=<title>&per_page=10`, and OpenAlex documents `search=` as matching title
plus abstract and full text, ranked by relevance and citation count, with `per_page`
up to 100. *papio* then applies an exact-normalized-title test to whichever ten
records that broad ranking returned, so an exactly-titled record ranked eleventh is
a ten-credit "no result". Three variants (`search=` with `per_page=100`,
scoped `title.search=`, and the current shape) cost the same per call and remain
available for the operator to authorise, since running the comparison spends real
credits. Yield must never be "improved" by loosening the acceptance predicate
(ADR-0025).

### 13. An exhausted local budget parks truthfully, and never reads as "no copies exist"

**A local policy budget being exhausted is not evidence that no legal candidate
exists.** Before this ADR, `resolve` converted `budget.ErrExceeded` into `continue`
without recording any gate fact — so with every source exceeded and no candidates,
resolution reached ordinary `no_legal_candidates` and told the researcher "no copies
exist" when the truth was "out of allowance until the window resets". It was dormant
only because monetary reservations were all `0.0`; the fuse makes it live.

An exhausted budget is a **timed** gate, so the disposition is a durable park
carrying the reset that actually applies — UTC midnight for the credit fuse, the
monthly boundary for the pre-existing USD budget. Two budgets with different windows
must never collapse into one reset.

Representation is **one** typed local-budget refusal carrying unit, window and
reset (`BudgetKind`, `Window`, and an `Until` on `budget.ErrExceeded`), not two
sibling error types: one park path, and a third budget stays cheap to add.

## Consequences

- The production OpenAlex transport cannot negotiate HTTP/2, and a merge-`301`
  costs one debit per physical hop with correct per-hop identity attribution.
- A crash, a transport error, or a post-admission decline costs availability inside
  one UTC day. That is intended.
- The fuse bounds credits *papio* **authorizes**. It cannot bound what the provider
  bills: a request admitted at 23:59:59 may be accounted in either window, and a
  shared account can be drained by another consumer. The honest invariant is that
  *papio* bounds what it authorizes and stops as soon as it observes impending or
  actual prepaid use — so `prepaid-remaining-usd` falling below its baseline closes
  egress **regardless of local ceilings**, including under an unmetered `0`.
- Strict config decoding means a new field and the binary that understands it must
  deploy together.

## Not decided here

- **A per-job spending cap.** The fuse bounds a *day*, not a job, so one
  pathological job can consume the allowance again tomorrow and starve unrelated
  work each time. Deferred: a per-job cap resets on novel candidates and so never
  delivers a hard bound anyway, and lease fencing is unresolved —
  `InsertCandidates` performs no generation check, so a stale pass could rearm the
  current holder's authority.
- **Jitter on the budget-reset wake.** Operational smoothing, not an invariant.
- **Generalising the header-derived floor to other sources.** Four of ten
  configured sources publish nothing usable, and only OpenAlex has two pools; the
  transport hygiene generalises and has been applied, the floor does not.
- **Monthly USD admission.** `spent_usd` is left exactly as it was.
