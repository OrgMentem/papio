# Issue tracker: rumen findings ledger (+ `dev/plans/` for documents)

Two surfaces, split by shape.

| Artifact                                | Home                                             |
| --------------------------------------- | ------------------------------------------------ |
| A ticket: bug, task, chore, decision    | The rumen findings ledger, project id `papio`    |
| A spec or a wayfinder map              | `dev/plans/<feature-slug>/` in this repo         |

Documents hold the design and decision index. Findings hold tickets, native
relationships, and labels. Host-local claims live in rumen's state store.

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
rumen findings context <id>        # explicit edges, similar findings, anchors, verification, fix history
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
`--text`, `--since`, `--sort`, `--group-by`, `--view`, `--label`, `--blocks`,
`--blocked-by`, `--parent`. Default `--limit` is 50; inspect `truncated` before
treating a result count as a total.

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

`dev/plans/` is tracked, so specs and maps survive commits and remain visible
to agents in other worktrees. Claims are leases, not document fields.

- One feature per directory: `dev/plans/<feature-slug>/`
- The spec is `dev/plans/<feature-slug>/spec.md`
- A spec that spawns ledger tickets records each finding id beside the work item
  it covers, so the document points at the ledger.

`dev/active/` holds in-flight working notes and stays as it is; a new
multi-ticket effort gets a `dev/plans/<slug>/` directory instead.

### Wayfinding operations

Used by `/wayfinder`. Documents describe decisions; Findings hold tickets and coordination state.

- **Map**: keep Destination, Notes, Decisions so far, and unresolved questions in `dev/plans/<effort>/map.md`.
- **Effort root**: lodge a `task` Finding with `--source operator --dedupe-hint wayfinder:<effort>:root`.
  Add `wayfinder:map` and `wayfinder:<effort>` with `findings label`. Record its id in the map.
- **Child ticket**: lodge a `task` or `decision` Finding with a stable `--dedupe-hint wayfinder:<effort>:<ticket>`.
  Use `--input` for a JSON body. Include the question and links to retained evidence.
  Set its parent with `findings link <child> --parent <root>`.
  Add `wayfinder:<effort>` and one type label: `wayfinder:research`, `wayfinder:prototype`, `wayfinder:grilling`, or `wayfinder:task`.
- **Blocking**: `rumen findings link <dependent> --blocked-by <blocker>`.
  Use `findings unlink` with the same endpoints to remove that edge.
  Parent membership and blocking are separate relationships; create all Findings before wiring their edges.
  Keep the effort root open while a child is open; the vendor refuses to close it.
- **Frontier**: `rumen findings frontier --project papio --parent <root> --explain --json`.
  Use the first returned Finding. Do not sort by ticket number or reconstruct readiness from files.
  The CLI considers current blockers, deferrals, priority, and host-local claims.
  Missing evidence excludes affected work; `--explain` names the reason.
- **Claim**: `rumen findings claim <id> --project papio --holder <session> --pid <long-lived-pid> --ttl 10m --json`.
  Start only when `claimed` is true. A refusal can return exit 0 with `claimed:false`.
  Use the agent or shell PID, never the short-lived command PID.
  Renew with the same holder/PID and `--refresh`; release with `findings release <id> --holder <session>`.
  `findings claims --project papio --json` shows liveness and expiry.
  Claims coordinate only processes sharing one rumen state directory on one host.
  A close/reopen cycle invalidates a claim. Durable assignment and cross-host exclusion are not supported.
- **Resolve**: retain the answer in a document when it needs one, then close the Finding with the proper resolution class.
  Put the answer or its path in `--reason`. Add a named Finding reference to the map's Decisions so far.
  Use `rumen findings show <id>` to retrieve that reference; do not invent a web URL.

Do not create child-ticket files or store blocking and claim state in Markdown.
Do not use labels to emulate status, ownership, or priority. Only `user:*` and `wayfinder:*` labels accept operator writes.

## GitHub Issues and PRs

`github.com/OrgMentem/papio` has Issues enabled and they work, but they are not
the agent ticket surface (operator decision, 2026-09-19). Read an issue when a
user points at one; do not file agent work there.

**PRs as a request surface: no.** _(Set to `yes` if this repo starts treating
external PRs as feature requests; `/triage` reads this flag. `/triage` is not
installed today.)_ Renovate opens dependency PRs here; those are automation,
not requests.
