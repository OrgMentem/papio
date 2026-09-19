# live-cohort — measuring what papio does unattended

`make live-cohort` measures papio's acquisition behaviour against a cohort of
real works, through the running daemon: real resolvers, real providers, the
real browser bridge, the operator's real institution.

It exists because nothing else in the tree can see a field failure. `papio
bench` is hermetic by design — ephemeral store, resolvers wired to `httptest`
fixtures, never the network — so it measures a resolver change as a delta with
nothing else moving. Every adapter test runs the classifier against a captured
static page. Between them they cannot observe a provider that changed, an
adapter that stopped matching, a queue that quiesced, or a candidate papio held
and never tried. On 2026-09-19 the live store showed the cost of that blind
spot: the last successful import was 2026-08-27, twenty-three days earlier, and
no automated check had noticed.

## Running it

```
make live-cohort                                   # the committed public cohort
make live-cohort COHORT=dev/scratch/backlog.json   # your own works
go run ./cmd/live-cohort -cohort <file> -force -budget 6m -out dev/scratch/after.txt
```

The daemon must be running; the tool refuses rather than autostarting one, so a
run always measures the daemon you meant. Flags worth knowing:

| flag | effect |
|---|---|
| `-force` | submit even when a live job already exists for the work. Without it such works are **skipped and disclosed** — measuring a job this run did not create measures your history, not papio's current behaviour. Required for a repeat run. |
| `-budget` | per-work settlement budget. A work still working at the budget is recorded `timed_out`, which is a result, not an error. |
| `-park-settle` | how long a job seen in `awaiting_human` or `needs_review` must stay there before the run believes it. Default 60s. Those states are **not terminal** — the daemon advances out of them, and recording the first sighting counts successes as human stops. See the note below. |
| `-keep` | keep every job the run created. By default a created job that produced no artifact is cancelled. |
| `-no-store` | skip the read-only store read that fills the untried-candidate column. |
| `-json` / `-out` | machine form, and write the report to a path as well as stdout. |

## Reading the report

Two numbers, in a fixed order, and the order is the discipline:

1. **WRONG ACCEPTS** — works papio filed an artifact for when the cohort's judge
   said it should not have. This is the primary, unconditional gate. A change
   that raises it does not ship whatever it did to throughput. A wrong paper
   under the right citation is the worst outcome papio has.
2. **autonomous ready** — works that finished with no human asked at all. This
   is the throughput number, and it is compared **only after** wrong accepts are
   flat or down.

Same discipline as `dev/identity-corpus.md`, one layer up, for the same reason:
the thresholds it protects were once tuned against a measurement nobody saved.

Three supporting sections carry the diagnosis:

- **WHY THEY STOPPED** groups every non-ready result by outcome plus its stop
  detail — the open action kind, the terminal reason, the last transition
  reason. `papio status` prints the same sentence for forty-five different
  jobs; a report that only counted states would reproduce that blindness at
  scale.
- **asked a human while holding an untried candidate** counts works that stopped
  for a person while at least one fetch candidate was still `pending` — never
  attempted. It is the direct measure of papio asking for help with work it had
  not finished itself. "Untried" excludes `retryable` and `invalid` (tried and
  failed) and `skipped` (passed over for a stated reason) on purpose: the
  column answers one narrow question and a looser count would answer a softer
  one nobody asked.
- **SKIPPED** lists works the run did not measure. Every skip is printed because
  it changes what each rate above is a rate *of*.

## The unattended rule

A run never answers a human action, never opens a handoff tab, and never drives
the browser. A job parked on a person is a **settled measurement**, recorded
with the action kind that parked it.

That is not a limitation, it is the measurement. papio's field problem is not
that humans answer slowly; it is how often papio needs one at all. Grading an
unattended run against a human-judged cohort is what turns "papio asks for too
much" from an impression into a count.

It also means a `ready_after_human_boundary` expectation is **met** by a job
that parks at the boundary. Reaching the boundary is the whole of what papio is
responsible for there.

### A parked sighting is confirmed, not believed

`awaiting_human` and `needs_review` are **parking** states, not terminal ones.
They release the job lease so the scheduler moves on, but the daemon can and
does advance out of them without any human — a browser route completes, a
sibling sign-in lands, a re-check finds a copy.

The first real backlog run recorded this the wrong way round.
`job_d0acd0940b8d3294c0d14281f8` was observed parked on an `openurl_handoff`
at 324 s and reached `ready` 40 s later, so the report counted a success as
part of the human wall it exists to measure. An instrument that misreports in
the direction of its own thesis is worse than no instrument.

