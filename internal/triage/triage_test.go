// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package triage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/job"
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
	id, err := jobs.CreateRequest(context.Background(), requestID,
		work.Work{DOI: "10.1000/" + requestID, Title: "Review work"}, "", "",
		job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE jobs SET state = 'needs_review' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(context.Background(), id, "verify_identity", "review the quarantined PDF",
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

	unbound, err := jobs.CreateRequest(ctx, "wr_action_unbound",
		work.Work{DOI: "10.1000/wr_action_unbound", Title: "Unbound review work"}, "", "",
		job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET state = 'needs_review' WHERE id = ?`, unbound); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, unbound, "verify_identity", "legacy row with no binding"); err != nil {
		t.Fatal(err)
	}

	manual, err := jobs.CreateRequest(ctx, "wr_action_manual",
		work.Work{DOI: "10.1000/wr_action_manual", Title: "Manual download work"}, "", "",
		job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, manual, "manual_download", "a resolver returned a landing page"); err != nil {
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
	for _, op := range unboundItem.Ops {
		if op == "accept" {
			t.Fatalf("unbound (unpreviewable) item offered accept: %v", unboundItem.Ops)
		}
	}
	if !containsOp(unboundItem.Ops, "reject") {
		t.Fatalf("unbound item ops = %v, want reject available without a valid binding", unboundItem.Ops)
	}

	manualItem := byJob[manual]
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
	working, err := jobs.CreateRequest(context.Background(), "wr_triage_working", work.Work{DOI: "10.1000/working", Title: "Working"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(context.Background(), working, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	failed, err := jobs.CreateRequest(context.Background(), "wr_triage_failed", work.Work{DOI: "10.1000/failed", Title: "Failed"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil)
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
