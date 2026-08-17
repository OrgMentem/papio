# Event-time candidate eligibility pool snapshots

Status: scoping only (2026-08-17). No implementation in this change.

Parent: `dev/active/candidate-binding-measurement.md` §5 (backlog replay arm).
That arm is **descriptive stress coverage** today because the pool a historical
grab faced at settlement is unrecoverable (`grab.Grab` at `internal/grab/grab.go:64-86`
persists id, host, title, state, quarantine, job, outcome — not the pool;
`attemptAutoBind` enumerates live eligibility at selection time at
`internal/browser/bridge.go:7644-7651`). This document scopes the smallest durable
change that makes **future** backlog replay calibration-grade.

## Decision: record at candidate-admission settlement, not only on bind

**Record one eligibility-pool snapshot for every grab that reaches the
DOI-less settlement branch in `processSettledGrab`, regardless of whether
autonomous binding runs or commits.**

Concretely: after validation, when `len(pdf.FrontMatterDOIs(report.Text.Excerpt)) == 0`
(`internal/browser/bridge.go:7573-7610`), immediately before
`MarkParkedNoIdentifier` (rule off or abstention) or `MarkBoundToJobFenced`
(successful bind). Do **not** wait for `autoBindDecisionEnabled` or a non-empty
`bind_provenance`.

### Why not “record only when the rule runs”

`autoBindDecisionEnabled` is `false` in every shipped build
(`internal/browser/bridge.go:7634-7635`; only tests set it). A bind-only recorder
would produce **almost no rows** until the rule ships — exactly when you want the
pre-ship distribution of real pool sizes and compositions. `bind_provenance` already
covers the **decision that committed** for binds (`internal/grab/grab.go:434-455`,
`bridge.go:7766-7803`) including in-transaction re-enumeration (`7690-7706`), but
it is NULL for every park and for every row predating the column. Bind-only storage
cannot reconstruct “what was on the table when this DOI-less grab parked.”

### Why not “every grab terminal settlement”

Grabs that terminate on the conclusive-DOI path (`createGrabJob`, `bridge.go:7612-7624`)
never enter `SelectAutoBindCandidate`. Snapshots there would measure a population
production never offers to the selector, inflate storage, and invite misuse as
calibration input. Failed validation and abandonment never reach identity
settlement — no snapshot.

### What “admission settlement” includes

| Terminal path | Snapshot? |
|---|---|
| DOI-less → `MarkParkedNoIdentifier` (rule off, abstain, or post-fence abstain) | **Yes** — this is the backlog-replay population |
| DOI-less → `MarkBoundToJobFenced` / auto-bind success | **Yes** — same admission gate; bind also keeps `bind_provenance` |
| Conclusive DOI → `MarkJobCreated` | No |
| `MarkFailedValidation`, `MarkAbandoned*` | No |

**Implementation implication while the rule is off:** the bridge must still call
`job.ListCandidateEligibleJobs` (or the `Tx` variant inside the settling
transaction) to build the snapshot even when it skips `attemptAutoBind`. That is
acceptable overhead: one extra read on the rare DOI-less path, only while the
operator has a non-empty manual-download queue.

### Which instant to freeze

Store the pool from **one** enumeration per grab:

- **Default (rule off or pre-bind):** the same enumeration `attemptAutoBind` would
  use first — `ListCandidateEligibleJobs` semantics
  (`internal/job/candidate_eligibility.go:169-195`), i.e. full pool, deterministic
  order (oldest open `manual_download` action, then `job_id`).
- **On successful bind:** the **in-transaction** enumeration inside
  `MarkBoundToJobFenced`'s `decide` callback (`bridge.go:7690-7706`) is the
  pool that actually committed with the fence. That can differ from the
  pre-transaction list. For calibration, the committed instant matters for binds;
  for parks, the pre-park instant matters.

Simplest consistent rule: **one row per grab**; on bind, write the in-tx pool
(the fence snapshot); on park without bind, write the pre-park enumeration. Tag
`phase` in the payload (`"pre_bind"` | `"fenced_commit"`) so analysts do not
compare unlike instants.

`bind_provenance` remains the audit of the **decision** (verdicts, excerpt hash,
rule version). The snapshot is the **eligibility enumeration**, including binds
that never happened.

## What to record

Mirror what `CandidateEligibleJob` carries at enumeration time
(`candidate_eligibility.go:160-167`, `213-274`):

| Field | Purpose |
|---|---|
| `job_id` | Stable key; same as `BindCandidate.Key` |
| Bibliographic snapshot | `work.Work` fields needed to re-run `SelectAutoBindCandidate` / backlog truth without reading live `jobs`/`work_requests` rows later |
| `bound_dois` | Same attachment as `BoundDOIs(anchor, work)` today (`candidate_eligibility.go:192-208`) — cite-side DOIs the predicate may see |

Envelope metadata (not per job):

