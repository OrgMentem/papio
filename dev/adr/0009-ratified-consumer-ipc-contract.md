# ADR-0009: Ratified consumer IPC contract and acquiring-principal boundary

Status: Accepted (2026-07-30). Extends ADR-0007 and is governed by ADR-0001.

**Amended by ADR-0010 (2026-07-31), in three places. Read them before acting on
this document.** Decision 2's refusal of bulk `acquire.submit` stands, but the
narrow single-work `acquire.submit_v2` is now ratified as a seventh method.
Decision 4 names the wrong sanitiser: `redact.URL` appends a `?<redacted>`
marker and therefore emits query data, so `redact.Host` is the emitter.
Decision 5's worked example is invalid — the consumer's gate rejects both
`resolver-profile:institutional-openurl` and
`entitlement-profile:library-e-resource` — and its "Nothing is emitted yet" no
longer holds: `acquisition-bundle/2` is cut and emitting, with
`daemon_held_credential` a current mode rather than a future one.

## Context

*papio*'s first external consumer arrived through the seam ADR-0004 anticipated.
ADR-0007 named `jobs.receipt` and `jobs.add_component` only in Correction A's
historical account of an ADR-0001 reachability breach; it ratified only
`jobs.repair_awaiting_human` by name. A consumer that will not code against
unratified names is therefore blocked on a naming contract that does not exist.

This ADR supplies that contract, and its enforcement is a test rather than a
promise. The consumer pins a *papio* release, so ratification is precisely what
lets it pin: the release contains a mechanically checked surface rather than an
informal list that a later cleanup can silently change.

## Decision 1: The ratified surface

The following six methods are ratified. “Params” names the complete parameter
object; “result shape” names the top-level result. The row types and their keys
are part of each listed shape.

| Method | Params | Result shape | Guarantee |
| --- | --- | --- | --- |
| `jobs.list_v2` | `state?`, `limit?` | `{"jobs": [...], "truncated": bool}` | `truncated` is a proven fact: `true` means a row exists beyond this page and `false` means none does. |
| `actions.list_v2` | `open_only?`, `limit?` | `{"actions": [...], "truncated": bool}` | The same proven pagination fact, for the selected open-action view. |
| `actions.open` | `job_ids` | `{"queued": int, "session_live": bool}` | Batch query/open of the named jobs' handoff actions. |
| `jobs.receipt` | `job_id` | `api.Receipt` | Typed job outcome, including its terminal reason, for states no bundle can describe. |
| `jobs.add_component` | `job_id`, `path`, `role` | `{"components": [...], "truncated": bool}` | Human-adopted supplements and appendices; this method's present result is complete, so `truncated` is `false`. |
| `jobs.repair_awaiting_human` | `job_id` | `api.RepairResult` | The already-ratified orphan-only repair transition. |

The two pagination guarantees are deliberately stronger than the normal
`agentjson.Capped` convention. `Store.listJobs` fetches `limit+1` when its
`probe` is set and derives `truncated` only from `len(idList) > limit`
(`internal/job/job.go:1391-1438`); `ListHumanActionsPage` uses that same
page discipline (`internal/job/job.go:2400-2403`). This is not
`agentjson.Capped`'s documented “may be more, not a proof” signal
(`internal/agentjson/agentjson.go:98-101`). An exactly-full final page must
not be mistaken for a truncated cohort: that mistake would make a consumer
claim it had reconciled every work when it had not.

Ratification means operationally that these wire names and result shapes will
not be renamed, removed, or have their row keys changed. Additive evolution
gets a **new method name**, never a widened result. `internal/ipc` applies
`DisallowUnknownFields` while decoding concrete results
(`internal/ipc/protocol.go:138-160`); widening a result would make an older
CLI reject every response from a newer daemon. Version skew is routine because
one binary supplies the CLI, daemon, and native host, so that is a real outage,
not a theoretical compatibility concern.

## Decision 2: What is deliberately not ratified

- **Bulk `acquire.submit`** is not ratified. It would turn a consumer's
  reconciliation into an uncontrolled acquisition producer and bypass the
  explicit work-selection and operator review boundary.
- **A generic reopen verb** is not ratified. ADR-0007's reason stands: a verb
  accepting arbitrary action ids could close actions the consumer never read;
  the orphan-only repair remains the only narrow transition.
- **Method aliases** are not ratified. They conceal a breaking rename instead of
  making it fail at the pin test, leaving two names for one semantic contract.
- **Autonomous drain** is not ratified. A background consumer must not resolve,
  open, or retry human work on its own: its view can be stale and the action is
  intentionally operator-mediated.

## Decision 3: The acquiring-principal boundary

A route, yes; an identity, never. *papio* never authenticates a human and never
holds institutional credentials. Institutional access rides the operator's own
browser session — no CDP, no WebDriver, and no stored credentials. That is
*papio*'s core value invariant, not a gap. It can report an entitlement
**route**, never an identity it did not observe.

