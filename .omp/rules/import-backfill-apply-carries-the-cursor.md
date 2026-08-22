---
name: import-backfill-apply-carries-the-cursor
description: "Looping `import-backfill --apply` without carrying --cursor re-processes the same blocked head of the queue every time"
condition:
  - 'import-backfill(?![^\n]*--cursor)[^\n]*--apply'
scope: ["tool:bash"]
interruptMode: never
---

**`papio zotio import-backfill --apply` selects candidates oldest-first and does NOT skip jobs that failed last time. A block of failing jobs at the head of the queue is therefore re-selected on every invocation, so a loop without `--cursor` can run for half an hour and move nothing.**

Carry the `cursor` field from each `--json` result into the next call:

```sh
cur=""
for i in $(seq 1 12); do
  out=$(papio zotio import-backfill --include-not-requested --limit 10 --apply ${cur:+--cursor "$cur"} --json)
  echo "$out" | jq -c '.summary'
  cur=$(echo "$out" | jq -r '.cursor // ""')
  [ -z "$cur" ] && break
done
```

Measured: eight successive pages re-processed the same **ten** blocked jobs — 29 minutes, zero progress, and identical summaries that read like a stuck daemon rather than a paging mistake. Carrying the cursor drained the whole remaining queue in two pages.

Reading the result:

- `truncated: true` means more candidates exist past this page; `cursor` is where to resume.
- Repeated identical `{selected, failed}` across pages is the tell for this mistake.
- `already_in_library` counts reconciliations of papers the operator already has. `newly_filed: 0` with a high `already_in_library` is the queue correcting its bookkeeping, **not** an upload run — worth knowing before reporting that papers were filed.

A single bounded page without a cursor is fine, and a dry-run (no `--apply`) is free — this rule is about loops that apply.