- `schema`: `"eligibility_pool_snapshot/1"`
- `recorded_at`: UTC RFC3339Nano (same discipline as `store.Now()`)
- `phase`: `"pre_bind"` or `"fenced_commit"`
- `rule_enabled`: bool — value of `autoBindDecisionEnabled` at write time
- `auto_bind_attempted`: bool — whether `attemptAutoBind` ran
- `auto_bind_outcome`: `"bound"` \| `"abstained"` \| `"not_attempted"`
- `pool_size`: `len(entries)` — redundant but cheap for histograms
- `predicate`: frozen copy of `CandidateEligibleKind`, `CandidateEligibleStatus`,
  `StateAwaitingHuman` literals (`candidate_eligibility.go:15-22`) so a future
  predicate change does not silently rewrite history

Per-job entries (ordered exactly as returned by `queryCandidateEligibleJobs`):

- `job_id`
- `work`: `{title, authors, year, doi, pmid, arxiv, isbn, openalex}` — **no**
  `zotio_item_key`, **no** URLs of any kind
- `bound_dois`: normalized strings already produced by `BoundDOIs` (not raw
  submitted blobs)

Do **not** store: tab URL or host beyond what `pdf_grabs` already has, signed
CDN parameters, quarantine paths, excerpt text, or per-candidate gate reasons
(those belong in `bind_provenance` when a bind commits).

## Where it lives

**New table, one row per grab** — not a `pdf_grabs` column extension and not
merged into `bind_provenance`.

```sql
-- Migration 0039_eligibility_pool_snapshots.sql (number verified 2026-08-17)
CREATE TABLE pdf_grab_eligibility_snapshots (
  grab_id     TEXT PRIMARY KEY REFERENCES pdf_grabs(id) ON DELETE CASCADE,
  recorded_at TEXT NOT NULL,
  phase       TEXT NOT NULL CHECK (phase IN ('pre_bind','fenced_commit')),
  snapshot    TEXT NOT NULL  -- JSON, eligibility_pool_snapshot/1
);
CREATE INDEX pdf_grab_eligibility_snapshots_recorded
  ON pdf_grab_eligibility_snapshots(recorded_at);
```

**Why a table**

- `bind_provenance` is intentionally NULL unless an automatic bind committed
  (`0037_grab_bind_provenance.sql`, `grab.go:80-85`). Parks must have a sibling
  fact, not a fake provenance object.
- Keeps large JSON off the hot `pdf_grabs` row scans (`poll`, triage).
- `ON DELETE CASCADE` matches grab dismissal (`grab.Delete`) — no orphan
  measurement payload.
- One row per `grab_id` enforces “single event-time pool” and prevents unbounded
  append history.

**Write path:** same RW `store.Open` transaction as the terminal grab transition
(park or fenced bind). **Read path for measurement:** `SELECT snapshot FROM
pdf_grab_eligibility_snapshots WHERE grab_id = ?` on a `mode=ro` handle inside
`BEGIN READ ONLY` — never `store.Open` on an operator directory
(`identitycorpus/backlog.go:54-61`, `store.go:44-57`).

**Non-mutation invariant:** measurement and report tools must not apply
migrations or write snapshots. Only the daemon’s normal grab settlement path
writes. Pointing measurement at a live DB continues to use
`job.ListCandidateEligibleJobsTx` on a read-only transaction for **present-day**
arms; historical replay joins settled DOI-less grabs to this table instead of
re-enumerating eligibility.

## Migration test pins (implementer checklist)

Latest embedded migration today: **`0038`** (`internal/store/migrations/0038_pdf_grabs_abandoned_state.sql`).
Adding **`0039`** bumps `PRAGMA user_version` to **39** and **will fail CI** until
these four locations are updated (same discipline as ADR-0017):

| File | What to change |
|---|---|
| `internal/cli/clean_install_test.go` | `"schema version 38"` in doctor check (~line 102) **and** `want 38` (~line 131) → **39** |
| `internal/doctor/doctor_test.go` | `"schema version 38"` (~line 78) → **39** |
| `internal/store/migrate_guard_test.go` | future-version refusal strings that assume latest **38** → **39** |
| `internal/store/migrate_forward_test.go` | only if a new forward fixture is added; existing pins at schema **33** are intentional fixtures — do not bump unless adding a 38→39 repair test |

Also update `AGENTS.md` / changelog schema mentions if the repo convention
requires it when shipping the migration.

## Privacy, retention, exposure

This records **what papio was considering filing** — internal `job_id` values
and bibliographic keys from the operator’s own acquisition backlog. It is more
sensitive than aggregate counts and less sensitive than storing PDF text (explicitly
forbidden; excerpt identity stays `excerpt_sha256` in provenance only).

**Retention**

- Lifetime tied to the parent `pdf_grabs` row; cascade delete on grab dismissal.
- No separate TTL in v1. Parked grabs may sit indefinitely — that is existing
  triage semantics, not new data class.