So a parked observation is held for `-park-settle` (60 s by default) and
re-read before it is recorded; a terminal state is recorded immediately, since
nothing leaves one. The window does not *prove* a park is final — it bounds how
wrong the human-boundary counts can be, and the report prints the window it
used beside them for exactly that reason. Windows overlap, because one loop
drives every job, so confirming every park in a thirty-work cohort costs about
a minute overall rather than a minute per work.

## Side effects, stated plainly

A run submits **real jobs** to your daemon. It spends real source budget and
reaches real providers. Two guardrails, both deliberate:

- `auto_import` is always sent and always `false`, so no measurement reaches
  your library. It is sent explicitly rather than left to the daemon default,
  so the guarantee is not a configuration question.
- Cleanup cancels every job the run created that produced no artifact. A job
  that reached `ready` is **kept and named in the report**, and that is not a
  courtesy: `ready` is terminal, so `papio jobs cancel` refuses it
  (`was already ready; nothing to cancel`). papio has no verb that discards an
  acquired paper, so a measurement cannot undo one either. A public cohort
  acquires papers you never asked for and they stay in your ready queue
  permanently — read the report's `KEPT` line and decide what to do with them.

  As of 2026-09-19 there is **no verb that discards an acquired paper** —
  `papio jobs` offers `cancel`, `retry`, `refile` and no delete, and `cancel`
  refuses a terminal job. A `ready` job is a permanent queue entry. Two runs of
  the 18-work public cohort therefore left 18 papers in the ready queue and in
  `papio doctor`'s `uncollected_acquisitions` warning. Budget for that before
  running a public cohort repeatedly.

The untried-candidate column opens `papio.db` read-only. `mode=ro` makes the
driver refuse every write, but opening a WAL database for read still recreates
the `-wal`/`-shm` sidecars if the daemon has checkpointed and closed. Harmless,
and the next daemon open reuses them, but it is a visible effect of taking a
measurement.

## Cohort files

The format is `papio-bench-cohort/1` — the same schema `papio bench` reads, so
one cohort can be run hermetically and live. Strict decode and a closed
`expected_class` enum: a typo'd field fails the load rather than silently
running a weaker measurement than the file claims.

`dev/cohorts/open-access-v1.json` is the committed baseline. Every work is a
real, public paper, so the file belongs in the repository. Its judgements were
verified against the Unpaywall API and each entry's `source` records that.
**Re-verify before trusting an old run**: a work can go open access, and then
the expectation is wrong rather than papio being right.

A cohort drawn from your own backlog is the more useful measurement and must
**not** be committed — it names your reading. Keep it in `dev/scratch/`, which
is gitignored, the same rule `dev/identity-corpus.md` applies to its reports.
To build one, take parked works from your own store:

```
sqlite3 "file:$HOME/.local/share/papio/papio.db?mode=ro" \
  "SELECT json_extract(work, '\$.doi') FROM jobs WHERE state='awaiting_human' LIMIT 30;"
```

then judge each one by hand. The judgement is the expensive part and it is the
part that cannot be automated: `expected_class` records what a person believes
should happen, and a cohort that grades papio against papio's own opinion
measures nothing.

## Before and after, one increment at a time

```
go run ./cmd/live-cohort -cohort <file> -force -out dev/scratch/before.txt
# change exactly one thing
go run ./cmd/live-cohort -cohort <file> -force -out dev/scratch/after.txt
diff dev/scratch/before.txt dev/scratch/after.txt
```

Unlike `identity-corpus`, a live run is **not** reproducible: providers change,
sessions expire, rate limits bite, and the institution's holdings are not
yours to hold still. Read a one-work difference as noise and a shift across the
cohort as signal, and re-run a surprising result before believing it.

## Exit status

It is a measurement instrument, not a gate. It exits 0 once the run completes,
even with wrong accepts, because a caller comparing two runs needs both to exit
0 for the comparison to happen at all. It exits 1 only when the run could not be
taken: no cohort file, a malformed one, or no daemon.

## The first baseline, 2026-09-19

Recorded here because it is the number every later run is compared against, and
because the reason this tool exists is that the previous baseline was nobody's.

**`dev/cohorts/open-access-v1.json`** (18 works, run twice — 02:48Z and 03:10Z,
with identical verdicts):

```
WRONG ACCEPTS      0
autonomous ready   9 of 18   50.0%
expectation met    13 of 18   72.2%
```

