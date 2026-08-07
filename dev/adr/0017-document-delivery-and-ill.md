# ADR-0017: Document delivery and ILL become a durable, configured route

Status: Proposed (2026-08-07). Drafted from the integration consult
(dev/scratch/oracle/papio-integrations-r1.md, -r2.md); not yet reviewed against
code.

Extends ADR-0009 (ratified IPC contract, additive method evolution) and
ADR-0013 (operator experience taxonomy: Activity, Actions, status). Consumes
ADR-0016's `InstitutionResolution.DeliveryRoute`: LibKey's routing layer
preserves a document-delivery route rather than treating it as full text, and
hands it to the service this ADR specifies.

## Context

Papio already names the shape it never finished. `job.TerminalReason` has
carried `TerminalReasonDocumentDeliveryAvailable`
(`internal/job/job.go:70`) since before this consult, and the browser bridge's
outcome switch already recognises the `"document_delivery_available"` provider
observation (`internal/browser/bridge.go:2053`,
`internal/protocol/protocol.go:1878`). What that vocabulary triggers today is a
dead end: `bridge.outcome` folds `document_delivery_available` into the exact
same handling as `no_entitlement` — one institutional-route rediscovery pass,
then `jobs.Cancel`/terminal `StateUnavailable` with that reason
(`internal/browser/bridge.go:2053-2077`,
`internal/browser/bridge_test.go:2870`). Papio observes that a work is only
obtainable through interlibrary loan and then stops. There is no request
record, no idempotency, no polling, no reconciliation — an operator who
manually places an ILL request outside Papio gets nothing back from it, and
one who opens the resulting `manual_download` action gets no delivery-aware
guidance at all.

