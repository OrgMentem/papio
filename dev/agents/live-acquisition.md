# Live acquisition measurement

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Acquisition success (the only instrument that sees the field)
- **`papio bench` is hermetic and therefore blind to every failure that matters;
  `make live-cohort` is the one that is not.** bench wires resolvers to `httptest`
  fixtures (`internal/bench/runner.go:overlayConfig`) so a resolver change shows as a
  delta with nothing else moving, and every adapter test runs the classifier against a
  captured static page. Between them nothing observes a provider that changed, an
  adapter that stopped matching, a queue that quiesced, or a candidate papio held and
  never tried. That gap has a measured cost: on 2026-09-19 the live store's last
  successful import was 2026-08-27, twenty-three days earlier, and no automated check
  had noticed. `cmd/live-cohort` submits a `papio-bench-cohort/1` cohort through the
  **running daemon** — real resolvers, real providers, real browser bridge — and grades
  what papio does with nobody watching. Read `dev/live-cohort.md` before running it: a
  run submits REAL jobs, and a job that reaches `ready` cannot be cleaned up afterwards
  because `ready` is terminal and `jobs.cancel` refuses it.
- **A run is UNATTENDED on purpose.** It never answers a human action or drives the
  browser. It confirms parked observations over `-park-settle` (60 seconds by default),
  but a queued browser route can still complete later. Cleanup re-reads the state after
  cancellation, which is a successful no-op for a terminal job. WRONG ACCEPTS is an
  expectation-class check, not a PDF identity audit; zero does not establish that every
  acquired file is the requested paper. Retained artifacts need separate inspection.
- **Unlike identity-corpus, a live run is not reproducible** — providers change, sessions
  expire, rate limits bite, holdings are not yours to hold still. Read a one-work
  difference as noise and a shift across the cohort as signal. A cohort built from your
  own backlog names your reading: keep it in `dev/scratch/`, never commit it. The
  committed `dev/cohorts/open-access-v1.json` is public works only, and its
  `expected_class` judgements were verified against Unpaywall on a stated date —
  re-verify before trusting an old run, because a work can go open access and then the
  expectation is wrong rather than papio being right.

