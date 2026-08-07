# ADR-0010: Ratified submission, and the corrections ADR-0009 needed

Status: Accepted (2026-07-31). Amends ADR-0009 Decisions 2, 4, and 5. Governed
by ADR-0001; extends ADR-0007.

## Context

ADR-0009 ratified six IPC methods and released the first external consumer's
transport. It also left the consumer unable to *start* an acquisition: all six
ratified methods read or act on a job that already exists. The consumer
therefore had no ratified way to ask *papio* for anything, recorded every
uncovered work as `not_attempted(no_papio_job)` rather than inventing an
attempt, and *papio*'s acquisition success rate became unmeasurable from the
only side that was counting.

The question arrived framed as a choice: ratify a submission verb, or declare
the CLI the submission surface and state its stability. Both halves of that
framing are wrong, and saying why is most of this decision.

**The CLI is not a second surface.** `papio acquire` parses argv and calls
`acquire.submit_v2` (`internal/cli/acquire.go:166-167`, `:485-486`). Submitting
through it costs a subprocess to reach the same handler and adds flag-name risk
on top of method-name risk. Nor could it be *the* surface: ADR-0001 makes the
CLI the single source of truth for capabilities, and ADR-0007 Correction A
hardened that into `TestEveryDomainRPCIsReachableFromCLI`
(`internal/cli/rpc_reachability_test.go:22-63`), which walks the live router and
fails on any served method with no Cobra reachability. *papio* structurally
cannot have an IPC-only verb, and equally cannot have a CLI-only one. There is
also no CLI stability policy anywhere in the repository to point at:
`docs/reference/commands.md` is generated, and ADR-0009's six names were the
only stability statement in the tree.

**The verb already existed.** `acquire.submit_v2` takes one work, accepts a
caller-supplied `request_id`, and returns `{job_id, existing}`
(`internal/api/handler.go:159-161`, `:391-395`, `:42-45`).

## Decision 1: `acquire.submit_v2` is ratified

Added to `ratifiedConsumerMethods`. Frozen: the method name, the params keys
`request` / `auto_import` / `force`, the result keys `job_id` / `existing`, and
within the `work-request/1` document the identity subset `schema_version`,
`request_id`, `identifiers.{doi,pmid,arxiv,isbn,openalex}`, `title`, `authors`,
`year`, plus `access_mode_override` and its values
`conservative | assisted | delegated`.

Deliberately **not** frozen, though still served: `resolver`, `sources_allow`,
`sources_deny`, `max_cost_usd`, `collection`, `zotio_item_key`,
`desired_version`. These are operator policy, and a consumer must not pin
*papio*'s policy vocabulary.

Ratifying **params** is a heavier promise than ratifying a result, and this is
the only ratified method where it matters. The six prior methods take small
parameter objects — `job_id`, `limit`, `state`, `job_ids`, `path`/`role`; this
one embeds a whole `work-request/1` document, and `internal/ipc` decodes params
with `DisallowUnknownFields` too
(`internal/ipc/protocol.go:134-135,144-161`). For results the dangerous skew is
an old consumer reading a new daemon; for params it is the reverse — a newer
consumer sending a field an older daemon lacks has its entire call rejected,
not just the field. Hence the narrow subset.

