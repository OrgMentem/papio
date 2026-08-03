# ADR-0014: Consumer attribution, durable validation evidence, and the ADR-0007 clause this reverses

Status: Accepted (2026-08-03). Extends ADR-0009 and ADR-0010; governed by
ADR-0001. **Reverses one clause of ADR-0007** (the withdrawal of structured
`pdf.ValidationReport` evidence) with the reasoning that withdrawal was based on,
and records why that reasoning no longer applies to the shape built here.

Numbered after ADR-0013 but independent of it: the two were written the same day
on separate branches, and this one neither depends on nor modifies the operator
experience surfaces recorded there. The one place they meet is Decision 6.

## Context

The external consumer's dogfood run against a real 246-script cohort (309 cited
works, 43 acquisitions, 164 queued institutional routes) froze six *papio*
limitations into client-side workarounds. Two of the six touch decisions this
repository has already recorded, so they cannot be implemented by reading the
code alone.

Both were found by auditing the implementation against `dev/adr/` **after** it
was written and committed, not before. That is the wrong order and it cost a
rework; the finding is recorded here rather than quietly fixed because the
mechanism that was supposed to catch one of them did not.

## Decision 1: Consumer attribution is a caller's label, never an identity

`work_requests.requester` records the transport principal (`cli`, `mcp`,
`unknown`). It answers "how did this arrive", never "who asked for it", so a
daemon shared between people produces one undifferentiated total and no
multi-instructor accounting is possible on it.

A job may now carry an optional `consumer` string, supplied at submit time,
returned by the new readers below, and filterable.

**It is not an identity, and ADR-0009 Decision 3 continues to hold unchanged.**
*papio* authenticates nobody, holds no institutional credential, and can report
an entitlement *route* but never an entitlement *subject*. `consumer` is a label
a caller attaches to its own submissions for its own accounting; *papio* neither
verifies it nor can. It therefore carries the identical refusal ADR-0007
Correction D and ADR-0009 Decision 3 place on `Receipt.principal`: **it must not
be read as a rights input, an entitlement holder, or an acquiring principal.**
The consumer's own `acquiring_principal_id` remains theirs by right, and a
*papio*-supplied value there would be a fabricated rights record — the failure
mode ADR-0009 calls "worse than an absent one".

Three properties follow, each pinned by a test:

- **Absent stays absent.** The column is nullable with no backfill and no
  default; the JSON key is omitted rather than emitted as `""`, `null`, or
  `"unknown"`. A submission that named no consumer has none, and inventing one
  would be a fabricated fact.
- **Attribution binds to the acquisition, not the request.** It lives on `jobs`,
  not on `work_requests`. One `work_request` row is reused by `INSERT OR IGNORE`
  for every resubmission of its request id, so a column there made a fresh job
  silently inherit the previous submitter's name once the earlier jobs went
  terminal — B's acquisition reported and counted as A's. This was found in
  review, not in design.
- **Convergence never reassigns.** A submission matching a live job returns that
  job with its original attribution intact. The second caller did not create that
  acquisition.

The value is bounded (`[A-Za-z0-9._:@/+-]{1,128}`) and rejected rather than
sanitised when it does not match. It is rendered into the tab-separated text
listings, where a tab or newline would forge a field or record boundary, and it
partitions accounting totals, where a silently rewritten key attributes work to a
name nobody asked for.

## Decision 2: Structured validation evidence becomes durable, and ADR-0007's withdrawal of it is reversed

ADR-0007 states, in the bullet "Identity findings cannot be projected from the
artifact": *"Structured `pdf.ValidationReport` `Evidence` stays **withdrawn**…
Consumers may describe papio's validation findings; they may not claim to relay a
full report."* (ADR-0007:145-153).

That clause is reversed. The reasoning behind it is worth restating, because it
is correct and this decision is scoped to stay inside it:

> Identity is computed against a per-job target, but `UpsertArtifact`'s
> `ON CONFLICT … DO UPDATE SET identity_result` overwrites it for every job
> sharing the digest, so a later acquisition retroactively rewrites an earlier
> receipt. Identity belongs on the acquisition edge, not the blob.

Every word of that is about **projecting a per-job finding through a
content-addressed artifact**. It is an argument against one storage location, and
it was decided in a design whose only vehicle was the artifact row and the
receipt. It is not an argument that the evidence itself must not exist.

What was actually shipped is worse than "unexposed": the report was computed,
branched on, and **discarded**. Only the projections that fit the `artifacts` row
survived — page count, text characters, OCR use, encryption, active content,
identity result. The payload gate's reason, the structural rejection reason, and
the identity and capability evidence were unrecoverable after the fact. A
consumer making a rights or quality decision was re-deriving them from fragments
because the data was gone, not because it was withheld.

The evidence is now persisted per `(job_id, candidate_id)` as a versioned
`validation-report/1` document, and returned by `artifacts.validation`. This
satisfies ADR-0007's constraint rather than evading it:

