// Copyright 2026 OrgMentem. Licensed under MIT.

package retraction

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

func (n *recordingNotifier) Route(_ context.Context, intent notify.Intent) error {
	n.events = append(n.events, intent.Detail)
	return nil
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

// A notice is recomputed from Crossref for as long as the work stays in the
// library, so acknowledging one has to survive the next sweep — but only for
// the notice that was acknowledged.
func TestAcknowledgeHidesNoticeUntilTheNoticeChanges(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/original", 1)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	nature := "expression-of-concern"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"10.2000/notice","updated":"` + nature + `"}]}}`))
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
	items, err := sentinel.SnapshotItems(ctx, nil)
	if err != nil || len(items) != 1 {
		t.Fatalf("initial items = %#v, %v", items, err)
	}
	if got := items[0].Ops; !slices.Contains(got, "dismiss") {
		t.Fatalf("retraction ops = %v, want a dismiss operation", got)
	}

	applied, err := sentinel.AcknowledgeRetraction(ctx, items[0].ID)
	if err != nil || !applied {
		t.Fatalf("acknowledge = %v, %v; want true", applied, err)
	}
	if items, err := sentinel.SnapshotItems(ctx, nil); err != nil || len(items) != 0 {
		t.Fatalf("items after acknowledge = %#v, %v; want none", items, err)
	}
	if applied, err := sentinel.AcknowledgeRetraction(ctx, items[0].ID); err != nil || applied {
		t.Fatalf("repeat acknowledge = %v, %v; want false without an error", applied, err)
	}
	if _, err := sentinel.AcknowledgeRetraction(ctx, "retraction:10.1234/unknown"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("acknowledge of an unknown notice = %v, want sql.ErrNoRows", err)
	}

	// The same notice survives a re-sweep acknowledged.
	now = now.Add(25 * time.Hour)
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if items, err := sentinel.SnapshotItems(ctx, nil); err != nil || len(items) != 0 {
		t.Fatalf("items after re-sweep = %#v, %v; want none", items, err)
	}

	// An escalated nature is a different notice and must surface again, and the
	// superseded acknowledgement must not linger in the database.
	nature = "retraction"
	now = now.Add(25 * time.Hour)
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	escalated, err := sentinel.SnapshotItems(ctx, nil)
	if err != nil || len(escalated) != 1 || escalated[0].Retraction.Nature != "retraction" {
		t.Fatalf("items after escalation = %#v, %v; want the retraction notice", escalated, err)
	}
	var acks int
	if err := jobs.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM retraction_acks`).Scan(&acks); err != nil {
		t.Fatal(err)
	}
	if acks != 0 {
		t.Fatalf("acknowledgement rows = %d, want the superseded row pruned", acks)
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

func TestSweepCommitsPartialResultsAndRetainsFailedDOI(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/failing", 1)
	addReadyDOI(t, jobs, "10.1234/fresh", 2)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	previous := Finding{
		DOI: "10.1234/failing", Nature: NatureRetraction, NoticeDOI: "10.2000/previous",
		NoticedAt: now.Add(-48 * time.Hour),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/10.1234/failing" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"10.2000/fresh","updated":"correction"}]}}`))
	}))
	defer server.Close()
	notifier := &recordingNotifier{}
	sentinel := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(), Notifier: notifier,
		Now: func() time.Time { return now },
	})
	sentinel.mu.Lock()
	err := sentinel.writeCache(cache{
		Version: cacheVersion, CheckedAt: now.Add(-sweepEvery), Notices: map[string]Finding{
			findingKey(previous): previous,
		},
	})
	sentinel.mu.Unlock()
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatalf("partial sweep: %v", err)
	}

	sentinel.mu.Lock()
	persisted, ok := sentinel.readCache()
	sentinel.mu.Unlock()
	if !ok || len(persisted.Notices) != 2 {
		t.Fatalf("cache = %#v, valid = %v", persisted, ok)
	}
	if got := persisted.Notices[findingKey(previous)]; got != previous {
		t.Fatalf("carried finding = %#v, want %#v", got, previous)
	}
	fresh := Finding{DOI: "10.1234/fresh", Nature: NatureCorrection, NoticeDOI: "10.2000/fresh", NoticedAt: now}
	if got := persisted.Notices[findingKey(fresh)]; got != fresh {
		t.Fatalf("fresh finding = %#v, want %#v", got, fresh)
	}
	if len(notifier.events) != 1 || notifier.events[0].Message != noticeMessage(fresh) {
		t.Fatalf("events = %#v", notifier.events)
	}
}

