# Issue tracker: rumen findings ledger (+ `dev/plans/` for documents)

Two surfaces, split by shape.

| Artifact                                | Home                                             |
| --------------------------------------- | ------------------------------------------------ |
| A ticket: bug, task, chore, decision    | The rumen findings ledger, project id `papio`    |
| A spec, a wayfinder map, a child ticket | `dev/plans/<feature-slug>/` in this repo         |

The split exists because the ledger stores findings, not documents, and because
rumen exposes no dependency-link and no claim command, so a wayfinder frontier
cannot be computed from it.

## Hard rules

- **Never run `bd`.** `bd` is rumen's storage detail, not an interface. Direct
  use skips the resolution-class contract, the project and mission scoping, and
  the false-positive accounting that tunes model tiers. It also does not
  resolve from a repo checkout.
- **Never hand-edit** `~/.beads/**`, Dolt directories, rumen state, or SQLite
  files. rumen is the sole writer of durable state.
- **Read `skill://rumen` before the first ledger command**, then enumerate flags
  live with `rumen findings --help`. Do not guess flags.
- **Check current status before writing any batch or closing script.** Findings
  may already be closed, which makes the script a no-op.

## When a skill says "publish to the issue tracker"

```bash
rumen findings new --project papio --source operator \
  --type task --priority P2 \
  --title "<short imperative title>" \
  --body "<what and why>" \
  --path "internal/acquire/handoff.go:120-160#Adopt" \
  --suggested-validation "go test ./internal/acquire/..."
```

- `--source` is required: `operator`, `script`, or a configured harness id.
- `--type` is one of `bug`, `chore`, `task`, `decision`, `draft`, `suggestion`.
  Default is `bug`, so an implementation ticket must pass `--type task`.
- `--path` is `file[:startLine[-endLine]][#symbol]` and repeats.
- Other authoring flags: `--category`, `--impact`, `--root-cause`,
  `--fix-direction`, `--evidence` (repeatable), `--confidence`, `--dedupe-hint`.
- `--input <file|->` reads one finding object, or an array, as JSON, and cannot
  be combined with the authoring flags. Use it to lodge a batch.

Lodging dedupes by identity fingerprint, anchors to the current HEAD, and is
audited as a local CLI mutation.

## When a skill says "fetch the relevant ticket"

```bash
rumen findings show <id> --json
rumen findings context <id>        # anchors, verification commands, prior fix attempts, related findings
rumen findings show <id> --prompt  # the finding plus the exact commands that disposition it
```

## Browsing the queue

```bash
rumen findings query --project papio --status open --json
rumen findings query --project papio --priority P0 --json
rumen findings query --project papio --path internal/provider --json
rumen findings query --project papio --recurring --json   # zombies that keep resurfacing
```

Filters: `--category`, `--priority`, `--status`, `--resolution`, `--path`,
`--text`, `--since`, `--sort`, `--group-by`, `--view`. Default `--limit` is 50,
so a count of exactly 50 is a truncation, not a total.

`--json` returns an envelope, `{"findings": [...]}`, so narrow it with
`--select findings` before piping to `jq '.[]'`. Finding ids carry the project
id, for example `papio-6b042d71ede31381`.

## Closing

```bash
rumen findings close <id> --resolution <class> --reason "<evidence>"
```

`--resolution` is required. `rumen findings resolutions` lists the classes.

Closing a finding is a claim, not a dismissal. The class decides whether the
finding is suppressed or reopens on the next run, and it feeds the
false-positive rate that tunes model tiers, so a wrong class is worse than a
wrong reason.

- `fixed` — only when the correction is confirmed.
- `false_positive` — the claim was not true.
- `wont_fix` — real, deliberately not remediated.
- `duplicate` — requires `--duplicate-of <canonical-id>`.

A script that disposes of findings emits `rumen findings close`, never
`bd close`.

Other dispositions: `rumen findings defer --until <YYYY-MM-DD>`,
`rumen findings escalate`, `rumen findings set-priority`, `rumen findings split`.

## Documents under `dev/plans/`

`dev/plans/` is tracked, so specs, maps, and claims survive commits and are
visible to agents working in other worktrees.

- One feature per directory: `dev/plans/<feature-slug>/`
- The spec is `dev/plans/<feature-slug>/spec.md`
- A spec that spawns ledger tickets records each finding id beside the work item
  it covers, so the document points at the ledger.

`dev/active/` holds in-flight working notes and stays as it is; a new
multi-ticket effort gets a `dev/plans/<slug>/` directory instead.

### Wayfinding operations

Used by `/wayfinder`. The ledger cannot host these: rumen surfaces no
dependency link, no assignee, and no label on a finding, so blocking, claim,
and the `wayfinder:map` marker have nowhere to live. Beads has the underlying
capability; rumen deliberately does not expose it, and reaching past rumen to
`bd` is forbidden.

- **Map**: `dev/plans/<effort>/map.md` (the Notes / Decisions-so-far / Fog body).
- **Child ticket**: `dev/plans/<effort>/issues/NN-<slug>.md`, numbered from `01`,
  with the question in the body. A `Type:` line records the ticket type
  (`research`/`prototype`/`grilling`/`task`); a `Status:` line records
  `claimed`/`resolved`.
- **Blocking**: a `Blocked by: NN, NN` line near the top. A ticket is unblocked
  when every file it lists is `resolved`.
- **Frontier**: scan `dev/plans/<effort>/issues/` for files that are open,
  unblocked, and unclaimed; first by number wins.
- **Claim**: set `Status: claimed` and save before any work.
- **Resolve**: append the answer under an `## Answer` heading, set
  `Status: resolved`, then append a context pointer (gist + link) to the map's
  Decisions-so-far in `map.md`.

## GitHub Issues and PRs

`github.com/OrgMentem/papio` has Issues enabled and they work, but they are not
the agent ticket surface (operator decision, 2026-09-19). Read an issue when a
user points at one; do not file agent work there.

**PRs as a request surface: no.** _(Set to `yes` if this repo starts treating
external PRs as feature requests; `/triage` reads this flag. `/triage` is not
installed today.)_ Renovate opens dependency PRs here; those are automation,
not requests.
