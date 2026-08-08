---
name: papio
description: "Use when the user wants to find, fetch, or file scholarly papers — even if they don't say \"papio\": search discovery backends, queue bounded acquisition jobs, validate that each PDF is the paper that was asked for, and hand finished PDFs to Zotero (through zotio, preview-first) or any other destination through a hook. Trigger phrases: `get me this paper`, `download this DOI`, `find papers on X and fetch the PDFs`, `fetch the PDFs for these references`, `why did this acquisition fail`, `check this PDF is the right paper`, `import these papers into Zotero`, `watch for new papers on X`, `use papio`."
license: "MIT"
compatibility: "Requires the papio binary on PATH (macOS, Linux, Windows) and a completed `papio init`. Poppler (`pdftotext`) is strongly recommended for PDF validation; Tesseract is optional (scanned PDFs). Publisher-gated downloads additionally need the papio browser extension in the user's ordinary browser. Zotero filing needs zotio."
argument-hint: "<command> [args] | install"
allowed-tools: "Read Bash"
metadata:
  author: "OrgMentem"
  openclaw:
    requires:
      bins:
        - papio
---

# papio — scholarly PDF acquisition CLI

> Full command tree: ask the CLI at runtime — `papio --help`, `papio <command> --help`.
> Installation, configuration, and the operational workflow live in `README.md` and the
> [docs site](https://orgmentem.github.io/papio/).

## Prerequisites: install the CLI

This skill drives the `papio` binary. **Verify the CLI is installed before invoking any
command from this skill.** If it is missing, install it first:

1. Install:
   ```bash
   brew install orgmentem/tap/papio           # macOS / Linux
   brew install poppler tesseract             # PDF validation + OCR
   ```
   Windows: `scoop bucket add orgmentem https://github.com/OrgMentem/scoop-bucket && scoop install papio`.
   Linux distro packages (`.deb`/`.rpm`/`.apk`) and signed archives are on the
   [releases page](https://github.com/OrgMentem/papio/releases). From source:
   `go build ./cmd/papio`.
2. Verify: `papio version`
3. First run: `papio init` (writes config, creates the database, installs the browser
   connector, then runs `doctor`). It is interactive; for an unattended profile use
   `papio init --non-interactive --email you@example.org --skip-browser`.
4. Confirm readiness: `papio doctor --json`

If `papio version` reports "command not found" after install, the install step did not
put the binary on `$PATH`. Do not proceed with skill commands until verification succeeds.

## When to use this CLI

Use papio when a paper has to actually arrive as a file: fetch the PDFs for a reading
list or an exported RIS/BibTeX file, chase one DOI, snowball citations from a seed paper,
backfill Zotero items missing attachments, or explain why a set of acquisitions failed.
It is a *broker*, not a scraper: it tries open-access repositories and licensed APIs
first, and reaches a publisher only through the user's own logged-in browser, one
explicit work at a time.

## Gotchas

Non-obvious facts that defy reasonable assumptions — read before running commands:

- **`awaiting_human` and `needs_review` are outcomes, not errors.** `--wait` stops at a
  terminal state *or* a human action. A settled job or batch report can be parked on a
  person; do not retry it, do not treat it as failure, and never turn it into an implicit
  success. Report it and say what the human must do.
- **Never loop `papio actions open`.** The browser is one serial surface, and draining
  the queue into tabs nobody asked for is the autonomous behaviour ADR-0009 refuses.
  Inspect with `papio actions list` (or `actions open --dry-run`), then open at most the
  one row the user chose (`--job` / `--action`).
- **`actions resolve --accept` is an assertion, not a heuristic.** It says a human (or
  you, having actually read the file at the quarantine path in the action detail)
  confirmed the PDF is the requested work; the daemon then imports *that* file and
  records `user_confirmed`. It applies only to an open `verify_identity` action, and it
  does not waive wrong-work, encrypted, or active-content rejection.
- **`zotio apply` is the only path that writes to Zotero, and it needs the exact digest**
  printed by `papio zotio plan`. Do not recompute, truncate, or reuse another plan's
  digest; a mismatch is a safe failure — make a new plan and read it.
- **A bare ten-digit argument is refused on purpose.** `papio acquire <arg>` guesses the
  scheme, and ten digits is simultaneously a valid ISBN-10 and a valid PMID. Pass
  `--pmid` or `--isbn` instead of arguing with the guesser. Every other entry point
  (`--batch`, MCP, the daemon) takes named fields and never guesses.
- **`unavailable` with `no_identifier` or `doi_not_registered` will not improve on
  retry.** No fetchable identifier (books, chapters, reports, theses) and unregistered
  DOIs settle unavailable *instead of* opening an institutional handoff. Find a real DOI
  and resubmit; retrying the same request burns nothing but time.
- **`--json` list payloads are enveloped**, always `{"<name>": [...], "truncated": bool}`,
  never a bare array; `truncated: true` means the row cap filled, not that more rows
  certainly exist. Single-record commands (`jobs get`, `doctor`, `status`,
  `batch report`, `zotio plan`, `inbox`) return the object directly with no `truncated`.
- **A batch caps at 50 works and skips what the user already has.** With zotio (or a
  configured `library.sources` authority) present, works already holding a PDF are
  skipped; `--include-owned` overrides. Re-running the same file is safe — normalization
  and dedupe are identical across JSONL/RIS/BibTeX/CSL-JSON/NBIB.
- **`search --new-only` filters *after* the backend applies `--limit`**, so it legitimately
  returns fewer rows than the limit.
- **Most commands autostart the daemon; `daemon status`, `daemon stop`, and `doctor` do
  not** — that is what makes `doctor` able to report the daemon as down instead of
  papering over it. There is no `daemon restart`: stop it and let the next command start
  the new one.
- **papio never logs in, accepts terms, solves CAPTCHAs, or automates MFA.** Those park
  as human actions by design, and `access_mode` decides how far it goes:
  `conservative` records an institutional route without opening it, `assisted` opens it
  for the user, `delegated` may drive one download on an already-authenticated, granted
  provider host. Do not propose working around any of it.
- **Failures cluster; look before digging.** `papio jobs failures --since 30d` groups by
  state, provider host, and terminal reason with a sample job id — start there, not at
  `jobs get` on one unlucky job.

## Hero capabilities

One line each; run `papio <command> --help` for the full flag set.

### Discovery

- **`search`** — Query configured discovery backends (OpenAlex, Semantic Scholar).
  `--limit`, `--year-from/--year-to`, `--oa-only`, `--new-only` (omit works already in the
  library), `--source`. Results carry `owned`, `match_score`, and `match_kind` in `--json`.
- **`search --cites/--cited-by/--related-to <doi>`** — Citation snowballing from a seed
  paper: forward citations, backward references, OpenAlex-related works.
- **`watch add|list|run|remove`** — Repeat a search on a schedule (`--cadence daily|weekly|Nh`,
  `--limit-per-run`), acquiring (`--mode acquire`) or just reporting (`--mode alert`).
  `--kind backfill` takes no query and queues Zotero items missing a PDF.
- **`watch digest <id>`** + **`acquire --from-digest <id>`** — Review what an alert watch
  found, then queue all of it or only `--keys <id>` rows.
- **`inbox` / `inbox counts` / `inbox decide <item-id> --op acquire|dismiss`** — The triage
  queue of reported-but-unacquired works.

### Acquisition

- **`acquire <identifier>`** — One work. Prefer explicit `--doi`/`--pmid`/`--arxiv`/`--isbn`/
  `--openalex`, or `--title` + repeatable `--author` + `--year`. `--wait` blocks until
  terminal or human action; `--label` records query context.
- **`acquire --batch <file>`** — Up to 50 works from JSONL, RIS, BibTeX, CSL-JSON, or
  MEDLINE/NBIB (`-` reads stdin). The reliable way to ingest a discovery interface's own
  export.
- **`acquire --from-zotio`** — Queue Zotero items that have no attached PDF (`--limit`).
  It refuses `--auto-import`, `--wait`, `--force`, `--resolver`, and every one-work
  identity flag; import policy for those jobs comes from config (`zotio.auto_import`).
- **Per-request policy** — `--source`/`--deny-source`, `--desired-version published|accepted|preprint|any`,
  `--max-cost`, `--access-mode`, `--resolver`, `--request-id` (idempotency key), `--force`.
- **`jobs list --state <state>` / `jobs get <id> [--wait]` / `jobs retry <id>` / `jobs cancel <id>`**
  — The job surface. `jobs receipt <id>` is the outcome plus component index.
- **`status`** — Dashboard grouped into working, awaiting-human, needs-review, ready,
  imported, failed/unavailable. `--follow` refreshes every 2s (interactive only — do not
  use it from an agent).
- **`activity --limit N [--job <id>]`** — Newest-first daemon event feed.

### Human-in-the-loop

- **`actions list`** — Open human actions, with the quarantine path for identity reviews,
  plus `age_seconds`, `stale`, and the `revision` that `actions dismiss --revision` wants.
- **`actions open [--dry-run] [--job <id>|--action <id>]`** — Hand a parked job to the
  user's ordinary browser. One row, chosen deliberately.
- **`actions resolve <action-id> --accept|--reject`** — Settle one identity review.
- **`actions dismiss <action-id> --revision <n>`** — Retire a stale advisory without
  touching its job.
- **`browser sessions` / `browser use <id>`** — Which connected browser holds the handoff
  flow, and switching it.

### Evidence & triage

- **`doctor`** — Config, daemon, connector, extension, zotio, adoption root. Run it first
  when anything is unexpectedly broken.
- **`jobs failures --since 30d` / `failures [--by-provider]`** — Where acquisitions cluster.
- **`artifacts validation <job-id>`** — Per-candidate evidence: payload gate, structural
  parse, text extraction, identity decision, kept *and* rejected candidates.
- **`artifacts get <job-id>` / `artifacts locate <job-id>`** — The validated artifact row,
  and where its bytes are on disk.
- **`adapter diagnose <job-id>`** — Sanitized support report for a provider interaction.
- **`stats`** — Lifetime totals by access basis.

### Output & filing

- **`zotio plan <job-id>...`** → **`zotio apply <plan-id> --confirm-sha256 <digest>`** — The
  preview-first Zotero write. `zotio preflight` checks the configured zotio;
  `zotio tags reconcile` converges the `papio:needs-action` / `papio:unavailable` ledger.
- **`bundle document <job-id>` / `bundle export <job-id> -o <dir>`** — The validated
  acquisition bundle, printed or written idempotently.
- **`export ledger|job|batch|watch`** — Normalized citations as CSL-JSON, RIS, or BibTeX
  (`--format`, `-o`; `-o` is required with `--json`).
- **`batch report <batch-id|latest> [--markdown]`** — Manifest joined with live outcomes:
  imported, browser-fetched-then-imported, existing-item-attached, acquired (ready, not
  imported), import-failed, awaiting-human, needs-review, failed, skipped-owned,
  in-progress.

## Canonical acquisition loop

Discovery, acquisition, observation, identity review, and Zotero writes are separate
steps on purpose. Do not collapse them.

```bash
# 1. Discover, and keep what the user actually wants.
papio search "appropriate reliance on AI" --limit 20 --year-from 2023 --new-only --json

# 2. Queue the selection (a file of works, or one identifier).
papio acquire --batch works.ris --label "reliance review" --collection "AI reading" --json
papio acquire --doi 10.1371/journal.pone.0262026 --wait --json

# 3. Observe. Settled includes parked-on-a-human.
papio batch report latest --json
papio jobs list --state needs_review --json

# 4. Identity reviews: read the quarantined file, then assert.
papio actions list --json
papio actions resolve <action-id> --accept        # or --reject

# 5. File into Zotero: preview, read it, then apply that exact preview.
papio zotio plan <job-id> --json
papio zotio apply <plan-id> --confirm-sha256 <digest-from-the-plan>
```

When acquisitions fail unexpectedly: `papio doctor --json` first, then
`papio jobs failures --since 7d --json` to see where they cluster.

## Recipes

### Fetch the PDFs for an exported reference list

```bash
papio acquire --batch export.ris --label "spring literature review" --json
papio batch report latest --markdown
```

RIS, BibTeX, CSL-JSON, JSONL, and NBIB are all accepted; owned works are skipped and
re-running the file creates no duplicates.

### Backfill Zotero items that have no PDF

```bash
papio acquire --from-zotio --limit 25 --json
papio zotio plan <job-id> --json
papio zotio apply <plan-id> --confirm-sha256 <digest-from-the-plan>
```

`--from-zotio` rejects `--auto-import` outright, so either set `zotio.auto_import` in
config or file the ready jobs with the plan/apply pass above. A batch report lists a
ready-but-unimported job as `acquired`.

### Explain a wall of failures

```bash
papio jobs failures --since 30d --json
papio artifacts validation <sample-job-id> --json
```

The first groups by state, provider host, and terminal reason; the second is the
per-candidate evidence for one job, rejected candidates included.

### Snowball from a seed paper, open access only

```bash
papio search --cited-by 10.1000/example --limit 25 --oa-only --json
```

### Watch a topic and decide later

```bash
papio watch add "appropriate reliance on AI" --cadence weekly --mode alert
papio watch digest <watch-id> --json
papio acquire --from-digest <watch-id> --keys 10.1000/example
```

### Export the acquisition ledger as BibTeX

```bash
papio export ledger --state ready --since 720h --format bibtex -o ready.bib
```

## Agent mode

There is no `--agent` flag: `--json` is the whole contract, and it is available on every
command.

- **Enveloped lists** — `{"<name>": [...], "truncated": bool}`; empty is `[]`, never `null`.
- **Single records** — `jobs get`, `doctor`, `status`, `batch report`, `zotio plan`,
  `inbox` return the object directly.
- **Same shape over MCP** — the `papio://…` resources return the identical envelope, so one
  parser serves both surfaces.
- **Exit status** — `0` on success; non-zero with a message on stderr otherwise. JSON goes
  to stdout.
- **Non-interactive** — every input is a flag. Avoid `status --follow` (it never returns)
  and `init` without `--non-interactive`.
- **Bounded by default** — list commands cap rows (`--limit`); raise it deliberately rather
  than assuming you saw everything.

## Where the PDFs go

A validated PDF lives in papio's content-addressed artifact store until it is filed.

- **Zotero** — `papio zotio plan` → read the preview → `papio zotio apply --confirm-sha256`.
  `acquire --auto-import` uses the same machinery automatically.
- **Anything else** — the `on_ready` hook in `config.toml` runs one command per ready job
  with `PAPIO_DOI`, `PAPIO_TITLE`, `PAPIO_PDF`, and `PAPIO_SHA256` in the environment:

  ```toml
  [hooks]
  on_ready = 'papis add --from doi "$PAPIO_DOI" "$PAPIO_PDF"'
  ```

  It is best effort: a hook failure is recorded and never fails or retries the job.

Zotero is optional — answer `none` at `papio init`'s zotio prompt and `papio doctor`
reports it as `not configured (optional)`.

## MCP server (only for MCP hosts)

Driving the CLI directly is the efficient path and the one this skill takes. When the
host speaks MCP rather than shell, papio serves the same surface over stdio:

```bash
claude mcp add papio -- papio mcp
```

It exposes a command facade derived from the CLI — `papio_command_search` to discover
commands, `papio_command_run` to run one — plus `papio_acquire_batch` and
`papio_batch_wait`, and the read resources `papio://jobs`, `papio://artifacts`,
`papio://bundles`, `papio://zotio/plans`, `papio://exports`. Every safety rule above
holds identically there. Full reference:
<https://orgmentem.github.io/papio/reference/mcp-tools/>.

## Argument parsing

Parse `$ARGUMENTS`:

1. **Empty, `help`, or `--help`** → show `papio --help` output.
2. **Starts with `install`** → see Prerequisites above.
3. **Anything else** → Direct use (execute as a CLI command with `--json`).

## Direct use

1. Check the binary: `papio version`. If missing, offer to install (Prerequisites above).
2. Match the request to a command from Hero capabilities; when unsure, read
   `papio <command> --help` rather than guessing a flag.
3. Execute with `--json` and parse the envelope.
4. Stop at every human gate — identity review, browser handoff, terms, Zotero apply — and
   tell the user exactly which one is open and what it is waiting for.
