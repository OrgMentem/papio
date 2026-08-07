# ADR-0017: Document delivery and ILL become a durable, configured route

Status: Proposed (2026-08-07). Drafted from the integration consult
(dev/scratch/oracle/papio-integrations-r1.md, -r2.md); amended the same day
from consult rounds r3/r4 and independent deployment research (ILLiad/OCLC/
Rapido vendor documentation, AU s49 practice) before acceptance. Further
amended the same day (Decision 6, "Fulfillment retrieval") to specify how a
`fulfilled` request's document actually reaches quarantine — Decision 4
below stopped at "fulfilled enters the ordinary validation pipeline"
without saying how a papio-held file gets there in the first place. Not yet
implemented; Decision 6's live acceptance is explicitly out of scope (it
needs a real ILLiad site) — everything else there ships with this ADR.

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
kind = "openurl"        # openurl | libkey | illiad | custom
base_url = "https://ill.example.edu/request"
allowed_hosts = ["ill.example.edu"]
submit_policy = "auto_if_unconditional"   # never | prefill_only | auto_if_unconditional

# gate-profile declarations (Decision 3A) — facts papio cannot discover:
request_classes = ["digital_journal_article"]
legal_basis = "institution_policy"   # institution_policy | copyright_act_s49 | unknown
patron_attestation = "not_required"  # not_required | standing_completed | per_request | unknown
patron_fee_policy = "zero_standard"  # zero_standard | per_request | unknown
monthly_request_cap = 25
status_poll_minutes = 60

