# ADR-0004: Generic on_ready hook for library hand-off

Status: Accepted (2026-07-21)

## Context

papio's value ends at a validated, provenance-tracked PDF. Where that PDF is
*filed* differs per user: Zotero (through zotio, the deep integration —
ownership deduplication, idempotent plan/apply, import retry, enrichment,
collection filing), papis, Calibre, a plain folder, or a private script.

Bespoke importers do not scale. The zotio pipeline's depth is justified
because Zotero has server-assigned item keys, a Web API, and an ownership
model to reconcile against. papis has none of those primitives — it is local
YAML plus a `papis add` command — so mirroring the `[zotio]` machinery for it
would mean rebuilding a stateful pipeline against a backend that cannot
support its invariants, tracking a GPL-3.0 project's CLI surface, for a small
audience of terminal power-users who can wire their own glue.

Separately, `zotio.executable` was a required config field, so papio could
not run for users with no Zotero library at all.

## Options

### A. Bespoke `[papis]` importer mirroring `[zotio]`

Rejected: duplicate stateful pipeline against a backend without matching
primitives (no item keys, no ownership API); a second heavy convention owned
forever; slight cannibalization of the papio→zotio→Zotero path for marginal
reach.

### B. Generic `on_ready` shell hook + documented recipes (accepted)

One config seam (`[hooks] on_ready`) that runs a user command once per job
that reaches `ready`, with metadata in `PAPIO_*` environment variables. A
papis recipe is one documented line; every other manager gets the same seam
for free.

### C. Status quo (manual `papio bundle export`)

Rejected: no automation; the bundle stays the schema-validated hand-off for
tooling, but a human should not have to run an export per acquisition.

## Decision

Option B.

- **The env-var contract is public API.** `PAPIO_JOB_ID`, `PAPIO_REQUEST_ID`,
  `PAPIO_DOI`, `PAPIO_ARXIV`, `PAPIO_TITLE`, `PAPIO_SHA256`, `PAPIO_PDF`,
  `PAPIO_STATE` are frozen names; additions are allowed, renames are not.
- **Hooks are fire-and-forget.** One run per ready transition, bounded by
  `hooks.timeout_seconds`, never retried, never able to fail or block the
  job. The durable `hook.on_ready` job event (`status`, `exit_code`,
  `duration_ms`, `stderr_tail`) is the audit trail.
- **`PAPIO_PDF` is read-only.** It points into the immutable
  content-addressed artifact store; consumers copy.
- **zotio stays the only deep integration** (ADR: "zotio is the Zotero
  boundary" holds — papio still never writes Zotero directly).
- **`zotio.executable` becomes optional.** Empty disables auto-import,
  plan/apply, and queue; ownership lookup degrades to not-owned with a
  staleness warning; `doctor` reports zotio as not configured (optional).
  `zotio.auto_import = true` still requires the executable.

Scope tripwire: if the hook ever grows per-manager templating, result
parsing, or backend-specific flags, reopen the bespoke-importer option (A)
instead of growing the hook into an implicit integration framework.
