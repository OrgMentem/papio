// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package openalexyield measures the yield of OpenAlex's title.search query
// shape against the local papio store: how many accepted artifacts can be
// evidence-backed as coming from a title search, per credit spent on title
// searches. It is the free half of dev/openalex-yield.md's item 0 — no
// provider requests, read-only against the operator's own database.
//
// # Why "lower bound", not a computation
//
// Two structural facts make every number here an undercount, never an
// overcount, of what actually happened:
//
//   - job.sibling_search (internal/app/app.go) is written AFTER the search
//     that earns it, so a crash between the two loses the marker even though
//     the credits were already spent.
//   - internal/job.Store.FinishAttempt is called with its error discarded
//     ("_ = s.Jobs.FinishAttempt(...)") at every call site in
//     internal/app/app.go, so a write failure or a crash before it commits
//     silently loses an attempt's outcome.
//
// Measure reports both loss classes explicitly rather than folding them into
// the headline numbers, and reports a third, structural gap: OpenAlex's
// PRIMARY resolver lookup (internal/resolvers/openalex.Resolver.Resolve) can
// itself be a title search — when a job has no DOI or OpenAlex id yet — but
// the durable attempts row it leaves ("candidates=N") is byte-for-byte
// identical whether that lookup used a DOI, an OpenAlex id, or a title, so
// this package cannot tell those apart without inferring from adjacent job
// state at attempt time. This package refuses to do that (see
// EvidenceUnattributable below) and instead excludes that call site from both
// the numerator and the denominator, reporting how many accepted artifacts it
// had to exclude on that basis so the exclusion is visible, not silent.
//
// # What is durably, unambiguously attributable
//
// Only two call sites leave evidence this package trusts:
//
//  1. The fuzzy sibling hop (Service.resolveSiblings): every sibling-hop
//     OpenAlex search that reaches the wire and returns writes an attempts
//     row whose detail is exactly "sibling_candidates=<N>" — a format no
//     other call site ever produces. Every candidate that search hands back
//     is persisted with IdentityConfidence 0.6 (openalex.go's
//     ResolveSiblings), also a value no other call site ever assigns to an
//     openalex-sourced candidate. Two independent, code-verified markers
//     that never collide with anything else in the store.
//  2. Metadata enrichment (Service.enrich, the OpenAlex entry): every
//     completed enrichment search writes an attempts row whose detail is one
//     of exactly three literals — "no_confident_match",
//     "metadata_conflict_rejected", "metadata_enriched" — none of which any
//     other call site ever writes.
//
// Enrichment credits are counted in the denominator (a real, priced request
// was made) but its wins are NOT counted in the numerator: enrichment only
// ever fills in an identifier, which lets a LATER pass's exact DOI/OpenAlex-id
// lookup win at IdentityConfidence 1.0 — indistinguishable, in the accepted
// candidate, from a job that had that identifier from the start. See
// EvidenceUnattributable.
package openalexyield

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CreditsPerTitleSearch is OpenAlex's measured per-call price for a search
// (as opposed to a singleton entity GET, priced at 1): see
// dev/active/openalex-spend-remainders.md, "OpenAlex prices GET /works/{doi}
// at 1 credit and filter=title.search: at 10 (x-ratelimit-credits-used,
// measured live)". Pricing is per request, not per row, so this constant
// applies identically to every query shape item 0's paid half compares.
const CreditsPerTitleSearch = 10

// siblingHopConfidence and primaryTitleSearchConfidence are the exact
// IdentityConfidence values internal/resolvers/openalex.go assigns to a
// title-search-sourced candidate: 0.6 for every sibling-hop candidate
// (ResolveSiblings), 0.75 for the primary resolver's own title-search branch
// (Resolve, when search==true). An exact DOI/OpenAlex-id lookup is 1.0. A
// small epsilon absorbs float64 round-trip noise through SQLite's REAL
// storage class without risking a false match against another source's
// confidence value (verified against every resolver's IdentityConfidence
// literal: no other adapter ever emits exactly 0.6 or 0.75, and this package
// additionally requires candidates.source = 'openalex').
const (
	siblingHopConfidence         = 0.6
	primaryTitleSearchConfidence = 0.75
	confidenceEpsilon            = 0.01
)

