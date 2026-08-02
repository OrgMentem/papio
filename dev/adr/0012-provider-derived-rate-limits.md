# ADR-0012: Operator configuration is not evidence of provider headroom

Status: Accepted (2026-08-02), shipped in v0.16.0. Governs what `internal/config`
may express about a provider and how `internal/budget` treats a server
`Retry-After`.

**Scope.** This records a rule about configuration, which is decided and in
force. It does *not* describe a system that learns provider capacity at
runtime; papio has no such system, and the constraints on building one are
recorded at the end rather than assumed away.

## Context

A 309-work cohort froze at exactly zero throughput. OpenAlex answered an
exhausted daily allowance with `Retry-After` pointing at the next UTC midnight,
`budget.Acquire` slept that out inside an acquisition worker, and the worker's
lease heartbeat kept the job claimed for the whole window. That bug is fixed
elsewhere. What it exposed is the subject here: **papio did not know what its
own limits were, and had no way to find out except by hitting them.**

Three things were measured during the incident. All are reproducible.

**The limit was not what anyone believed.** The first external consumer measured
1000 requests/day and recorded multi-day cohort acquisition as a property of the
design. 1000 is OpenAlex's *anonymous* allowance; an account is roughly ten
times that, and papio's `api_key` field was empty. A real limit was measured
against an unconfigured client and nearly written down as a fact about the
world.

**The quota is shared with consumers papio cannot enumerate.** Two probes an
hour apart read `x-ratelimit-remaining` of 3 and then 0 while papio was provably
idle — zero attempts, zero runnable jobs. The credits went to the consumer's own
test suite, calling OpenAlex on every unit run since it was written. The
operator's browser hit the same wall the same hour.

**One source exposes several independent quota identities.** Measured at the
same instant on the same machine: the anonymous pool read `remaining: 0` while
an account key read `remaining: 9998`. An account is a *separate* budget, not a
larger one.

## Decision

**Operator configuration must never be treated as evidence of current provider
headroom.**

This is narrower than "operators may not configure limits". Configured values
may constrain admission; they may never be represented, surfaced, or relied on
as provider capacity. Where both exist, the effective gate is the intersection —
configuration can only make papio *more* conservative than the provider
requires, never less.

Four concepts, not two:

| Concept | Examples | Authority |
| --- | --- | --- |
| Quota identity | anonymous IP pool, API-key account | selected by credentials |
| Provider observation | `Retry-After`, `remaining`, `reset`, request cost | the provider's response |
| Operator policy | `enabled`, `rate_per_sec`, `burst`, `max_cost_usd` | configuration |
| Local accounting | requests papio made, cost papio estimated | papio's own records |

A configured 2 rps means "papio will not exceed 2 rps", never "the provider
permits 2 rps". `max_cost_usd` is operator *authorisation* — neither a
credential nor provider capacity — and ADR-0010 already classifies it that way.
A prepaid balance existing is not permission to spend it.

## What is already true

Every claim here is checkable in the tree at v0.16.0.

`config.Source` carries `Enabled`, `APIKey`, `RatePerSec`, `Burst`,
`MaxCostUSD` and a loopback dev override. **No field expresses a provider
limit**, and none is to be added; the decision costs nothing to hold because
there is nothing to remove.

`budget.Defer` persists a server `Retry-After` and never shortens an existing
gate, but never honours one beyond `budget.MaxDeferHorizon`. That constant is a
**local safety policy**, not a claim about providers: papio will not let one
unverified server value make a source unusable without a further observation.
HTTP places no maximum on `Retry-After`, so this bound is papio's choice and is
named as such.

`papio doctor` warns rather than passing when `sources.openalex.api_key` is
unset, naming the roughly tenfold difference. It detects an empty string; it
does **not** verify the credential or query capacity.

Negative `rate_per_sec`, `burst` and `max_cost_usd` are rejected at load,
because the budget manager reads a rate at or below zero as unlimited and a cost
ceiling at or below zero as unmetered — so a negative silently removed the
protection it appeared to configure, which is configuration weakening a gate
rather than narrowing it.

## Why not a configured limit

`max_requests_per_day` was considered and rejected on four grounds, ascending.

**It couples deployment.** Config decoding is strict, so a new `[sources.*]`
field makes an older binary reject the whole file. Config and binary must then
ship together, for a value the provider already sends.

**It cannot express the shape.** OpenAlex meters credits with per-request costs,
a USD ceiling and a prepaid balance. One integer models none of that, and the
model changed under us mid-incident.

**It cannot see the other consumers.** The number an operator would write is
papio's share of a machine-wide budget, and that share depends on a test suite,
a browser, and whatever else runs on the host.

**And decisively: it records a belief, not a fact.** A configured limit encodes
what someone once understood about a provider. That understanding is exactly
what proved wrong here, and it changed twice within one day in both directions.
A configured number institutionalises the error and makes it durable,
authoritative-looking, and wrong.

## Consequences

papio has no advance knowledge of a source's headroom and discovers a limit by
being told about it. That is accepted: an unknown that resolves on first contact
is honest, where a configured number is confidently wrong at exactly the moments
that matter.

A credential change can invalidate a persisted `next_allowed_at` set under the
old identity, which happened here when an account key arrived mid-incident.
`MaxDeferHorizon` bounds the damage to a day. There is deliberately no automatic
re-probe before the gate expires: papio does not second-guess a `Retry-After` it
was given.

## Not decided here

**Learning provider capacity at runtime.** Nothing in papio observes a response:
`budget.Snapshot` exposes only local accounting and the `Retry-After` gate, and
`sourcegate` reserves before forwarding without inspecting what comes back. Two
constraints are recorded so whoever builds it does not rederive them:

- *Quota state is keyed by source name alone.* The evidence above shows one
  source with several quota identities, so every learned field would be
  mis-scoped. Observations need something like `(source, quota identity,
  provider bucket)`, where the stored identity is a non-secret fingerprint,
  never the credential. Adding learned state before this only makes the wrong
  answer durable.
- *Accounting is not structurally complete.* `sourcegate` has one call site —
  discovery. Resolvers and the Crossref enricher reserve at the logical call
  level instead, so a lookup issuing two requests is counted once.

**Surfacing headroom.** Widening a ratified result is forbidden by ADR-0009, so
any external contract needs a new method. Asked directly, the first external
consumer said it would not poll a credits figure and would not schedule against
another system's accounting, but would use a **submit-time preflight**: one
answer before a batch is committed — "N works, M credits, expect to park at
roughly X%". Expectation-setting at the moment of decision, not a gauge.

**Proactive deferral on `remaining: 0`.** Not safe as a generic rule: OpenAlex
exposes daily and prepaid pools separately and the prepaid pool can fund calls
after the daily allowance is spent. The decision belongs to each source adapter,
which alone knows its provider's zero and reset semantics.
