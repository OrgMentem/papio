# MCP agent guide

Run Papio as an MCP stdio server with:

```sh
papio mcp
```

The server uses the same local configuration, daemon, durable jobs, and Zotio
boundary as the CLI. Its tools are named `papio_*`; its read resources are
`papio://jobs`, `papio://artifacts`, `papio://bundles`, `papio://zotio/plans`,
and `papio://exports`.
When acquisitions fail unexpectedly, run `papio_doctor` first.

## Canonical acquisition loop

Use the loop below for a research set. It separates discovery, durable
acquisition, observation, identity review, and Zotero mutation.

1. Call `papio_search` with a bounded query or citation-snowball DOI.
2. Create the persisted acquisition batch from selected works with
   `papio_acquire_batch`. It accepts 1–50 bare work objects or discovered-work
   envelopes and returns a `batch_id`. Use item-level `papio_acquire` only when
   submitting one work; it returns a `job_id`, not a `batch_id`.
3. Once there is a persisted batch ID, call `papio_batch_wait` with a bounded
   timeout. It stops either when all work is settled, including human-review
   work, or when its timeout expires.
4. Call `papio_batch_report` for JSON or Markdown outcomes.
5. Call `papio_actions_list`. For an open `verify_identity` action, inspect the
   local quarantine path in its detail and then call `papio_actions_resolve`
   with `accept` or `reject`.
6. For ready jobs that need Zotero mutation, call `papio_zotio_plan`, inspect
   every immutable preview, then call `papio_zotio_apply` with the returned
   plan ID and exact confirmation SHA-256.

Do not turn a human-action state into an implicit success. A settled report can
contain `awaiting_human` or `needs_review`; those are explicit outcomes, not
errors to bypass.

For the complete tool, parameter, boundary, resource, and CLI-equivalence
reference, see [MCP tools](../reference/mcp-tools.md).

## Safety semantics

### Identity review is an assertion, not a heuristic override

Only resolve an open `verify_identity` action. The action detail identifies a
local quarantine file precisely so the human or agent can inspect it. Passing
`verdict: "accept"` is an assertion that the file is the requested work; it is
not permission to accept a merely plausible PDF. Passing `reject` records the
opposite. The resolution surface does not apply to other human-action kinds.

### Zotio writes require a separate confirmation

`papio_zotio_plan` is a preview step. Inspect the returned plans and preserve
the returned `confirmation_sha256` for each one. `papio_zotio_apply` requires
the exact `plan_id` and exact digest from that preview. Do not substitute a
locally recomputed digest, truncate it, or reuse one from a different plan. A
confirmation mismatch is a safe failure; create and inspect a new preview.

`auto_import` on acquisition is policy-driven daemon behavior. It does not make
`papio_acquire` itself a Zotero-write tool, and an auto-import failure remains
reportable rather than changing the validated acquisition into a failed PDF.