# institution-issued API integration only:
api_key = "..."
patron_ref = "configured-non-secret-reference"
```

Strict config accepts a `kind` only when its adapter has shipped: v1 accepts
`openurl`, `libkey`, `illiad`, and `custom`. `oclc` and `rapido` are intended
providers this ADR names, but a value whose implementation does not exist must
not parse — the same fail-closed rule `validSourceNames` applies to sources.
`patron_ref` is not a secret but it is personal identity data: 0600 config
only, redacted from events, diagnostics, and delivery provenance.
`standing_completed` counts only when the institution has confirmed that a
registration-time agreement covers API-created requests of the configured
class; papio never infers it from the existence of an account, a missing
checkbox in one render, an API accepting a request, or the institution's
country or hostname.

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

## Decision 3: submission policy compiles to a static gate profile, evaluated by a seven-point per-request gate; configuration is the consent

The consult's original nine-condition checklist read as nine dice rolls per
paper. The deployment evidence (r3/r4 Topic 2, plus vendor documentation)
shows the conditions are three different kinds of fact — static deployment
facts, per-request facts, and bookkeeping — and that one "condition" was a
state-machine bug. Decision 3 therefore restructures into a compiled profile
(3A), a runtime gate (3B), and the surfaces that report them (3C).

### Decision 3A: `internal/delivery` compiles an institution gate profile at configuration time

The stable classification unit is **institution profile × provider ×
patron class × request class × legal basis** — not the institution alone. Two
Alma-family universities can legitimately land on opposite sides, and one
institution can be auto-capable for staff articles while prefill-only for
student chapters. The compiled profile is:

`auto_capable | prefill_only | invalid`, with a closed blocker vocabulary
(`provider_not_implemented`, `provider_not_auto_capable`,
`api_credential_missing`, `patron_mapping_unverified`,
`request_class_unsupported`, `per_request_login`, `per_request_terms`,
`per_request_copyright_declaration`, `per_request_purpose_statement`,
`patron_fee_not_zero`, `patron_fee_unknown`, `reconciliation_unavailable`,
`institution_policy_unknown`) and recorded evidence per blocker.

Static inputs: the provider adapter's declared capabilities (supported
request classes, required bibliographic fields, create/status/patron-list
capability, idempotency and reconciliation strategy) and the operator's
Decision 2 declarations (`legal_basis`, `patron_attestation`,
`patron_fee_policy`). Three hard rules:

- **V1 auto-submission covers digital journal articles only**, at
  **zero configured patron fee only** (`patron_fee_policy =
  "zero_standard"`). Books, chapters, theses, physical loans, rush service,
  and any nonzero or provider-quoted fee are prefill-only until separately
  modelled — `max_fee_usd` is no longer an auto-authority field, because it
  conflates a patron charge with the borrowing library's lender-cost
  commitment (OCLC's `Maximum Cost` is the library's money, never papio's to
  spend).
- **Only source-controlled API integrations can compile `auto_capable`** —
  v1: `illiad`, whose Web Platform API documents create, transaction lookup,
  patron mapping, and patron-request listing. `openurl`, `libkey`, and
  `custom` routes are permanently prefill-only: they route to a form and
  supply no deterministic submission-and-reconciliation contract. Rapido
  starts prefill-only; a specific profile may compile auto-capable only after
  live verification that no declaration is configured and the API accepts
  the standard digital-article request without papio asserting agreement —
  papio never sets a copyright-agreement field merely to satisfy a mandatory
  parameter, and never sends provider compliance flags (e.g. ILLiad's
  `CopyrightAlreadyPaid`) outside the institution-approved mapping.
- **An `auto_capable` compile additionally requires recorded live
  acceptance**: one supervised submit-and-reconcile against the real
  deployment under the institution's authority. A compiled adapter plus
  matching config is necessary but not sufficient.

Under `legal_basis = "copyright_act_s49"` (Australian document supply), the
patron's declaration is an affirmative, request-scoped statutory act — it
includes "not previously supplied", which no standing declaration can
truthfully cover. Such profiles compile `prefill_only` by law, not by
caution: the product there is **automatic prefill followed by one human
declaration**, and papio must never tick, script, or represent the
declaration itself. An AU-jurisdiction profile defaults
`patron_attestation = "unknown"` → prefill-only until the institution
confirms otherwise; the legal basis is configured, never inferred from a
hostname.

### Decision 3B: only an `auto_capable` profile reaches the per-request gate

All seven must hold:

1. the job's **effective access mode is `delegated`** — `submit_policy`
   narrows what the global `access_mode` permits, never widens it (the
   `NarrowAccessMode` only-narrowing rule in `internal/config/config.go`).
   Under `conservative` the route is discovered and recorded, never opened
   or submitted; under `assisted` the prefilled form opens but submission
   stays human;
2. the profile's `submit_policy` is `auto_if_unconditional`;
3. the request class is supported and configured;
4. **every provider-required field is present and consistent after papio's
   normal metadata enrichment** — a DOI or PMID makes enrichment likely but
   is not universally sufficient (providers require mapped users, process
   types, citation fields); no conflicting identity remains;
5. **no step is required from the papio operator before creating the
   request** — no login, MFA, terms, declaration, purpose statement, or
   payment. Library-side mediation *after* submission (staff or rule-engine
   copyright processing of a lodged request) does not fail this condition;
6. the zero-patron-fee policy applies to this request;
7. the monthly auto-submit cap has headroom.

Then: all true → submit; a human step or fee issue → prefill; metadata
incomplete → enrich, then prefill if still incomplete.

**The former condition 8 ("not already submitted") was a state-machine bug,
not a gate.** Falling back to prefill on an existing request is precisely how
a duplicate gets created. It is an idempotency **branch evaluated before the
gate**: no existing row → evaluate the gate; `submitted`/`pending` → join and
poll; `fulfilled` → fetch, adopt, validate; `unknown_outcome` → reconcile
(Decision 4); `declined`/`cancelled` → apply the explicit resubmission
policy. The former condition 9 (reconciliation capability) is a Decision 3A
compile input — a provider without it can never compile `auto_capable` — plus
runtime API health, not a per-paper re-evaluation.

**Any gate condition false *or unknown* routes to `prefill_only` behaviour** —
papio opens the prefilled form and stops. Unknown is missing information,
never permission. Every gate decision persists a redaction-safe event
(`delivery.gate_evaluated`: profile class, profile digest, decision,
blockers) so `papio delivery get` and `jobs get_v3` can explain why a
nominally auto-capable profile did or did not submit.

Configuration is the consent, matching every other automation boundary papio
ships: access mode and source enablement are declarations, not per-action
prompts, and ADR-0012 draws the same line for `max_cost_usd`. There is no
per-work "are you sure?" for a submission the compiled profile and gate
already authorized unconditionally at zero fee.

### Decision 3C: init and doctor state the compiled answer plainly

`papio init` prints the compiled gate class before saving — `AUTO-CAPABLE`
with its evidence lines, or `PREFILL ONLY` with the specific blocker ("your
institution requires a copyright declaration on every digital-copy request").
An operator must never enable `auto_if_unconditional` in good faith and wait
for a path that cannot fire. `papio doctor` verifies what is verifiable
(API authentication, patron mapping, transaction and patron-request lookup,
adapter conformance version) and **distinguishes `PASS`/`OBSERVED` facts from
`DECLARED` configuration** — it never prints `PASS` for a policy it merely
read from config, never records `live-accepted` without the Decision 3A
acceptance event, and never creates a probe request. User documentation must
not claim automatic document delivery until one real institution profile has
completed one live request with zero operator steps.

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

## Decision 6: fulfillment retrieval — patron-web form-75, through the ordinary browser handoff (amended 2026-08-07)

Decision 4 says a `fulfilled` request "enters the exact same quarantine,
structural, and identity validation pipeline as every other candidate" but
never says how the file gets there. This amendment closes that gap for v1's
only source-controlled provider, ILLiad, and restates the boundary Decision
1 already implies: **`fulfilled` means the provider supplied the document,
never that papio holds trusted bytes.** A lodged request that ILLiad marks
fulfilled is a promise papio has not yet redeemed; only a file that clears
quarantine, structural validation, and identity confirmation lets the job go
ready, exactly as Decision 4's "Marking the job `ready` on submission" was
already rejected for the pending case.

**`document_delivery.patron_web_base_url` is new, distinct configuration —
never derived from `base_url`.** ILLiad institutions commonly run the Web
Platform API (`base_url`, used for `CreateTransaction`/status lookups) and
the patron-facing ILLiadWeb portal on different hosts or paths; guessing one
from the other would be exactly the branding/hostname inference Decision 2
already forbids for `kind` itself. Absent means absent: with no configured
`patron_web_base_url`, papio cannot construct a retrieval route and falls
back to Decision 4's reconciliation action (an operator-visible human
action — `open_request_history` — rather than silently dropping a fulfilled
request). Config is strict-mode (AGENTS.md); this field ships in the same
deploy as the daemon build that understands it, like every other
`document_delivery` field before it.

**`fulfillment_channel = "patron_web"` is a new compiled capability,
independent of Decision 3A's `Class`.** It compiles only when
`kind = "illiad"` and `patron_web_base_url` is configured — never for
`openurl`/`libkey`/`custom`, which have no ILLiad transaction to view in the
first place. Critically, **submission auto-capability and fulfillment
retrieval are orthogonal**: a profile can compile `auto_capable` (creates
requests automatically) with no fulfillment channel, meaning every
fulfilled request still lands on the manual reconciliation action — v1
never claims end-to-end automation it cannot back. The distinction is
surfaced in three places so an operator cannot miss it: the compiled
`GateProfile.FulfillmentChannel` field, one `papio doctor` line per profile
(`fulfillment: patron_web` vs `fulfillment: none`, alongside but never
conflated with the `AUTO-CAPABLE`/`PREFILL ONLY` submission verdict), and
the persisted `delivery.gate_evaluated` event, so `papio delivery get`/
`jobs get_v3` can explain a fulfilled-but-unretrieved request without
recomputing against a since-edited profile. `submit_policy =
auto_if_unconditional` semantics are otherwise unchanged.

**Retrieval is browser-side, through the existing openurl_handoff
machinery — no parallel dispatch.** On a `fulfilled` row, papio builds
`patron_web_base_url + "?Action=10&Form=75&Value=<provider transaction
reference>"`: ILLiad's numeric Action/Form query convention has no
self-describing endpoint, so `Action=10` ("view this transaction") and
`Form=75` ("View PDF") are recorded as named constants
(`internal/delivery.FulfillmentRetrievalURL`) rather than left as magic
numbers. That URL is carried the same way a one-time OA candidate URL
already is — as a durable marker + URL in an `openurl_handoff` human
action's `Detail` — so `internal/browser`'s existing offer/access-mode
dispatch drives it with zero new protocol fields and zero new access-mode
logic: delegated drives the tab immediately, assisted opens it, and
conservative (Decision 3B condition 1, exactly as it already applies to
submission and prefill) never opens anything and only records a
`delivery.retrieval_discovered` event. A downloaded file lands through the
ordinary browser-managed adoption directory and quarantine → structural →
identity pipeline used by every other browser-driven capture; Firefox's
existing no-download-steering limitation is unchanged and remains
human-assisted there by design.