- Keyed by job **and candidate**, never by content hash. Two jobs obtaining
  identical bytes each keep their own decision against their own requested work.
  No reader recovers provenance by "picking some job holding this digest"
  (ADR-0007 rule (c)).
- `artifacts.get` is **unchanged**. It still returns the shared artifact row and
  nothing else, and ADR-0007's prohibition on projecting identity through it
  stands.
- Rejected candidates are recorded too, which is the set "why not this one?" is
  asked about — and the set no artifact row can ever describe, because a rejected
  candidate leaves no artifact.
- **This is not a second success record.** ADR-0009 rejected a candidate-bound
  IPC method because "success provenance belongs only to the accepted candidate"
  and a second such method would create a competing success record. The
  accepted candidate's bundle remains the sole success provenance document: it
  carries version, licence, access basis, and the validation *verdict*. This
  method carries the *evidence behind the verdict*, for every candidate, and
  asserts nothing about entitlement, licence, or version. A consumer must not
  treat a `pass` report as a substitute for the bundle.
- The consumer-side claim ADR-0007 restricts is unchanged for the states where it
  was written: a **failed** acquisition still has no artifact row and no bundle,
  and its receipt still carries a coarse terminal reason rather than findings.
- **No backfill.** Jobs validated before this release list no reports. That is an
  absence, not an empty verdict, and no report is reconstructed by inference.

Two constraints on the document itself:

- The extracted text excerpt is **not** persisted. It is an input to the identity
  decision; the decision and its evidence are what a consumer needs.
- Every reason and evidence line is bounded to 500 bytes with control characters
  stripped. These strings are not *papio*'s prose — several are a third-party
  parser's stderr produced while reading a publisher-supplied file, and one
  carried the absolute quarantine path until this ADR's review. `app.safeType`
  refuses to persist upstream error text at all and `api.safeMessage` bounds
  every error crossing IPC; a durable, MCP-reachable document inherits that
  discipline rather than exempting itself from it.

## Decision 3: Additive evolution of a ratified method's PARAMS also takes a new method name

Consumer attribution was first built as a `consumer` param on
`acquire.submit_v2`. That breached ADR-0010 Decision 1, which freezes that
method's params at exactly `request` / `auto_import` / `force` and reasons at
length that param widening is the *worse* direction of skew: "a newer consumer
sending a field an older daemon lacks has its entire call rejected, not just the
field."

ADR-0009 Decision 1's rule — "additive evolution gets a **new method name**,
never a widened result" — therefore applies to a ratified params object exactly
as it applies to a ratified result. `acquire.submit_v3` carries the attribution;
`acquire.submit_v2` is byte-identical to what it was ratified as, and an
unattributed submission still uses it, so the ordinary path gains no new failure
mode.

**The pin test did not catch this, and that is the more important finding.** It
asserted that *one arbitrary unknown key* (`idempotency`) is refused, which stays
green while a fourth known key is added — and did. It now pins the frozen set by
name, refusing `consumer` among others, so the same mistake fails locally instead
of reaching a consumer. ADR-0009 promised "enforcement is a test rather than a
promise"; a test that only samples the negative space is a promise wearing a
test's clothes.

## Decision 4: The four new readers are ratified; `acquire.submit_v3` is not

Added to `ratifiedConsumerMethods`, at the consumer's request and pinned to the
live router:

| Method | Params | Result shape |
| --- | --- | --- |
| `jobs.list_v3` | `state?`, `limit?`, `consumer?` | `{"jobs": [...], "truncated": bool}` |
| `actions.list_v3` | `open_only?`, `limit?`, `consumer?` | `{"actions": [...], "truncated": bool}` |
| `jobs.get_v2` | `job_id` | `{"job": {...}, "events": [...], "actions": [...]}` |
| `artifacts.validation` | `job_id` | `{"job_id": str, "reports": [...]}` |

The two paged methods keep ADR-0009's proven-`truncated` guarantee: the store
reaches one row past the limit, so `false` means the list is complete. Their rows
are the ratified `job.Row` / `job.HumanAction` key sets **plus** `consumer` and,
for actions, `age_seconds` and `stale` — carried on wrapper types so the ratified
rows themselves stay untouched for every already-installed reader.

Each `artifacts.validation` report carries its own `schema_version` and its
`document` as JSON **text**, for the reason `bundle.document` does: results
decode with `DisallowUnknownFields` recursively, so an inline object would freeze
the evidence shape into this method and force a new method name for every field
the pipeline learns to report.

`acquire.submit_v3` is deliberately **not** ratified. Ratifying params is the
heavier promise ADR-0010 describes, nobody has asked for this one to be frozen,
and freezing it a day after writing it would repeat the mistake Decision 3
records.

## Decision 5: Staleness is a label, and nothing acts on it

