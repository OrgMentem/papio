// Copyright 2026 OrgMentem. Licensed under MIT.

package retraction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/zotio"
)

func TestRetractedDOIsListsOnlyRetractionsAndIgnoresAcknowledgement(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	for i, doi := range []string{"10.1234/retracted", "10.1234/corrected", "10.1234/concern"} {
		addReadyDOI(t, jobs, doi, i+1)
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	withdrawn := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if withdrawn {
			writeRetractionDataset(t, w, csvNotice{title: "Other", noticeDOI: "10.2000/n9", workDOI: "10.1234/unrelated", nature: "retraction"})
			return
		}
		writeRetractionDataset(t, w,
			csvNotice{title: "R", noticeDOI: "10.2000/n1", workDOI: "10.1234/retracted", nature: "retraction"},
			csvNotice{title: "C", noticeDOI: "10.2000/n2", workDOI: "10.1234/corrected", nature: "correction"},
			csvNotice{title: "E", noticeDOI: "10.2000/n3", workDOI: "10.1234/concern", nature: "expression-of-concern"},
		)
	}))
	defer server.Close()
	sentinel := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(),
		Notifier: &recordingNotifier{}, Now: func() time.Time { return now },
	})

	if _, err := sentinel.RetractedDOIs(ctx); err == nil {
		t.Fatal("RetractedDOIs before any sweep = nil error, want unknown notice set")
	}
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := sentinel.RetractedDOIs(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"10.1234/retracted"}) {
		t.Fatalf("RetractedDOIs = %v, %v; want only the retraction (correction and concern excluded)", got, err)
	}

	items, err := sentinel.SnapshotItems(ctx, nil)
	if err != nil || len(items) != 3 {
		t.Fatalf("items = %v, %v", items, err)
	}
	for _, item := range items {
		if _, err := sentinel.AcknowledgeRetraction(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := sentinel.SnapshotItems(ctx, nil); len(left) != 0 {
		t.Fatalf("items after acknowledge = %v, want none", left)
	}
	got, err = sentinel.RetractedDOIs(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"10.1234/retracted"}) {
		t.Fatalf("RetractedDOIs after acknowledge = %v, %v; acknowledging must not drop the retraction", got, err)
	}

	withdrawn = true
	now = now.Add(25 * time.Hour)
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = sentinel.RetractedDOIs(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("RetractedDOIs after withdrawal = %v, %v; want none", got, err)
	}
}

type e2eZotio struct {
	tags map[string]bool
	ops  []string
}

func (f *e2eZotio) Preflight(context.Context) (*zotio.PreflightResult, error) {
	return &zotio.PreflightResult{Executable: "zotio", Version: "0.13.0"}, nil
}
func (f *e2eZotio) MissingPDF(context.Context, string, int) ([]zotio.MissingPDFItem, error) {
	return nil, nil
}
func (f *e2eZotio) GetItem(context.Context, string) (*zotio.Item, error) { return nil, nil }
func (f *e2eZotio) Sync(context.Context) error                           { return nil }
func (f *e2eZotio) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "items find --doi") {
		doi := args[len(args)-1]
		key := map[string]string{"10.1234/retracted": "AAAAAAAA", "10.1234/corrected": "BBBBBBBB"}[doi]
		return json.Marshal(map[string]any{"results": []map[string]any{{"key": key, "data": map[string]any{"DOI": doi}}}})
	}
	key := args[len(args)-1]
	f.ops = append(f.ops, joined)
	if strings.Contains(joined, "tags add") {
		f.tags[key] = true
	} else {
		delete(f.tags, key)
	}
	return json.Marshal(map[string]any{"result": map[string]any{"items": []map[string]any{{"key": key, "status": "applied"}}}})
}

// TestRetractionTagEndToEndFromSentinelToZotero drives the real sentinel into
// the zotio reconciler: a sweep with a retraction and a correction tags only
// the retracted item, acknowledging the inbox alert leaves the tag, and a
// withdrawn notice removes it.
func TestRetractionTagEndToEndFromSentinelToZotero(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/retracted", 1)
	addReadyDOI(t, jobs, "10.1234/corrected", 2)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	withdrawn := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if withdrawn {
			writeRetractionDataset(t, w, csvNotice{title: "Other", noticeDOI: "10.2000/n9", workDOI: "10.1234/unrelated", nature: "retraction"})
			return
		}
		writeRetractionDataset(t, w,
			csvNotice{title: "R", noticeDOI: "10.2000/n1", workDOI: "10.1234/retracted", nature: "retraction"},
			csvNotice{title: "C", noticeDOI: "10.2000/n2", workDOI: "10.1234/corrected", nature: "correction"},
		)
	}))
	defer server.Close()
	sentinel := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(),
		Notifier: &recordingNotifier{}, Now: func() time.Time { return now },
	})
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	cli := &e2eZotio{tags: map[string]bool{}}
	service := &zotio.Service{CLI: cli, Store: jobs, RetractionTags: true, Retractions: sentinel}

	if _, err := service.ReconcileRetractionTags(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !cli.tags["AAAAAAAA"] || cli.tags["BBBBBBBB"] || len(cli.tags) != 1 {
		t.Fatalf("tags = %v, want only the retracted item tagged", cli.tags)
	}
	items, err := sentinel.SnapshotItems(ctx, nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %v, %v", items, err)
	}
	for _, item := range items {
		if _, err := sentinel.AcknowledgeRetraction(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.ReconcileRetractionTags(ctx, true); err != nil || !cli.tags["AAAAAAAA"] {
		t.Fatalf("acknowledging removed the tag: tags=%v err=%v", cli.tags, err)
	}
	withdrawn = true
	now = now.Add(25 * time.Hour)
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReconcileRetractionTags(ctx, true); err != nil || len(cli.tags) != 0 {
		t.Fatalf("withdrawn notice left tags=%v err=%v", cli.tags, err)
	}
}

func TestRetractedDOIsRefusesFailureOnlyAndTruncatedCaches(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/retracted", 1)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		writeRetractionDataset(t, w, csvNotice{title: "R", noticeDOI: "10.2000/n1", workDOI: "10.1234/retracted", nature: "retraction"})
	}))
	defer server.Close()
	dir := t.TempDir()
	sentinel := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: dir,
		Notifier: &recordingNotifier{}, Now: func() time.Time { return now },
	})
	_ = sentinel.RunDue(ctx)
	if got, err := sentinel.RetractedDOIs(ctx); err == nil {
		t.Fatalf("failure-only cache returned %v, nil; want an error", got)
	}
	fail = false
	now = now.Add(25 * time.Hour)
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sentinel.RetractedDOIs(ctx); err != nil {
		t.Fatal(err)
	}
	sentinel.mu.Lock()
	cached, _, _ := sentinel.readCacheDetailed()
	cached.Truncated = true
	if err := sentinel.writeCache(cached); err != nil {
		t.Fatal(err)
	}
	sentinel.mu.Unlock()
	if got, err := sentinel.RetractedDOIs(ctx); err == nil {
		t.Fatalf("truncated cache returned %v, nil; want an error", got)
	}
}
