# Use in a coding agent

*papio* is designed to be driven by a coding agent as naturally as by a person.
There are two ways to do that, and they are not equal:

- **The agent skill** drives the `papio` CLI directly — the efficient path, with
  no server in the middle. Prefer it. Install it as shown in
  [Getting started](getting-started.md#the-agent-skill).
- **The MCP server** (`papio mcp`) exposes the same surface to hosts that speak
  MCP rather than shell. Use it when your host cannot run commands.

Both obey identical safety boundaries: human gates stay human, and `zotio apply`
remains the only path that writes to Zotero.

## Invoke the skill

In Claude Code (or another supported agent), invoke the skill with your goal:

```
/papio fetch the PDFs for refs.ris and tell me which ones need me
```

The skill picks the commands and flags, reads the JSON, and stops at every human
gate instead of guessing past it.

## How agents discover the surface at runtime

*papio* is introspectable, so an agent never has to hard-code a command list:

```bash
papio --help                # the command tree
papio <command> --help      # per-command flags
papio doctor --json         # config, daemon, connector, extension, zotio
papio version
```

Add `--json` to any command for structured output. The contract is one shape
everywhere: a list-shaped payload is always
`{"<name>": [...], "truncated": bool}`, never a bare array, and an empty result
is `[]` rather than `null`. `truncated: true` means the row cap filled — raise
`--limit` or paginate to see whether more exist. Commands returning a single
record — `jobs get`, `doctor`, `status`, `batch report`, `zotio plan`, `inbox` —
return that object directly, with no `truncated` key. The MCP resources return
the identical envelope, so one parser serves both surfaces. The full flag set
for every command is in the [command reference](../reference/commands.md).

## Canonical acquisition loop

Use the loop below for a research set. It keeps discovery, acquisition,
observation, identity review, and Zotero writes as separate steps.

```bash
# 1. Discover.
papio search "appropriate reliance on AI" --limit 20 --year-from 2023 --new-only --json

# 2. Acquire the selection — a file of works, or one identifier.
papio acquire --batch works.ris --label "reliance review" --json
papio acquire --doi 10.1371/journal.pone.0262026 --wait --json

# 3. Observe. "Settled" includes work parked on a human.
papio batch report latest --json
papio jobs list --state needs_review --json

# 4. Resolve identity reviews after reading the quarantined file.
papio actions list --json
papio actions resolve <action-id> --accept        # or --reject

# 5. File into Zotero: preview, read it, apply exactly that preview.
papio zotio plan <job-id> --json
papio zotio apply <plan-id> --confirm-sha256 <digest-from-the-plan>
```

When acquisitions fail unexpectedly, run `papio doctor --json` first, then
`papio jobs failures --since 7d --json` to see where failures cluster before
opening individual jobs.

## Safety semantics

### A human gate is an outcome, not an error

`--wait` stops at a terminal state **or** a human action. A settled
`batch report` can contain `awaiting_human` or `needs_review`; those are explicit
outcomes to report, not errors to retry or bypass. Which gate is open decides
what the person must do — an institutional sign-in, a publisher's terms
decision, or an identity review — and none of them are papio's to make. See
[access modes](../concepts/access-modes.md).

### Do not drain the handoff queue

`papio actions list` (or `papio actions open --dry-run`) inspects the queue
without touching a browser. `papio actions open` hands a job to the user's
ordinary browser, and `--job`/`--action` open exactly one chosen row. Looping it
over every row builds the autonomous drain
[ADR-0009](../contributing/architecture-decisions.md) does not ratify: the
browser is one serial surface, and filling it with tabs nobody asked for is not
acquisition progress.

### Identity review is an assertion, not a heuristic override

Only resolve an open `verify_identity` action. The action detail names a local
quarantine file precisely so a human or agent can inspect it.
`papio actions resolve <action-id> --accept` asserts that the file **is** the
requested work; the daemon then imports that same file and records the result as
`user_confirmed`, not as an automatic match. It is not permission to accept a
merely plausible PDF — use `--reject` to record the opposite. Resolution does not
apply to other human-action kinds, and never waives an explicit wrong-work,
encrypted, or active-content rejection.

### zotio writes require a separate confirmation

`papio zotio plan` is a preview step. Inspect the returned plans and keep each
one's `confirmation_sha256`. `papio zotio apply <plan-id> --confirm-sha256
<digest>` requires the exact digest from that preview — do not recompute it
locally, truncate it, or reuse one from another plan. A mismatch is a safe
failure: create and inspect a new preview. `zotio apply` is the only path that
mutates Zotero.

`--auto-import` on acquisition is a policy setting *papio* applies through the
same plan/apply machinery. It does not make acquisition a Zotero-write
operation, and an import failure stays reportable rather than turning a
validated acquisition into a failed one.

## MCP hosts

For a host that speaks MCP rather than shell:

```bash
claude mcp add papio -- papio mcp
```

The server uses a command facade derived from the CLI: `papio_command_search`
discovers runnable commands and `papio_command_run` runs one, so there is no
parallel tool layer that can drift. The composite tools `papio_acquire_batch`
and `papio_batch_wait` have no single-command equivalent and remain first-class.
Read resources are `papio://jobs`, `papio://artifacts`, `papio://bundles`,
`papio://zotio/plans`, and `papio://exports`; they return the same
`{"<name>": [...], "truncated": bool}` envelope, capped at 100 rows — when
`truncated` is true, use the facade (`jobs list` with `--state`/`--limit`) for
filtered access.

Every safety boundary above holds identically over MCP: `papio_command_run` with
`name="actions resolve"` is the same assertion, and with `name="zotio apply"` it
still demands the exact digest from `zotio plan`. For the complete tool,
parameter, resource, and CLI-equivalence reference, see
[MCP tools](../reference/mcp-tools.md).