An open human action older than `actions.stale_after_seconds` (default 7 days) is
reported `stale`, with `age_seconds` beside it, in the new action readers.

Nothing expires, cancels, closes, or sweeps as a consequence. ADR-0007's "handoff
offers do not hard-expire" and ADR-0009's "Nothing expires" both stand: giving up
on an acquisition is a person's decision, and a timer that made it would discard
an institutional route the operator was still working. The threshold is
deliberately separate from `browser.action_expiry_seconds`, which is a 30-minute
*reminder* cadence and would have called a handoff queued over lunch abandoned.

The daemon evaluates the verdict so every consumer of one daemon agrees on which
rows are abandoned instead of each inventing a threshold. `created_at` was
already on every action row, so the age was always derivable; the shared policy
is the new part.

## Decision 6: The row selector narrows an operator's choice; it does not license a drain loop

ADR-0009 lists **autonomous drain** as not ratified — "a background consumer must
not resolve, open, or retry human work on its own: its view can be stale and the
action is intentionally operator-mediated" — and ADR-0013 preserves that clause
verbatim for the extension's Retry control. `actions.open` itself *is* ratified,
so the boundary is not about who may call it; it is about a caller driving the
queue unattended.

`--job` / `--action` sit on the safe side of that line and in fact move toward it
from the wrong side: before them, the only way to reach a chosen handoff was
`papio actions open` with no selector, which opens every openable row up to
`--limit`. The selector opens **one**, named deliberately, and adds no verb: it
cannot resolve, dismiss, or retry anything, and a selector matching several open
actions is refused rather than resolved by picking one.

What the selector must not become is the inner statement of a loop. A consumer
that ranks the queue and then calls `actions open --job` for each row in turn has
built autonomous drain out of narrow parts, and the clause above still forbids it
— not because the call is dangerous but because the operator's browser is a
single serial surface and a machine filling it with tabs nobody asked for is the
"uncontrolled producer" ADR-0009 refuses in every other form. The selector exists
so a consumer can say *"this is the one worth a human's attention now"*, which is
the opposite of draining.

This is a boundary on consumer behaviour that *papio* deliberately does not
enforce in code. A gate would be theatre: a script passes any flag a human
passes, and an `--operator-intent` flag would break the principle that an agent
driving the CLI gets exactly what a human gets. A rate limit is worse — the
daemon cannot distinguish an operator clicking through their own ranked queue
from a script doing it, so a limit low enough to stop the script obstructs the
person it protects.

**So the prohibition is made auditable instead.** `actions.open` records a
`handoff.opened` event per job carrying the owning consumer, the transport
principal, and the batch size, so "consumer X opened N human actions in M
minutes" is answerable from the event stream and a drain shows up as an anomaly
in `doctor` or the activity feed. Batch size is what makes it legible: the drain
pattern is many single-job opens in quick succession, which looks nothing like
one operator opening a queue. Until this event existed the boundary was neither
enforced nor observable, which is the one combination *papio* should never ship —
truthful evidence over a fake gate.

The event names the handoff's **owner**, recorded at submit, not a self-declared
opener: an opener label would be an unverifiable string supplied by the caller
under audit, and carrying it would mean a new param on a ratified method. A
consumer looping its own ranked queue is opening its own jobs, so the burst is
attributable either way.

The physical blast radius is separately bounded, and not by this decision: the
browser bridge caps unsettled institutional handoffs per session and the
extension drives a small fixed number of concurrent tabs, so a drain loop cannot
flood a browser even unaudited. That is a happy accident of another design, not a
licence — it bounds the damage, not the behaviour.

**Tripwire.** If audit shows a background consumer looping the selector, the
remedy is to ratify a proper consumer verb for what it is actually trying to do,
or to revoke its access — **never** a rate limit. A limit would degrade the
operator's own use of a ratified verb to punish a caller who should either be
served properly or refused outright.

## Consequences

- Four more names *papio* can no longer clean up, and the first ratified reader
  that returns a document *papio* did not previously keep. The pin test fails on
  a rename, a removal, or a row-key change.
- `consumer` is an accounting label, not an authorization boundary. No new method
  scopes reads by consumer: any caller on the socket reads any consumer's rows,
  because the 0600 owner-only socket is the actual boundary and every "consumer"
  on a shared daemon is the same OS user. A consumer that read `--consumer` as
  tenancy would be wrong.
- ADR-0007's withdrawal clause is now historical. Anyone reading it must read
  Decision 2 here: the constraint it protects (no per-job identity through a
  content-addressed artifact) is still enforced, in `internal/job`'s keying and in
  `artifacts.get`'s unchanged shape.
- An ADR audit belongs **before** the implementation. Two of the decisions here
  exist because it happened after: one produced a reworked method surface, the
  other a migration moved from `work_requests` to `jobs` after review found the
  misattribution it caused.