- Backup/export treats this table like `pdf_grabs` and `jobs`: operator-local
  SQLite, not telemetry.

**Never persist**

- Full tab URLs, query strings, or signed CDN URLs (bearer credentials; same rule
  as `0025_pdf_grabs.sql` host-only discipline).
- `zotio_item_key` in the snapshot JSON (correlates to external library rows;
  not needed to replay the selector).

**Exposure rules (default: most conservative that still serves measurement)**

| Surface | Default |
|---|---|
| Browser extension / `pdf_grab_result` | **Never** — wire vocabulary unchanged |
| `papio pulse` / daemon info logs | **Never** — log pool **size** only if needed for debugging, not job ids or titles |
| `papio doctor` / `--json` health | **Never** — schema version only |
| MCP resources | **Never** unless a future explicit, opt-in export tool is added |
| `identity-corpus` / backlog replay | **Yes** — read via `mode=ro` join; documented in runbook privacy section |
| Operator CLI | No new default command; optional hidden `papio debug grab-pool-snapshot <id>` acceptable later |

Aggregates for dashboards (pool size histograms) may be computed offline from
exports; they must not become daemon telemetry by default.

## Size estimate and bounds

**Per grab (one snapshot row):**

- Let `N` = `pool_size` (today’s operator backlog is on the order of tens; the
  measurement plan quotes ~27 as an operator-run figure with no repo pin — treat
  as illustrative).
- Per entry ≈ 150–600 bytes JSON (`job_id`, short title, author list, identifiers,
  `bound_dois`).
- **Typical:** N=30 → ~12–20 KiB raw JSON.
- **Stress:** N=200 → ~80–120 KiB.

**Growth rate** [INFERENCE]: bounded by DOI-less grab rate × typical N. A heavy
manual-download queue with frequent Send PDF on DOI-less PDFs might produce tens
of snapshots per month → sub-megabyte per year at N≈30. Not a scaling risk
relative to artifact storage.

**Enforcement (if unbounded queue):**

Production already passes **the entire** eligible list to the selector with no cap
(`bridge.go:7653-7660`, `identitycorpus/backlog.go:227-234`). Do not silently
truncate for measurement.

1. **Soft guard:** if `len(snapshot)` > **512 KiB**, fail the grab settlement
   transaction with a structured error and leave the grab non-terminal — same
   severity as any other settlement failure. This should be unreachable for
   normal queues; it bounds worst-case DB growth.
2. **Operational guard:** if `pool_size` > **1000**, emit a single daemon metric
   / log line (`pool_size`, `grab_id` only) so operators know the queue is
   abnormal; still store the full enumeration unless the byte guard trips.

No sampling, no “top-N by recency” — truncation would bias calibration toward
recent jobs and destroy time-weighted replay.

## What this does **not** buy (read this before treating as unblocking)

- **No retroactive calibration.** Every grab that already settled without a
  snapshot — essentially all production history — remains **descriptive-only** in
  the backlog arm. `BacklogCaveat` in `identitycorpus/backlog.go:23-38` stays
  true for that population forever.
- **Does not enable choosing a pool cap from today’s data alone.** Present-day
  `ListCandidateEligibleJobsTx` on a read-only handle is still one instant of one
  queue, not a time-weighted distribution over past grabs.
- **Does not replace synthetic arms.** Conjunction and composite arms in the
  measurement plan still gate the rule; snapshots only fix the **backlog replay**
  estimand for grabs settled **after** deployment.
- **Does not turn the rule on.** Recording is compatible with
  `autoBindDecisionEnabled == false` and is valuable precisely because the rule
  is off during the observation window.

Earliest payoff: after months of normal use, backlog replay can join
`pdf_grabs` (DOI-less parks and binds) to `pdf_grab_eligibility_snapshots` and
report wrong-bind / abstention rates against **the pool that actually existed at
each settlement**, with cluster structure by grab rather than a single live
enumeration.

## Implementation sketch (out of scope here)

1. Add `0039` migration and `internal/grab` helper
   `RecordEligibilitySnapshotTx(ctx, tx, grabID, phase, payload)`.
2. In `processSettledGrab` DOI-less branch: open settlement tx, enumerate via
   `job.ListCandidateEligibleJobsTx`, insert snapshot, then park or call existing
   bind path (bind path’s inner tx already holds fence enumeration — merge or
   nest carefully with single-writer SQLite).
3. Extend `identitycorpus.BuildBacklogArm` to prefer per-grab snapshots when
   present; keep live enumeration fallback with `BacklogCaveat` when absent.
4. Tests: settlement writes snapshot with rule disabled; cascade on delete; ro
   reader never migrates; byte guard if added.

No changes to `internal/pdf/identity.go` or `internal/pdf/candidate_select.go`
in this slice.
