// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"papio/internal/protocol"
	"papio/internal/store"
)

type fakeCLI struct {
	collection string
	limit      int
	items      []MissingPDFItem
	details    map[string]*Item
	find       map[string]json.RawMessage
	syncErr    error
	syncCalls  int
}

func (f *fakeCLI) Preflight(context.Context) (*PreflightResult, error) {
	return &PreflightResult{Executable: "zotio", Version: "1.0.0"}, nil
}
func (f *fakeCLI) MissingPDF(_ context.Context, collection string, limit int) ([]MissingPDFItem, error) {
	f.collection, f.limit = collection, limit
	return append([]MissingPDFItem(nil), f.items...), nil
}
func (f *fakeCLI) GetItem(_ context.Context, key string) (*Item, error) {
	item, ok := f.details[key]
	if !ok {
		return nil, fmt.Errorf("missing item %s", key)
	}
	copy := *item
	return &copy, nil
}
func (f *fakeCLI) Sync(context.Context) error {
	f.syncCalls++
	return f.syncErr
}
func (f *fakeCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	if len(args) >= 5 && strings.Join(args[:3], " ") == "--agent items find" {
		key := strings.TrimPrefix(args[3], "--") + ":" + args[4]
		if raw := f.find[key]; raw != nil {
			return raw, nil
		}
		return json.RawMessage("[]"), nil
	}
	return nil, fmt.Errorf("unexpected RunJSON %q", args)
}

type fakeSubmitter struct {
	requests []protocol.WorkRequest
}

func (f *fakeSubmitter) Submit(_ context.Context, request protocol.WorkRequest) (string, error) {
	f.requests = append(f.requests, request)
	return "job_" + request.ZotioItemKey, nil
}

