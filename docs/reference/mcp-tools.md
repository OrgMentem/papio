# MCP tools

Run *papio* as an MCP stdio server:

```sh
papio mcp
```

The MCP command surface derives from the *papio* CLI command tree, which is
the single source of truth. The default surface is a compact command facade:
two command tools, three composite tools, and read resources. All tool results
are JSON; object keys below are JSON keys.

## Read resources

All resources return JSON, each result wrapped in the same two-key envelope
the CLI's `--json` list output uses (see the [command reference](commands.md#json-output-contract)'s
JSON output contract): `{"<name>": [...], "truncated": false}`, never a bare
array. `papio_command_run` inherits this contract from whichever CLI command
it runs. They expose recent durable state; they do not create jobs, export
bundles, or mutate Zotero.

| Resource | Contents |
| --- | --- |
| `papio://jobs` | Up to 100 recent durable acquisition jobs. |
| `papio://artifacts` | Up to 100 recent validated, content-addressed PDF artifacts. |
| `papio://bundles` | Up to 100 bundle export records. |
| `papio://zotio/plans` | Up to 100 immutable zotio preview records. |
| `papio://exports` | Up to 100 bundle, zotio-plan, and zotio-apply ledger records. |

## Command facade (default)

The default command surface contains these two tools:

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_command_search` | `query` (optional case-insensitive substring match over command names and summaries); `name` (optional exact, space-separated command path, such as `"zotio apply"`) | Returns JSON. Omitting both parameters returns a bare JSON array of command summaries — the one tool result that is not the `{"<name>": [...], "truncated": ...}` envelope. Supplying `name` returns that command's summary, `read_only`, `takes_args`, and command-local `flags` (each flag's name, type, default, and description). |
| `papio_command_run` | `name` (required exact, space-separated command path, such as `"status"` or `"zotio apply"`); `flags` (optional object of command-local flags by name); `args` (optional string of positional arguments only) | Executes the command in-process against the same daemon, jobs, and zotio boundary as the CLI, and returns JSON. The server injects `--json`. Raw flag tokens in `args` are rejected. Inherited global `--config` and `--json` flags are never exposed and are rejected. |

The facade therefore reaches `papio jobs receipt`, `papio jobs
repair-awaiting-human`, and `papio inbox decide` for
MCP consumers through `papio_command_search` and `papio_command_run`; no
per-method MCP tools exist.
For an accepted main component, the bundle is the canonical provenance document;
the receipt is the canonical job outcome and component inventory for every state.

### Reading a paper's text

`papio_command_run` with `"name": "artifacts text"` returns the extracted text of a
validated artifact, so an agent does not have to parse the PDF itself. It is
read-only. The argument is a job id, or an artifact hash with the `sha256` flag.
The result is `{text, first_page, last_page, pages_total, chars, truncated,
ocr_used}`.

- One call returns at most 131072 bytes (128 KiB) of text. That limit is
  chosen so the JSON-escaped result always fits the daemon's 1 MiB result cap.
  The `max-bytes` flag can lower it, never raise it.
- Page ranges are the paging mechanism. Pass `"pages": "2-3"` (or `"5"`). When
  `truncated` is `true`, read on from `last_page`.
- `ocr_used` is always `false`. A scanned PDF with no text layer returns little
  or no text; *papio* does not run OCR for this command.
- Only the artifact of a job in state `ready` or `imported` is served. A job in
  any other state, a quarantined file, a hash held only by jobs that are not
  ready or imported, and an unknown hash are each refused with a distinct
  `error_class`. A daemon host without `pdftotext` (Poppler) answers
  `artifact_text_tool_unavailable`, never an empty success.

For example, applying a zotio plan uses the command facade rather than a
standalone zotio tool:

```json
{
  "name": "zotio apply",
  "args": "<plan-id>",
  "flags": {
    "confirm-sha256": "<digest-from-zotio-plan>"
  }
}
```

`zotio plan` previews an item or attachment mutation and prints a confirmation SHA-256 for each
plan. `zotio apply` requires the exact plan ID and that digest, and it is the only
item and attachment import path. `zotio tags reconcile` writes tags with no preview, so it is a terminal-only command hidden from MCP; the daemon already runs the same pass automatically.

## Mirror surface

Set `PAPIO_MCP_SURFACE=mirror` to expose one MCP tool per runnable CLI command.
Each tool is named `papio_<path>`, where the space-separated command path is
joined with underscores: for example, `papio_status`, `papio_search`,
`papio_zotio_apply`, and `papio_watch_add`.

Mirror tools expose their command-local flags as native parameters plus an
`args` string for positional arguments. They use the same validation as the
facade: raw flag tokens in `args`, and inherited global `--config` and
`--json` flags, are rejected.

## Composite tools

These tools are always present in both the default facade and mirror surfaces
because no single CLI command supplies their MCP operation.

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_acquire_batch` | `works` (required; 1–50 bare work objects or discovered-work envelopes); `auto_import` (optional, default `true`); `collection` (optional; defaults to `label`); `resolver` (optional); `label` (optional); `include_owned` (optional, default `false`) | Bulk-input equivalent of `acquire --batch`, whose stdin path is unavailable over MCP. Returns the batch manifest/routing result, including `batch_id`. |
| `papio_batch_wait` | `batch_id` (required persisted batch ID or `latest`); `timeout_seconds` (optional, whole seconds 1–600, default `300`); `poll_seconds` (optional, whole seconds, default `5`); a fractional value is rejected | Read-only polling of one batch report. Returns `report` and `settled`. A human-review outcome is settled, not implicitly successful. |
| `papio_changes` | `after_seq` (optional cursor, whole number ≥ 0, default `0`); `limit` (optional, 1–500, default `100`); `batch_id` (optional batch ID or `latest`) or `job_id` (optional), not both | Read-only page of durable job events with `seq` greater than `after_seq`, oldest first. Returns `changes` (`seq`, `job_id`, `at`, `kind`, `detail`), `truncated` (more events are already waiting), and `next_seq`. Pass `next_seq` back as `after_seq`: each event arrives once, across reconnects and daemon restarts. Returns immediately; it does not wait for new events. A `batch_id` scope is refused while any of its works has no job yet (submission still running or interrupted), because those events would otherwise be paged past. |

## Hidden commands

Commands annotated `mcp:hidden`, and their whole subtrees, are excluded from
both surfaces. The excluded commands are `init`, `config`, `daemon`,
`native-host`, `mcp`, `browser reload`, `drive resume`, `zotio tags reconcile`, `jobs supply-pdf`, and
`jobs add-component` (the last two read a file on the local disk). Commands annotated `mcp:read-only` report
`read_only: true` through `papio_command_search`.

