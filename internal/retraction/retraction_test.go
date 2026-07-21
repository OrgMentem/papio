// Copyright 2026 OrgMentem. Licensed under MIT.

package retraction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/work"
)

type recordingBudget struct {
	mu       sync.Mutex
	acquires int
	err      error
}

func (b *recordingBudget) Acquire(_ context.Context, source string, policy config.Source, cost float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if source != config.SourceRetractionWatch {
		return fmt.Errorf("source = %q", source)
	}
	if !policy.Enabled || cost != 0 {
		return fmt.Errorf("policy/cost = %+v/%v", policy, cost)
	}
	b.acquires++
	return b.err
}

type recordingNotifier struct {
	events []notify.Event
}

func (n *recordingNotifier) Send(_ context.Context, _ string) {}

func (n *recordingNotifier) SendEvent(_ context.Context, event notify.Event) {
	n.events = append(n.events, event)
}

func TestSweepExposesRecognizedNotices(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		update     string
		wantNature Nature
	}{
		{name: "retraction", update: "retraction", wantNature: NatureRetraction},
		{name: "correction", update: "correction", wantNature: NatureCorrection},
		{name: "concern", update: "expression-of-concern", wantNature: NatureConcern},
		{name: "none", update: "", wantNature: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := testStore(t)
			addReadyDOI(t, jobs, "10.1234/original", 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.update == "" {
					_, _ = w.Write([]byte(`{"message":{"update-to":[]}}`))
					return
				}
				_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"10.2000/notice","updated":"` + tc.update + `"}]}}`))
			}))
			defer server.Close()
			budget := &recordingBudget{}
			notifier := &recordingNotifier{}
			sentinel := New(Options{
				Store: jobs, Budgets: budget, Policy: config.Source{Enabled: true, RatePerSec: 1, Burst: 1},
				Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(), Notifier: notifier,
				Now: func() time.Time { return now },
			})
			if err := sentinel.RunDue(ctx); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			items, err := sentinel.SnapshotItems(ctx, nil)
			if err != nil {
				t.Fatalf("snapshot items: %v", err)
			}
			if tc.wantNature == "" {
				if len(items) != 0 || len(notifier.events) != 0 {
					t.Fatalf("items/events = %#v/%#v, want none", items, notifier.events)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %#v, want one", items)
			}
			item := items[0]
			if item.ID != "retraction:10.1234/original" || item.Kind != "retraction" || item.Retraction == nil {
				t.Fatalf("item core = %+v", item)
			}
			if got := item.Retraction; got.DOI != "10.1234/original" || got.Nature != string(tc.wantNature) || !got.NoticedAt.Equal(now) || got.NoticeDOI != "10.2000/notice" {
				t.Fatalf("retraction = %+v", got)
			}
			if len(notifier.events) != 1 || notifier.events[0].Kind != "library.retraction" {
				t.Fatalf("events = %#v", notifier.events)
			}
			if budget.acquires != 1 {
				t.Fatalf("budget acquires = %d, want 1", budget.acquires)
			}
		})
	}
}

func TestSweepDeduplicatesReadyDOIAndPersistsNotice(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/duplicate", 1)
	addReadyDOI(t, jobs, "10.1234/duplicate", 2)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"10.2000/notice","updated":"retraction"}]}}`))
	}))
	defer server.Close()
	budget := &recordingBudget{}
	notifier := &recordingNotifier{}
	dataDir := t.TempDir()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	first := New(Options{Store: jobs, Budgets: budget, Policy: config.Source{Enabled: true}, Client: server.Client(), BaseURL: server.URL, DataDir: dataDir, Notifier: notifier, Now: func() time.Time { return now }})
	if err := first.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || budget.acquires != 1 || len(notifier.events) != 1 {
		t.Fatalf("requests/budget/events = %d/%d/%d, want 1/1/1", requests, budget.acquires, len(notifier.events))
	}

	second := New(Options{Store: jobs, Budgets: budget, Policy: config.Source{Enabled: true}, Client: server.Client(), BaseURL: server.URL, DataDir: dataDir, Notifier: notifier, Now: func() time.Time { return now.Add(25 * time.Hour) }})
	if err := second.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(notifier.events) != 1 {
		t.Fatalf("repeat sweep requests/events = %d/%d, want 2/1", requests, len(notifier.events))
	}
	items, err := second.SnapshotItems(ctx, nil)
	if err != nil || len(items) != 1 || items[0].Retraction == nil || !items[0].Retraction.NoticedAt.Equal(now) {
		t.Fatalf("restart snapshot = %#v, %v", items, err)
	}

	var persisted cache
	data, err := os.ReadFile(filepath.Join(dataDir, cacheFileName))
	if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.Version != cacheVersion || len(persisted.Notices) != 1 {
		t.Fatalf("cache = %#v, read err = %v", persisted, err)
	}
}

func TestSweepTemporaryAndMalformedResponsesFailClosed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		temporary bool
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, temporary: true},
		{name: "malformed json", status: http.StatusOK, body: `{`, temporary: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := testStore(t)
			addReadyDOI(t, jobs, "10.1234/failure", 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			sentinel := New(Options{Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true}, Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir()})
			err := sentinel.RunDue(ctx)
			if err == nil {
				t.Fatal("sweep succeeded")
			}
			var temporary *resolver.TemporaryError
			if errors.As(err, &temporary) != tc.temporary {
				t.Fatalf("temporary = %v for %v, want %v", temporary, err, tc.temporary)
			}
			items, snapshotErr := sentinel.SnapshotItems(ctx, nil)
			if snapshotErr != nil || len(items) != 0 {
				t.Fatalf("items after failed sweep = %#v, %v", items, snapshotErr)
			}
		})
	}
}

func TestSweepRespectsDisabledPolicyAndBudget(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/budget", 1)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()

	disabled := New(Options{Store: jobs, Budgets: &recordingBudget{}, Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir()})
	if err := disabled.RunDue(ctx); err != nil || requests != 0 {
		t.Fatalf("disabled sweep/request = %v/%d", err, requests)
	}

	budget := &recordingBudget{err: errors.New("budget exhausted")}
	enabled := New(Options{Store: jobs, Budgets: budget, Policy: config.Source{Enabled: true}, Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir()})
	if err := enabled.RunDue(ctx); err == nil || requests != 0 || budget.acquires != 1 {
		t.Fatalf("budget sweep/request/acquires = %v/%d/%d", err, requests, budget.acquires)
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func addReadyDOI(t *testing.T, db *store.Store, doi string, index int) {
	t.Helper()
	jobs := &job.Store{S: db}
	id, err := jobs.CreateRequest(context.Background(), fmt.Sprintf("wr_retraction_%02d", index), work.Work{DOI: doi, Title: "Library work"}, "", "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", Resolver: "test", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	sha := fmt.Sprintf("%064x", index)
	if err := jobs.UpsertArtifact(context.Background(), job.Artifact{SHA256: sha, SizeBytes: 1, MIME: "application/pdf", Path: "/tmp/artifact.pdf", IdentityResult: "pass"}); err != nil {
		t.Fatalf("upsert artifact: %v", err)
	}
	for _, transition := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := jobs.Transition(context.Background(), id, transition[0], transition[1], nil); err != nil {
			t.Fatalf("transition %s->%s: %v", transition[0], transition[1], err)
		}
	}
	if err := jobs.Transition(context.Background(), id, job.StateValidating, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatalf("ready transition: %v", err)
	}
}
