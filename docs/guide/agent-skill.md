# MCP agent guide

Run Papio as an MCP stdio server with:

```sh
papio mcp
```

The server uses the same local configuration, daemon, durable jobs, and Zotio
boundary as the CLI. Its tools are named `papio_*`; its read resources are
`papio://jobs`, `papio://artifacts`, `papio://bundles`, `papio://zotio/plans`,
and `papio://exports`.

## Canonical acquisition loop

Use the loop below for a research set. It separates discovery, durable
acquisition, observation, identity review, and Zotero mutation.

1. Call `papio_search` with a bounded query or citation-snowball DOI.
2. Create the acquisition batch from the selected works. The current MCP surface
   exposes item-level `papio_acquire`; use it once per selected work with a
   stable `request_id`. To create a persisted **batch** with a `batch_id` for
   waiting and reporting, use the CLI equivalence `papio acquire --batch` with
   JSONL input. `papio_acquire` returns a `job_id`, not a `batch_id`.
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

## Tool reference

All object keys below are JSON keys. “Optional” means the field may be omitted;
the server applies the documented default where one exists.

### Discovery and acquisition

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_search` | `query` (optional unless no snowball DOI); `limit` (optional, 1–50); `year_from`, `year_to`, `oa_only` (optional); `cites`, `cited_by`, `related_to` (optional DOI filters); `new_only` (optional) | Bounded, read-only OpenAlex results. Results include `owned` and `owned_item_key` when library lookup is available. `new_only` filters owned results after the OpenAlex limit. |
| `papio_acquire` | `request_id` (optional stable idempotency key); `identifiers` (optional DOI, PMID, arXiv, ISBN, or OpenAlex identity); or `title`, `authors`, and `year`; `zotio_item_key`, `collection`, `desired_version`, `access_mode_override`, `max_cost_usd`, `sources_allow`, `sources_deny`, `auto_import` (all optional) | Queues one bounded policy-controlled job and returns `job_id`. It does **not** write to Zotero. `desired_version` is `published`, `accepted`, `preprint`, or `any`; each source list is limited to 50 entries. |
| `papio_batch_wait` | `batch_id` (required; a persisted batch ID or `latest`); `timeout_seconds` (optional, 1–600; 0 or omitted defaults to 300); `poll_seconds` (optional, default 5) | Read-only polling of one batch report. Returns `report` and `settled`; it submits, imports, and resolves nothing. |
| `papio_batch_report` | `batch_id` (required; persisted batch ID or `latest`); `format` (optional `json` or `markdown`, default `json`) | Read-only manifest, job, event, and human-action report. |
| `papio_status` | none | Read-only grouped snapshot of working, human-review, ready, and failed jobs. |

### Actions, artifacts, and Zotio

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_actions_list` | none | Read-only open actions with IDs, job IDs, kinds, and details. `verify_identity` details intentionally include the local quarantine path. |
| `papio_actions_resolve` | `action_id` (required open `verify_identity` action); `verdict` (required `accept` or `reject`) | Resolves only a verified-identity action and returns the job ID and new state. `accept` asserts that the caller inspected the quarantined artifact and verified it is the requested work; `reject` records that it is not. |
| `papio_export_bundle` | `job_id` (required ready job); `output_dir` (optional private destination) | Read-only/idempotent export of a validated PDF and provenance bundle. Without `output_dir`, Papio uses its data directory. |
| `papio_zotio_plan` | `job_ids` (required; 1–50 ready job IDs) | Exports ready jobs and persists immutable Zotio mutation previews. It never applies a Zotero mutation. |
| `papio_zotio_apply` | `plan_id` (required); `confirmation_sha256` (required exact value from `papio_zotio_plan`) | Applies exactly one preview. Replaying the same confirmed plan is idempotent. This is the only MCP tool that can make Zotero writes. |

### Scheduled discovery

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_watch_add` | `query` (required); `cadence_hours` (required positive integer); `label`, `filters`, `collection`, `per_run_cap` (optional). `filters` contains optional `year_from`, `year_to`, and `oa_only`; `per_run_cap` defaults to 10. | Creates a bounded, durable scheduled discovery watch and returns its state. |
| `papio_watch_list` | none | Read-only watch rows, including last-run and failure state. |
| `papio_watch_remove` | `id` (required scheduled watch ID) | Permanently removes the watch and returns `removed`. It does not delete earlier jobs or Zotero items. |

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

## CLI equivalence

The MCP tools use the same application surface as these CLI commands. “No
single command” means the CLI has no one-call equivalent, not that the operation
is unavailable.

| MCP tool | Closest CLI operation | Notes |
| --- | --- | --- |
| `papio_search` | `papio search [query]` | CLI supports `--cites`, `--cited-by`, `--related-to`, `--new-only`, and the same bounded filters. |
| `papio_acquire` | `papio acquire [identifier]` | CLI can also create a persisted batch with `papio acquire --batch <file-or->`. |
| `papio_batch_wait` | No single command | Poll `papio batch report <batch-id-or-latest>` or use `papio status --follow`. |
| `papio_batch_report` | `papio batch report <batch-id-or-latest>` | Add `--markdown` for the Markdown digest. |
| `papio_status` | `papio status` | Add `--follow` for a two-second refresh loop. |
| `papio_actions_list` | `papio actions list` | Add `--all` to include resolved CLI actions. |
| `papio_actions_resolve` | `papio actions resolve <action-id> --accept` or `--reject` | Both surfaces accept or reject only identity reviews. |
| `papio_export_bundle` | `papio bundle export <job-id>` | CLI destination is `--output <directory>`. |
| `papio_zotio_plan` | `papio zotio plan <job-id> [job-id...]` | Preview first. |
| `papio_zotio_apply` | `papio zotio apply <plan-id> --confirm-sha256 <digest>` | The SHA-256 is mandatory. |
| `papio_watch_add` | `papio watch add <query>` | CLI cadence is `daily`, `weekly`, or `Nh`; MCP takes positive `cadence_hours`. |
| `papio_watch_list` | `papio watch list` | Both are read-only. |
| `papio_watch_remove` | `papio watch remove <id>` | Neither removes prior jobs or Zotero items. |
