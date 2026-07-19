# Handoff: `--from-zotio` jobs can't be planned/exported — queue drops authors → bundle validation fails

**From:** papio session (reliability/adoption work), 2026-07-17
**To:** zotio integration dev (owns the `zotio.queue` RPC / `internal/zotio`)
**Severity:** P1 — blocks the entire `papio acquire --from-zotio` workflow (fill missing PDFs for existing Zotero items). New-item acquisition (`acquire --auto-import`) is unaffected.

> Diagnosis only — I did not edit `internal/zotio/*` (your uncommitted WIP was in the tree). The daemon I ran was rebuilt from a clean `HEAD` worktree, so everything below reproduces against **committed** code.

---

## TL;DR

`papio acquire --from-zotio` creates work-requests with **no authors** (only title/year/item-key are carried from the Zotero item). Every downstream step that needs a valid `AcquisitionBundle` then fails, because `AcquisitionBundle.Validate()` requires **1..100 authors** (`internal/protocol/protocol.go:315`). The failure surfaces as an opaque `internal: operation failed [unknown]` from `papio zotio plan`, and `operation failed` from `papio bundle export` — the real error (`identity.authors must have 1..100 entries`) is swallowed. The `attachments add` / `existing_item` route is a **red herring**: it's never reached, because `planJob` calls `Bundle.Export` first and that's what fails.

---

## Root cause chain

1. `acquire --from-zotio` → RPC `zotio.queue` (handled in `internal/zotio`, your area) reads Zotero items missing a PDF and creates a work-request per item. **It does not populate `authors`** (nor `year`). Confirmed in the DB:

   ```
   -- from-zotio work_requests:
   job_de2d97483c0d1e08dd804fd844  authors_json = NULL   title = "Eye movements in reading…"
   job_480d37478f1956dc53b19f7616  authors_json = NULL   title = "Do as AI say…"
   -- a normal `acquire --doi` work_request, for contrast:
   job_14e5f2e66f27ece612bfa8f5f9  authors_json = ["Shuai Ma","Ying Lei", … 7 authors]
   ```

2. Job runs fine, reaches `ready` with a valid artifact (identity `pass`, file present, candidate provenance present).

3. `Bundle.Export` (`internal/bundle/export.go:79-103`) builds `BundleIdentity.Authors = row.Work.Authors` (empty) then calls `b.Validate()`, which fails:

   ```go
   // internal/protocol/protocol.go:315
   if len(b.Identity.Authors) == 0 || len(b.Identity.Authors) > 100 {
       return fmt.Errorf("identity.authors must have 1..100 entries")
   }
   ```

   (Note: year is *not* the problem — `Year == 0` is explicitly allowed at L318. Only authors are mandatory.)

4. `zotio.planJob` (`internal/zotio/plan.go:126`) calls `Bundle.Export` at the very top (before the create/attach branch), so the export failure aborts the plan for **any** `--from-zotio` job, `existing_item` route or not.

---

## Reproduce

```
# FAILS (both a fresh browser download and a cached artifact):
papio zotio plan job_de2d97483c0d1e08dd804fd844      # -> internal: operation failed [unknown]
papio bundle export job_de2d97483c0d1e08dd804fd844 --output /tmp/x   # -> operation failed  (nothing written)

# WORKS (has authors):
papio zotio plan job_14e5f2e66f27ece612bfa8f5f9      # -> zplan_… manifest_create
papio bundle export job_14e5f2e66f27ece612bfa8f5f9 --output /tmp/y   # -> Exported /tmp/y/bundle.json
```

Isolation: nothing lands in the export dir on failure, so it aborts **before** `os.MkdirAll` (export.go:108) — i.e., in the checks/`Validate` block (L39-103), not in artifact materialization or the zotio CLI. Confirmed each precondition individually: state `ready` ✓, artifact present + `identity_result="pass"` ✓, `FindCandidateByArtifact` returns a candidate ✓ — leaving `b.Validate()` on empty authors as the only failing gate.

The Zotio CLI attach path is fine, for the record: `zotio --agent attachments add <key> <real.pdf> --mode linked-file` previews cleanly (`ok:true`). So this is **not** a `zotio attachments add` bug.

---

## Suggested fix

Primary (your area): **have `zotio.queue` carry the Zotero item's creators into the work-request `authors`** (and `year` while you're there). The item already has this metadata in Zotero — it's just being dropped when the queue builds the request. That alone unblocks the whole workflow.

Secondary / defense in depth (I can take these if you want, they're in papio-side code):
- **Enrichment gap**: these jobs have DOIs and went through `resolving`, yet `authors` stayed empty — the `fill_missing_only` metadata enrichment did not backfill authors (cached-artifact jobs like `480d` may skip enrichment entirely). Worth confirming enrichment fills authors by DOI so any authorless request self-heals.
- **Observability**: the real error is invisible in both places an operator looks —
  - `internal/api/handler.go` `zotioFailure()` (L613) returns only `{class, hint, httpStatus}`; unclassified → `class=unknown`, `hint=""`, so the CLI prints just `[unknown]` and drops the wrapped message.
  - the generic `failure()` (L599) returns a bare `operation failed`.
  - the daemon's stdout/stderr go to `/dev/null` via the autostarter (`internal/daemon/autostart.go:85`), so there's no server log either.
  Surfacing the wrapped error (at least in logs, or as a real error class/hint) would have made this a one-minute diagnosis instead of a spelunk.
- **Regression test**: no test exercises `Bundle.Export` for an authorless request; a `zotio.queue` test asserting queued requests carry authors would catch the source.

---

## Affected jobs — nothing lost

All `ready` with valid stored artifacts; they just need authors backfilled, then `zotio plan`+`apply` will attach. Re-runnable.

| Job | DOI | Zotero item key | authors |
|---|---|---|---|
| `job_480d37478f1956dc53b19f7616` | 10.1038/s41746-021-00385-9 | JND5SC9C | ∅ |
| `job_de2d97483c0d1e08dd804fd844` | 10.1037/0033-2909.124.3.372 | PKE66QVE | ∅ |
| `job_732e49b02458125f2dcdb0ecc4` | 10.1037/0021-9010.88.6.989 | U9S8PITR | ∅ |
| `job_8db83af36101f848daaaa131cc` | 10.1037/a0021320 | SZSGFWEH | ∅ |

(Two more from-zotio items are additionally parked on JSTOR's terms modal; they would hit this same authors wall once downloaded. Note a separate, papio-side extension bug also blocks their terms re-drive after consent — the `download_initiated` latch-on-attempt issue — but that's mine, tracked separately, not yours.)

---

## Environment
- papio daemon rebuilt from clean `HEAD` (`a71c38e`); my committed fixes only, **not** your uncommitted `internal/zotio/*` WIP.
- zotio: configured executable `~/@dev/zotio/bin/zotio` = **1.0.0** (preflight OK). (A Homebrew `zotio` 0.9.0 is also on `PATH`; papio uses the configured 1.0.0.)
- `[zotio] attachment_mode = 'linked-file'`.
- Your uncommitted WIP (`plan.go` +43 incl. `recordPlan` signature + reservation-in-progress error, `client.go` +12, `errors.go` +5) is in the plan/apply reservation area — related but downstream of this authors bug; fixing the queue's author population is the unblock.
