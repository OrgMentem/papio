# openalex-yield — measuring the yield of OpenAlex's title.search

`cmd/openalexyield` answers the question `dev/active/openalex-spend-remainders.md`
item 0 poses: of every credit spent on an OpenAlex title search (the fuzzy
sibling hop, and metadata enrichment), how many accepted artifacts can be
evidence-backed as coming from that search?

```
accepted artifacts attributable to OpenAlex title.search
--------------------------------------------------------
              title.search credits spent
```

This does **not** gate the egress fuse or the identity-authority work
(items 1+2 and 5) — those close crash, storage-failure, and wrong-identity
holes regardless of whether the fuzzy search is worth its price. It **does**
gate item 7 (the sibling hop's re-earn protocol): if the measurement says the
hop should be narrowed or removed, building that protocol first is sunk cost.

## Two halves, one strict boundary

- **The free half** (`go run ./cmd/openalexyield`, or `make openalex-yield`)
  reads your local papio store read-only. It makes **no provider requests**
  and spends **no credits**. It is always safe to run.
- **The paid half** (`-compare -confirm-spend`) spends real OpenAlex credits
  comparing three query shapes against a sample drawn from your own library.
  It refuses to run without `-confirm-spend`, and always prints the exact
  cost it is about to spend — shapes, sample size, credits — before issuing a
  single request, whether or not you confirm.

## The free half: a lower bound, never a computation

**Treat every number this tool prints as a lower-bound estimate, not a
computation.** Three structural facts make this true, and the report
discloses all three rather than hiding them inside a headline ratio:

1. **`job.sibling_search` is written post-wire.** The event that marks a
   completed sibling-hop search is written *after* the search itself, so a
   daemon crash between the two loses the marker even though the credits
   were already spent. The report's "job.sibling_search events lost to the
   post-wire write window" line estimates this from the gap between
   provably-completed search attempts and the events actually recorded.
2. **`FinishAttempt` is best-effort everywhere** in `internal/app/app.go`
   (`_ = s.Jobs.FinishAttempt(...)`). An attempt that started but never
   finished — a crash, a write failure — leaves a durable `attempts` row with
   no outcome at all. The report counts these (aged past ten minutes, so a
   genuinely in-flight call is never miscounted as lost) separately, because
   it cannot tell whether the missing outcome would have been a title search.
3. **"Attributable" needs winner/candidate provenance, and one call site
   doesn't have any.** The primary OpenAlex resolver lookup
   (`internal/resolvers/openalex.Resolver.Resolve`) can itself be a title
   search — when a job has no DOI or OpenAlex id yet — but the durable
   attempt it leaves ("candidates=N") is identical whether the underlying
   call used a DOI, an OpenAlex id, or a title. **This tool refuses to infer
   which from adjacent job state or timing** — that is exactly the kind of
   temporal-adjacency reasoning the plan warns against ("if the ledger does
   not record which candidate won, do not manufacture causation from a
   title-search attempt happening to precede a ready job"). Those wins are
   reported separately as "excluded primary-resolve title-search wins" and
   counted in **neither** the numerator nor the denominator, rather than
   counted on one side only (which would inflate the ratio).

What the free half **does** trust, because the evidence is unambiguous and
verified against the current source of `internal/app/app.go` and
`internal/resolvers/openalex/openalex.go`:

- **Sibling-hop searches**: `attempts` rows whose `detail` is exactly
  `"sibling_candidates=<N>"` — a format no other call site ever writes — and
  whose winning candidates are persisted at `IdentityConfidence` 0.6, a value
  no other call site ever assigns to an `openalex`-sourced candidate.
- **Enrichment searches**: `attempts` rows whose `detail` is one of exactly
  three literals `enrich()` writes on a completed, non-pre-wire outcome
  (`no_confident_match`, `metadata_conflict_rejected`, `metadata_enriched`).
  Their credits are counted in the denominator (a real, priced request went
  out), but their wins are **not** counted in the numerator: enrichment only
  ever adds an identifier, letting a *later* pass's exact-lookup candidate win
  at confidence 1.0 — indistinguishable, in the accepted candidate, from a job
  that had that identifier from the start.
- **Accepted-artifact provenance** is resolved with the same rule
  `internal/job.Store.CandidateForArtifact` uses (ADR-0007): a job's own
  `selected_candidate_id`, trusted only when it names *that job's own*
  `accepted` candidate (never a rejected selection carried forward by crash
  recovery), falling back to a content-hash scan for cache-completed jobs. A
  win the ledger cannot resolve either way is reported as **unattributable**,
  never guessed.

### Running it

```
go run ./cmd/openalexyield
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-store` | `<data_dir>/papio.db` | store path (read-only; the default is the same one `internal/config.Default().DataDir` resolves — see the caveat below) |
| `-since` | `0` (all history) | restrict to the last N, e.g. `-since 720h` |
| `-json` | off | emit the report as indented JSON instead of the rendered text report |

**`-store`'s default reads `config.Default().DataDir`, not your live
`config.toml`.** If you have relocated `data_dir`, pass `-store` explicitly —
the same caveat `dev/identity-corpus.md` documents for `papioDataDir`, for the
same reason: guessing the wrong location silently reads (or fails to find)
the wrong database rather than telling you.

The tool opens `papio.db` with SQLite's `mode=ro`, never runs a migration, and
never writes — it is safe to run while the daemon is live (WAL mode supports
any number of concurrent readers alongside the daemon's one writer).

## The paid half: opt-in, and it costs real credits

**The 2.3% headline number in the plan is the yield of one specific query
shape** — `?search=<title>&per_page=10`, OpenAlex's broad relevance search
(title, abstract, and full text) truncated at ten rows, filtered locally by
an exact-normalized-title test. An exactly-titled record ranked eleventh is a
paid miss under that shape. Before spending engineering effort rationing or
removing the fuzzy hop, compare it against two cheap variants, same cost per
call in every case (pricing is per request, not per row):

1. `search=<title>&per_page=100` — same broad relevance search, ten times the
   truncation headroom.
2. `filter=title.search:<title>` — a title-scoped filter (currently marked
   deprecated by OpenAlex, but still live), narrower than the bare `search=`.

```
go run ./cmd/openalexyield -compare -sample 25 -confirm-spend
```

Without `-confirm-spend`, the tool prints the cost preview and stops — no
request is made:

```
go run ./cmd/openalexyield -compare -sample 25
```

prints something like:

```
openalex-yield paid comparison — NOT YET RUN
query shapes to compare (3):
  - search=<title>&per_page=10 (current shape)
  - search=<title>&per_page=100
  - filter=title.search:<title> (deprecated, still live)
sample size:   25 titles drawn from your local library
cost per call: 10 credits (pricing is per request, identical across shapes)
total calls:   75 (3 shapes x 25 titles)
TOTAL CREDITS TO SPEND: 750

Re-run with --confirm-spend to actually spend these credits.
```

`-sample` is bounded (`openalexyield.MaxSample`, 200) even after you have
confirmed, so a mistyped sample size cannot spend an unbounded number of
credits on a single opt-in.

The comparison samples titles from your own `work_requests` table (the same
bibliography you have actually submitted to papio) rather than synthetic
examples, and issues every request through the same bounded,
non-redirect-following HTTP client construction every other OpenAlex
integration in this tree uses
(`fetch.MetadataTransport`/`fetch.NewSecureHTTPClientNoRedirect`, the same two
calls `internal/bootstrap.go`'s `mustOpenAlexClient` makes) — never a bare
`http.Client{}` of its own. It does **not** go through the daemon's
budget/sourcegate admission stack: this is a deliberate, once-off,
operator-confirmed sample outside the automated resolver loop, not part of
acquisition.

**Do not "improve" yield by loosening the acceptance predicate.** Whatever
these numbers say, the fix is to change the query shape or the sibling hop's
gating — never to accept a weaker title match. That trades papio's worst
outcome (the wrong PDF filed under the right citation) for a metric.

## Privacy

**The report names your own library.** Even though the free-half report is
aggregate counts (job and artifact counts, never titles or attachment keys),
those counts are still a partial census of your library's size and papio's
behavior against it, and the paid-half sample explicitly reads titles out of
your own submitted bibliography. **Never** paste a run — free or paid — into
a GitHub issue, a commit message, or `CHANGELOG.md`. If you need to share
evidence for a regression or a design decision, share the aggregate ratio and
counts by hand, not a captured report file.
