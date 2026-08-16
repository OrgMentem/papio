// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package triage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/watch"
	"papio/internal/work"
)

type staticSource struct{ items []Item }

func (source staticSource) SnapshotItems(context.Context, *sql.Tx) ([]Item, error) {
	return append([]Item(nil), source.items...), nil
}

func triageTestService(t *testing.T) (*Service, *watch.Store, *job.Store) {
	t.Helper()
	db, err := store.Open(context.Background(), storetest.DataDir(t))
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

func TestLegacySnapshotPaginationSkipsPdfGrabBeforeLimit(t *testing.T) {
	service, _, jobs := triageTestService(t)
	createTriageAction(t, jobs, "wr_triage_legacy_grab")
	service.RegisterSource(staticSource{items: []Item{
		{
			Kind: KindPdfGrab, ID: PdfGrabIDPrefix + "grab_legacy_1", Title: "Reading copy",
			Ops:     []string{"provide_identifier", "dismiss"},
			PdfGrab: &PdfGrab{GrabID: "grab_legacy_1", State: "parked_no_identifier"},
		},
		{
			Kind: KindPdfGrab, ID: PdfGrabIDPrefix + "grab_legacy_2", Title: "Reading copy two",
			Ops:     []string{"provide_identifier", "dismiss"},
			PdfGrab: &PdfGrab{GrabID: "grab_legacy_2", State: "parked_no_identifier"},
		},
	}})

	legacy, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Schema: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Items) != 1 || legacy.Items[0].Kind != KindHumanAction || legacy.HasMore || legacy.Counts.PendingTotal != 1 {
		t.Fatalf("legacy page retained grab or pagination/count slot: %+v", legacy)
	}
	v4, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Schema: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(v4.Items) != 1 || v4.Items[0].Kind != KindPdfGrab || !v4.HasMore || v4.Counts.PendingTotal != 3 {
		t.Fatalf("v4 page did not expose grab/count it before action: %+v", v4)
	}
	v4Second, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: v4.Cursor, Schema: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(v4Second.Items) != 1 || v4Second.Items[0].Kind != KindPdfGrab || !v4Second.HasMore || v4Second.Counts.PendingTotal != 3 {
		t.Fatalf("v4 second page lost global count: %+v", v4Second)
	}
	v4Third, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: v4Second.Cursor, Schema: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(v4Third.Items) != 1 || v4Third.Items[0].Kind != KindHumanAction || v4Third.HasMore || v4Third.Counts.PendingTotal != 3 {
		t.Fatalf("v4 complete page did not validate global count: %+v", v4Third)
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
func createProjectionAction(t *testing.T, jobs *job.Store, requestID, kind, detail string, access job.AccessClassification, opts ...job.OpenHumanActionOption) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID, Title: requestID}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	state := job.StateAwaitingHuman
	if kind == "verify_identity" {
		state = job.StateNeedsReview
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET state = ? WHERE id = ?`, state, id); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, kind, detail, access, opts...); err != nil {
		t.Fatal(err)
	}
	return id
}

func createGate(t *testing.T, jobs *job.Store, id, scope, owner string, dependent []string) {
	t.Helper()
	members := append([]string{owner}, dependent...)
	if err := jobs.UpsertHumanGateObservation(context.Background(), job.HumanGateObservation{
		ID: id, GateType: job.HumanGateLogin,
		ScopeClass: string(job.HumanGateScopeAuthenticationClaim), ScopeKey: scope,
		ObservationRevision: 1, Status: job.HumanGateOpen, DetailJSON: `{}`,
		DependentJobIDs: dependent, ClaimMemberJobIDs: members,
	}); err != nil {
		t.Fatal(err)
	}
}

func familyByJob(snapshot Snapshot) map[string]*FamilyAssignment {
	out := make(map[string]*FamilyAssignment)
	for _, item := range snapshot.Items {
		if item.HumanAction != nil {
			out[item.HumanAction.JobID] = item.Family
		}
	}
	return out
}

func TestFamilyProjectionRunIdentityContiguityAndKey(t *testing.T) {
	service, _, jobs := triageTestService(t)
	for i := 0; i < 39; i++ {
		createProjectionAction(t, jobs, fmt.Sprintf("family-39-%02d", i), "manual_download", "download it", job.Access(false, "landing_page"))
	}
	first, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 5, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 5 || !first.HasMore {
		t.Fatalf("first page = items %d, has_more %v", len(first.Items), first.HasMore)
	}
	if first.Counts.FamilyBreakdownComplete == nil || !*first.Counts.FamilyBreakdownComplete || len(first.Counts.FamilyRuns) != 1 {
		t.Fatalf("family breakdown = complete %v runs %d", first.Counts.FamilyBreakdownComplete, len(first.Counts.FamilyRuns))
	}
	run := first.Counts.FamilyRuns[0]
	if run.Count != 39 || run.FirstRank != first.Items[0].Rank || run.RunKey == "" {
		t.Fatalf("run = %+v, first item = %+v", run, first.Items[0])
	}
	wantInput := []any{5, first.Items[0].ID, KindHumanAction, run.RouteClass, run.ActionKind, run.NextActor, run.GuidanceVariant, run.OperationVariant}
	raw, err := json.Marshal(wantInput)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	wantKey := "fr1_" + hex.EncodeToString(sum[:])[:32]
	if run.RunKey != wantKey {
		t.Fatalf("run key = %q, want independently derived %q", run.RunKey, wantKey)
	}
	for _, item := range first.Items {
		if item.Family == nil || item.Family.RunKey != run.RunKey ||
			item.Family.NextActor != run.NextActor ||
			item.Family.GuidanceVariant != run.GuidanceVariant ||
			item.Family.OperationVariant != run.OperationVariant {
			t.Fatalf("row %s family = %+v, want quartet from run %+v", item.ID, item.Family, run)
		}
	}
	again, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 5, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts.FamilyRuns[0].RunKey != run.RunKey {
		t.Fatalf("unchanged projection changed key: %q then %q", run.RunKey, again.Counts.FamilyRuns[0].RunKey)
	}

	changedService, _, changedJobs := triageTestService(t)
	seed := createProjectionAction(t, changedJobs, "changed-seed", "manual_download", "closed", job.Access(false, "landing_page"))
	if _, err := changedJobs.S.DB().ExecContext(context.Background(), `UPDATE human_actions SET status = 'resolved' WHERE job_id = ?`, seed); err != nil {
		t.Fatal(err)
	}
	createProjectionAction(t, changedJobs, "changed-first", "manual_download", "download it", job.Access(false, "landing_page"))
	createProjectionAction(t, changedJobs, "changed-second", "manual_download", "download it", job.Access(false, "landing_page"))
	changed, err := changedService.Snapshot(context.Background(), SnapshotRequest{Limit: 5, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Counts.FamilyRuns[0].RunKey == run.RunKey {
		t.Fatalf("changing first member did not change key: %q", run.RunKey)
	}

	// Amended plan §11 rule 5: the same family variant separated by an
	// intervening row is one block, because the daemon now orders action rows
	// by family rather than by raw insertion order.
	splitService, _, splitJobs := triageTestService(t)
	for i := range 20 {
		createProjectionAction(t, splitJobs, fmt.Sprintf("split-a-%02d", i), "manual_download", "download it", job.Access(false, "landing_page"))
	}
	exception := createProjectionAction(t, splitJobs, "split-exception", "verify_identity", "inspect", job.Access(false, ""))
	for i := range 19 {
		createProjectionAction(t, splitJobs, fmt.Sprintf("split-b-%02d", i), "manual_download", "download it", job.Access(false, "landing_page"))
	}
	split, err := splitService.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(split.Counts.FamilyRuns) != 2 {
		t.Fatalf("split runs = %+v, want one run per family", split.Counts.FamilyRuns)
	}
	if split.Counts.FamilyRuns[0].GuidanceVariant != "manual_download" || split.Counts.FamilyRuns[0].Count != 39 ||
		split.Counts.FamilyRuns[1].GuidanceVariant != "verify_identity" || split.Counts.FamilyRuns[1].Count != 1 {
		t.Fatalf("split run counts = %+v, want manual_download 39 then verify_identity 1", split.Counts.FamilyRuns)
	}
	for i := 1; i < len(split.Counts.FamilyRuns); i++ {
		prev, current := split.Counts.FamilyRuns[i-1], split.Counts.FamilyRuns[i]
		if prev.FirstRank > current.FirstRank || (prev.FirstRank == current.FirstRank && prev.RunKey >= current.RunKey) {
			t.Fatalf("family runs not ordered by (first_rank, run_key): %+v", split.Counts.FamilyRuns)
		}
	}
	seenKeys := map[string]bool{}
	for i, item := range split.Items {
		if item.Rank != humanActionRankBase+i {
			t.Fatalf("rank[%d] = %d, want %d", i, item.Rank, humanActionRankBase+i)
		}
		if item.Family == nil {
			t.Fatalf("row %s missing family quartet", item.ID)
		}
		seenKeys[item.Family.RunKey] = true
		if item.HumanAction.JobID == exception && i != len(split.Items)-1 {
			t.Fatalf("intervening exception at index %d, want it after the whole manual-download block", i)
		}
	}
	if len(seenKeys) != 2 {
		t.Fatalf("participating run keys = %v, want 2 distinct keys", seenKeys)
	}
	if split.Counts.FamilyRuns[0].FirstRank != split.Items[0].Rank {
		t.Fatalf("first run rank = %d, want the oldest action's rank %d", split.Counts.FamilyRuns[0].FirstRank, split.Items[0].Rank)
	}
}
func TestFamilyGuidanceAndOperationVariantsUseDurableFacts(t *testing.T) {
	type want struct {
		kind, guidance, operation string
		access                    job.AccessClassification
	}
	cases := []want{
		{kind: "manual_download", guidance: "manual_download", operation: "open_and_dismiss", access: job.Access(false, "landing_page")},
		{kind: "openurl_handoff", guidance: "institution_sign_in", operation: "open_and_dismiss", access: job.Access(true, "paywall")},
		{kind: "openurl_handoff", guidance: "open_page", operation: "open_and_dismiss", access: job.Access(false, "landing_page")},
		{kind: "verify_identity", guidance: "verify_identity", operation: "accept_reject_open", access: job.Access(false, "")},
		{kind: "document_delivery", guidance: "document_delivery", operation: "delivery_reconcile", access: job.Access(false, "")},
		{kind: "downloads_access_required", guidance: "downloads_access", operation: "open_and_dismiss", access: job.Access(false, "landing_page")},
		{kind: "terms_acceptance_required", guidance: "terms_acceptance", operation: "open_and_dismiss", access: job.Access(false, "landing_page")},
		{kind: "openurl_available", guidance: "open_page", operation: "open_and_dismiss", access: job.Access(false, "landing_page")},
	}
	service, _, jobs := triageTestService(t)
	for i, tc := range cases {
		createProjectionAction(t, jobs, fmt.Sprintf("mapping-%02d", i), tc.kind, "no adapter appears only in this detail", tc.access)
	}
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		var got *FamilyAssignment
		for j := range snapshot.Items {
			item := &snapshot.Items[j]
			if item.HumanAction == nil || item.HumanAction.ActionKind != tc.kind || item.Family == nil {
				continue
			}
			if tc.kind == "openurl_handoff" {
				auth := item.HumanAction.RequiresAuth != nil && *item.HumanAction.RequiresAuth
				if auth != (i == 1) {
					continue
				}
			}
			got = item.Family
			break
		}
		if got == nil {
			t.Fatalf("case %d kind %q missing family assignment; items = %+v", i, tc.kind, snapshot.Items)
		}
		if got.GuidanceVariant != tc.guidance || got.OperationVariant != tc.operation {
			t.Errorf("%s case %d family = guidance %q operation %q, want %q/%q", tc.kind, i, got.GuidanceVariant, got.OperationVariant, tc.guidance, tc.operation)
		}
		if got.NextActor != "researcher" {
			t.Errorf("%s case %d next actor = %q, want researcher", tc.kind, i, got.NextActor)
		}
	}
	// The five live manual-download reasons. Before the durable column all of
	// them rendered as one "manual_download" family, so a rejected file and a
	// missing adapter carried the same instruction.
	for _, tc := range []struct{ diagnosis, guidance string }{
		{job.DiagnosisReasonProviderAdapterDrift, "manual_download_page_undriveable"},
		{job.DiagnosisReasonAdoptedPDFInvalid, "manual_download_rejected_file"},
		{job.DiagnosisReasonWrongWork, "manual_download_wrong_work"},
		{job.DiagnosisReasonProviderAdapterMissing, "manual_download_adapter_missing"},
		{job.DiagnosisReasonLandingPageOnly, "manual_download"},
	} {
		reasonService, _, reasonJobs := triageTestService(t)
		createProjectionAction(t, reasonJobs, "reason-"+tc.diagnosis, "manual_download",
			// Prose that would sniff as a different reason entirely: only the
			// structured diagnosis may decide the family.
			"No source-controlled adapter matched this provider page.",
			job.Access(false, "landing_page"), job.WithHumanActionDiagnosis(tc.diagnosis))
		projected, err := reasonService.Snapshot(context.Background(), SnapshotRequest{Limit: 10, Schema: 5})
		if err != nil {
			t.Fatal(err)
		}
		if len(projected.Items) != 1 || projected.Items[0].Family == nil ||
			projected.Items[0].Family.GuidanceVariant != tc.guidance {
			t.Fatalf("%s projected = %+v, want guidance %q", tc.diagnosis, projected.Items, tc.guidance)
		}
		if projected.Counts.FamilyBreakdownComplete == nil || !*projected.Counts.FamilyBreakdownComplete {
			t.Fatalf("%s left the family breakdown incomplete", tc.diagnosis)
		}
		// The per-item prose reason stays on the wire for the row-level line.
		var detail string
		for _, fact := range projected.Items[0].Facts {
			if fact.Label == "Detail" {
				detail = fact.Text
			}
		}
		if detail != "No source-controlled adapter matched this provider page." {
			t.Fatalf("%s Detail fact = %q, want the durable prose preserved", tc.diagnosis, detail)
		}
	}

	// A row with no diagnosis at all — every action that predates the column —
	// still renders as a plain manual download rather than crashing or
	// dropping out of the breakdown.
	legacyService, _, legacyJobs := triageTestService(t)
	createProjectionAction(t, legacyJobs, "reason-legacy-null", "manual_download",
		"the adopted download failed validation; please supply a different file", job.Access(false, "landing_page"))
	legacy, err := legacyService.Snapshot(context.Background(), SnapshotRequest{Limit: 10, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Items) != 1 || legacy.Items[0].Family == nil || legacy.Items[0].Family.GuidanceVariant != "manual_download" {
		t.Fatalf("NULL-diagnosis projection = %+v, want a plain manual_download family", legacy.Items)
	}
	if legacy.Counts.FamilyBreakdownComplete == nil || !*legacy.Counts.FamilyBreakdownComplete {
		t.Fatal("a NULL diagnosis must not make the family breakdown incomplete")
	}

	// An unmapped diagnosis is never guessed: the row stays standalone and the
	// breakdown says so.
	unknownService, _, unknownJobs := triageTestService(t)
	createProjectionAction(t, unknownJobs, "reason-unmapped", "manual_download", "download it",
		job.Access(false, "landing_page"), job.WithHumanActionDiagnosis(job.DiagnosisReasonRetryWait))
	unknown, err := unknownService.Snapshot(context.Background(), SnapshotRequest{Limit: 10, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Items) != 1 || unknown.Items[0].Family != nil {
		t.Fatalf("unmapped diagnosis projection = %+v, want a standalone row with no family", unknown.Items)
	}
	if unknown.Counts.FamilyBreakdownComplete == nil || *unknown.Counts.FamilyBreakdownComplete {
		t.Fatal("an unmapped diagnosis must make the family breakdown incomplete")
	}
	if unknown.Counts.FamilyRuns != nil {
		t.Fatalf("incomplete breakdown still published runs: %+v", unknown.Counts.FamilyRuns)
	}

	workingService, _, workingJobs := triageTestService(t)
	workingHandoff := createProjectionAction(t, workingJobs, "working-handoff", "openurl_handoff", "handoff", job.Access(false, ""))
	owner := createProjectionAction(t, workingJobs, "working-owner", "openurl_handoff", "handoff", job.Access(true, "paywall"))
	sibling := createProjectionAction(t, workingJobs, "working-sibling", "document_delivery", "fulfilled delivery continuation", job.Access(false, ""))
	createGate(t, workingJobs, "working-gate", "working-claim", owner, []string{sibling})
	working, err := workingService.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	byJob := familyByJob(working)
	if byJob[workingHandoff] != nil {
		t.Fatalf("unknown-auth handoff assignment = %+v, want standalone", byJob[workingHandoff])
	}
	if byJob[owner] == nil || byJob[sibling] == nil {
		t.Fatalf("working assignments = %+v", byJob)
	}
	if byJob[sibling].GuidanceVariant != "papio_continuing" || byJob[sibling].OperationVariant != "none" || byJob[sibling].NextActor != "papio" {
		t.Fatalf("fulfilled-delivery continuation = %+v", byJob[sibling])
	}
}

func TestUnmappedFamilyStatesStayStandaloneAndInvalidateBreakdown(t *testing.T) {
	service, _, jobs := triageTestService(t)
	firstDownload := createProjectionAction(t, jobs, "mapped-before-unknown", "manual_download", "normal", job.Access(false, "landing_page"))
	unknown := createProjectionAction(t, jobs, "unmapped-action", "new_route_not_mapped", "normal", job.Access(false, "landing_page"))
	identity := createProjectionAction(t, jobs, "mapped-identity", "verify_identity", "inspect", job.Access(false, ""))
	secondDownload := createProjectionAction(t, jobs, "mapped-after-unknown", "manual_download", "normal", job.Access(false, "landing_page"))
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	var unknownItem *Item
	for i := range snapshot.Items {
		if snapshot.Items[i].HumanAction != nil && snapshot.Items[i].HumanAction.JobID == unknown {
			unknownItem = &snapshot.Items[i]
		}
	}
	if unknownItem == nil || unknownItem.Family != nil {
		t.Fatalf("unmapped item family = %+v, want nil", unknownItem)
	}
	if snapshot.Counts.FamilyBreakdownComplete == nil || *snapshot.Counts.FamilyBreakdownComplete || snapshot.Counts.FamilyRuns != nil {
		t.Fatalf("unmapped breakdown = complete %v runs %+v, want false and absent", snapshot.Counts.FamilyBreakdownComplete, snapshot.Counts.FamilyRuns)
	}
	// The unmapped row joins no family: it is ordered by its own position, so
	// it lands after the manual-download block it interrupted and before the
	// later verify_identity family.
	want := []string{firstDownload, secondDownload, unknown, identity}
	got := actionJobOrder(snapshot)
	if !slices.Equal(got, want) {
		t.Fatalf("emitted order = %v, want %v", got, want)
	}
}

func TestTurnsIncludePdfGrabsAndRespectBounds(t *testing.T) {
	service, _, jobs := triageTestService(t)
	createProjectionAction(t, jobs, "turn-pdf-action", "manual_download", "normal", job.Access(false, "landing_page"))
	service.RegisterSource(staticSource{items: []Item{{
		Kind: KindPdfGrab, ID: PdfGrabIDPrefix + "turn-grab", Title: "PDF",
		Ops:     []string{"provide_identifier", "dismiss"},
		PdfGrab: &PdfGrab{GrabID: "turn-grab", State: "parked_no_identifier"},
	}}})
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Counts.TurnsRequired == nil || *snapshot.Counts.TurnsRequired != 2 || snapshot.Counts.TurnsWorking == nil || *snapshot.Counts.TurnsWorking != 0 {
		t.Fatalf("turn counts = required %v working %v", snapshot.Counts.TurnsRequired, snapshot.Counts.TurnsWorking)
	}
	if snapshot.Counts.RequiredTurnsComplete == nil || !*snapshot.Counts.RequiredTurnsComplete || len(snapshot.Counts.RequiredTurns) != 2 {
		t.Fatalf("required turns = complete %v entries %d", snapshot.Counts.RequiredTurnsComplete, len(snapshot.Counts.RequiredTurns))
	}
	for _, turn := range snapshot.Counts.RequiredTurns {
		switch turn.ItemKind {
		case KindPdfGrab:
			if turn.GrabID == "" || turn.ActionID != 0 || turn.JobID != "" || turn.GateClaimID != "" || turn.DependentJobs != 0 {
				t.Fatalf("pdf turn fields = %+v", turn)
			}
		case KindHumanAction:
			if turn.ActionID == 0 || turn.JobID == "" || turn.GrabID != "" {
				t.Fatalf("action turn fields = %+v", turn)
			}
		default:
			t.Fatalf("unknown required turn kind = %+v", turn)
		}
	}

	tooManyService, _, tooManyJobs := triageTestService(t)
	for i := range 1025 {
		createProjectionAction(t, tooManyJobs, fmt.Sprintf("turn-limit-%04d", i), "manual_download", "normal", job.Access(false, "landing_page"))
	}
	tooMany, err := tooManyService.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tooMany.RequiredTurnsComplete == nil || *tooMany.RequiredTurnsComplete || tooMany.RequiredTurns != nil {
		t.Fatalf("over-limit required turns = complete %v entries nil? %v", tooMany.RequiredTurnsComplete, tooMany.RequiredTurns == nil)
	}

	// Alternating kinds used to produce one run per row. Family ordering
	// collapses them into one block per family; the 128-run wire bound is
	// still enforced against the run count itself, exercised below.
	manyRunsService, _, manyRunsJobs := triageTestService(t)
	for i := range 129 {
		kind := "manual_download"
		if i%2 == 1 {
			kind = "verify_identity"
		}
		createProjectionAction(t, manyRunsJobs, fmt.Sprintf("run-limit-%04d", i), kind, "normal", job.Access(false, "landing_page"))
	}
	manyRuns, err := manyRunsService.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manyRuns.FamilyBreakdownComplete == nil || !*manyRuns.FamilyBreakdownComplete || len(manyRuns.FamilyRuns) != 2 {
		t.Fatalf("alternating kinds = complete %v runs %+v, want one run per family", manyRuns.FamilyBreakdownComplete, manyRuns.FamilyRuns)
	}
	if manyRuns.FamilyRuns[0].Count != 65 || manyRuns.FamilyRuns[1].Count != 64 {
		t.Fatalf("alternating run counts = %+v, want 65 then 64", manyRuns.FamilyRuns)
	}
}

// TestFamilyRunCountStillTripsTheWireBound keeps the 128-run contract bound
// defended. Family ordering makes 129 blocks unreachable from the closed
// variant vocabulary, so the run counter is exercised directly.
func TestFamilyRunCountStillTripsTheWireBound(t *testing.T) {
	items := make([]Item, 129)
	rows := make([]familyRow, 129)
	for i := range items {
		items[i] = Item{Kind: KindHumanAction, ID: fmt.Sprintf("action:%d", i+1), Rank: humanActionRankBase + i}
		assignment := &FamilyAssignment{
			NextActor: "researcher", GuidanceVariant: fmt.Sprintf("variant-%03d", i),
			OperationVariant: "open_and_dismiss", RouteClass: "manual_download", ActionKind: "manual_download",
		}
		rows[i] = familyRow{assignment: assignment, tuple: familyTupleOf(KindHumanAction, assignment)}
	}
	var counts Counts
	if runs := buildFamilyRuns(items, rows, &counts); runs != 129 {
		t.Fatalf("run count = %d, want 129 so the projection's 128 bound fails closed", runs)
	}
}
func TestTypedGateAggregationAndNoGateFallback(t *testing.T) {
	service, _, jobs := triageTestService(t)
	owner := createProjectionAction(t, jobs, "gate-owner", "openurl_handoff", "sign in", job.Access(true, "paywall"))
	siblingOne := createProjectionAction(t, jobs, "gate-sibling-one", "manual_download", "download", job.Access(false, "landing_page"))
	siblingTwo := createProjectionAction(t, jobs, "gate-sibling-two", "document_delivery", "continue", job.Access(false, ""))
	siblingThree := createProjectionAction(t, jobs, "gate-sibling-three", "terms_acceptance_required", "accept", job.Access(false, "landing_page"))
	createGate(t, jobs, "gate-one", "claim-one", owner, []string{siblingOne, siblingTwo, siblingThree})
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	byJob := familyByJob(snapshot)
	if snapshot.Counts.TurnsRequired == nil || *snapshot.Counts.TurnsRequired != 1 ||
		snapshot.Counts.TurnsWorking == nil || *snapshot.Counts.TurnsWorking != 3 {
		t.Fatalf("one gate turn counts = required %v working %v", snapshot.Counts.TurnsRequired, snapshot.Counts.TurnsWorking)
	}
	if len(snapshot.Counts.RequiredTurns) != 1 || snapshot.Counts.RequiredTurns[0].GateClaimID != "gate-one" || snapshot.Counts.RequiredTurns[0].DependentJobs != 3 {
		t.Fatalf("one gate required turn = %+v", snapshot.Counts.RequiredTurns)
	}
	if byJob[owner] == nil || byJob[owner].GateClaimID != "gate-one" || byJob[owner].DependentJobs != 3 || byJob[owner].NextActor != "researcher" {
		t.Fatalf("gate owner assignment = %+v", byJob[owner])
	}
	for _, id := range []string{siblingOne, siblingTwo, siblingThree} {
		if byJob[id] == nil || byJob[id].NextActor != "papio" || byJob[id].GuidanceVariant != "papio_continuing" || byJob[id].OperationVariant != "none" {
			t.Fatalf("gate sibling %s assignment = %+v", id, byJob[id])
		}
	}

	twoService, _, twoJobs := triageTestService(t)
	first := createProjectionAction(t, twoJobs, "two-gates-first", "openurl_handoff", "sign in", job.Access(true, "paywall"))
	second := createProjectionAction(t, twoJobs, "two-gates-second", "openurl_handoff", "sign in", job.Access(true, "paywall"))
	createGate(t, twoJobs, "gate-a", "claim-a", first, nil)
	createGate(t, twoJobs, "gate-b", "claim-b", second, nil)
	two, err := twoService.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if two.Counts.TurnsRequired == nil || *two.Counts.TurnsRequired != 2 || len(two.Counts.RequiredTurns) != 2 {
		t.Fatalf("two independent gate counts = %+v required %+v", two.Counts.TurnsRequired, two.Counts.RequiredTurns)
	}
	byTwo := familyByJob(two)
	if byTwo[first].GateClaimID == byTwo[second].GateClaimID || byTwo[first].GateClaimID == "" || byTwo[second].GateClaimID == "" {
		t.Fatalf("independent gate claims collapsed: first %+v second %+v", byTwo[first], byTwo[second])
	}

	noGateService, _, noGateJobs := triageTestService(t)
	noGateOne := createProjectionAction(t, noGateJobs, "no-gate-one", "manual_download", "download", job.Access(false, "landing_page"))
	noGateTwo := createProjectionAction(t, noGateJobs, "no-gate-two", "verify_identity", "inspect", job.Access(false, ""))
	noGateAdvisory := createProjectionAction(t, noGateJobs, "no-gate-advisory", "openurl_available", "advisory", job.Access(false, "landing_page"))
	noGate, err := noGateService.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if noGate.Counts.TurnsRequired == nil || *noGate.Counts.TurnsRequired != 3 || noGate.Counts.TurnsWorking == nil || *noGate.Counts.TurnsWorking != 0 {
		t.Fatalf("no-gate counts = required %v working %v", noGate.Counts.TurnsRequired, noGate.Counts.TurnsWorking)
	}
	noGateByJob := familyByJob(noGate)
	for _, id := range []string{noGateOne, noGateTwo, noGateAdvisory} {
		if noGateByJob[id] == nil || noGateByJob[id].GateClaimID != "" || noGateByJob[id].NextActor != "researcher" {
			t.Fatalf("no-gate assignment = %+v", noGateByJob[id])
		}
		if noGateByJob[id].NextActor == "reference" {
			t.Fatalf("advisory inference produced reference actor for %s", id)
		}
	}
}

// actionJobOrder returns the emitted human-action rows as their job IDs, in
// snapshot order, so a test can assert exactly which block each row lands in.
func actionJobOrder(snapshot Snapshot) []string {
	out := make([]string, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if item.HumanAction != nil {
			out = append(out, item.HumanAction.JobID)
		}
	}
	return out
}

// TestActionRowsRenderOneBlockPerFamily seeds the interleaving observed on a
// real library: ten insertion-order runs across four families. The daemon now
// orders action rows by family, families by earliest member, so the same rows
// project as one block per family.
func TestActionRowsRenderOneBlockPerFamily(t *testing.T) {
	service, _, jobs := triageTestService(t)
	download := job.Access(false, "landing_page")
	handoff := job.Access(true, "paywall")
	plain := job.Access(false, "")
	seeded := 0
	seed := func(kind string, access job.AccessClassification, count int) []string {
		ids := make([]string, 0, count)
		for range count {
			ids = append(ids, createProjectionAction(t, jobs, fmt.Sprintf("live-%02d", seeded), kind, "detail", access))
			seeded++
		}
		return ids
	}
	downloads := seed("manual_download", download, 8)
	handoffs := seed("openurl_handoff", handoff, 1)
	deliveries := seed("document_delivery", plain, 4)
	downloads = append(downloads, seed("manual_download", download, 2)...)
	deliveries = append(deliveries, seed("document_delivery", plain, 2)...)
	downloads = append(downloads, seed("manual_download", download, 1)...)
	available := seed("openurl_available", download, 1)
	downloads = append(downloads, seed("manual_download", download, 16)...)
	identity := seed("verify_identity", plain, 1)
	handoffs = append(handoffs, seed("openurl_handoff", handoff, 1)...)

	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Counts.FamilyBreakdownComplete == nil || !*snapshot.Counts.FamilyBreakdownComplete {
		t.Fatalf("family breakdown complete = %v", snapshot.Counts.FamilyBreakdownComplete)
	}
	type run struct {
		guidance string
		count    int
	}
	want := []run{
		{"manual_download", 27},
		{"institution_sign_in", 2},
		{"document_delivery", 6},
		{"open_page", 1},
		{"verify_identity", 1},
	}
	got := make([]run, 0, len(snapshot.Counts.FamilyRuns))
	for _, entry := range snapshot.Counts.FamilyRuns {
		got = append(got, run{entry.GuidanceVariant, entry.Count})
	}
	if !slices.Equal(got, want) {
		t.Fatalf("family runs = %+v, want one block per family in earliest-member order %+v", got, want)
	}
	for i, entry := range snapshot.Counts.FamilyRuns {
		if i > 0 {
			previous := snapshot.Counts.FamilyRuns[i-1]
			if previous.FirstRank > entry.FirstRank || (previous.FirstRank == entry.FirstRank && previous.RunKey >= entry.RunKey) {
				t.Fatalf("family runs not ordered by (first_rank, run_key): %+v", snapshot.Counts.FamilyRuns)
			}
		}
		if entry.RunKey == "" {
			t.Fatalf("run %+v has no key", entry)
		}
	}

	// Insertion order survives inside every family, and the oldest action
	// overall still opens the first block.
	wantOrder := slices.Concat(downloads, handoffs, deliveries, available, identity)
	if order := actionJobOrder(snapshot); !slices.Equal(order, wantOrder) {
		t.Fatalf("emitted order = %v, want %v", order, wantOrder)
	}
	if snapshot.Items[0].HumanAction.JobID != downloads[0] {
		t.Fatalf("first row = %s, want the oldest action %s", snapshot.Items[0].HumanAction.JobID, downloads[0])
	}
	if snapshot.Counts.FamilyRuns[0].FirstRank != snapshot.Items[0].Rank {
		t.Fatalf("first run rank = %d, want %d", snapshot.Counts.FamilyRuns[0].FirstRank, snapshot.Items[0].Rank)
	}
	for i := range snapshot.Items {
		if snapshot.Items[i].Rank != humanActionRankBase+i {
			t.Fatalf("rank[%d] = %d, want %d", i, snapshot.Items[i].Rank, humanActionRankBase+i)
		}
	}

	// Ordering only: the same 37 rows keep the same turn counts.
	if snapshot.Counts.TurnsRequired == nil || *snapshot.Counts.TurnsRequired != 37 ||
		snapshot.Counts.TurnsWorking == nil || *snapshot.Counts.TurnsWorking != 0 {
		t.Fatalf("turn counts = required %v working %v, want 37 and 0", snapshot.Counts.TurnsRequired, snapshot.Counts.TurnsWorking)
	}
	if snapshot.Counts.RequiredTurnsComplete == nil || !*snapshot.Counts.RequiredTurnsComplete || len(snapshot.Counts.RequiredTurns) != 37 {
		t.Fatalf("required turns = complete %v entries %d", snapshot.Counts.RequiredTurnsComplete, len(snapshot.Counts.RequiredTurns))
	}
}

// TestFamilyOrderingKeepsGuidanceVariantsApart proves family identity is the
// full variant tuple, not the action kind: two manual-download rows with
// different guidance stay in different blocks.
func TestFamilyOrderingKeepsGuidanceVariantsApart(t *testing.T) {
	service, _, jobs := triageTestService(t)
	plainFirst := createProjectionAction(t, jobs, "variant-plain-first", "manual_download", "download it", job.Access(false, "landing_page"))
	diagnosed := createProjectionAction(t, jobs, "variant-diagnosed", "manual_download", "download it", job.Access(false, "landing_page"),
		job.WithHumanActionDiagnosis(job.DiagnosisReasonProviderAdapterMissing))
	plainSecond := createProjectionAction(t, jobs, "variant-plain-second", "manual_download", "download it", job.Access(false, "landing_page"))
	snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Counts.FamilyRuns) != 2 {
		t.Fatalf("runs = %+v, want the two guidance variants kept apart", snapshot.Counts.FamilyRuns)
	}
	if snapshot.Counts.FamilyRuns[0].GuidanceVariant != "manual_download" || snapshot.Counts.FamilyRuns[0].Count != 2 ||
		snapshot.Counts.FamilyRuns[1].GuidanceVariant != "manual_download_adapter_missing" || snapshot.Counts.FamilyRuns[1].Count != 1 {
		t.Fatalf("runs = %+v, want manual_download 2 then manual_download_adapter_missing 1", snapshot.Counts.FamilyRuns)
	}
	want := []string{plainFirst, plainSecond, diagnosed}
	if order := actionJobOrder(snapshot); !slices.Equal(order, want) {
		t.Fatalf("emitted order = %v, want %v", order, want)
	}
}

func TestSnapshotCursorSurvivesMutationAndRejectsSchemaSwitchAndStaleAnchor(t *testing.T) {
	service, watches, jobs := triageTestService(t)

	seeded := []*watch.Watch{
		createTriageWatch(t, watches, "cursor-mutate-seed-a"),
		createTriageWatch(t, watches, "cursor-mutate-seed-b"),
		createTriageWatch(t, watches, "cursor-mutate-seed-c"),
		createTriageWatch(t, watches, "cursor-mutate-seed-d"),
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	initial := []watch.DigestEntry{
		{WorkKey: "10.1000/a", Title: "A", DOI: "10.1000/a"},
		{WorkKey: "10.1000/b", Title: "B", DOI: "10.1000/b"},
		{WorkKey: "10.1000/c", Title: "C", DOI: "10.1000/c"},
		{WorkKey: "10.1000/d", Title: "D", DOI: "10.1000/d"},
	}
	for index := range seeded {
		if _, err := watches.RecordDigest(context.Background(), seeded[index].ID, now.Add(time.Duration(index)*time.Second), []watch.DigestEntry{initial[index]}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Schema: 5})
	if err != nil || !first.HasMore || first.Cursor == "" || len(first.Items) != 1 {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	firstID := first.Items[0].ID

	// Schema switch without page movement must be rejected, not silently
	// mis-sliced: a schema-4 cursor encodes the pdf_grab filtering that
	// preceded pagination, so replaying it under schema 5 (or vice versa)
	// would shift offsets.
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: first.Cursor, Schema: 4}); err == nil {
		t.Fatal("schema-switched replay was accepted; expected invalid triage cursor")
	}

	// Remove the item the cursor was anchored on. The anchored item is a
	// watch_hit whose work_key is the last component of its ID;
	// ConsumeDigest wants the watched-digest work_key, not the grouped hit ID.
	for wIdx, w := range seeded {
		if _, err := watches.ConsumeDigest(context.Background(), w.ID, []string{initial[wIdx].WorkKey}); err == nil {
			break
		}
	}
	if _, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: first.Cursor, Schema: 5}); err == nil {
		t.Fatalf("cursor anchored on removed %q was accepted; expected invalid triage cursor", firstID)
	}

	// Insertion-only walk must still yield every item exactly once: never
	// skip or duplicate, regardless of a mid-page insert ahead of the cursor.
	service2, watches2, _ := triageTestService(t)
	seeded2 := make([]*watch.Watch, 0, 4)
	for i := range 4 {
		seeded2 = append(seeded2, createTriageWatch(t, watches2, fmt.Sprintf("cursor-walk-%d", i)))
	}
	seedEntries := []watch.DigestEntry{
		{WorkKey: "10.1000/w1", Title: "W1", DOI: "10.1000/w1"},
		{WorkKey: "10.1000/w2", Title: "W2", DOI: "10.1000/w2"},
		{WorkKey: "10.1000/w3", Title: "W3", DOI: "10.1000/w3"},
		{WorkKey: "10.1000/w4", Title: "W4", DOI: "10.1000/w4"},
	}
	for index := range seeded2 {
		if _, err := watches2.RecordDigest(context.Background(), seeded2[index].ID, now.Add(time.Duration(index)*time.Second), []watch.DigestEntry{seedEntries[index]}); err != nil {
			t.Fatal(err)
		}
	}
	first2, err := service2.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Schema: 5})
	if err != nil || len(first2.Items) != 1 {
		t.Fatalf("walk first page = %+v, %v", first2, err)
	}
	seen := map[string]int{}
	seen[first2.Items[0].ID]++
	cursor := first2.Cursor
	injected := createTriageWatch(t, watches2, "cursor-walk-injected")
	if _, err := watches2.RecordDigest(context.Background(), injected.ID, now.Add(-time.Hour), []watch.DigestEntry{
		{WorkKey: "10.1000/injected", Title: "Injected", DOI: "10.1000/injected"},
	}); err != nil {
		t.Fatal(err)
	}
	for {
		page, err := service2.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: cursor, Schema: 5})
		if err != nil {
			t.Fatalf("walk page after insert = %v", err)
		}
		for _, item := range page.Items {
			seen[item.ID]++
		}
		if !page.HasMore {
			break
		}
		cursor = page.Cursor
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("walk produced duplicate/skip for %q: count %d, seen %v", id, count, seen)
		}
	}
	if want := 5; len(seen) != want {
		t.Fatalf("walk covered %d items, want %d: %v", len(seen), want, seen)
	}

	// Schema-boundary regression: a client that enumerates under schema 3
	// must not have a cursor minted under schema 5 silently mis-slice as if
	// pdf_grabs were invisible.
	service3, _, jobs3 := triageTestService(t)
	createTriageAction(t, jobs3, "wr_cursor_schema_human")
	service3.RegisterSource(staticSource{items: []Item{
		{Kind: KindPdfGrab, ID: PdfGrabIDPrefix + "grab_cursor_schema", Title: "Reading copy", Ops: []string{"provide_identifier", "dismiss"}, PdfGrab: &PdfGrab{GrabID: "grab_cursor_schema", State: "parked_no_identifier"}},
	}})
	schema5Page, err := service3.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Schema: 5})
	if err != nil || len(schema5Page.Items) != 1 || schema5Page.Items[0].Kind != KindPdfGrab {
		t.Fatalf("schema 5 first page = %+v, %v", schema5Page, err)
	}
	if _, err := service3.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: schema5Page.Cursor, Schema: 3}); err == nil {
		t.Fatal("pdf_grab-anchored cursor replayed under legacy schema was accepted; expected invalid triage cursor")
	}
	// Offset-only cursors from the pre-fix wire shape are invalid now; we do
	// not silently reinterpret them as a keyset.
	offsetPayload, _ := json.Marshal(pageCursor{Version: SchemaVersion, Schema: 5, Offset: 1})
	offsetOnly := base64.RawURLEncoding.EncodeToString(offsetPayload)
	if _, err := service3.Snapshot(context.Background(), SnapshotRequest{Limit: 1, Cursor: offsetOnly, Schema: 5}); err == nil {
		t.Fatal("offset-only cursor was accepted; expected invalid triage cursor")
	}

	_ = jobs
}