**A custom, non-inline-PDF landing page is not scanned for "PDF-looking"
links.** ILLiad's `View PDF` form can render an inline PDF or a custom HTML
page depending on institutional configuration and document format; papio
does not heuristically hunt that page for a download link — that ends in a
human action. A fixture-backed ILLiad response adapter that actually parses
the page is future work, named here rather than built speculatively.

**Live acceptance is out of scope for this amendment.** Decision 3A's "one
supervised submit-and-reconcile" requirement governs submission
auto-capability; it says nothing about retrieval, and v1 ships no automatic
live-acceptance test for the patron-web route either — verifying it needs a
real ILLiad deployment with a genuinely fulfilled request, which is not
reproducible in CI. Every other part of this Decision (config, compile,
routing, fixtures, tests against a fake browser handoff) ships now.

## Rejected alternatives

**Scripted submission of the patron web form (browser-form auto-submit).** No
vendor offers a patron-delegated submission API — every documented surface
(ILLiad Web Platform, Alma/Rapido REST, OCLC Resource Sharing Request) is an
institution-key integration, and vendor terms prohibit bot-driven form
submission. Driving the SSO'd Primo/ILLiad web form programmatically would be
non-deterministic (no submission-and-reconciliation contract), against the
vendors' terms, and — where the form carries a statutory declaration — would
launder a per-request human act into an automated one. Papio prefills and
opens forms; it never scripts their submission.

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
- Every institution that wants auto-submission must supply a source-controlled
  provider integration with recorded live acceptance plus the Decision 2
  declarations (`submit_policy`, `legal_basis`, `patron_attestation`,
  `patron_fee_policy`, `monthly_request_cap`); without them, papio still
  improves on today by prefilling a form instead of terminating the job
  `unavailable` — and under AU s49 that prefill-plus-one-declaration flow *is*
  the ceiling, by statute rather than caution.
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
