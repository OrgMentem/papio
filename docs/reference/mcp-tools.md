# MCP tools

Run *papio* as an MCP stdio server:

```sh
papio mcp
```

The MCP command surface derives from the *papio* CLI command tree, which is
the single source of truth. The default surface is a compact command facade:
two command tools, two composite tools, and read resources. All tool results
are JSON; object keys below are JSON keys.

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

## Command facade (default)

The default command surface contains these two tools:

| Tool | Parameters | Result and boundary |
| --- | --- | --- |
| `papio_command_search` | `query` (optional case-insensitive substring match over command names and summaries); `name` (optional exact, space-separated command path, such as `"zotio apply"`) | Returns JSON. Omit both parameters to list every runnable command. Supplying `name` returns that command's summary, `read_only`, `takes_args`, and command-local `flags` (each flag's name, type, default, and description). |
| `papio_command_run` | `name` (required exact, space-separated command path, such as `"status"` or `"zotio apply"`); `flags` (optional object of command-local flags by name); `args` (optional string of positional arguments only) | Executes the command in-process against the same daemon, jobs, and Zotio boundary as the CLI, and returns JSON. The server injects `--json`. Raw flag tokens in `args` are rejected. Inherited global `--config` and `--json` flags are never exposed and are rejected. |

For example, applying a Zotio plan uses the command facade rather than a
standalone Zotio tool:

```json
{
  "name": "zotio apply",
  "args": "<plan-id>",
  "flags": {
    "confirm-sha256": "<digest-from-zotio-plan>"
  }
}
```

`zotio plan` previews a mutation and prints a confirmation SHA-256 for each
plan. `zotio apply` requires the exact plan ID and that digest; it is the only
path that mutates Zotero.

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
| `papio_batch_wait` | `batch_id` (required persisted batch ID or `latest`); `timeout_seconds` (optional, 1–600, default `300`); `poll_seconds` (optional, default `5`) | Read-only polling of one batch report. Returns `report` and `settled`. A human-review outcome is settled, not implicitly successful. |

## Hidden commands

Commands annotated `mcp:hidden`, and their whole subtrees, are excluded from
both surfaces. The excluded commands are `init`, `config`, `daemon`,
`native-host`, and `mcp`. Commands annotated `mcp:read-only` report
`read_only: true` through `papio_command_search`.

## Old tool -> command_run

The former typed tools below are replaced on the default surface by
`papio_command_run`. `args` is positional arguments only; `flags` uses the
CLI's hyphenated command-local flag names.

| Old tool | `papio_command_run` invocation |
| --- | --- |
| `papio_search` | `name: "search"`; `args: "<query>"`; `flags: {limit, year-from, year-to, oa-only, cites, cited-by, related-to, new-only}` |
| `papio_acquire` | `name: "acquire"`; `args: "<identifier>"`; `flags: {doi, pmid, arxiv, isbn, openalex, title, author, year, request-id, zotio-item-key, collection, desired-version, access-mode, resolver, max-cost, source, deny-source, auto-import}` |
| `papio_status` | `name: "status"`; `flags: {follow}` |
| `papio_doctor` | `name: "doctor"` |
| `papio_actions_list` | `name: "actions list"`; `flags: {all}` |
| `papio_actions_resolve` | `name: "actions resolve"`; `args: "<action-id>"`; `flags: {accept}` **or** `flags: {reject}` (exactly one) |
| `papio_export_bundle` | `name: "bundle export"`; `args: "<job-id>"`; `flags: {output}` |
| `papio_zotio_plan` | `name: "zotio plan"`; `args: "<job-id> <job-id> ..."` |
| `papio_zotio_apply` | `name: "zotio apply"`; `args: "<plan-id>"`; `flags: {"confirm-sha256": "<digest-from-zotio-plan>"}` |
| `papio_watch_add` | `name: "watch add"`; `args: "<query>"`; `flags: {label, collection, cadence, limit-per-run, oa-only, year-from, year-to}` |
| `papio_watch_list` | `name: "watch list"` |
| `papio_watch_remove` | `name: "watch remove"`; `args: "<id>"` |
| `papio_batch_report` | `name: "batch report"`; `args: "<batch-id-or-latest>"`; `flags: {markdown}` |
