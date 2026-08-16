// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalexyield

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/redact"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// newFixtureStore builds a fresh, fully-migrated papio store in a temp
// directory — never the operator's real database. Measure is exercised
// directly against the store's own writable *sql.DB: Measure only issues
// SELECTs, so a second read-only connection buys nothing in a test and only
// OpenReadOnly (exercised by the CLI, not here) needs to prove mode=ro works.
func newFixtureStore(t *testing.T) (*job.Store, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &job.Store{S: s}, s.DB()
}

func createJob(t *testing.T, js *job.Store, requestID string) string {
	t.Helper()
	id, err := js.CreateRequest(context.Background(), requestID, work.Work{
		DOI: "10.1000/" + requestID, Title: "Example Paper " + requestID, Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func fakeSHA(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// acceptJob drives a queued job to ready with one candidate — at the given
// source and confidence — as the accepted acquisition, mirroring the proven
// pattern in internal/bundle/export_test.go's readyFixtureWithIdentity.
func acceptJob(t *testing.T, js *job.Store, id, source string, confidence float64) (sha string, candidateID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := js.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: source, URLRedacted: redact.URL("https://example.test/" + id + ".pdf"), URLKey: id + "-key",
		Version: "published", AccessBasis: "open_access", ReuseLicense: "cc-by-4.0",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: confidence, Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := js.NextPendingCandidate(ctx, id)
	if err != nil || candidate == nil {
		t.Fatalf("candidate missing: %v", err)
	}
	if err := js.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	sha = fakeSHA(id)
	if err := js.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: 10, MIME: "application/pdf", PageCount: 1, TextChars: 100,
		IdentityResult: "pass", Path: "/dev/null",
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := js.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := js.Transition(ctx, id, job.StateValidating, job.StateReady, nil,
		job.WithCandidate(candidate.ID), job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	return sha, candidate.ID
}

func TestMeasureAttributesSiblingHopWins(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-sibling-win")

	attempt, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex")
	if err != nil {
		t.Fatal(err)
	}
	if err := js.FinishAttempt(ctx, attempt, "success", 0, "sibling_candidates=1"); err != nil {
		t.Fatal(err)
	}
	if err := js.RecordEvent(ctx, id, "job.sibling_search", map[string]any{"basis": "abc"}); err != nil {
		t.Fatal(err)
	}
	acceptJob(t, js, id, "openalex", siblingHopConfidence)

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.SiblingHopSearches != 1 {
		t.Errorf("SiblingHopSearches = %d, want 1", report.SiblingHopSearches)
	}
	if report.TitleSearchCredits != CreditsPerTitleSearch {
		t.Errorf("TitleSearchCredits = %d, want %d", report.TitleSearchCredits, CreditsPerTitleSearch)
	}
	if report.SiblingHopAttributedWins != 1 {
		t.Errorf("SiblingHopAttributedWins = %d, want 1", report.SiblingHopAttributedWins)
	}
	if !report.HasCredits {
		t.Fatal("HasCredits = false, want true")
	}
	wantYield := 1.0 / float64(CreditsPerTitleSearch)
	if report.YieldLowerBound != wantYield {
		t.Errorf("YieldLowerBound = %v, want %v", report.YieldLowerBound, wantYield)
	}
	if report.LostSiblingSearchEvents != 0 {
		t.Errorf("LostSiblingSearchEvents = %d, want 0 (event was recorded)", report.LostSiblingSearchEvents)
	}
}

// TestMeasureCountsUnattributableAccepted pins the assignment's core honesty
// requirement: when the ledger cannot establish which candidate won an
// accepted artifact — here, a stale selection whose candidate is no longer
// 'accepted', exactly the ADR-0007 scenario internal/job.CandidateForArtifact
// exists to guard against — Measure reports it as unattributable rather than
// guessing.
func TestMeasureCountsUnattributableAccepted(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-unattributable")
	_, candidateID := acceptJob(t, js, id, "unpaywall", 1.0)

	// Simulate the stale-selection defect ADR-0007 documents: the candidate
	// the job still points at is no longer 'accepted'.
	if _, err := db.ExecContext(ctx, `UPDATE candidates SET status = 'skipped' WHERE id = ?`, candidateID); err != nil {
		t.Fatal(err)
	}

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalAcceptedArtifacts != 1 {
		t.Errorf("TotalAcceptedArtifacts = %d, want 1", report.TotalAcceptedArtifacts)
	}
	if report.UnattributableAccepted != 1 {
		t.Errorf("UnattributableAccepted = %d, want 1", report.UnattributableAccepted)
	}
	if report.SiblingHopAttributedWins != 0 {
		t.Errorf("SiblingHopAttributedWins = %d, want 0 (must not guess)", report.SiblingHopAttributedWins)
	}
}

// TestMeasureExcludesPrimaryResolveWins pins the package's most important
// scope boundary: a primary-resolve title-search win (IdentityConfidence
// 0.75) is real, but its cost cannot be isolated in the attempts ledger, so
// it must be disclosed and excluded from BOTH the numerator and the
// denominator, never counted as a win with no matching spend.
func TestMeasureExcludesPrimaryResolveWins(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-primary-title")
	acceptJob(t, js, id, "openalex", primaryTitleSearchConfidence)

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.ExcludedPrimaryResolveWins != 1 {
		t.Errorf("ExcludedPrimaryResolveWins = %d, want 1", report.ExcludedPrimaryResolveWins)
	}
	if report.SiblingHopAttributedWins != 0 {
		t.Errorf("SiblingHopAttributedWins = %d, want 0", report.SiblingHopAttributedWins)
	}
	if report.TitleSearchCredits != 0 {
		t.Errorf("TitleSearchCredits = %d, want 0 (no sibling-hop or enrichment search happened)", report.TitleSearchCredits)
	}
}

// TestMeasureRefusesTemporalAdjacencyAttribution is the assignment's named
// regression: a metadata-enrichment title search that succeeds immediately
// before an accepted artifact must NOT be counted as the cause of that win.
// The accepted candidate here is a plain exact DOI lookup (confidence 1.0,
// not 0.6 or 0.75) — exactly what enrichment's success actually produces
// (see the package doc comment) — and adjacency alone must not promote it
// into the numerator.
func TestMeasureRefusesTemporalAdjacencyAttribution(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-adjacency")

	attempt, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex")
	if err != nil {
		t.Fatal(err)
	}
	if err := js.FinishAttempt(ctx, attempt, "success", 0, "metadata_enriched"); err != nil {
		t.Fatal(err)
	}
	// Immediately afterward, in the very next moment, the job is accepted via
	// an exact-lookup candidate — the shape a naive "search happened shortly
	// before acceptance" heuristic would wrongly credit to the search.
	acceptJob(t, js, id, "openalex", 1.0)

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.EnrichmentSearches != 1 {
		t.Errorf("EnrichmentSearches = %d, want 1 (still correctly counted in the denominator)", report.EnrichmentSearches)
	}
	if report.SiblingHopAttributedWins != 0 {
		t.Errorf("SiblingHopAttributedWins = %d, want 0 — adjacency must not manufacture attribution", report.SiblingHopAttributedWins)
	}
	if report.ExcludedPrimaryResolveWins != 0 {
		t.Errorf("ExcludedPrimaryResolveWins = %d, want 0 (confidence 1.0 is an exact lookup, not a title search)", report.ExcludedPrimaryResolveWins)
	}
	if report.UnattributableAccepted != 0 {
		t.Errorf("UnattributableAccepted = %d, want 0 (this win IS attributable — to an exact lookup, not to the search)", report.UnattributableAccepted)
	}
}

func TestMeasureCountsLostSiblingSearchEvents(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-lost-event")

	attempt, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex")
	if err != nil {
		t.Fatal(err)
	}
	if err := js.FinishAttempt(ctx, attempt, "success", 0, "sibling_candidates=0"); err != nil {
		t.Fatal(err)
	}
	// No job.sibling_search event recorded: simulates the crash window
	// between FinishAttempt committing and RecordEvent committing.

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.SiblingHopSearches != 1 {
		t.Errorf("SiblingHopSearches = %d, want 1", report.SiblingHopSearches)
	}
	if report.LostSiblingSearchEvents != 1 {
		t.Errorf("LostSiblingSearchEvents = %d, want 1", report.LostSiblingSearchEvents)
	}
}

func TestMeasureCountsLostToFinishAttempt(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-lost-finish")

	old, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex")
	if err != nil {
		t.Fatal(err)
	}
	backdated := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `UPDATE attempts SET started_at = ? WHERE id = ?`, backdated, old); err != nil {
		t.Fatal(err)
	}

	// A second, genuinely in-flight attempt started just now must NOT be
	// misclassified as lost.
	if _, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex"); err != nil {
		t.Fatal(err)
	}

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.LostToFinishAttempt != 1 {
		t.Errorf("LostToFinishAttempt = %d, want 1 (only the backdated attempt, not the in-flight one)", report.LostToFinishAttempt)
	}
}

func TestMeasureCountsAmbiguousResolveAttempts(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-ambiguous")

	attempt, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex")
	if err != nil {
		t.Fatal(err)
	}
	if err := js.FinishAttempt(ctx, attempt, "retryable", 0, "*resolver.TemporaryError"); err != nil {
		t.Fatal(err)
	}

	report, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.AmbiguousResolveAttempts != 1 {
		t.Errorf("AmbiguousResolveAttempts = %d, want 1", report.AmbiguousResolveAttempts)
	}
	if report.SiblingHopSearches != 0 || report.EnrichmentSearches != 0 {
		t.Errorf("an ambiguous failed attempt must not be counted as a completed search: sibling=%d enrichment=%d",
			report.SiblingHopSearches, report.EnrichmentSearches)
	}
}

func TestMeasureSinceWindowExcludesOlderEvidence(t *testing.T) {
	js, db := newFixtureStore(t)
	ctx := context.Background()
	id := createJob(t, js, "wr-window")

	attempt, err := js.StartAttempt(ctx, id, 0, "resolve", "openalex")
	if err != nil {
		t.Fatal(err)
	}
	if err := js.FinishAttempt(ctx, attempt, "success", 0, "sibling_candidates=0"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `UPDATE attempts SET started_at = ? WHERE id = ?`, old, attempt); err != nil {
		t.Fatal(err)
	}

	since := time.Now().Add(-7 * 24 * time.Hour)
	report, err := Measure(ctx, db, since)
	if err != nil {
		t.Fatal(err)
	}
	if report.SiblingHopSearches != 0 {
		t.Errorf("SiblingHopSearches = %d, want 0 (the attempt is outside the window)", report.SiblingHopSearches)
	}

	all, err := Measure(ctx, db, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if all.SiblingHopSearches != 1 {
		t.Errorf("unwindowed SiblingHopSearches = %d, want 1", all.SiblingHopSearches)
	}
}
