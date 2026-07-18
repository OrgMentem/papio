# MCP tools

Run Papio as an MCP stdio server:

```sh
papio mcp
```

The server uses the same local configuration, daemon, durable jobs, and Zotio
boundary as the CLI. Its tools are named `papio_*`. All object keys below are
JSON keys; optional fields may be omitted and use the stated default where one
exists.

## Read resources

All resources return JSON. They expose recent durable state; they do not create
jobs, export bundles, or mutate Zotero.

| Resource | Contents |
| --- | --- |
| `papio://jobs` | Up to 100 recent durable acquisition jobs. |
| `papio://artifacts` | Up to 100 recent validated, content-addressed PDF artifacts. |
| `papio://bundles` | Up to 100 bundle export records. |
| `papio://zotio/plans` | Up to 100 immutable Zotio preview records. |
| `papio://exports` | Up to 100 bundle, Zotio-plan, and Zotio-apply ledger records. |

## Discovery and acquisition

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_search` | `query` (optional unless no citation-snowball DOI is supplied); `limit` (optional, clamped to 1–50); `year_from`, `year_to`, `oa_only` (optional); `cites`, `cited_by`, `related_to` (optional DOI filters); `new_only` (optional) | Bounded, read-only OpenAlex results. Results include `owned` and `owned_item_key` when local Zotio lookup is available. `new_only` filters owned results after the OpenAlex limit, so fewer results can be returned. It never creates a job. |
| `papio_acquire` | `request_id` (optional stable idempotency key); `identifiers` (optional DOI, PMID, arXiv, ISBN, or OpenAlex identity); or `title`, `authors`, and `year`; `zotio_item_key`, `collection`, `desired_version`, `access_mode_override`, `resolver`, `max_cost_usd`, `sources_allow`, `sources_deny`, `auto_import` (all optional) | Queues one bounded, policy-controlled job and returns `job_id`; it does not write to Zotero. `desired_version` is `published`, `accepted`, `preprint`, or `any`; `access_mode_override` is `conservative`, `assisted`, or `maximal`; `resolver` selects a named institutional OpenURL resolver profile. Each source list is limited to 50 entries. Omitting `auto_import` uses the configured default. |
| `papio_acquire_batch` | `works` (required; 1–50 bare work objects or discovered-work envelopes); `auto_import`, `collection`, `resolver`, `label`, `include_owned` (optional) | Creates a durable batch using the CLI batch path's ownership routing, deterministic request IDs, auto-import policy, and manifest. Returns `batch_id` plus submitted and ownership-routing outcomes. `auto_import` defaults to `true`; `collection` defaults to `label` when unset; `include_owned` defaults to `false`. |
| `papio_batch_report` | `batch_id` (required persisted batch ID or `latest`); `format` (optional `json` or `markdown`, default `json`) | Read-only manifest, job, event, and human-action report. |
| `papio_batch_wait` | `batch_id` (required persisted batch ID or `latest`); `timeout_seconds` (optional, 1–600; `0` or omitted defaults to 300); `poll_seconds` (optional, default 5) | Read-only polling of one batch report. Returns `report` and `settled`; it submits, imports, and resolves nothing. A human-review outcome is settled rather than implicitly successful. |
| `papio_status` | None | Read-only snapshot of active and recently completed jobs, grouped as working, awaiting human review, needs review, ready, or failed/unavailable. Each job includes `id`, `title`, `provider`, `state`, and `age`; review and failed/unavailable jobs also return `reason`, actionable `category`, and one-line `guidance`. Ready jobs return `import_status`. |
| `papio_doctor` | None | Read-only integration diagnostics for loaded configuration, the in-process daemon, browser-extension connectivity, Chrome and Firefox native-messaging manifests, and Zotio preflight. Returns `ok` and pass/warn/fail/skip checks with remediation guidance. |

## Actions, artifacts, and Zotio

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_actions_list` | None | Read-only open actions with IDs, job IDs, kinds, and details. A `verify_identity` action intentionally includes the local quarantine path for inspection. |
| `papio_actions_resolve` | `action_id` (required open `verify_identity` action); `verdict` (required `accept` or `reject`) | Resolves only a verified-identity action and returns the job ID and new state. `accept` asserts that the caller inspected the quarantined artifact and verified that it is the requested work; `reject` records that it is not. |
| `papio_export_bundle` | `job_id` (required ready job); `output_dir` (optional private destination) | Read-only, idempotent export of a validated PDF and provenance bundle. Without `output_dir`, Papio uses its data directory. |
| `papio_zotio_plan` | `job_ids` (required; 1–50 ready job IDs) | Exports ready jobs, routes them through Zotio, and persists immutable mutation previews. It never applies a Zotero mutation. |
| `papio_zotio_apply` | `plan_id` (required); `confirmation_sha256` (required exact value returned by `papio_zotio_plan`) | Applies exactly one immutable preview. Replaying the same confirmed plan is idempotent. This is the only MCP tool that writes to Zotero. |