// finishAttemptInFlightGrace bounds how old an unfinished attempts row must be
// before it is counted as lost rather than merely still running. OpenAlex
// requests are bounded well under a minute by the daemon's own transport and
// admission timeouts; ten minutes is a wide margin against false positives
// from a genuinely in-flight call at the instant this tool runs.
const finishAttemptInFlightGrace = 10 * time.Minute

// FreeReport is the result of the free half: a lower-bound estimate, never a
// computation, of accepted artifacts attributable to OpenAlex title.search
// per title.search credit spent, read entirely from the local store.
type FreeReport struct {
	// Since is the window's inclusive start, RFC3339, or "" for all history.
	Since string
	// Until is when this report was generated, RFC3339.
	Until string

	// SiblingHopSearches is the count of completed fuzzy sibling-hop OpenAlex
	// title searches (attempts.detail = "sibling_candidates=<N>").
	SiblingHopSearches int
	// EnrichmentSearches is the count of completed OpenAlex metadata
	// enrichment title searches (attempts.detail one of the three literals
	// enrich() writes on a finished, non-pre-wire outcome).
	EnrichmentSearches int
	// TitleSearchCredits is (SiblingHopSearches + EnrichmentSearches) *
	// CreditsPerTitleSearch — the denominator. It deliberately excludes the
	// primary resolver's own title-search branch; see
	// ExcludedPrimaryResolveWins.
	TitleSearchCredits int

	// SiblingHopAttributedWins is the numerator: accepted artifacts (job's
	// main component) whose winning candidate is durably, unambiguously
	// evidence-backed as sibling-hop-sourced (source=openalex,
	// IdentityConfidence≈0.6).
	SiblingHopAttributedWins int
	// UnattributableAccepted counts accepted artifacts whose winning
	// candidate could not be established from the ledger at all — the ledger
	// simply does not record which candidate won. Reported rather than
	// guessed, per this package's evidence-only rule.
	UnattributableAccepted int
	// TotalAcceptedArtifacts is every accepted artifact (job_artifacts,
	// role='main') in the window, for context.
	TotalAcceptedArtifacts int
	// ExcludedPrimaryResolveWins counts accepted artifacts whose winning
	// candidate is openalex-sourced at IdentityConfidence≈0.75 — a title
	// search performed by the PRIMARY resolver lookup, not the sibling hop.
	// These are real title-search wins, but their corresponding SPEND cannot
	// be isolated in the attempts ledger (see the package doc comment), so
	// counting the win without its cost would inflate the yield estimate.
	// Disclosed here, counted in neither the numerator nor the denominator.
	ExcludedPrimaryResolveWins int

	// LostSiblingSearchEvents estimates how many completed sibling-hop
	// searches (proven by their attempts row) never got a corresponding
	// job.sibling_search event — the post-wire write-loss window the package
	// doc comment describes. Never negative; a shortfall the other direction
	// (more events than provably-completed attempts, which the code cannot
	// produce) is clamped to zero rather than reported as negative loss.
	LostSiblingSearchEvents int
	// LostToFinishAttempt counts OpenAlex resolve-stage attempts that were
	// started (StartAttempt) but never finished (FinishAttempt), aged past
	// finishAttemptInFlightGrace so a genuinely in-flight call is not
	// misclassified. These may or may not have been title searches — the
	// lost outcome is exactly what would have said so.
	LostToFinishAttempt int
	// AmbiguousResolveAttempts counts OpenAlex resolve-stage attempts that DID
	// finish (retryable or failed) but whose detail is the generic error-type
	// string every resolve call site writes on failure, so this package
	// cannot tell whether the underlying request was a title search, a DOI
	// lookup, or an OpenAlex-id lookup. Disclosed, never counted as spend.
	AmbiguousResolveAttempts int

	// YieldLowerBound is SiblingHopAttributedWins / TitleSearchCredits. Zero
	// and meaningless when HasCredits is false.
	YieldLowerBound float64
	// HasCredits reports whether TitleSearchCredits > 0; when false,
	// YieldLowerBound carries no information (avoids a division by zero
	// silently reading as "0% yield").
	HasCredits bool
}