// TestIntegrityNoticeCopyNamesARecoverableSurface pins the two forms of scan
// copy: one finding keeps its DOI identity, a whole scan reuses the shared
// integrity vocabulary, and neither leaves the reader without a papio surface
// on which the durable notices can be found.
func TestIntegrityNoticeCopyNamesARecoverableSurface(t *testing.T) {
	retracted := Finding{DOI: "10.1234/one", Nature: NatureRetraction, NoticeDOI: "10.2000/one"}
	corrected := Finding{DOI: "10.1234/two", Nature: NatureCorrection}
	cases := []struct {
		name     string
		findings []Finding
		want     string
	}{
		{name: "one finding with a notice DOI", findings: []Finding{retracted},
			want: "Library retraction notice for DOI 10.1234/one (notice DOI 10.2000/one) — open the papio inbox"},
		{name: "one finding without a notice DOI", findings: []Finding{corrected},
			want: "Library correction notice for DOI 10.1234/two — open the papio inbox"},
		{name: "whole scan", findings: []Finding{retracted, corrected},
			want: "2 library integrity notices — open the papio inbox"},
	}
	for _, tc := range cases {
		if got := integrityNoticeMessage(tc.findings); got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSweepAllLookupFailuresLeaveCacheUntouched(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/first", 1)
	addReadyDOI(t, jobs, "10.1234/second", 2)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	previous := Finding{
		DOI: "10.1234/first", Nature: NatureRetraction, NoticeDOI: "10.2000/previous",
		NoticedAt: now.Add(-48 * time.Hour),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	dataDir := t.TempDir()
	sentinel := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: dataDir, Now: func() time.Time { return now },
	})
	sentinel.mu.Lock()
	err := sentinel.writeCache(cache{
		Version: cacheVersion, CheckedAt: now.Add(-sweepEvery), Notices: map[string]Finding{
			findingKey(previous): previous,
		},
	})
	sentinel.mu.Unlock()
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dataDir, cacheFileName))
	if err != nil {
		t.Fatalf("read initial cache: %v", err)
	}

	if err := sentinel.RunDue(ctx); err == nil {
		t.Fatal("all-failed sweep succeeded")
	}

	after, err := os.ReadFile(filepath.Join(dataDir, cacheFileName))
	if err != nil {
		t.Fatalf("read final cache: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("cache changed after total failure:\n before = %s\n after = %s", before, after)
	}
}