## Scheduled discovery

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_watch_add` | `query` (required); `cadence_hours` (required positive integer); `label`, `filters`, `collection`, `per_run_cap` (optional). `filters` contains optional `year_from`, `year_to`, and `oa_only`; `label` defaults to `query`; `per_run_cap` defaults to 10. | Creates a bounded, durable scheduled discovery watch and returns its state. Each run applies Papio's existing batch, ownership, auto-import, collection, and notification policy. |
| `papio_watch_list` | None | Read-only scheduled-watch rows, including last-run and failure state. |
| `papio_watch_remove` | `id` (required scheduled watch ID) | Permanently removes the watch and returns `id` and `removed`. It does not delete jobs or Zotero items created by earlier watch runs. |

## CLI equivalence

The MCP tools use the same application surface as these CLI commands. “No
single command” means the CLI has no one-call equivalent, not that the operation
is unavailable.

| MCP tool | Closest CLI operation | Notes |
| --- | --- | --- |
| `papio_search` | `papio search [query]` | CLI supports `--cites`, `--cited-by`, `--related-to`, `--new-only`, and the same bounded filters. |
| `papio_acquire` | `papio acquire [identifier]` | Submits one acquisition request. |
| `papio_acquire_batch` | `papio acquire --batch <file-or->` | The CLI reads JSONL work input and creates a persisted batch. |
| `papio_batch_report` | `papio batch report <batch-id-or-latest>` | Add `--markdown` for the Markdown digest. |
| `papio_batch_wait` | No single command | Poll `papio batch report <batch-id-or-latest>` or use `papio status --follow`. |
| `papio_status` | `papio status` | Add `--follow` for a two-second refresh loop. |
| `papio_doctor` | `papio doctor` | Both return integration diagnostics; the MCP tool also uses the daemon's in-process readiness handles. |
| `papio_actions_list` | `papio actions list` | Add `--all` to include resolved CLI actions. |
| `papio_actions_resolve` | `papio actions resolve <action-id> --accept` or `--reject` | Both surfaces accept or reject only identity reviews. |
| `papio_export_bundle` | `papio bundle export <job-id>` | CLI destination is `--output <directory>`. |
| `papio_zotio_plan` | `papio zotio plan <job-id> [job-id...]` | Preview first. |
| `papio_zotio_apply` | `papio zotio apply <plan-id> --confirm-sha256 <digest>` | The SHA-256 is mandatory. |
| `papio_watch_add` | `papio watch add <query>` | CLI cadence is `daily`, `weekly`, or `Nh`; MCP takes positive `cadence_hours`. |
| `papio_watch_list` | `papio watch list` | Both are read-only. |
| `papio_watch_remove` | `papio watch remove <id>` | Neither removes prior jobs or Zotero items. |
