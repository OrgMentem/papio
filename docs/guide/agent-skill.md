# MCP agent guide

Run *papio* as an MCP server with:

```sh
papio mcp
```

The server uses a command facade: `papio_command_search` discovers runnable
commands and `papio_command_run` runs them. The composite tools
`papio_acquire_batch` and `papio_batch_wait` remain first-class tools. Its read
resources are `papio://jobs`, `papio://artifacts`, `papio://bundles`,
`papio://zotio/plans`, and `papio://exports`. List resources return a
`{"<name>": [...], "truncated": bool}` envelope capped at 100 rows; when
`truncated` is true, use the facade (`jobs list` with `--state`/`--limit`)
for filtered access.
When acquisitions fail unexpectedly, run `papio_command_run` with
`name="doctor"` first, and `name="jobs failures"` to see where failures
cluster. Use `papio_command_search` to discover commands and
their command-local flags.

## Canonical acquisition loop

Use the loop below for a research set. It separates discovery, acquisition,
observation, identity review, and Zotero writes.

1. Use `papio_command_run` with `name="search"` and a search query as `args`
   (or use `papio_acquire_batch` directly for already selected works).
2. Create the saved acquisition batch from selected works with
   `papio_acquire_batch`. It accepts 1–50 bare work objects or discovered-work
   envelopes and returns a `batch_id`.
3. Once there is a persisted batch ID, call `papio_batch_wait` with a
   timeout. It stops either when all work is settled, including human-review
   work, or when its timeout expires.
4. Use `papio_command_run` with `name="batch report"` and the batch ID as
   `args` for outcomes.
5. Use `papio_command_run` with `name="actions list"` to list open actions.
   For an open `verify_identity` action, inspect the local quarantine path in
   its detail, then use `papio_command_run` with `name="actions resolve"`,
   `args="<action-id>"`, and exactly one of `flags: {"accept": true}` or
   `flags: {"reject": true}`.
6. For ready jobs that need Zotero mutation, use `papio_command_run` with
   `name="zotio plan"` and the job IDs as `args`, inspect every immutable
   preview, then use it with `name="zotio apply"`, `args="<plan-id>"`, and
   `flags: {"confirm-sha256": "<exact digest>"}`.

Do not turn a human-action state into an implicit success. A settled report can
contain `awaiting_human` or `needs_review`; those are explicit outcomes, not
errors to bypass.

For the complete tool, parameter, boundary, resource, and CLI-equivalence
reference, see [MCP tools](../reference/mcp-tools.md).

## Safety semantics

### Identity review is an assertion, not a heuristic override

Only resolve an open `verify_identity` action. The action detail identifies a
local quarantine file precisely so the human or agent can inspect it. Running
`papio_command_run` with `name="actions resolve"`, the action ID as `args`, and
`flags: {"accept": true}` is an assertion that the file is the requested work;
it is not permission to accept a merely plausible PDF. Use
`flags: {"reject": true}` to record the opposite. The resolution surface does
not apply to other human-action kinds.

### zotio writes require a separate confirmation

`papio_command_run` with `name="zotio plan"` is a preview step. Inspect the
returned plans and preserve the returned `confirmation_sha256` for each one.
`papio_command_run` with `name="zotio apply"` requires the exact `plan_id` as
`args` and exact digest from that preview as
`flags: {"confirm-sha256": "<exact digest>"}`. Do not substitute a locally
recomputed digest, truncate it, or reuse one from a different plan. A
confirmation mismatch is a safe failure; create and inspect a new preview.
Only `papio_command_run` with `name="zotio apply"` can mutate Zotero.

`auto_import` on acquisition is a policy setting *papio* applies automatically. It does not make
acquisition a Zotero-write operation, and an auto-import failure remains
reportable rather than changing the validated acquisition into a failed PDF.