**The override may narrow, never widen.** `Config.NarrowAccessMode`
(`internal/config/config.go:707-736`) clamps the request against the configured
mode on the `conservative < assisted < delegated` ladder, and the clamped value
is what the job snapshots, so `diagnose` never reports an override the daemon
declined to honour. Ratifying `access_mode_override` invites non-interactive
consumers to set it, and the daemon-wide `access_mode` is the operator's
standing decision — first-run setup refuses to choose one silently. It is also
the only brake that exists (see Decision 1's consequences), so a submitter able
to raise it could mint unbounded never-expiring handoff tabs on a daemon whose
operator opted out of opening any. Narrowing is what the override is actually
for, and it stays fully supported.

`acquire.submit` (v1) is **not** ratified. It remains served, because removing
it would break an older CLI against a newer daemon and one binary is CLI,
daemon, and native host. It is now only the CLI's `unknown_method` fallback:
`internal/batch` moved to v2.

### Why this is not a reversal of ADR-0009 Decision 2

Decision 2 refused **bulk** `acquire.submit` because it "would turn a
consumer's reconciliation into an uncontrolled acquisition producer". That
reason is about volume and survives intact at one work per call. Bulk, a generic
reopen verb, method aliases, and autonomous drain all remain unratified.

The refusal also did not buy what it appeared to. The exposure it names is real
and is **already reachable**: the IPC socket is owner-only `0600`
(`internal/ipc/transport_unix.go:19-41`) with no per-caller authorisation, so
anything that can call `jobs.repair_awaiting_human` can already submit.
Ratification adds a promise, not an attack surface. What genuinely bounds a
cohort-scale submitter is unchanged and worth stating plainly:

- **No admission control.** `createRequest` inserts with no queue-depth
  predicate and no cap (`internal/job/job.go:367-443`).
- **Almost no operator brake.** `jobs.cancel` takes one `job_id`; there is no
  pause, drain, or cancel-all, and `daemon stop` is cancellation rather than
  drain. The one brake that does exist is the configured `access_mode`, which
  the clamp above turns into a real ceiling.
- **Three workers** claiming one row at a time
  (`internal/bootstrap/bootstrap.go:296-303`, `internal/job/job.go:1019-1045`).
- **Per-source, not per-consumer, rate limits**, and exhaustion does not queue —
  the attempt is marked `budget_blocked` and the job walks on
  (`internal/budget/budget.go:64-94`, `internal/app/app.go:441-453`).

Under `assisted`/`delegated` an exhausted work parks on a human action that
never expires — though not universally: a work with no fetchable identifier, no
configured OpenURL base, or an already-exhausted institutional route goes
`unavailable` instead (`internal/app/app.go:918-935`). Where it does park, a
cohort run can mint hundreds of operator tabs. That is the failure mode worth
designing against, and it is why conservative-first cohort submission is the
recommended shape: conservative records an `openurl_available` advisory and
opens nothing (`internal/app/app.go:937-947`).

## Decision 2: `access_mode_override` now governs the decision path

A prerequisite defect, found while validating the above and fixed here.
`access_mode_override` was validated
(`internal/protocol/protocol.go:228-230`), snapshotted into
`job.Policy.AccessMode` (`internal/app/app.go:197-205`), and printed by
`papio diagnose` (`internal/cli/diagnose.go:154`) — while the only production
code that decides whether to open an institutional handoff read the daemon-wide
`s.Config.AccessMode` instead. The override looked like it worked and reported
that it worked. No test covered it: every mode test set the config.

`Config.EffectiveAccessMode(policyMode)`
(`internal/config/config.go:686-710`) now resolves the job's snapshot **and
re-clamps it against the current configuration**, so the ceiling is continuous
rather than applied only at submit. Honouring the snapshot alone was itself a
regression, found in review: an operator tightening `delegated` to
`conservative` would not have restrained jobs already recorded, which are
exactly the jobs they were trying to stop. Re-clamping is monotone — it can only
lower the mode. Both decision sites use it: the exhaustion gate
(`internal/app/app.go:906`) and the `job_offer` frame
(`internal/browser/bridge.go`).

That frame is **not** an enforcement point today, and the ADR should not imply
otherwise: the extension parses and range-checks `access_mode` and then never
reads it (`extension/src/protocol.ts:766-767` is its only appearance), so
unattended-download capability is in practice gated by granted host permissions.
Sending the job's own mode is still correct — the daemon must not assert
something false — but making the field load-bearing needs an extension change,
failing closed to `assisted`, and is not in this change set.

The browser half carried a second hazard. `papio-browser/1` permits only
`assisted` and `delegated` in a `job_offer`, and a non-nil error out of `Sync`
is treated by the native host as a dead connection. A parked job resolving to
`conservative` would therefore have torn down the whole native-messaging session
rather than dropping one offer — the `reviewPreview` failure class again. Such a
job is now skipped.

Making the snapshot authoritative broke the recovery path the conservative
advisory itself prescribes. The advisory says "a route exists; this mode will
not take it", and `Store.Retry`'s own comment records the remedy as *switch
access mode and retry* — but a retry preserves `policy_json`, so the job would
re-exhaust under its original conservative snapshot and reopen the same advisory
forever. `Retry` therefore releases the pinned mode in the same statement that
cancels the advisory (`internal/job/job.go:1144-1160`), letting the job follow
the operator's current configuration from there. Scoped to jobs that actually
carry the advisory: an ordinary failed or retry-wait retry keeps the mode it was
submitted with.

## Decision 3: ADR-0009's sanitised-reference rule named the wrong sanitiser

ADR-0009 Decision 4 cited `redact.URL` as the emission-time enforcement.
`redact.URL` clears userinfo, query, and fragment and then, when the raw string
contained a `?`, returns `u.String() + "?<redacted>"`
(`internal/redact/redact.go:26-29`) — a deliberate "evidence is partial" marker
for operator logs, and query data to a consumer. This is not hypothetical:
`validateOpenURLBase` requires only https and a host and permits a query
(`internal/config/config.go:662-668`), and the documented OpenURL example
carries one.

The correct emitter is **`redact.Host`** (`internal/redact/redact.go:35-41`),
which yields `scheme://host` with no path, query, or fragment. It returns a
placeholder for unparseable input, which is treated as "no route" so the
entitlement object is omitted rather than shipping a value the consumer must
reject. `protocol.validateBareRoute`
(`internal/protocol/protocol.go:497-521`) re-checks for a literal `?` or `#`,
for userinfo, and for any path at all, so reaching for the wrong helper is a
test failure here rather than a rejection downstream. That validator is held to
the published schema's own `^https://[^/?#@]+$` pattern deliberately: a
validator laxer than its schema is a decoder disagreement waiting to be
exported, and a path is exactly where signed tokens live in several CDN schemes.

## Decision 4: ADR-0009's proposed v2 entitlement shape was invalid

ADR-0009 Decision 5 proposed `"route": "resolver-profile:institutional-openurl"`
and `"entitlement_ref": "entitlement-profile:library-e-resource"`. The consumer
ran both against its actual gate and **both are rejected**: a route must be an
http/https URL with a host and no query or credentials, or a closed opaque
`route:sha256:<64>`, or one of three manual-import literals. A bare identifier
has no scheme and fails. This is precisely the outcome the agreed migration
ordering exists to produce — prove the gate before cutting the schema.

`acquisition-bundle/2` is cut with this shape. The path is shown in full
deliberately: `entitlement` hangs off the **accepted candidate**, never off the
document root. A reader that looks top-level finds nothing and concludes *papio*
emitted a v2 bundle carrying no rights information — wrong, and silent. That
misreading has already been made once from an abbreviated example, and a
consumer that refuses a bundle without an entitlement would have rejected every
acquisition in a 43-work set on the strength of it.

```json
{
  "schema_version": "acquisition-bundle/2",
  "candidate": {
    "source": "crossref_tdm",
    "access_basis": "licensed_api",
    "entitlement": {
      "route": "https://api.crossref.org",
      "entitlement_ref": "entitlement:source:crossref_tdm",
      "acquisition_mode": "daemon_held_credential"
    }
  }
}
```

`acquisition_mode` is **derived** from the accepted candidate's existing,
already-validated `access_basis` — no new state and no inference:

| `access_basis` | `acquisition_mode` | Producers |
| --- | --- | --- |
| `open_access` | `open_access` | arxiv, europepmc, openalex, unpaywall |
| `licensed_api` | `daemon_held_credential` | core, crossref_tdm |
| `institutional` | *(entitlement omitted)* | see below |
| `manual` | *(entitlement omitted)* | none |

**`daemon_held_credential` is current, not future.** ADR-0009 described it as
distinguishing "a future daemon-held-credential acquisition". It already has two
producers: CORE sends `Authorization: Bearer <configured APIKey>`
(`internal/resolvers/core/core.go:130,451-457`) and Crossref TDM sends
`Crossref-Plus-API-Token` (`internal/resolvers/crossreftdm/crossreftdm.go:122-124`),
whose own comment records that `licensed_api` is emitted only for a configured
token.

`entitlement_ref` uses the **cleartext** `entitlement:source:<name>` form.
Hashing a public constant such as `crossref_tdm` buys no secrecy and destroys
legibility in an audit trail whose entire value is knowing which entitlement
obtained the work. The digest form stays accepted so an opaque reference remains
expressible. The prefix is `entitlement:`, never `credential:`: an entitlement is
a right held, a credential is a secret used, and in the TDM case the licence
regime is the entitlement — held *via* the key rather than being the key. The
reference names a **source, not a credential instance**: rotating a key does not
change it, and no rotation semantics may be built on it. A hash of the API key
itself was refused outright — a digest of a secret is secret-derived.

`route` is `redact.Host` of the accepted candidate's URL. The whole object is
omitted when no route resolves.

**`route` names where the bytes came from, not where a credential went.** For a
licensed API those can differ: Crossref's Plus token is metadata-only and its
own code says publisher download URLs never receive it, so a `crossref_tdm`
bundle carries the publisher's origin as `route` alongside
`acquisition_mode: daemon_held_credential`. Both facts are true — the
acquisition was authorised by a credential *papio* holds, and the file came from
that origin — but read as "this host received our credential" the pair would be
false, so the published schema states the narrower meaning explicitly.

**`operator_browser_session` therefore has no producer yet, and that is
deliberate.** The only writer of `institutional` is browser adoption
(`internal/app/browser_adopt.go:103-105`), which records that basis
unconditionally — including for an open-access PDF handed to the browser because
a provider's anti-bot wall refused *papio*'s own fetch
(`internal/app/app.go:909-915`). That acquisition never touched the institution.
The adopted candidate URL is the synthetic `browser://adopted-download`, so
there is no observed route to name either, and reconstructing one from the
current OpenURL configuration would be worse still: configuration is mutable, so
re-exporting after an operator edits it would silently rewrite an
already-published provenance record and its digest. ADR-0007's asymmetry decides
it — a false positive invents rights evidence, a false negative costs a field.
The enum value stays reserved; recording the true basis and route at adoption
time is the fix that gives it a producer.

> **Superseded on this point by ADR-0018 (2026-08-07).** The fix named in the
> previous sentence landed: 0.17.0's `delivery_context_v1` records the route and
> session evidence on the candidate row, so `operator_browser_session` now has a
> producer, gated on `session_evidence = fresh_auth`. The paragraph above stands
> as the reasoning that was correct while no such record existed — including the
> mutable-config hazard, which ADR-0018 honours by quoting the row and never
> reconstructing an origin. One claim in it did **not** survive: browser adoption
> is not the only writer of `institutional` (a resolver-produced candidate
> reaches it from its own paywall metadata), which is why ADR-0018 gates on the
> recorded evidence rather than on the basis.

A v1 document carrying an `entitlement` is rejected, including an explicit
`null`: v1 froze its shape with `additionalProperties: false`, and declaring the
field in Go made the key known to `DisallowUnknownFields`, so presence is
checked in the raw document rather than the decoded value
(`internal/protocol/protocol.go:344-380`).

### Migration status

ADR-0009 fixed the order: *papio* ratifies (1), drafts v2 (2), the consumer
lands dual v1/v2 decoding (3), and only then does *papio* switch emission (4),
retaining v1 decoding indefinitely. Step 3 landed and was verified against the
consumer's parser, so this ADR ships step 4. `Validate` accepts both versions;
v1 decoding is retained indefinitely.

## Consequences

- One more name *papio* can no longer clean up, and the first ratified verb that
  creates durable state. Its params are pinned as tightly as its result.
- `request_id` remains a **live-job convergence key, not an idempotency key**,
  and it converges along two independent paths that a consumer must not
  conflate. `liveJobForRequest` (`internal/job/job.go:445-448`) matches on the
  request id alone and runs *first*, so reusing a live job's `request_id` while
  submitting a different work returns that prior job — `existing: true` does not
  assert that the live job owns the work just submitted. `liveJobForCanonicalWork`
  (`internal/job/job.go:450-465`) then matches on normalised DOI/PMID/arXiv/
  ISBN/OpenAlex regardless of `request_id`, so two request ids for one DOI
  collapse to one job. Only when neither matches is a job created, and both
  predicates consider live jobs only (`internal/job/job.go:467-486`), so a
  terminal job plus the same `request_id` usually mints a new job — unless some
  other request currently holds a live job for the same canonical work.
  Consumers must persist the returned `job_id` and must use a fresh
  `request_id` per work. This is documented rather than fixed: an idempotency
  key was offered and declined, because persisting the handle is the consumer's
  job and the honest contract is the one that says so.
- **Deduplication happens at submit and nowhere else, so two live jobs can name
  one work.** A title-only request correctly matches nothing, because
  `liveJobForCanonicalWork` keys on strong identifiers. Enrichment
  (`internal/app/app.go:714`) can supply a DOI much later, and at that instant
  the duplication becomes knowable. Observed at 2 in 309 on the first real
  cohort, where one citation of each pair carried a DOI and the other's
  extraction had been defeated.

  papio **records this and does not converge**. `existing` answers a question
  asked at submit, about a handle issued at submit; consumers persist that
  `job_id` on this ADR's instruction and poll it. Merging afterwards would
  redefine a handle already in use — the consumer here was polling both, so a
  silent merge costs it a work it believes it is tracking, against a duplicate
  fetch that content addressing already collapses to one stored file. Same
  asymmetry ADR-0007 and ADR-0008 turn on: the false merge is the expensive
  error. `Store.RecordDuplicateWork` writes a `job.duplicate_work_detected`
  event naming the other job, and changes nothing else.

  That event is not yet on the ratified surface. `jobs.receipt` cannot gain a
  field, so exposing it to consumers is a separate decision — and the pending
  `jobs.status_by_ids` design is the natural place, now that there is evidence
  a consumer wants it rather than a guess that one might.
- Bundles now emit `acquisition-bundle/2`. Because `acquisition_mode` and
  `entitlement_ref` are both derivable from v1's required `access_basis` and
  `source` fields, an existing v1 corpus can be backfilled without re-export;
  only `route` is genuinely new information.
- **`bundle.export` had to be split.** Its result carries the whole bundle, and
  `internal/ipc` applies `DisallowUnknownFields` recursively, so shipping a v2
  body would have made an older CLI reject every `papio bundle export` response
  against a newer daemon — the precise hazard this ADR reasons about for
  `acquire.submit_v2`'s params, missed in the nested case. `bundle.export` now
  returns the path alone and `bundle.export_v2` carries the body, with the CLI
  preferring v2 and falling back on `unknown_method`. Removing a field is safe
  for an old decoder; adding one is not.
- `jobs.retry` releasing a job's pinned mode also discards a *submitter's*
  deliberate narrowing, not just an operator's. It cannot exceed the ceiling —
  the released job follows the current configuration, which is itself clamped —
  but a consumer that narrows to `conservative` and later retries gets the
  daemon default. `jobs.retry` is unratified and explicit; consumers relying on
  a narrowing should resubmit rather than retry.

Scope tripwire: if `entitlement` ever grows a field expressing a *judgement* —
whether the terms permit an action, rather than which terms applied — stop. That
belongs to the consumer, and ADR-0007's tripwire covers it.