// Measure reads db (opened read-only by the caller) and computes a FreeReport
// over [since, now]. A zero since means all history. Measure makes no writes
// and no provider requests.
func Measure(ctx context.Context, db *sql.DB, since time.Time) (FreeReport, error) {
	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.UTC().Format(time.RFC3339Nano)
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	inFlightCutoff := now.Add(-finishAttemptInFlightGrace).Format(time.RFC3339Nano)

	report := FreeReport{Since: sinceStr, Until: nowStr}

	siblingSearches, err := scalarCount(ctx, db, `
		SELECT COUNT(*) FROM attempts
		 WHERE source = 'openalex' AND stage = 'resolve' AND outcome = 'success'
		   AND detail LIKE 'sibling_candidates=%'
		   AND started_at >= ?`, sinceStr)
	if err != nil {
		return FreeReport{}, fmt.Errorf("counting sibling-hop searches: %w", err)
	}
	report.SiblingHopSearches = siblingSearches

	enrichmentSearches, err := scalarCount(ctx, db, `
		SELECT COUNT(*) FROM attempts
		 WHERE source = 'openalex' AND stage = 'resolve' AND outcome = 'success'
		   AND detail IN ('no_confident_match', 'metadata_conflict_rejected', 'metadata_enriched')
		   AND started_at >= ?`, sinceStr)
	if err != nil {
		return FreeReport{}, fmt.Errorf("counting enrichment searches: %w", err)
	}
	report.EnrichmentSearches = enrichmentSearches

	report.TitleSearchCredits = (report.SiblingHopSearches + report.EnrichmentSearches) * CreditsPerTitleSearch
	report.HasCredits = report.TitleSearchCredits > 0

	siblingSearchEvents, err := scalarCount(ctx, db, `
		SELECT COUNT(*) FROM events WHERE kind = 'job.sibling_search' AND at >= ?`, sinceStr)
	if err != nil {
		return FreeReport{}, fmt.Errorf("counting job.sibling_search events: %w", err)
	}
	if lost := report.SiblingHopSearches - siblingSearchEvents; lost > 0 {
		report.LostSiblingSearchEvents = lost
	}

	lostToFinish, err := scalarCount(ctx, db, `
		SELECT COUNT(*) FROM attempts
		 WHERE source = 'openalex' AND stage = 'resolve' AND ended_at IS NULL
		   AND started_at >= ? AND started_at <= ?`, sinceStr, inFlightCutoff)
	if err != nil {
		return FreeReport{}, fmt.Errorf("counting unfinished openalex attempts: %w", err)
	}
	report.LostToFinishAttempt = lostToFinish

	ambiguous, err := scalarCount(ctx, db, `
		SELECT COUNT(*) FROM attempts
		 WHERE source = 'openalex' AND stage = 'resolve' AND outcome IN ('retryable', 'failed')
		   AND started_at >= ?`, sinceStr)
	if err != nil {
		return FreeReport{}, fmt.Errorf("counting ambiguous openalex attempts: %w", err)
	}
	report.AmbiguousResolveAttempts = ambiguous

	if err := measureAcceptedArtifacts(ctx, db, sinceStr, &report); err != nil {
		return FreeReport{}, err
	}

	if report.HasCredits {
		report.YieldLowerBound = float64(report.SiblingHopAttributedWins) / float64(report.TitleSearchCredits)
	}
	return report, nil
}