func TestSnapshotItemsDoesNotWaitForSweepLookup(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/blocked", 1)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	previous := Finding{
		DOI: "10.1234/blocked", Nature: NatureRetraction, NoticeDOI: "10.2000/previous",
		NoticedAt: now.Add(-48 * time.Hour),
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"message":{"update-to":[]}}`))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	sentinel := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(), Now: func() time.Time { return now },
	})
	sentinel.mu.Lock()
	err := sentinel.writeCache(cache{
		Version: cacheVersion, CheckedAt: now.Add(-sweepEvery), Notices: map[string]Finding{
			findingKey(previous): previous,
		},
	})
	sentinel.mu.Unlock()
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	sweepDone := make(chan error, 1)
	go func() { sweepDone <- sentinel.RunDue(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sweep did not begin lookup")
	}

	type snapshotResult struct {
		items int
		err   error
	}
	snapshotDone := make(chan snapshotResult, 1)
	go func() {
		items, err := sentinel.SnapshotItems(ctx, nil)
		snapshotDone <- snapshotResult{items: len(items), err: err}
	}()
	select {
	case result := <-snapshotDone:
		if result.err != nil || result.items != 1 {
			t.Fatalf("snapshot while sweeping = %+v", result)
		}
	case <-time.After(time.Second):
		close(release)
		<-sweepDone
		<-snapshotDone
		t.Fatal("snapshot waited for lookup")
	}
	close(release)
	if err := <-sweepDone; err != nil {
		t.Fatalf("sweep: %v", err)
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

func TestSharedNoticeDOIStillSurfacesEachAffectedWork(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	shared := "10.2000/shared"

	t.Run("newFindings includes every affected DOI", func(t *testing.T) {
		jobs := testStore(t)
		addReadyDOI(t, jobs, "10.1234/alpha", 1)
		addReadyDOI(t, jobs, "10.1234/beta", 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"` + shared + `","updated":"retraction"}]}}`))
		}))
		defer server.Close()
		notifier := &recordingNotifier{}
		sentinel := New(Options{
			Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
			Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(), Notifier: notifier,
			Now: func() time.Time { return now },
		})
		if err := sentinel.RunDue(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if len(notifier.events) != 1 {
			t.Fatalf("events = %#v, want 1", notifier.events)
		}
		detail, ok := notifier.events[0].Detail["findings"].([]map[string]any)
		if !ok || len(detail) != 2 {
			t.Fatalf("findings detail = %#v, want 2", notifier.events[0].Detail["findings"])
		}
		got := map[string]bool{}
		for _, row := range detail {
			got[row["doi"].(string)] = true
			if row["notice_doi"].(string) != shared {
				t.Fatalf("notice_doi = %q, want %q", row["notice_doi"], shared)
			}
		}
		if !got["10.1234/alpha"] || !got["10.1234/beta"] {
			t.Fatalf("notification DOIs = %#v, want both affected DOIs", got)
		}
		sentinel.mu.Lock()
		cached, _ := sentinel.readCache()
		sentinel.mu.Unlock()
		if len(cached.Notices) != 2 {
			t.Fatalf("cache notices = %d, want 2", len(cached.Notices))
		}
		if _, ok := cached.Notices[findingKey(Finding{DOI: "10.1234/alpha", Nature: NatureRetraction, NoticeDOI: shared})]; !ok {
			t.Fatal("alpha finding missing from cache")
		}
		if _, ok := cached.Notices[findingKey(Finding{DOI: "10.1234/beta", Nature: NatureRetraction, NoticeDOI: shared})]; !ok {
			t.Fatal("beta finding missing from cache")
		}
		items, err := sentinel.SnapshotItems(ctx, nil)
		if err != nil || len(items) != 2 {
			t.Fatalf("snapshot items = %#v, %v; want 2", items, err)
		}
	})

	t.Run("triage snapshot includes every affected DOI", func(t *testing.T) {
		jobs := testStore(t)
		sentinel := New(Options{
			Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
			DataDir: t.TempDir(), Now: func() time.Time { return now },
		})
		a := Finding{DOI: "10.1234/alpha", Nature: NatureRetraction, NoticeDOI: shared, NoticedAt: now}
		b := Finding{DOI: "10.1234/beta", Nature: NatureRetraction, NoticeDOI: shared, NoticedAt: now}
		sentinel.mu.Lock()
		if err := sentinel.writeCache(cache{Version: cacheVersion, CheckedAt: now, Notices: map[string]Finding{findingKey(a): a, findingKey(b): b}}); err != nil {
			sentinel.mu.Unlock()
			t.Fatalf("seed cache: %v", err)
		}
		sentinel.mu.Unlock()
		items, err := sentinel.SnapshotItems(ctx, nil)
		if err != nil || len(items) != 2 {
			t.Fatalf("snapshot items = %#v, %v; want 2", items, err)
		}
		got := map[string]bool{}
		for _, item := range items {
			got[item.Retraction.DOI] = true
		}
		if !got["10.1234/alpha"] || !got["10.1234/beta"] {
			t.Fatalf("snapshot DOIs = %#v, want both", got)
		}
	})
}

func TestGenuineDuplicateCollapsesToOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	dup := Finding{DOI: "10.1234/alpha", Nature: NatureRetraction, NoticeDOI: "10.2000/shared", NoticedAt: now}

	sentinel := New(Options{
		Store: testStore(t), Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	})
	sentinel.mu.Lock()
	if err := sentinel.writeCache(cache{
		Version: cacheVersion, CheckedAt: now,
		Notices: map[string]Finding{
			findingKey(dup): dup,
			// Different map key for the same per-work identity; validNotices will
			// reject it, leaving only the canonical entry. Either way SnapshotItems
			// must still surface just one item.
			"alias": dup,
		},
	}); err != nil {
		sentinel.mu.Unlock()
		t.Fatalf("seed cache: %v", err)
	}
	sentinel.mu.Unlock()
	items, err := sentinel.SnapshotItems(ctx, nil)
	if err != nil || len(items) != 1 {
		t.Fatalf("snapshot items = %#v, %v; want 1", items, err)
	}
	if items[0].Retraction.DOI != dup.DOI || items[0].Retraction.NoticeDOI != dup.NoticeDOI {
		t.Fatalf("retraction = %+v, want %#v", items[0].Retraction, dup)
	}

	// newFindings path: a sweep that re-discovers the same affected DOI with
	// the same notice must be considered already-seen.
	jobs := testStore(t)
	addReadyDOI(t, jobs, "10.1234/alpha", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"10.2000/shared","updated":"retraction"}]}}`))
	}))
	defer server.Close()
	notifier := &recordingNotifier{}
	earlier := now.Add(-24 * time.Hour)
	sentinel2 := New(Options{
		Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
		Client: server.Client(), BaseURL: server.URL, DataDir: t.TempDir(), Notifier: notifier,
		Now: func() time.Time { return now },
	})
	sentinel2.mu.Lock()
	if err := sentinel2.writeCache(cache{Version: cacheVersion, CheckedAt: earlier, Notices: map[string]Finding{findingKey(dup): Finding{DOI: dup.DOI, Nature: dup.Nature, NoticeDOI: dup.NoticeDOI, NoticedAt: earlier}}}); err != nil {
		sentinel2.mu.Unlock()
		t.Fatalf("seed cache: %v", err)
	}
	sentinel2.mu.Unlock()
	// Advance past sweepEvery so RunDue actually sweeps.
	sentinel2.now = func() time.Time { return earlier.Add(sweepEvery + time.Second) }
	if err := sentinel2.RunDue(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(notifier.events) != 0 {
		t.Fatalf("events = %#v, want none for a genuine duplicate", notifier.events)
	}
}

func TestEmptyAffectedDOIDedupsByNotice(t *testing.T) {
	emptyA := Finding{DOI: "", Nature: NatureRetraction, NoticeDOI: "10.2000/shared", NoticedAt: time.Now()}
	emptyB := Finding{DOI: "", Nature: NatureRetraction, NoticeDOI: "10.2000/shared", NoticedAt: time.Now()}
	if noticeKey(emptyA) != noticeKey(emptyB) {
		t.Fatalf("empty-DOI notice keys differ: %q vs %q", noticeKey(emptyA), noticeKey(emptyB))
	}
	if noticeKey(emptyA) != "10.2000/shared" {
		t.Fatalf("empty-DOI notice key = %q, want the notice DOI alone", noticeKey(emptyA))
	}
	// FindingKey still separates empty-DOI findings from real ones.
	real := Finding{DOI: "10.1234/alpha", Nature: NatureRetraction, NoticeDOI: "10.2000/shared"}
	if findingKey(emptyA) == findingKey(real) {
		t.Fatal("findingKey must distinguish an empty affected DOI from a real one")
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
	id, err := jobs.CreateRequest(context.Background(), fmt.Sprintf("wr_retraction_%02d", index), work.Work{DOI: doi, Title: "Library work"}, "", "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", Resolver: "test", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
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
