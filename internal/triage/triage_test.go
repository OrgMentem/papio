// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package triage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/watch"
	"papio/internal/work"
)

type staticSource struct{ items []Item }

func (source staticSource) SnapshotItems(context.Context, *sql.Tx) ([]Item, error) {
	return append([]Item(nil), source.items...), nil
}

func triageTestService(t *testing.T) (*Service, *watch.Store, *job.Store) {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	watches := watch.NewStore(db)
	jobs := &job.Store{S: db}
	service := New(db, watches, jobs)
	service.now = func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) }
	return service, watches, jobs
}

func createTriageWatch(t *testing.T, watches *watch.Store, query string) *watch.Watch {
	t.Helper()
	created, err := watches.Create(context.Background(), watch.CreateInput{
		Query: query, Filters: watch.Filters{YearFrom: 2020, OAOnly: true},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func createTriageAction(t *testing.T, jobs *job.Store, requestID string) string {
	t.Helper()
	id, err := jobs.CreateRequest(context.Background(), requestID, work.Work{DOI: "10.1000/" + requestID, Title: "Review work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE jobs SET state = 'needs_review' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(context.Background(), id, "verify_identity", "review the quarantined PDF", job.Access(false, ""),
		job.WithHumanActionBinding(job.HumanActionBinding{
			CandidateID: 1, QuarantinePath: "/tmp/review.pdf",
			QuarantineSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}),
	); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSnapshotGroupsWatchHitsAndAssignsRanks(t *testing.T) {
	service, watches, jobs := triageTestService(t)
	first := createTriageWatch(t, watches, "first")
	second := createTriageWatch(t, watches, "second")
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	entry := watch.DigestEntry{
		WorkKey: "10.1000/grouped", Title: "Grouped work", Authors: "Ada, Grace", Year: 2026,
		DOI: "10.1000/grouped", IsOA: true, Abstract: "A bounded abstract.",
	}
	for _, watched := range []*watch.Watch{first, second} {
		if _, err := watches.RecordDigest(context.Background(), watched.ID, now, []watch.DigestEntry{entry}); err != nil {
			t.Fatal(err)
		}
	}
	createTriageAction(t, jobs, "wr_triage_action")
	service.RegisterSource(staticSource{items: []Item{{
		Kind: KindRetraction, ID: "retraction:10.1000/notice", Title: "Retracted work",
		Facts:      []Fact{{Label: "Nature", Text: "retraction"}},
		Retraction: &Retraction{DOI: "10.1000/notice", Nature: "retraction", NoticedAt: now},
	}}})

	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Counts.PendingTotal != 3 || snapshot.Counts.WatchHits != 1 || snapshot.Counts.Actions != 1 || snapshot.Counts.Retractions != 1 || snapshot.Counts.JobsNeedsReview != 1 {
		t.Fatalf("counts = %+v", snapshot.Counts)
	}
	if len(snapshot.Items) != 3 || snapshot.Items[0].Kind != KindRetraction || snapshot.Items[1].Kind != KindHumanAction || snapshot.Items[2].Kind != KindWatchHit {
		t.Fatalf("ranked items = %+v", snapshot.Items)
	}
	if snapshot.Items[0].Rank >= snapshot.Items[1].Rank || snapshot.Items[1].Rank >= snapshot.Items[2].Rank {
		t.Fatalf("ranks = %d, %d, %d", snapshot.Items[0].Rank, snapshot.Items[1].Rank, snapshot.Items[2].Rank)
	}
	hit := snapshot.Items[2].WatchHit
	if hit.Abstract != entry.Abstract || len(hit.Watches) != 2 || hit.Watches[0].ID != first.ID || hit.Watches[1].ID != second.ID {
		t.Fatalf("grouped hit = %+v", hit)
	}
	if got := snapshot.Items[1].HumanAction; got.Revision != 1 || got.SHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || got.SizeBytes != 0 {
		t.Fatalf("action binding = %+v", got)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "generated_at", "counts", "items", "has_more", "unsupported_items_count"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("snapshot envelope missing %q: %s", key, encoded)
		}
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["items"], &items); err != nil {
		t.Fatal(err)
	}
	if _, leaked := items[2]["work_key"]; leaked {
		t.Fatalf("snapshot leaked an internal work key: %s", encoded)
	}
	if _, ok := items[2]["abstract"]; !ok {
		t.Fatalf("watch hit missing abstract: %s", encoded)
	}
}

func TestHumanActionItemsCarryWorkIdentityAndCorrectOps(t *testing.T) {
	service, _, jobs := triageTestService(t)
	ctx := context.Background()

	bound := createTriageAction(t, jobs, "wr_action_bound")

	unbound, err := jobs.CreateRequest(ctx, "wr_action_unbound", work.Work{DOI: "10.1000/wr_action_unbound", Title: "Unbound review work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET state = 'needs_review' WHERE id = ?`, unbound); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, unbound, "verify_identity", "legacy row with no binding", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	manual, err := jobs.CreateRequest(ctx, "wr_action_manual", work.Work{DOI: "10.1000/wr_action_manual", Title: "Manual download work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, manual, "manual_download", "a resolver returned a landing page", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET requires_auth = 1, blocked_by = 'paywall' WHERE job_id = ?`, bound); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET requires_auth = 0, blocked_by = 'landing_page' WHERE job_id = ?`, manual); err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.Snapshot(ctx, SnapshotRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	byJob := make(map[string]Item, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if item.HumanAction != nil {
			byJob[item.HumanAction.JobID] = item
		}
	}
	if len(byJob) != 3 {
		t.Fatalf("human action items = %d, want 3: %+v", len(byJob), snapshot.Items)
	}

	boundItem := byJob[bound]
	if boundItem.HumanAction.RequiresAuth == nil || !*boundItem.HumanAction.RequiresAuth || boundItem.HumanAction.BlockedBy != "paywall" {
		t.Fatalf("bound item access = requires_auth %v, blocked_by %q, want true/paywall",
			boundItem.HumanAction.RequiresAuth, boundItem.HumanAction.BlockedBy)
	}
	if boundItem.Title != "Review work" {
		t.Fatalf("bound item title = %q, want the paper title", boundItem.Title)
	}
	if len(boundItem.Links) != 1 || boundItem.Links[0].URL == "" {
		t.Fatalf("bound item links = %+v, want a DOI link", boundItem.Links)
	}
	wantOps := map[string]bool{"accept": true, "reject": true, "open": true}
	for _, op := range boundItem.Ops {
		if !wantOps[op] {
			t.Fatalf("bound item ops = %v, unexpected %q", boundItem.Ops, op)
		}
		delete(wantOps, op)
	}
	if len(wantOps) != 0 {
		t.Fatalf("bound item ops = %v, missing %v", boundItem.Ops, wantOps)
	}

	unboundItem := byJob[unbound]
	if unboundItem.Title != "Unbound review work" {
		t.Fatalf("unbound item title = %q, want the paper title", unboundItem.Title)
	}
	if unboundItem.HumanAction.RequiresAuth != nil || unboundItem.HumanAction.BlockedBy != "" {
		t.Fatalf("unclassified action access = requires_auth %v, blocked_by %q, want absent",
			unboundItem.HumanAction.RequiresAuth, unboundItem.HumanAction.BlockedBy)
	}
	for _, op := range unboundItem.Ops {
		if op == "accept" {
			t.Fatalf("unbound (unpreviewable) item offered accept: %v", unboundItem.Ops)
		}
	}
	if !containsOp(unboundItem.Ops, "reject") {
		t.Fatalf("unbound item ops = %v, want reject available without a valid binding", unboundItem.Ops)
	}

	manualItem := byJob[manual]
	if manualItem.HumanAction.RequiresAuth == nil || *manualItem.HumanAction.RequiresAuth || manualItem.HumanAction.BlockedBy != "landing_page" {
		t.Fatalf("manual item access = requires_auth %v, blocked_by %q, want false/landing_page",
			manualItem.HumanAction.RequiresAuth, manualItem.HumanAction.BlockedBy)
	}
	if manualItem.Title != "Manual download work" {
		t.Fatalf("manual item title = %q, want the paper title", manualItem.Title)
	}
	if !containsOp(manualItem.Ops, "dismiss") {
		t.Fatalf("manual_download item ops = %v, want dismiss (it has no accept/reject flow)", manualItem.Ops)
	}
	for _, op := range manualItem.Ops {
		if op == "accept" || op == "reject" {
			t.Fatalf("manual_download item offered a review-only op: %v", manualItem.Ops)
		}
	}
}

func containsOp(ops []string, want string) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}

func TestSnapshotCursorPaginationAndCounts(t *testing.T) {
	service, watches, jobs := triageTestService(t)
	watched := createTriageWatch(t, watches, "cursor")
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	entries := []watch.DigestEntry{
		{WorkKey: "10.1000/one", Title: "One", DOI: "10.1000/one"},
		{WorkKey: "10.1000/two", Title: "Two", DOI: "10.1000/two"},
		{WorkKey: "10.1000/three", Title: "Three", DOI: "10.1000/three"},
	}
	if _, err := watches.RecordDigest(context.Background(), watched.ID, now, entries); err != nil {
		t.Fatal(err)
	}
	working, err := jobs.CreateRequest(context.Background(), "wr_triage_working", work.Work{DOI: "10.1000/working", Title: "Working"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(context.Background(), working, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	failed, err := jobs.CreateRequest(context.Background(), "wr_triage_failed", work.Work{DOI: "10.1000/failed", Title: "Failed"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE jobs SET state = 'failed', terminal_reason = 'network' WHERE id = ?`, failed); err != nil {
		t.Fatal(err)
	}

	first, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1})
	if err != nil || !first.HasMore || first.Cursor == "" || len(first.Items) != 1 {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: first.Cursor})
	if err != nil || second.Items[0].ID == first.Items[0].ID {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	third, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: second.Cursor})
	if err != nil || third.HasMore || len(third.Items) != 1 || third.Items[0].ID == first.Items[0].ID || third.Items[0].ID == second.Items[0].ID {
		t.Fatalf("third page = %+v, %v", third, err)
	}
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("invalid cursor was accepted")
	}
	counts, err := service.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.PendingTotal != 3 || counts.WatchHits != 3 || counts.JobsWorking != 1 || counts.FailureGroups7d != 1 {
		t.Fatalf("counts = %+v", counts)
	}
}

// createStatsJob drives a fresh job request straight to targetState via the
// shortest legal transition path, for browser-stats aggregation tests that
// only care about the terminal state and its updated_at.
func createStatsJob(t *testing.T, jobs *job.Store, requestID, targetState string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID, Title: "Stats work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	transition := func(from, to string) {
		t.Helper()
		if err := jobs.Transition(ctx, id, from, to, nil); err != nil {
			t.Fatalf("%s->%s: %v", from, to, err)
		}
	}
	switch targetState {
	case job.StateReady:
		transition(job.StateQueued, job.StateResolving)
		transition(job.StateResolving, job.StateReady)
	case job.StateImported:
		transition(job.StateQueued, job.StateResolving)
		transition(job.StateResolving, job.StateReady)
		transition(job.StateReady, job.StateImported)
	case job.StateFailed:
		transition(job.StateQueued, job.StateResolving)
		transition(job.StateResolving, job.StateFailed)
	case job.StateUnavailable:
		transition(job.StateQueued, job.StateResolving)
		transition(job.StateResolving, job.StateUnavailable)
	case job.StateCancelled:
		transition(job.StateQueued, job.StateCancelled)
	default:
		t.Fatalf("unsupported stats test target state %q", targetState)
	}
	return id
}

// createHandoffAcquiredJob parks a job in awaiting_human, opens a human
// action against it (exactly as an institutional handoff does), then lets it
// reach ready — the shape handoffs_required counts.
func createHandoffAcquiredJob(t *testing.T, jobs *job.Store, requestID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID, Title: "Stats handoff work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, "openurl_handoff", "handoff available", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateAwaitingHuman, job.StateResolving},
		{job.StateResolving, job.StateReady},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	return id
}

// acceptCandidate inserts and accepts one candidate for jobID, the shape
// Stats reads back as the job's access basis.
func acceptCandidate(t *testing.T, jobs *job.Store, jobID, urlKey, accessBasis string) {
	t.Helper()
	ctx := context.Background()
	if _, err := jobs.InsertCandidates(ctx, jobID, []job.Candidate{{
		Source: "fixture", URLRedacted: "https://example.test/" + urlKey, URLKey: urlKey,
		Version: resolver.VersionPublished, AccessBasis: accessBasis, ReuseLicense: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.NextPendingCandidate(ctx, jobID)
	if err != nil || candidate == nil {
		t.Fatalf("next pending candidate for %s: %+v, %v", jobID, candidate, err)
	}
	if err := jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
}

// setJobUpdatedAt overrides a job's updated_at directly, since Transition
// always stamps the real wall clock. Stats no longer buckets its weekly
// series by this column (see setReadyTransitionAt below); this now exists
// only to prove the series ignores it, by deliberately conflicting with the
// same job's ready-transition time.
func setJobUpdatedAt(t *testing.T, jobs *job.Store, jobID string, at time.Time) {
	t.Helper()
	if _, err := jobs.S.DB().ExecContext(context.Background(),
		`UPDATE jobs SET updated_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}

// setReadyTransitionAt overrides the timestamp of a job's ready-transition
// event — the timestamp Stats' weekly series buckets by — since Transition
// always stamps the real wall clock and the series needs a deterministic
// acquisition time to assert against.
func setReadyTransitionAt(t *testing.T, jobs *job.Store, jobID string, at time.Time) {
	t.Helper()
	result, err := jobs.S.DB().ExecContext(context.Background(),
		`UPDATE events SET at = ? WHERE job_id = ? AND kind = 'job.transition' AND json_extract(detail_json, '$.to') = 'ready'`,
		at.UTC().Format(time.RFC3339Nano), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		t.Fatal(err)
	} else if changed != 1 {
		t.Fatalf("setReadyTransitionAt %s: matched %d ready-transition events, want 1", jobID, changed)
	}
}

func TestStats(t *testing.T) {
	service, _, jobs := triageTestService(t)
	ctx := context.Background()

	// Current-week acquired job: open-access candidate plus a human-action
	// handoff before reaching ready.
	handoffJob := createHandoffAcquiredJob(t, jobs, "stats-handoff")
	acceptCandidate(t, jobs, handoffJob, "handoff-candidate", resolver.AccessOpen)
	setReadyTransitionAt(t, jobs, handoffJob, time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))

	// Oldest-window-week acquired job that is imported much later: the
	// series must bucket it by its ready transition, not by updated_at
	// (which the later ready -> imported transition moves to the same week
	// as handoffJob's ready transition — bucketing by updated_at would drop
	// this job from series[0], and double-count series[11] instead).
	importedJob := createStatsJob(t, jobs, "stats-imported", job.StateImported)
	acceptCandidate(t, jobs, importedJob, "imported-candidate", resolver.AccessInstitutional)
	setReadyTransitionAt(t, jobs, importedJob, time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC))
	setJobUpdatedAt(t, jobs, importedJob, time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC))

	// Acquired job outside the 12-week series window: licensed-API candidate.
	// Counts toward totals and access but must not land in any bucket.
	outsideJob := createStatsJob(t, jobs, "stats-outside", job.StateReady)
	acceptCandidate(t, jobs, outsideJob, "outside-candidate", resolver.AccessLicensedAPI)
	setReadyTransitionAt(t, jobs, outsideJob, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	// Acquired job with no accepted candidate -> Other; mid-window week.
	noCandidateJob := createStatsJob(t, jobs, "stats-no-candidate", job.StateReady)
	setReadyTransitionAt(t, jobs, noCandidateJob, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))

	createStatsJob(t, jobs, "stats-failed", job.StateFailed)
	createStatsJob(t, jobs, "stats-unavailable", job.StateUnavailable)
	createStatsJob(t, jobs, "stats-cancelled", job.StateCancelled)

	stats, err := service.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AcquiredTotal != 4 {
		t.Fatalf("acquired_total = %d, want 4", stats.AcquiredTotal)
	}
	if stats.FailedTotal != 2 {
		t.Fatalf("failed_total = %d, want 2", stats.FailedTotal)
	}
	if stats.HandoffsRequired != 1 {
		t.Fatalf("handoffs_required = %d, want 1", stats.HandoffsRequired)
	}
	wantAccess := StatsAccess{OpenAccess: 1, Institutional: 1, LicensedAPI: 1, Other: 1}
	if stats.Access != wantAccess {
		t.Fatalf("access = %+v, want %+v", stats.Access, wantAccess)
	}
	if len(stats.Series) != 12 {
		t.Fatalf("series length = %d, want 12", len(stats.Series))
	}
	// importedJob lands here by its ready transition, not its later,
	// conflicting updated_at.
	if got, want := stats.Series[0], (StatsBucket{PeriodStart: time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC), Acquired: 1}); !got.PeriodStart.Equal(want.PeriodStart) || got.Acquired != want.Acquired {
		t.Fatalf("series[0] = %+v, want %+v", got, want)
	}
	if got, want := stats.Series[6], (StatsBucket{PeriodStart: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), Acquired: 1}); !got.PeriodStart.Equal(want.PeriodStart) || got.Acquired != want.Acquired {
		t.Fatalf("series[6] = %+v, want %+v", got, want)
	}
	// handoffJob's ready transition only: importedJob's conflicting
	// updated_at in this same week must not also land here.
	if got, want := stats.Series[11], (StatsBucket{PeriodStart: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), Acquired: 1}); !got.PeriodStart.Equal(want.PeriodStart) || got.Acquired != want.Acquired {
		t.Fatalf("series[11] = %+v, want %+v", got, want)
	}
	total := 0
	for _, bucket := range stats.Series {
		total += bucket.Acquired
	}
	if total != 3 {
		t.Fatalf("series total = %d, want 3 (outside-window job excluded)", total)
	}
}

// TestStatsFallsBackToUpdatedAtWhenReadyEventIsMissing pins the deliberate
// degrade for AGENTS.md's documented class of surprise: a long-running dev
// papio.db can hold rows that predate a later behavior change. If a
// ready/imported job's ready-transition event is missing (an old row from
// before event logging covered that transition, not reachable through any
// current code path), Stats must not fail outright and must not drop the
// job from the series — it falls back to updated_at, the exact signal the
// pre-fix query used for every row.
func TestStatsFallsBackToUpdatedAtWhenReadyEventIsMissing(t *testing.T) {
	service, _, jobs := triageTestService(t)
	ctx := context.Background()

	legacyJob := createStatsJob(t, jobs, "stats-legacy", job.StateReady)
	if _, err := jobs.S.DB().ExecContext(ctx,
		`DELETE FROM events WHERE job_id = ? AND kind = 'job.transition' AND json_extract(detail_json, '$.to') = 'ready'`,
		legacyJob); err != nil {
		t.Fatal(err)
	}
	setJobUpdatedAt(t, jobs, legacyJob, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))

	stats, err := service.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats must not fail for a job missing its ready-transition event: %v", err)
	}
	if stats.AcquiredTotal != 1 {
		t.Fatalf("acquired_total = %d, want 1 (the legacy job must still count)", stats.AcquiredTotal)
	}
	if got, want := stats.Series[6], (StatsBucket{PeriodStart: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), Acquired: 1}); !got.PeriodStart.Equal(want.PeriodStart) || got.Acquired != want.Acquired {
		t.Fatalf("series[6] = %+v, want %+v (falls back to updated_at)", got, want)
	}
}