// measureAcceptedArtifacts walks every accepted main artifact in the window
// and classifies its winning candidate's provenance, mirroring
// internal/job.Store.CandidateForArtifact's ADR-0007 rule: the job's own
// selected_candidate_id is trusted only when it points at THIS job's own
// 'accepted' candidate; otherwise a content-hash fallback scan (also gated on
// 'accepted') recovers a cache-completed job's source acquisition. A win with
// neither is unattributable, per this package's evidence-only rule — never
// guessed from timing.
func measureAcceptedArtifacts(ctx context.Context, db *sql.DB, sinceStr string, report *FreeReport) error {
	rows, err := db.QueryContext(ctx, `
		SELECT job_id, artifact_sha256 FROM job_artifacts
		 WHERE role = 'main' AND created_at >= ?`, sinceStr)
	if err != nil {
		return fmt.Errorf("listing accepted main artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type accepted struct{ jobID, sha string }
	var artifacts []accepted
	for rows.Next() {
		var a accepted
		if err := rows.Scan(&a.jobID, &a.sha); err != nil {
			return fmt.Errorf("scanning accepted main artifact: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing accepted main artifacts: %w", err)
	}

	report.TotalAcceptedArtifacts = len(artifacts)
	for _, a := range artifacts {
		source, confidence, found, err := winningCandidate(ctx, db, a.jobID, a.sha)
		if err != nil {
			return fmt.Errorf("resolving winning candidate for job %s: %w", a.jobID, err)
		}
		switch {
		case !found:
			report.UnattributableAccepted++
		case source == "openalex" && withinEpsilon(confidence, siblingHopConfidence):
			report.SiblingHopAttributedWins++
		case source == "openalex" && withinEpsilon(confidence, primaryTitleSearchConfidence):
			report.ExcludedPrimaryResolveWins++
		}
	}
	return nil
}

// winningCandidate resolves one job's accepted-artifact provenance using
// exactly internal/job.Store.CandidateForArtifact's two-step rule (own
// selection first, content-hash fallback second, both gated on
// status='accepted'). Reimplemented here rather than imported because that
// method lives on internal/job.Store, which requires the write-capable
// store.Store this package deliberately never constructs — see the package
// doc comment above ("Why the store is opened read-only" reasoning) and
// openReadOnly in store.go.
func winningCandidate(ctx context.Context, db *sql.DB, jobID, sha string) (source string, confidence float64, found bool, err error) {
	var ownCandidate sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT selected_candidate_id FROM jobs WHERE id = ?`, jobID).
		Scan(&ownCandidate); err != nil && err != sql.ErrNoRows {
		return "", 0, false, err
	}
	if ownCandidate.Valid {
		var s string
		var c float64
		err := db.QueryRowContext(ctx, `
			SELECT source, identity_confidence FROM candidates
			 WHERE id = ? AND job_id = ? AND status = 'accepted'`, ownCandidate.Int64, jobID).Scan(&s, &c)
		if err == nil {
			return s, c, true, nil
		}
		if err != sql.ErrNoRows {
			return "", 0, false, err
		}
	}
	var fallbackID sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT j.selected_candidate_id FROM jobs j
		JOIN candidates c ON c.id = j.selected_candidate_id
		WHERE j.artifact_sha256 = ? AND j.state IN ('ready', 'imported') AND c.status = 'accepted'
		ORDER BY j.updated_at ASC LIMIT 1`, sha).Scan(&fallbackID)
	if err == sql.ErrNoRows || !fallbackID.Valid {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	var s string
	var c float64
	if err := db.QueryRowContext(ctx, `
		SELECT source, identity_confidence FROM candidates WHERE id = ?`, fallbackID.Int64).Scan(&s, &c); err != nil {
		if err == sql.ErrNoRows {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	return s, c, true, nil
}

func withinEpsilon(value, target float64) bool {
	diff := value - target
	if diff < 0 {
		diff = -diff
	}
	return diff <= confidenceEpsilon
}

func scalarCount(ctx context.Context, db *sql.DB, query string, args ...any) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Render renders the report as aligned plain text, matching the ratio shape
// dev/active/openalex-spend-remainders.md item 0 specifies.
func (r FreeReport) Render() string {
	var b strings.Builder
	window := "all history"
	if r.Since != "" {
		window = r.Since + " .. " + r.Until
	}
	fmt.Fprintf(&b, "openalex-yield report — LOWER-BOUND ESTIMATE, not a computation (see dev/openalex-yield.md)\n")
	fmt.Fprintf(&b, "window: %s\n\n", window)

	fmt.Fprintf(&b, "accepted artifacts attributable to OpenAlex title.search\n")
	fmt.Fprintf(&b, "--------------------------------------------------------\n")
	fmt.Fprintf(&b, "              title.search credits spent\n\n")
	fmt.Fprintf(&b, "numerator (sibling-hop-attributed accepted artifacts): %d\n", r.SiblingHopAttributedWins)
	fmt.Fprintf(&b, "denominator (title.search credits spent):              %d\n", r.TitleSearchCredits)
	if r.HasCredits {
		fmt.Fprintf(&b, "yield (lower bound):                                    %.3f%%\n\n", r.YieldLowerBound*100)
	} else {
		fmt.Fprintf(&b, "yield: n/a — zero title.search credits spent in this window\n\n")
	}

	fmt.Fprintf(&b, "spend breakdown\n")
	fmt.Fprintf(&b, "  sibling-hop searches:  %d (%d credits)\n", r.SiblingHopSearches, r.SiblingHopSearches*CreditsPerTitleSearch)
	fmt.Fprintf(&b, "  enrichment searches:   %d (%d credits)\n", r.EnrichmentSearches, r.EnrichmentSearches*CreditsPerTitleSearch)

	fmt.Fprintf(&b, "\nattribution\n")
	fmt.Fprintf(&b, "  accepted artifacts in window:                    %d\n", r.TotalAcceptedArtifacts)
	fmt.Fprintf(&b, "  sibling-hop-attributed wins (the numerator):     %d\n", r.SiblingHopAttributedWins)
	fmt.Fprintf(&b, "  unattributable (ledger names no winning candidate): %d\n", r.UnattributableAccepted)
	fmt.Fprintf(&b, "  excluded primary-resolve title-search wins:      %d (real wins; cost is not isolable in the ledger — see below)\n", r.ExcludedPrimaryResolveWins)

	fmt.Fprintf(&b, "\nlossiness (this is why the estimate is a LOWER bound)\n")
	fmt.Fprintf(&b, "  job.sibling_search events lost to the post-wire write window: %d\n", r.LostSiblingSearchEvents)
	fmt.Fprintf(&b, "  attempts lost to best-effort FinishAttempt (started, never finished, >10m old): %d\n", r.LostToFinishAttempt)
	fmt.Fprintf(&b, "  ambiguous resolve attempts (finished, but detail cannot prove a title search): %d\n", r.AmbiguousResolveAttempts)

	if r.ExcludedPrimaryResolveWins > 0 {
		fmt.Fprintf(&b, "\nnote: the primary OpenAlex resolver lookup can itself be a title search (a job\n")
		fmt.Fprintf(&b, "with no DOI or OpenAlex id yet), but its attempts row (\"candidates=N\") is\n")
		fmt.Fprintf(&b, "identical whether it used a DOI, an OpenAlex id, or a title — this tool refuses\n")
		fmt.Fprintf(&b, "to infer which from adjacent job state, so those %d win(s) are excluded from\n", r.ExcludedPrimaryResolveWins)
		fmt.Fprintf(&b, "both the numerator and the denominator rather than counted on one side only.\n")
	}

	return b.String()
}