func TestQueueMissingPDFSubmitsDeterministicRequestsAndSkipsUnidentified(t *testing.T) {
	cli := &fakeCLI{
		items: []MissingPDFItem{
			{Key: "AB12CD34", Title: "DOI paper", DOI: "https://doi.org/10.1000/One"},
			{Key: "EF56GH78", Title: "Tuple paper"},
			{Key: "JK90LM12", Title: "Incomplete"},
		},
		details: map[string]*Item{
			"EF56GH78": {Key: "EF56GH78", Title: "Tuple paper", Authors: []string{"Ada Lovelace"}, Year: 2024},
			"JK90LM12": {Key: "JK90LM12", Title: "Incomplete"},
		},
	}
	submitter := &fakeSubmitter{}
	service := &Service{CLI: cli, Submitter: submitter}
	maxCost := 2.5
	result, err := service.QueueMissingPDF(context.Background(), QueueOptions{
		Collection:     "ZX98YU76",
		Limit:          12,
		DesiredVersion: "published",
		MaxCostUSD:     &maxCost,
		SourcesDeny:    []string{" unpaywall "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cli.collection != "ZX98YU76" || cli.limit != 0 {
		t.Fatalf("complete-scan args = (%q,%d)", cli.collection, cli.limit)
	}
	if len(result.Queued) != 2 || len(result.Skipped) != 1 || len(submitter.requests) != 2 {
		t.Fatalf("result = %+v requests=%+v", result, submitter.requests)
	}
	first := submitter.requests[0]
	if first.RequestID != "request_zotio_AB12CD34" || first.ZotioItemKey != "AB12CD34" || first.Identifiers == nil || first.Identifiers.DOI != "10.1000/one" {
		t.Fatalf("DOI request = %+v", first)
	}
	second := submitter.requests[1]
	if second.RequestID != "request_zotio_EF56GH78" || second.Title != "Tuple paper" || second.Year != 2024 || len(second.Authors) != 1 {
		t.Fatalf("tuple request = %+v", second)
	}
	if second.DesiredVersion != "published" || second.MaxCostUSD == nil || *second.MaxCostUSD != 2.5 || len(second.SourcesDeny) != 1 || second.SourcesDeny[0] != "unpaywall" {
		t.Fatalf("policy propagation = %+v", second)
	}
	if result.Skipped[0].ZotioItemKey != "JK90LM12" || result.Skipped[0].Reason == "" {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

// A DOI-bearing queue row must still carry the Zotero item's creators/year: the
// missing-PDF list row omits them, so the request is enriched from GetItem.
// Regression for authorless `--from-zotio` requests that broke bundle export.
func TestQueueMissingPDFEnrichesDOIItemsWithAuthors(t *testing.T) {
	cli := &fakeCLI{
		items: []MissingPDFItem{
			{Key: "AB12CD34", Title: "list-row title", DOI: "https://doi.org/10.1000/One"},
		},
		details: map[string]*Item{
			"AB12CD34": {Key: "AB12CD34", Title: "canonical title", Authors: []string{"Lee, John D.", "See, Katrina A."}, Year: 2004},
		},
	}
	submitter := &fakeSubmitter{}
	service := &Service{CLI: cli, Submitter: submitter}
	if _, err := service.QueueMissingPDF(context.Background(), QueueOptions{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if len(submitter.requests) != 1 {
		t.Fatalf("requests = %+v", submitter.requests)
	}
	req := submitter.requests[0]
	if req.Identifiers == nil || req.Identifiers.DOI != "10.1000/one" {
		t.Fatalf("DOI not anchored: %+v", req.Identifiers)
	}
	if len(req.Authors) != 2 || req.Authors[0] != "Lee, John D." || req.Year != 2004 {
		t.Fatalf("authors/year not enriched from item: authors=%v year=%d", req.Authors, req.Year)
	}
	if req.Title != "canonical title" {
		t.Fatalf("title not refreshed from item: %q", req.Title)
	}
}

// When the item lookup misses, a DOI row still queues (DOI-anchored), never
// erroring — enrichment is best-effort, not a new hard dependency.
func TestQueueMissingPDFDOIItemQueuesWhenItemLookupMisses(t *testing.T) {
	cli := &fakeCLI{
		items: []MissingPDFItem{
			{Key: "AB12CD34", Title: "list-row title", DOI: "https://doi.org/10.1000/One"},
		},
	}
	submitter := &fakeSubmitter{}
	service := &Service{CLI: cli, Submitter: submitter}
	if _, err := service.QueueMissingPDF(context.Background(), QueueOptions{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if len(submitter.requests) != 1 {
		t.Fatalf("requests = %+v", submitter.requests)
	}
	req := submitter.requests[0]
	if req.Identifiers == nil || req.Identifiers.DOI != "10.1000/one" {
		t.Fatalf("DOI not anchored on lookup miss: %+v", req.Identifiers)
	}
	if len(req.Authors) != 0 || req.Title != "list-row title" {
		t.Fatalf("expected DOI-only fallback: authors=%v title=%q", req.Authors, req.Title)
	}
}

func TestQueueMissingPDFDefaultsLimitAndDesiredVersion(t *testing.T) {
	items := make([]MissingPDFItem, 26)
	for i := range items {
		items[i] = MissingPDFItem{
			Key:   fmt.Sprintf("ITEM%04d", i),
			Title: "Paper",
			DOI:   "10.1000/default",
		}
	}
	cli := &fakeCLI{items: items}
	service := &Service{CLI: cli, Submitter: &fakeSubmitter{}}
	result, err := service.QueueMissingPDF(context.Background(), QueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cli.limit != 0 || len(result.Queued) != 25 || result.Preflight.Version != "1.0.0" {
		t.Fatalf("limit=%d queued=%d preflight=%+v", cli.limit, len(result.Queued), result.Preflight)
	}
}

func TestLookupWorksUnconfiguredDegradesToNotOwned(t *testing.T) {
	result, err := (&Service{}).LookupWorks(context.Background(), LookupWorksRequest{Works: []LookupWork{
		{DOI: "10.1000/anything"},
		{ArXiv: "arXiv:2601.12345v2"},
	}})
	if err != nil {
		t.Fatalf("unconfigured lookup must degrade, not error: %v", err)
	}
	if len(result.Works) != 2 {
		t.Fatalf("works = %d, want 2", len(result.Works))
	}
	for i, ownership := range result.Works {
		if ownership.Status != OwnershipNotOwned {
			t.Fatalf("work %d status = %q, want not_owned", i, ownership.Status)
		}
	}
	if result.StalenessWarning != "Zotio is not configured; ownership was not checked" {
		t.Fatalf("staleness warning = %q", result.StalenessWarning)
	}
	if _, err := (&Service{}).LookupWorks(context.Background(), LookupWorksRequest{}); err == nil {
		t.Fatal("empty request must still error on bounds")
	}
}

func TestLookupWorksClassifiesOwnedPDFMissingPDFAndNewWork(t *testing.T) {
	cli := &fakeCLI{
		items: []MissingPDFItem{{Key: "MISS0001"}},
		find: map[string]json.RawMessage{
			"doi:10.1000/with-pdf": json.RawMessage(`[{"key":"PDF00001","data":{}}]`),
			"doi:10.1000/missing":  json.RawMessage(`[{"key":"MISS0001","data":{}}]`),
			"arxiv:2601.12345v2":   json.RawMessage(`[]`),
		},
		syncErr: errors.New("offline"),
	}
	result, err := (&Service{CLI: cli}).LookupWorks(context.Background(), LookupWorksRequest{Works: []LookupWork{
		{DOI: "10.1000/with-pdf"},
		{DOI: "10.1000/missing"},
		{ArXiv: "arXiv:2601.12345v2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cli.syncCalls != 1 || cli.collection != "" || cli.limit != 500 {
		t.Fatalf("mirror calls = sync:%d collection:%q limit:%d", cli.syncCalls, cli.collection, cli.limit)
	}
	if result.StalenessWarning == "" {
		t.Fatal("missing staleness warning after failed sync")
	}
	want := []WorkOwnership{
		{Status: OwnershipOwnedWithPDF, ItemKey: "PDF00001"},
		{Status: OwnershipOwnedMissingPDF, ItemKey: "MISS0001"},
		{Status: OwnershipNotOwned},
	}
	if len(result.Works) != len(want) {
		t.Fatalf("works = %+v", result.Works)
	}
	for i := range want {
		if result.Works[i] != want[i] {
			t.Fatalf("work %d = %+v, want %+v", i, result.Works[i], want[i])
		}
	}
}

// Regression: repeat backfill runs re-counted items whose deterministic
// request already had a live job, turning stuck jobs into recurring
// "queued 1" notifications. Live requests must report as skipped.
func TestQueueMissingPDFSkipsItemsWithLiveJobs(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seed := func(requestID, jobID, state string) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO work_requests (id, created_at) VALUES (?, '2026-07-20T00:00:00Z')`, requestID); err != nil {
			t.Fatalf("seed request: %v", err)
		}
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
			 VALUES (?, ?, ?, '{}', '2026-07-20T00:00:00Z', '2026-07-20T00:00:00Z')`,
			jobID, requestID, state); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	// LIVEKEY01 has an in-flight job; DONEKEY01's only job is terminal.
	seed("request_zotio_LIVEKEY01", "job_live", "awaiting_human")
	seed("request_zotio_DONEKEY01", "job_done", "failed")

	cli := &fakeCLI{items: []MissingPDFItem{
		{Key: "LIVEKEY01", Title: "Stuck paper", DOI: "10.1000/live"},
		{Key: "DONEKEY01", Title: "Retryable paper", DOI: "10.1000/done"},
	}}
	submitter := &fakeSubmitter{}
	service := &Service{CLI: cli, Submitter: submitter, Store: st}

	result, err := service.QueueMissingPDF(ctx, QueueOptions{Limit: 1})
	if err != nil {
		t.Fatalf("QueueMissingPDF: %v", err)
	}
	if cli.limit != 0 {
		t.Fatalf("missing-PDF limit = %d, want complete scan", cli.limit)
	}
	if len(result.Queued) != 1 || result.Queued[0].ZotioItemKey != "DONEKEY01" {
		t.Fatalf("queued = %+v, want only DONEKEY01", result.Queued)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].ZotioItemKey != "LIVEKEY01" {
		t.Fatalf("skipped = %+v, want only LIVEKEY01", result.Skipped)
	}
	if want := "already queued as job_live"; result.Skipped[0].Reason != want {
		t.Fatalf("skip reason = %q, want %q", result.Skipped[0].Reason, want)
	}
	if len(submitter.requests) != 1 || submitter.requests[0].RequestID != "request_zotio_DONEKEY01" {
		t.Fatalf("submitted = %+v, want only the terminal-job item", submitter.requests)
	}
}

func TestQueueMissingPDFBoundsSkippedOutputWhileScanning(t *testing.T) {
	cli := &fakeCLI{items: []MissingPDFItem{
		{Key: "SKIPKEY01", Title: "Invalid DOI", DOI: "not a DOI"},
		{Key: "SKIPKEY02", Title: "Also invalid", DOI: "still not a DOI"},
		{Key: "QUEUEKEY1", Title: "Queue me", DOI: "10.1000/queue"},
	}}
	submitter := &fakeSubmitter{}
	service := &Service{CLI: cli, Submitter: submitter}

	result, err := service.QueueMissingPDF(context.Background(), QueueOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cli.limit != 0 {
		t.Fatalf("missing-PDF limit = %d, want complete scan", cli.limit)
	}
	if len(result.Queued) != 1 || result.Queued[0].ZotioItemKey != "QUEUEKEY1" {
		t.Fatalf("queued = %+v, want only QUEUEKEY1", result.Queued)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].ZotioItemKey != "SKIPKEY01" {
		t.Fatalf("skipped = %+v, want bounded output beginning with SKIPKEY01", result.Skipped)
	}
	if len(submitter.requests) != 1 || submitter.requests[0].ZotioItemKey != "QUEUEKEY1" {
		t.Fatalf("submitted = %+v, want only QUEUEKEY1", submitter.requests)
	}
}
