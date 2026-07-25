# ADR-0005: Change notification uses derived topic versions, not signalled bumps

Status: Rejected (2026-07-25). Resolves the live-push question ADR-0001 deferred
("the current bridge is a pull loop; a subscription mechanism is a separate
decision") by deciding not to build it. ADR-0001's *solicited only* rule and its
deferred-work note stand as originally written; this ADR does not amend either
— see [Relationship to ADR-0001](#relationship-to-adr-0001).

## Context

The inbox, the toolbar badge, and the acquisition-history page all need to
notice daemon-side change. Before this ADR every one of them polled: the badge
on the extension's one-minute keepalive alarm, the inbox on an eight-second
visibility-gated counts request, the history page not at all.

Three facts about the existing transport shaped the decision, all verified
against the code rather than assumed:

- **The daemon has no change signal.** No store write hook, no event bus, no
  version column. Nothing in the daemon can say "the inbox changed".
- **`events` covers only job-scoped change.** Every write in
  `internal/job/job.go` appends an event, but nothing in `internal/watch` or
  `internal/retraction` does. A watermark over `events` would silently miss new
  watch hits and new retraction notices — two of the inbox's three item kinds.
- **Delivery is already a 2s relay poll.** `internal/nativehost` runs an idle
  poll goroutine on a 2s ticker (`pollInterval`) calling `browser.sync`, which
  is how `job_offer` and `cancel` reach the extension today. Latency to the
  extension is therefore already bounded at ~2s without any new timer.

So "push" was never about inventing a transport. It was about finding something
worth pushing.

The naive design — hash the triage counts inside the existing holder-only
`poll()` and emit on change — was rejected. It makes the daemon re-query every
2s per session whether or not any page is open (strictly more database work than
the visible-only page poll it replaces), and because `poll()` is holder-only by
design ("Pending sessions poll but never receive offer/cancel traffic") it would
reach one browser, leaving the page poll in place anyway. Two mechanisms for a
few seconds of latency.

The obvious alternative — a `triageVersion` counter bumped at each write path —
was also rejected, and this is the load-bearing judgement of the ADR. The inbox
read model has roughly four independent writers today (job transitions, human
action create, human action resolve, watch-hit inserts, retraction notices), and
every future feature that touches one inherits the obligation to bump. The
failure mode of forgetting is a **permanently** stale consumer, which is worse
than the poll it replaces, and it is invisible in review.

## The approach considered: derive, don't signal

The question the daemon still had to answer, before the rejection below, was
*how* a change feed should work if built at all — not whether to build one.
This design was carried all the way to implementation, and it is preserved
here because it remains the right answer if a future ADR reopens push: prefer
a derived fingerprint over a signalled counter, for the reasons below.

**A topic's version is derived from the data, never signalled by the code that
mutates it.**

`internal/change` holds a `Registry` mapping a `Topic` to a `Source`, where
`Source.Fingerprint(ctx) (uint64, error)` returns a cheap watermark over the
tables that topic reads. The registry compares fingerprints and exposes a
monotonic `int64` version that starts at 1 and increments only when the
fingerprint actually moves.

Consequences of deriving rather than signalling:

- A new write path anywhere moves the fingerprint with **zero new code**. There
  is nothing to remember and nothing to forget.
- The correctness burden moves from "every writer, forever" to "one fingerprint
  per topic, co-located with the read model it describes".
- The worst case degrades to *late*, not *wrong* — see the backstop below.

### A fingerprint is not the read query

Fingerprints must be materially cheaper than serving the topic. `StatsFingerprint`
must not recompute the 12-week series; a count plus `MAX(updated_at)` over
terminal jobs is sufficient and provably moves. Triage combines `COUNT(*)` and
`MAX(id)` per source table (count alone misses an add-and-remove in one tick,
max alone misses a delete) plus a value that moves when an open action is
updated in place, because the inbox renders `revision`.

Recomputation coalesces to once per second inside the registry, so N connected
browsers cost one query per topic per tick rather than N. This is what makes a
single authoritative change feed cheaper than the per-page pollers it replaces.

### Topics, not messages

The wire carried two message types under `papio-browser/1` — `subscribe`
(`{topics}`) and `data_changed` (`{topic, version}`) — plus feature
`change_push_v1`. Topics registered: `triage`, `stats`.

Adding a future consumer (a side panel, a live jobs view, a watches view) would
have cost one topic string and one `Fingerprint` function. **No new message
type, no schema edit, no parser change.** Structure is validated strictly; the
topic *vocabulary* is deliberately open, so a newer extension may name a topic
an older daemon does not serve and simply receive nothing for it.
`Registry.Version` returns `ok=false` with no error for an unregistered topic:
not serving a topic is a normal answer, not a failure.

### Notifications are content-free

`data_changed` carries a topic name and an integer. No title, URL, host, or
identifier can ride a change notification, so the bridge's existing data
boundary holds structurally rather than by review discipline.

### Emitted per session, not via the holder-only poll

`data_changed` was emitted from `Sync` for **every** known session, holder and
pending alike, with per-session last-sent version state on `browserSession`.
This matches the existing treatment of triage reads, which are deliberately
stateless and allowed from any session, and deliberately differs from
offer/cancel traffic, which must reach exactly one browser.

### A registered topic is not automatically a subscribed one

The daemon registered and served both `triage` and `stats`, but the extension
subscribed only to `triage`. Fingerprinting is driven by what sessions actually
subscribed to, so an unsubscribed topic costs nothing. The history page has no
periodic poll, so nothing kept the worker awake on its behalf, and a `stats`
subscription would have cost a fingerprint every tick to deliver notifications
that essentially never arrive — a direct instance of the lifetime problem in
the rejection below. **The prerequisite for subscribing a topic is a consumer
that keeps the worker awake**, which is precisely the dependency that sank the
feature.

### No field is added to any existing message

The extension's parser fails closed on unknown fields, so adding `version` to
`triage_snapshot_response` would make an older extension reject a newer
daemon's snapshot outright. That is precisely why snapshots carry explicit
`schema_versions` negotiation. Both additions were new message types.

### The daemon layer owns it

`internal/change` had no browser dependency. The bridge was one adapter over
it, so a future `--follow` CLI or TUI could have consumed the same versions
through the daemon layer without touching the browser protocol — preserving
ADR-0001's rule that the CLI is the single source of truth and no surface
grows logic the CLI cannot reach. This property would carry over unchanged if
the design above is ever revived.

## Relationship to ADR-0001

This ADR was originally accepted, and at that point it amended ADR-0001 in two
places: narrowing the *solicited only* rule to admit `data_changed` as an
unsolicited exception, and marking the deferred-work note ("a subscription
mechanism is a separate decision") as resolved. Both amendments are reverted
by the rejection below. ADR-0001 stands exactly as written: every new message
type remains solicited, and its deferred question — whether to build a
subscription mechanism — is now answered "no", for the reasons that follow.

## Rejection (2026-07-25)

The design above was implemented in full — the wire types, the
`internal/change` registry, the bridge integration, and page-side consumption
— then measured in use and removed. Four findings, in order of how they were
discovered:

1. **No consumer needs sub-10s freshness.** The badge is ambient chrome, not a
   live readout. The inbox is already covered by a visible poll plus
   refresh-on-return, which is when a person actually looks at it. The history
   page needs neither: it is opened deliberately and refreshes on open. Nothing
   in the product asked for a change feed; the project supplied one anyway.
2. **The MV3 platform caps ambient latency at the extension's own keepalive
   wake, regardless of transport.** With no *papio* tab open, the service
   worker sleeps and only wakes on its ~1-minute alarm. Push cannot deliver its
   headline promise — sub-second freshness — in the state where a headline
   promise would matter; it can only improve on the case where a page is
   already open and already polling.
3. **Push does not replace the poll it was meant to obsolete — it depends on
   it.** `internal/nativehost`'s outbound writer only touches the port when the
   daemon has frames to send, so an idle daemon produces zero port traffic and
   never resets the worker's ~30s idle timer. The only thing that keeps the
   worker awake long enough to *receive* a push is a consumer page's own
   periodic message — the inbox's poll, running underneath the "push" feature,
   the whole time. This was measured directly: setting the inbox's backstop
   poll to 60s (intending it as a pure backstop, since push was supposedly
   covering freshness) silently starved the worker between polls and dropped
   effective freshness from a guaranteed 8s to a best-effort 60s. Push was
   never a second, independent mechanism; it was a second mechanism riding on
   the first one's exhaust, for the price of a genuinely new failure mode.
4. **The fingerprint obligation is a silent-failure tax.** "A fingerprint must
   cover every field the read model renders" is a correctness invariant with no
   compiler or test to enforce it — a fingerprint that is cheap but blind
   under-notifies invisibly, exactly the review-invisible failure mode this ADR
   set out to avoid in the *signalled-counter* alternative. Deriving the
   version from data rather than a write-path bump removed the *forgetting to
   bump* failure, but not the *forgetting to fingerprint a field* failure — it
   only moved the same class of mistake to a different obligation.

Together: for a feature whose entire justification was shaving seconds off an
already-bounded, already-polled surface, the permanent carrying cost — two
message types, a registry, a per-session subscription model, and an
open-ended fingerprint-completeness obligation — was not worth it. The pull
loop ADR-0001 deferred a decision about is kept; the decision is "stay a pull
loop."

**What would reopen this.** A genuinely latency-sensitive surface — for
example live per-job acquisition progress, where seconds visibly matter while
someone is watching a job run — or a multi-device / shared-inbox scenario,
where one browser's local poll cannot observe another session's writes at all.
Neither exists in *papio* today.

**What to keep if it reopens.** The derived-fingerprint approach in
[The approach considered](#the-approach-considered-derive-dont-signal) above
is still the right design for *how* to version a topic — nothing about that
reasoning was invalidated. The failure here was building push at all before a
consumer needed it, not how push was designed once the decision was made.