The consumer's model already draws this boundary. Its `AccessAttestation`
takes `tier`, `route`, `tool`, `tool_version`, `acquired_at`, `content_hash`,
and an optional `credential_or_entitlement_ref` from *papio*, while
`acquiring_principal_id` is required
(`/Users/ellis/@dev/inscribi/src/inscribi/models/rights.py:235-258`). That
required acquiring principal is theirs by
right: their operator authorised and ran the acquisition. Its
`entitlement_subject_id` is optional and defaults to `None`
(`/Users/ellis/@dev/inscribi/src/inscribi/models/rights.py:176-205`). Therefore
a route reference with no subject fully satisfies the consumer's attestation;
there is no identity gap for *papio* to close.

Fabricating an identity *papio* never observed would be a false rights record,
worse than an absent one. This is the same asymmetry ADR-0008 adopts for
holdings: a false positive silently withholds a requested paper, while a false
negative costs one download (ADR-0008:94-101). Here the false positive is
stronger still: it invents entitlement evidence that may be used to retain or
transfer bytes.

Ratifying `jobs.receipt` does **not** ratify its `principal` field as a rights
input. ADR-0007 Correction D establishes that it is request-origin
classification, not the entitlement holder (ADR-0007:68-73); the consumer
enforces the same refusal in code. A cache-completed job makes the failure mode
plain: source-acquisition provenance and the current request's `principal` can
legitimately describe different events.

## Decision 4: The sanitised-reference rule

This is a constraint on *papio*, not a note about the consumer. Any route or
entitlement reference *papio* emits **MUST** be a bare reference: no URL query
data, embedded URL credentials, or session tokens. *papio* enforces that rule
at emission and fails closed, so it never sends a value the consumer must
reject. The downstream gate confirms the required boundary: `_validate_safe_route`
rejects query data and URL credentials
(`/Users/ellis/@dev/inscribi/src/inscribi/models/rights.py:344-353`), while an
entitlement reference is accepted only as a closed opaque, non-secret reference
(`/Users/ellis/@dev/inscribi/src/inscribi/models/rights.py:364-376`).

Consequently, bundle v1's `candidate.landing_url` is unsuitable as `route`:
it retains query strings (`protocol/acquisition-bundle-v1.schema.json:45-61`).
This matches *papio*'s existing fixture-sanitisation discipline: `redact.URL`
clears URL userinfo, query, and fragment before a value reaches durable storage
or logs (`internal/redact/redact.go:2-28`). A resolver profile or institution
identifier is a route; a session token and `https://…/?access=…` are not.

## Decision 5: `acquisition-bundle/2` carries it, and the migration order

`acquisition-bundle/1` cannot carry this object. Its root and `candidate` both
set `additionalProperties: false`
(`protocol/acquisition-bundle-v1.schema.json:1-7,45-61`), and bundle decoding
recursively rejects unknown fields with `DisallowUnknownFields`
(`internal/protocol/protocol.go:58-72`). A candidate-bound IPC method was
rejected because success provenance belongs only to the accepted candidate;
that method would create the second success record the consumer's ADR-020
Decision 6 forbids.

The proposed v2 shape places `entitlement` on the accepted candidate:

```json
{
  "schema_version": "acquisition-bundle/2",
  "candidate": {
    "source": "resolver-name",
    "version": "published",
    "access_basis": "institutional",
    "reuse_license": "unknown",
    "entitlement": {
      "route": "resolver-profile:institutional-openurl",
      "entitlement_ref": "entitlement-profile:library-e-resource",
      "acquisition_mode": "operator_browser_session"
    }
  }
}
```

`route` is the required sanitised resolver, institution, or other bare route
reference — never `landing_url`. `entitlement_ref` is an optional opaque
entitlement/profile reference and is omitted when *papio* did not observe one.
`acquisition_mode` is an enum: `operator_browser_session` says the operator's
own browser session obtained access; `daemon_held_credential` distinguishes a
future daemon-held-credential acquisition; `open_access` says neither was
needed. The whole `entitlement` object is omitted when its route is unknown;
no field is filled by inference. This is a proposal in this ADR, **not** a v2
schema, Go type, or emitted payload.

The agreed migration order is fixed:

1. *papio* ratifies these six names with the pin test. This alone releases the
   consumer's transport.
2. *papio* drafts the `acquisition-bundle/2` shape.
3. The consumer implements decoding for **both** v1 and v2 and lands it.
4. Only then does *papio* switch emission to v2, while retaining v1 decoding
   indefinitely.

This ADR ships steps 1 and 2 only. **Nothing is emitted yet.** Switching early
would strand the only external consumer on a version it correctly rejects.

## Consequences

- A ratified name is a name *papio* can no longer clean up. The pin test fails
  on a rename, removal, or row-key drift — which is the point: it turns an
  external integration break into a release-blocking local failure.
- The pin test protects method names, ratified row keys, receipt tags, and the
  proven truncation contract. It does **not** promise row values or ordering;
  consumers must not convert a current ordering or observed value into a
  compatibility guarantee.
- Receipt and bundle remain non-overlapping: `jobs.receipt` records typed
  terminal reasons and component inventory where no bundle may exist; the
  accepted candidate's bundle is the success provenance document. The proposed
  v2 object extends that one success record only after the consumer can decode
  it.