The consult's prioritized portfolio (r1 §2, rank 3) calls this "First-class
document-delivery/ILL route" and proposes promoting it "from a vague/manual
end state into a configured, durable route." Its deep design is r1 §3.3
("First-class document delivery and ILL"); r2 §2 ("Delivery requests and
integrity notices on existing surfaces") corrects the job-state mapping so a
pending request does not masquerade as a required human action, and narrows
the eventual human action to reconciliation only, never resubmission. This ADR
records both.

## Decision 1: `delivery_requests` is a new durable table, idempotency-keyed, owned by a new `internal/delivery` service

States: `offered | submitted | pending | fulfilled | declined | cancelled |
unknown_outcome`.

Idempotency key: **institution profile + canonical work identity + provider +
request type**. One work produces at most one live subscription-provider
request; a resubmission attempt for the same key must resolve against the
existing row, never open a second one. This is the same shape ADR-0008
protects for holdings claims and ADR-0014 Decision 1 protects for job
attribution — one durable row per (scope, identity) pair, not one per
attempt.

`internal/delivery` owns this table, the provider-specific status-poll budget
(separate from `internal/budget`'s resolver/HTTP retry accounting — a delivery
poll is not a resolver attempt), and the reconciliation routes in Decision 4.
`jobs` gains a foreign reference to its live delivery request; `internal/job`
keeps owning job state and the human-action table, exactly as it already keeps
owning both while `internal/browser` only observes and reports.

New CLI surface: `papio delivery get <job-id>`, `papio delivery submit
<job-id>`, `papio delivery cancel <job-id>` where the provider supports
cancellation. Per papio's existing pattern, the command-derived MCP facade
exposes the same three operations without separate MCP-side work.

**A store migration is required and bumps `user_version`.** Per AGENTS.md, three
tests hardcode that number and must move together with the new
`internal/store/migrations/NNNN_*.sql`: `internal/cli/clean_install_test.go`
(the "schema version N" string, twice), `internal/doctor/doctor_test.go`, and
`internal/store/migrate_forward_test.go`.

## Decision 2: three route sources feed one delivery layer; every credential stays daemon-side

```text
OpenURL request forms (prefill)
LibKey delivery routes            — ADR-0016's InstitutionResolution.DeliveryRoute
institution-issued API integrations (ILLiad / OCLC Tipasa / Rapido)
                                            ↓
                                   internal/delivery
```

**OpenURL request forms.** The existing per-profile OpenURL fields
(`Institution.OpenURLBase`, `ShibbolethEntityID`, `ProquestAccountID` —
`internal/config/config.go:176-182`) already select an institution's link
resolver. A document-delivery request form reached through that same resolver
is prefilled from the job's bibliographic identity and opened through the
existing browser handoff; it adds no new credential.

**LibKey delivery routes.** ADR-0016's routing layer distinguishes a
document-delivery route from a full-text route in `InstitutionResolution`
precisely so this layer can consume it — the LibKey layer never follows or
downloads it. When a `DeliveryRoute` is present, `internal/delivery` treats it
as one more configured route to the same request lifecycle below, not as a
second delivery product.

**Institution-issued API integrations.** ILLiad, OCLC/Tipasa, and Rapido keys
are application credentials the institution issues to Papio, not to the
operator. Configured under
`browser.resolvers.<profile>.document_delivery` alongside the existing
`Institution` fields, an API key here is read only by `internal/delivery` in
the daemon. It is never sent to, stored in, or observable from the extension
or the browser wire — the identical boundary ADR-0013 already draws for
browser-local vs. daemon-owned state ("the daemon owns jobs, files,
validation, and transitions"; extension owns only browser-local interaction),
and the same boundary this consult states LibKey's API key must respect.

```toml
[browser.resolvers.campus.document_delivery]
kind = "openurl"        # openurl | libkey | illiad | oclc | rapido | custom
base_url = "https://ill.example.edu/request"
allowed_hosts = ["ill.example.edu"]
submit_policy = "auto_if_unconditional"   # never | prefill_only | auto_if_unconditional
max_fee_usd = 0
monthly_request_cap = 25
status_poll_minutes = 60

# institution-issued API integration only:
api_key = "..."
patron_ref = "configured-non-secret-reference"
```

`kind` is explicit configuration. **Papio never guesses which ILL system an
institution runs** from branding, page text, or a landing page. No such
inference exists anywhere in the resolver profile today, and this decision
establishes the rule for `document_delivery.kind` rather than inheriting an
enforced precedent — the consult's config sketch names sibling fields
(`ezproxy_prefix`, `services_api_kind`) that are likewise proposals, not
shipped code. A broken or
misconfigured document-delivery integration falls back to the profile's
OpenURL route rather than disabling institutional access; it never falls back
to a *different* institution's profile.

## Decision 3: submission is `never | prefill_only | auto_if_unconditional`, gated by a nine-condition checklist; configuration is the consent

Auto-submission may fire only when **all** of the following hold:

1. the job's **effective access mode is `delegated`** — the profile's
   `submit_policy` narrows what the global `access_mode` already permits, it
   never widens it (the same only-narrowing rule `NarrowAccessMode` in
   `internal/config/config.go` enforces for per-request overrides). Under
   `conservative` the route is discovered and recorded, never opened or
   submitted; under `assisted` the prefilled form opens but submission stays
   human (r1 §3.3's access-mode table, restated here so the checklist cannot
   be read as profile-only consent);
2. the institution profile's `submit_policy` enables it;
3. the provider integration has a deterministic, tested contract (fixture- or
   API-conformance-tested, not a scraped form);
4. the bibliographic identity is sufficient for the provider to accept;
5. zero human-only steps remain — no login, MFA, CAPTCHA, terms acceptance,
   copyright declaration, or purpose statement;
6. the fee is known and does not exceed `max_fee_usd`;
7. `monthly_request_cap` is not exhausted;
8. the request has not already been submitted (Decision 1's idempotency key);
9. Papio can reconcile an ambiguous response before attempting another
   submission (Decision 4).

**Any one condition being false *or unknown* routes to `prefill_only`
behaviour instead** — Papio opens the prefilled form and stops. An unknown fee,
an unknown identity match, or an unresolvable declaration is not treated as
permission; it is treated as the missing information it is.

Configuration is the consent, matching every other automation boundary papio
already ships: access mode (`conservative`/`assisted`/`delegated`) and source
enablement are declarations, not per-action prompts, and ADR-0012 draws the
same line for `max_cost_usd` — an operator ceiling authorizes spend, it does
not manufacture certainty. There is no per-work "are you sure?" for a
submission that the profile's `submit_policy`, `max_fee_usd`, and
`monthly_request_cap` already authorized unconditionally at zero fee.

## Decision 4: pending stays job-in-flight state; only exhausted reconciliation opens a human action, and that action never offers resubmission

Today `document_delivery_available` collapses into `no_entitlement` handling
and the job goes terminal `unavailable` (Context, above) — there is no
representation of "a request was lodged and Papio is waiting on it." This
decision replaces that collapse for the case where `internal/delivery` has
actually submitted a request.

**Pending, pollable.** A delivery request in `pending` with a known
`provider_reference` puts the job in the existing `StateRetryWait`
(`internal/job/job.go:36`) with a new reason,
`document_delivery_pending`, and a scheduler-visible `next_check_at`. Delivery
polling draws on its own budget (Decision 1), never on ordinary resolver or
HTTP retry counts — a slow ILL turnaround must not exhaust the acquisition
waterfall's retry budget. This shows under `papio status`'s **WORKING**
section (provider, reference, last-checked, next-check) and in the Activity
feed's bounded read of the events table, per ADR-0013's activity model. It is
**not** an open action: `actions.list` is defined as the open-human-action
surface, and a self-driving poll there would make that taxonomy lie, exactly
as ADR-0013's Option A rejected inventing a second read model for what a poll
already covers.

**`unknown_outcome`, only after exhausting deterministic reconciliation.**
Before Papio ever asks a human, it must have tried, in order: (1) lookup by
provider reference; (2) lookup by Papio's own idempotency key, where the
provider supports it; (3) search the patron's request list by exact
work identity; (4) one delayed status re-check. Only once all four are
exhausted does the delivery request move to `unknown_outcome` and the job to
the existing `StateAwaitingHuman`
(`internal/job/job.go:35`), opening a new human-action kind,
`document_delivery`, alongside the existing `openurl_handoff`,
`manual_download`, `openurl_available`, and `verify_identity` kinds
(`internal/job/job.go:2681`). Its action offers exactly three operations —
`open_request_history`, `confirm_request_exists`, `confirm_request_absent` —
and **never** `retry_submission`. Papio must not submit a second request while
an earlier one's outcome is unknown; that is what the idempotency key in
Decision 1 exists to prevent, and offering resubmission from the action UI
would be the same autonomous-drain shortcut ADR-0009 and ADR-0014 Decision 6
already refuse: a background or impatient caller retrying human-mediated work
it cannot verify.

**A lodged request never makes a job ready.** Only a validated PDF does.
`fulfilled` enters the exact same quarantine, structural, and identity
validation pipeline as every other candidate — the boundary ADR-0013 already
states for browser-adopted files ("copied into quarantine and passes the
ordinary payload, structure, and identity validation pipeline") applies
identically to a file a delivery provider hands back.

## Decision 5: new surfaces extend ADR-0013's taxonomy; nothing widens a ratified result

`papio status` gains the pending-delivery line under **WORKING** described
above; it is prose output, not a ratified JSON result, so it needs no new
method.

`papio jobs get` gains a delivery-requests section (provider, reference,
state, submitted/checked/next-check timestamps, fee). `jobs.get_v2` is
ratified and closed (`internal/api/ratified_contract_test.go:38`,
`internal/ipc` decodes with `DisallowUnknownFields`); ADR-0009's rule — additive
evolution takes a new method name, never a widened result — applies exactly as
it did when attribution needed `jobs.get_v2` over the original `jobs.get`
(ADR-0014 Decision 3). This section therefore ships as `jobs.get_v3`, decoded
by wrapper types beside the untouched ratified rows, following ADR-0014's
precedent exactly.

`papio actions list` shows only the reconciliation case (`unknown_outcome`);
pending delivery never appears there. The currently-ratified `actions.list_v3`
already carries a generic `kind`/detail shape sufficient for text output; a
closed, machine-readable operations enum for `document_delivery` (the three
operations in Decision 4) is new structured surface. **Whether that ships as
`actions.list_v3`-compatible detail or needs its own `actions.list_v4` is not
decided here** — it is additive-evolution work under ADR-0009's rule either
way, and is deferred to the CLI/API implementation.

**Extension rendering is a declared dependency, not decided here — and the
consult's proposed vehicle needs correcting against what already shipped.**
r2 §2 proposes carrying the new item kind through a "proposed
`triage-snapshot/2`" (r2 lines 499-501), written as if that schema did not yet
exist. It does: `triage_snapshot_schema_v2` is already negotiated and shipped
(`internal/browser/bridge.go:52`,
`internal/protocol/protocol.go:2321-2346`), and its `human_action.blocked_by`
is a closed enum of exactly `anti_bot`, `paywall`, `landing_page`
(`internal/protocol/protocol.go:2123`) — it has no `document_delivery` item
kind and no room for one without becoming a different, unratified schema.
Rendering this action in the extension therefore needs a **new** schema
version reached the same way schema 2 was — an additional entry in
`schema_versions` negotiation, immutable once published, old schemas served
unchanged — not an amendment to the schema already shipped. This ADR does not
ratify that schema's shape; it only corrects the record so a future ADR does
not start from the consult's stale assumption.

## Rejected alternatives

**Auto-opening every `manual_download` action to drive delivery.** This is
autonomous drain built from the wrong primitive: a background process ranking
and opening a human-mediated action queue on its own is exactly what ADR-0009
lists as not ratified and ADR-0014 Decision 6 formalizes as auditable-not-gated
policy. `manual_download` is a generic "a human needs to place a file" action;
it carries no request state, no idempotency, and no provider semantics. Giving
it delivery behavior would mean guessing, from an undifferentiated action,
whether an ILL request already exists — the precise ambiguity Decision 1's
idempotency key exists to make unnecessary.

**RapidILL as an independent end-user API.** Reach RapidILL, where relevant,
through the institution's own patron-facing ILLiad/Tipasa/Rapido path
(Decision 2), not as a fourth standalone provider integration. Papio does not
hold a RapidILL credential of its own to target it directly, and doing so
would duplicate exactly the institution-issued-credential model Decision 2
already establishes for the other three.

**Marking the job `ready` on submission.** A submitted or even `pending`
request is a promise of future bytes, not bytes. Decision 4 records this
explicitly: only a validated PDF advances a job past `awaiting_human`/
`retry_wait`; a successfully lodged ILL request is progress recorded in
`delivery_requests`, never acquisition success.

## Consequences

- A new table, a new service package, and a new human-action kind the store,
  doctor, and CLI conformance tests must all learn about — the same shape of
  cost ADR-0014 accepted for attribution and validation evidence.
- Every institution that wants auto-submission must supply a provider-tested
  integration and explicit `submit_policy`/`max_fee_usd`/`monthly_request_cap`
  configuration; without it, papio still improves on today by prefilling a
  form instead of terminating the job `unavailable`.
- `document_delivery_available` (terminal reason) and the collapsed
  `no_entitlement`/`document_delivery_available` bridge handling
  (`internal/browser/bridge.go:2053`) are superseded for the case where a
  delivery route was actually configured and pursued; the terminal reason
  remains correct for the case where no route exists at all and Papio truly
  has nothing further to try.
- `internal/delivery`'s provider budgets are keyed by institution profile and
  credential identity, the same non-secret-fingerprint discipline ADR-0012
  requires of source quota state — a shared ILLiad key across profiles must
  not be double-counted or under-counted against one budget.
- Extension-side rendering of the reconciliation action is explicitly blocked
  on a schema decision this ADR does not make; until it lands, the CLI
  (`papio actions list`/`papio jobs get`) is the only faithful surface for
  `unknown_outcome`, consistent with ADR-0001's CLI-first rule.