**`dev/scratch/backlog-v1.json`** (26 works from this operator's own store,
judged against Unpaywall; this run predates the parked-state confirmation fix):

```
WRONG ACCEPTS      0
autonomous ready   0 of 26   0.0%
expectation met    17 of 26   65.4%
outcomes           human_boundary 22, unavailable 3, timed_out 1
```

Both runs report zero expectation-based wrong accepts. This metric checks
outcome classes; it does not inspect whether an acquired PDF is the right
document. Zero therefore does not establish artifact identity safety.
The backlog result also undercounts at least one completion: a recorded
handoff reached `ready` before the run ended.

Five public-cohort works marked open access still stopped for a person.
Open-access metadata establishes availability, not whether a particular
download route works in the current session.

The baseline exposed these failures:

1. **The Europe PMC fallback reached a PDF that the extension treated as
   HTML.** In the 03:10Z run, PNAS, OUP, JMIR, and MDPI produced `ui_changed`
   outcomes on `europepmc.org`. The initial report attributed those outcomes
   to a missing article adapter. Later inspection corrected that diagnosis:
   the retained captures are Chrome PDF shells at `/api/getPdf`, with no
   article metadata to classify. A native capture of the PNAS route and a
   desktop screenshot confirm an 11-page PDF at
   `/api/getPdf?pmcid=PMC8053968`. No `adapter_id` alone does not distinguish
   a missing adapter from a direct PDF that entered the wrong classifier.

2. **Honest failure is slow.** An unregistered DOI took 306–420 s to report
   `doi_not_registered`. One backlog work (`10.3316/informit.…`) never settled
   inside a 7-minute budget at all.

3. **Every stop looks the same to the operator.** 21 of 26 backlog works parked
   on `openurl_handoff` with identical guidance text. That is the wall
   `papio status` prints, reproduced under measurement.

One work exposed a defect in this tool rather than in papio, which is recorded
under "A parked sighting is confirmed, not believed" above.

## Track 2 measurement, 2026-09-19

Run `20260919T085928Z` uses the same 18-work public cohort, a 7-minute budget,
and a 60-second parked-state confirmation window:

```
WRONG ACCEPTS      0       (expectation-based, not a PDF identity audit)
autonomous ready   12 of 18   66.7%
expectation met    16 of 18   88.9%
outcomes           autonomous_ready 12, human_boundary 5, unavailable 1
```

The raw report is local at `dev/scratch/live-cohort-track2.json`.
The earlier public baseline records 9 autonomous completions. The three new
ones — PNAS, JMIR, and OUP — use direct Europe PMC HTTP downloads, not the
extension. Previously acquired works can also reuse local artifacts.
This comparison therefore does not isolate the effect of the browser change.

The MDPI work (`10.3390/ijerph17186469`) supplies the live browser proof.
It parks at 09:01:58Z, queues its Europe PMC handoff, then downloads at
09:05:08Z and reaches `ready` without intervention. The retained file has
13 pages and the requested DOI. The extension records a direct delivery from
`europepmc.org`; no provider outcome enters the HTML classifier.
This is later than the report's confirmed parked observation, so the raw
report still counts MDPI as a human boundary. Thirteen cohort jobs actually
hold PDFs by cleanup; only twelve meet the report's autonomous definition.

The run also exposes two cleanup defects in the instrument. A nil IPC result
destination reports `json: Unmarshal(nil)` after cancellation already commits.
Cancellation also succeeds without changing an already-ready job. Cleanup now
decodes the acknowledgement and reads the resulting state before reporting
either cancellation or a retained artifact. Four parked jobs in this run are
actually cancelled; MDPI remains ready. The raw report preserves the original
decoder errors rather than rewriting the measurement.

Separately, the original PLOS mismatch example reaches `ready` and yields the
correct five-page paper. Its new live attempt succeeds through Europe PMC
direct HTTP. Regression tests, rather than that live attempt, establish that a
wrong first candidate now yields to a correct second candidate. Tests also
cover exhaustion, quarantine integrity, cancellation, and atomic review
parking; reverting each relevant change makes its regression fail.

The proposed claim-expiry predicate change does not ship. A settled
institutional effect can acknowledge navigation while the PDF remains
unresolved. The existing expiry rule and its late-result regression correctly
protect that claim. Changing the rule to protect only occupying permits breaks
the regression and can allow duplicate work.

No measurement imports into Zotero. This Track 2 run plus the separate PLOS
probe leave fourteen additional ready jobs; papio still has no discard verb.
