# Command reference

Papio commands submit and inspect durable local acquisition work. Use [`../guide/user-guide.md`](../guide/user-guide.md) for the operational workflow and [`config-reference.md`](config-reference.md) for configuration.

## Global flags

These persistent flags are available on every command and subcommand.

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--config` | `string` | `""` | Config TOML path. |
| `--json` | `bool` | `false` | Emit structured JSON. |

## `papio init`

Set up Papio for a first run. It writes configuration, initializes local state, checks Zotio, optionally installs browser integration, and runs `doctor`.

```sh
papio init [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--non-interactive` | `bool` | `false` | Do not prompt; retain existing values unless a flag overrides them. |
| `--email` | `string` | `""` | Contact email for polite API pools. |
| `--zotio-path` | `string` | `""` | Zotio executable path. |
| `--attachment-mode` | `string` | `""` | Zotio attachment mode: `stored` or `linked-file`. |
| `--openurl-base` | `string` | `""` | Institution OpenURL resolver base URL. |
| `--shibboleth-entity-id` | `string` | `""` | Shibboleth IdP entityID for federated login routing. |
| `--proquest-account-id` | `string` | `""` | ProQuest account ID, or a ProQuest URL containing `accountid=`. |
| `--extension-id` | `string` | `""` | Chrome extension ID allowed to reach the native host. |
| `--firefox-extension-id` | `string` | `""` | Firefox add-on ID allowed to reach the native host. |
| `--skip-browser` | `bool` | `false` | Skip Chrome extension and native-host setup. |

## `papio config`

Manage Papio configuration.

### `papio config init`

Write explicit first-run configuration.

```sh
papio config init --access-mode <mode> [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--access-mode` | `string` | `""` | Required. One of `conservative`, `assisted`, or `maximal`. |
| `--email` | `string` | `""` | Contact email for polite APIs. |
| `--data-dir` | `string` | `""` | Artifact and database directory. |
| `--force` | `bool` | `false` | Replace an existing config. |

## `papio acquire`

Submit one paper-acquisition request. Supply at most one positional identifier or identifier flag; a complete title, authors, and year can identify a work without one. With `--batch`, read work records from JSONL; with `--from-zotio`, queue Zotio items missing an attached PDF.

```sh
papio acquire [identifier] [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--doi` | `string` | `""` | DOI. |
| `--pmid` | `string` | `""` | PubMed ID. |
| `--arxiv` | `string` | `""` | arXiv ID. |
| `--isbn` | `string` | `""` | ISBN. |
| `--openalex` | `string` | `""` | OpenAlex work ID. |
| `--title` | `string` | `""` | Work title. |
| `--author` | `stringSlice` | `nil` | Author; repeatable. |
| `--year` | `int` | `0` | Publication year. |
| `--request-id` | `string` | `""` | Stable idempotency key. |
| `--zotio-item-key` | `string` | `""` | Existing Zotero item key. |
| `--collection` | `string` | `""` | Target Zotero collection name; a key when used with `--from-zotio`. |
| `--desired-version` | `string` | `any` | `published`, `accepted`, `preprint`, or `any`. |
| `--access-mode` | `string` | `""` | Per-request access-mode override. |
| `--resolver` | `string` | `""` | Named institutional OpenURL resolver profile. |
| `--max-cost` | `float64` | `0` | Maximum paid-source cost in USD. |
| `--source` | `stringSlice` | `nil` | Allow only this source; repeatable. |
| `--deny-source` | `stringSlice` | `nil` | Deny this source; repeatable. |
| `--wait` | `bool` | `false` | Wait for a terminal or human-action state. |
| `--from-zotio` | `bool` | `false` | Queue Zotio items missing an attached PDF. |
| `--limit` | `int` | `25` | Maximum Zotio queue rows (1–500). |
| `--batch` | `string` | `""` | Submit JSONL works from a file or `-` for standard input. |
| `--include-owned` | `bool` | `false` | With `--batch`, submit works already carrying a PDF in Zotio. |
| `--label` | `string` | `""` | Batch query context; also the default target collection when `--collection` is unset. |
| `--auto-import` | `bool` | `false` | Plan and apply Zotio import automatically when ready. |

## `papio batch`

Inspect persisted acquisition batches.

### `papio batch report`

Join a batch manifest with live acquisition outcomes.

```sh
papio batch report <batch-id|latest> [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--markdown` | `bool` | `false` | Emit an agent-ready Markdown digest. Cannot be combined with `--json`. |

## `papio search`

Search OpenAlex for scholarly works. A query is required unless a citation-snowball DOI is supplied.

```sh
papio search [query] [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--limit` | `int` | `20` | Maximum results (1–50). |
| `--year-from` | `int` | `0` | Minimum publication year. |
| `--year-to` | `int` | `0` | Maximum publication year. |
| `--oa-only` | `bool` | `false` | Return only open-access works. |
| `--new-only` | `bool` | `false` | Omit works already in your library; filters after `--limit` and may return fewer results. |
| `--cites` | `string` | `""` | DOI to find papers citing it (forward citations). |
| `--cited-by` | `string` | `""` | DOI to find papers it cites (backward references). |
| `--related-to` | `string` | `""` | DOI to find OpenAlex-related papers. |

## `papio watch`

Manage scheduled discovery watchlists.

### `papio watch add`

Add a scheduled discovery watch.

```sh
papio watch add <query> [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--label` | `string` | `""` | Human label; defaults to the query. |
| `--collection` | `string` | `""` | Zotio collection for queued papers. |
| `--cadence` | `string` | `daily` | `daily`, `weekly`, or `Nh`. |
| `--limit-per-run` | `int` | `10` | Maximum new papers queued per run (1–50). |
| `--year-from` | `int` | `0` | Minimum publication year. |
| `--year-to` | `int` | `0` | Maximum publication year. |
| `--oa-only` | `bool` | `false` | Return only open-access works. |

### `papio watch list`

List scheduled discovery watches. It has no command-specific flags.

```sh
papio watch list
```

### `papio watch remove`

Remove a scheduled discovery watch. It has no command-specific flags.

```sh
papio watch remove <id>
```

### `papio watch run`

Force-run a scheduled discovery watch now. It has no command-specific flags.

```sh
papio watch run <id>
```

## `papio jobs`

Inspect and control acquisition jobs.

### `papio jobs list`

List jobs.

```sh
papio jobs list [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--state` | `string` | `""` | Filter by exact job state. |
| `--limit` | `int` | `100` | Maximum rows (1–500). |

### `papio jobs get`

Show one job with events and actions.

```sh
papio jobs get <job-id> [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--wait` | `bool` | `false` | Wait for completion or human action. |

### `papio jobs retry`

Explicitly retry a failed, unavailable, or retry-wait job. It has no command-specific flags.

```sh
papio jobs retry <job-id>
```

### `papio jobs cancel`

Cancel a nonterminal job. It has no command-specific flags.

```sh
papio jobs cancel <job-id>
```

## `papio status`

Show active and recent acquisition jobs.

```sh
papio status [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--follow` | `bool` | `false` | Refresh every 2 seconds. |

## `papio actions`

Inspect required human actions.

### `papio actions list`

List open human actions.

```sh
papio actions list [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--all` | `bool` | `false` | Include resolved actions. |

### `papio actions open`

Open the current browser handoff queue.

```sh
papio actions open [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--limit` | `int` | `0` | Maximum actions to open; `0` opens all. |
| `--dry-run` | `bool` | `false` | Print URLs without opening them. |

### `papio actions resolve`

Accept or reject a parked identity review.

```sh
papio actions resolve <action-id> (--accept | --reject)
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--accept` | `bool` | `false` | Accept the identity review. Exactly one of `--accept` or `--reject` is required. |
| `--reject` | `bool` | `false` | Reject the identity review. Exactly one of `--accept` or `--reject` is required. |

## `papio artifacts`

Inspect validated immutable artifacts.

### `papio artifacts get`

Show a validated artifact.

```sh
papio artifacts get <job-id-or-sha256> [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--sha256` | `bool` | `false` | Interpret the argument as an artifact hash. |

## `papio bundle`

Export validated acquisition bundles.

### `papio bundle export`

Export an idempotent bundle directory.

```sh
papio bundle export <job-id> --output <directory>
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `-o`, `--output` | `string` | `""` | Destination directory. Required. |

## `papio doctor`

Check acquisition-core readiness. It has no command-specific flags.

```sh
papio doctor
```

## `papio zotio`

Preview and apply Zotero integration through Zotio.

### `papio zotio preflight`

Verify the configured Zotio version and capabilities. It has no command-specific flags.

```sh
papio zotio preflight
```

### `papio zotio plan`

Export ready jobs and preview exact Zotio mutations. It accepts 1–50 job IDs and has no command-specific flags.

```sh
papio zotio plan <job-id> [job-id...]
```

### `papio zotio apply`

Apply one immutable Zotio plan after SHA-256 confirmation.

```sh
papio zotio apply <plan-id> --confirm-sha256 <digest>
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--confirm-sha256` | `string` | `""` | Required exact confirmation SHA-256 printed by `papio zotio plan`. |

## `papio daemon`

Run or control the local acquisition daemon. `--socket` is persistent on this command and applies to `daemon`, `daemon status`, and `daemon stop`.

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--socket` | `string` | `""` | Unix socket path. |

### `papio daemon`

Run the local acquisition daemon.

```sh
papio daemon [--socket <path>]
```

### `papio daemon status`

Check the running daemon without autostarting one. It accepts the persistent `--socket` flag.

```sh
papio daemon status [--socket <path>]
```

### `papio daemon stop`

Stop the running daemon without autostarting one. It accepts the persistent `--socket` flag.

```sh
papio daemon stop [--socket <path>]
```

## `papio native-host`

Manage browser native-messaging host registration.

### `papio native-host install`

Register native-messaging host manifests and executable symlink.

```sh
papio native-host install [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--manifest-dir` | `string` | `""` | Override the Chrome native-messaging manifest directory. |
| `--firefox-manifest-dir` | `string` | `""` | Override the Firefox native-messaging manifest directory. |

### `papio native-host status`

Report native-messaging host registration state.

```sh
papio native-host status [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--manifest-dir` | `string` | `""` | Override the Chrome native-messaging manifest directory. |
| `--firefox-manifest-dir` | `string` | `""` | Override the Firefox native-messaging manifest directory. |

### `papio native-host uninstall`

Remove native-messaging host manifests and executable symlink.

```sh
papio native-host uninstall [flags]
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--manifest-dir` | `string` | `""` | Override the Chrome native-messaging manifest directory. |
| `--firefox-manifest-dir` | `string` | `""` | Override the Firefox native-messaging manifest directory. |

## `papio mcp`

Serve Papio tools and resources over MCP stdio. It has no command-specific flags.

```sh
papio mcp
```

See [`../guide/agent-skill.md`](../guide/agent-skill.md) for the MCP tool surface.

## `papio version`

Print version information. It has no command-specific flags.

```sh
papio version
```
