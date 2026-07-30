# Filing: Zotero, papis & more

*papio*'s deep integration is [zotio](https://github.com/OrgMentem/zotio) →
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

- **Fires once per ready transition.** Import retries and daemon restarts do not re-fire `on_ready`.
- **Fire-and-forget, best effort.** A slow or failing hook never blocks or
  fails the job, but filing is not guaranteed. *papio* does not retry hooks.
- **Audited.** Each run records a durable `hook.on_ready` job event with
  `status`, `exit_code`, and `duration_ms`. Hook stdout/stderr is **not**
  recorded (it could carry secrets from your environment) — have your
  command do its own logging if you need output.
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
| `PAPIO_PMID` | PubMed ID | empty when absent |
| `PAPIO_TITLE` | requested title | empty when absent |
| `PAPIO_SHA256` | artifact content hash | yes |
| `PAPIO_PDF` | absolute path to the validated PDF | yes |
| `PAPIO_STATE` | `ready` | yes |

**Treat `PAPIO_PDF` as read-only.** It points into *papio*'s immutable
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

`zotio.executable` is optional — answer `none` at the zotio prompt in
`papio init`, run `papio init --zotio-path ""`, or clear the key in
`config.toml`. With it empty, the deep Zotero integration (auto-import,
`papio zotio …` commands) is disabled, ownership lookup treats every work as
new unless you configure a library source (below), and `papio doctor` reports
zotio as `not configured (optional)` instead of failing. Hooks are then the only
automatic hand-off — *papio* acquires and validates; your hook files.

## De-duplicating against a non-Zotero library

Point *papio* at an export of what you already hold and it stops re-acquiring it.
`papio search` marks those results `[in library]`, and `papio acquire --batch`
skips them.

```toml
[[library.sources]]
name   = "owned-pdfs"
kind   = "file"
path   = "~/library/with-pdfs.bib"
format = "bibtex"
claim  = "pdf_present"
```

`claim` is the whole contract, and it has no default:

- **`pdf_present`** — every entry is something whose full text you hold. A match
  skips acquisition.
- **`record_present`** — citations only. A match annotates `papio search` but
  never skips, because a citation without full text is exactly what you would
  want acquired.

*papio* does not read BibTeX `file` or papis `files` to guess which one applies:
that is per-manager convention, and a wrong guess would skip a paper you asked
for. Export the subset you mean, or declare `record_present`.

Matching is exact on identifiers represented by the source format. BibTeX
supports DOI, arXiv, and PMID; CSL-JSON and NBIB support DOI and PMID; RIS
supports DOI only. No format supports every identifier, and titles are never
matched. ISBN is excluded — an edited volume shares one ISBN with every
chapter in it, so one match would suppress twenty distinct requests. A `.bib`
also cannot say *which* manifestation it holds, so a source never satisfies an
explicit `--desired-version published` request.

Any tool that exports RIS, BibTeX, CSL-JSON, or MEDLINE/NBIB works — papis,
JabRef, Calibre, Mendeley, EndNote, a hand-kept `.bib`. There is no supported-app
list because there is nothing app-specific to support.

!!! warning "An unreadable source is not an empty library"
    If zotio is configured, generic `library.sources` are ignored. Otherwise,
    if a configured source cannot be read, *papio* refuses to guess.
    `--batch` creates **no jobs** and tells you which source failed. Generic
    `library.sources` are consulted by discovery **acquire** watches only;
    alert watches retain their historical zotio ownership path and do not consult
    generic sources. Before the fifth consecutive failure, each cadence
    attempts another run; a successful run resets the failure count. The fifth
    consecutive failure disables the watch; there is no re-enable command.
    After fixing the source, you may force-run it once with
    `papio watch run <id>`, but scheduled execution resumes only if you recreate
    the watch. `--include-owned` is available only for
    `papio acquire --batch`, meaning "proceed despite ownership uncertainty".
    `papio doctor` performs a fresh one-shot probe of each source and reports
    that read's record count and outcome; it does not report daemon cached age,
    count-collapse detection, or retained failure state.

