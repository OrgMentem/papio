# Hooks: hand off acquisitions to any library manager

papio's deep integration is [zotio](https://github.com/OrgMentem/zotio) →
Zotero: ownership deduplication, idempotent plan/apply, import retry,
enrichment, and collection filing. If you file papers somewhere else —
[papis](https://github.com/papis/papis), Calibre, a plain folder, your own
script — use the generic **`on_ready` hook** instead.

## What `on_ready` does

When a job reaches `ready` (its PDF passed structural and identity
validation), the daemon runs your configured shell command once, with the
job's metadata in `PAPIO_*` environment variables:

```toml
[hooks]
on_ready = 'papis add --from doi "$PAPIO_DOI" "$PAPIO_PDF"'
timeout_seconds = 120   # optional; default 120, range 5..600
```

Contract:

- **Fires once per ready transition.** Import retries and daemon restarts do
  not re-fire it.
- **Fire-and-forget.** A slow or failing hook never blocks or fails the job.
  papio does not retry hooks.
- **Audited.** Each run records a durable `hook.on_ready` job event with
  `status`, `exit_code`, `duration_ms`, and (on failure) a bounded
  `stderr_tail`.
- **Shell semantics.** The command runs via `/bin/sh -c` (`cmd /C` on
  Windows). The recipes below are POSIX-shell.
- **Concurrency.** Concurrent ready jobs may run hooks concurrently; if your
  command needs serialization, own it (e.g. `flock`).

## Environment variables

| Variable | Value | Always set? |
| --- | --- | --- |
| `PAPIO_JOB_ID` | job id | yes |
| `PAPIO_REQUEST_ID` | originating work-request id | yes |
| `PAPIO_DOI` | DOI (`10.…`) | empty when the work has no DOI |
| `PAPIO_ARXIV` | arXiv id | empty when absent |
| `PAPIO_TITLE` | requested title | empty when absent |
| `PAPIO_SHA256` | artifact content hash | yes |
| `PAPIO_PDF` | absolute path to the validated PDF | yes |
| `PAPIO_STATE` | `ready` | yes |

**Treat `PAPIO_PDF` as read-only.** It points into papio's immutable
content-addressed artifact store. Copy the file if you need to move or rename
it — `papis add` and `cp` both copy by default.

## Recipes

### papis

```toml
[hooks]
on_ready = 'papis add --from doi "$PAPIO_DOI" "$PAPIO_PDF"'
```

Works acquired without a DOI need a fallback; point the hook at a small
wrapper script instead:

```sh
#!/bin/sh
if [ -n "$PAPIO_DOI" ]; then
    exec papis add --batch --from doi "$PAPIO_DOI" "$PAPIO_PDF"
fi
exec papis add --batch --set title "$PAPIO_TITLE" "$PAPIO_PDF"
```

### Plain folder

```toml
[hooks]
on_ready = 'cp "$PAPIO_PDF" "$HOME/Papers/"'
```

## Running without zotio

`zotio.executable` is optional. With it empty, the deep Zotero integration
(auto-import, `papio zotio …` commands) is disabled, ownership lookup treats
every work as new, and `papio doctor` reports zotio as
`not configured (optional)`. Hooks are then the only automatic hand-off —
papio acquires and validates; your hook files.
