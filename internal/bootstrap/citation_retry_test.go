// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

type citationRetryLookup struct {
	err   error
	calls int
}

func (l *citationRetryLookup) LookupWork(context.Context, string) (discovery.DiscoveredWork, error) {
	l.calls++
	if l.err != nil {
		return discovery.DiscoveredWork{}, l.err
	}
	return discovery.DiscoveredWork{Work: work.Work{Title: "Recovered title", Authors: []string{"Smith"}, Year: 2024}}, nil
}

// Validate persisted citation data, rather than echoing the wrapper's outcome.
type citationValidatingImporter struct {
	jobs  *job.Store
	calls int
}

func (i *citationValidatingImporter) PlanAndApply(ctx context.Context, id string) (string, string, string, error) {
	i.calls++
	row, err := i.jobs.Get(ctx, id)
	if err != nil {
		return "", "", "", err
	}
	if row.Work.Title == "" || len(row.Work.Authors) == 0 {
		return "", "", "", errors.New("no citation record for this paper")
	}
	return "applied", "PARENT", "ATTACH", nil
}

func TestCitationLookupFailureDoesNotSpendImportAttempts(t *testing.T) {
	for _, failure := range []string{"budget", "upstream"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, storetest.DataDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			jobs := &job.Store{S: db}
			id, err := jobs.CreateRequest(ctx, "citation-retry", work.Work{DOI: "10.1000/citation"}, "", "", job.Policy{AutoImport: true}, nil, job.PrincipalCLI)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET state='ready' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			svc := app.New(config.Default(), jobs, nil, nil)
			now := time.Now().UTC()
			svc.Now = func() time.Time { return now }
			lookup := &citationRetryLookup{}
			svc.Discovery = lookup
			next := &citationValidatingImporter{jobs: jobs}
			svc.AutoImporter = citationEnrichingImporter{svc: svc, next: next}
			for pass := range 7 {
				now = now.Add(24 * time.Hour)
				lookup.err = errors.New("metadata source returned HTTP 503")
				if failure == "budget" {
					lookup.err = &budget.ErrDeferred{Until: now.Add(time.Hour)}
				}
				if err := svc.ImportRetrier().RunDue(ctx); err != nil {
					t.Fatal(err)
				}
				events, err := jobs.Events(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				var waits int
				for _, event := range events {
					if event["kind"] != "zotio.auto_import" {
						continue
					}
					detail, _ := event["detail"].(map[string]any)
					message, _ := detail["error_message"].(string)
					if detail["status"] != "queued" || detail["reason"] != "citation_metadata_unavailable" || !strings.Contains(message, "lookup unavailable") {
						t.Fatalf("lookup failure spent an import attempt or lost its cause: %+v", detail)
					}
					waits++
				}
				if waits != pass+1 || next.calls != 0 {
					t.Fatalf("pass %d: waits=%d imports=%d", pass, waits, next.calls)
				}
				latest := events[len(events)-1]["detail"].(map[string]any)
				retryAt, err := time.Parse(time.RFC3339Nano, latest["retry_at"].(string))
				if err != nil {
					t.Fatal(err)
				}
				delays := []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 12 * time.Hour}
				wantDelay := delays[min(pass, len(delays)-1)]
				if failure == "budget" {
					wantDelay = max(wantDelay, time.Hour)
				}
				if got := retryAt.Sub(now); got != wantDelay {
					t.Fatalf("pass %d retry delay = %v, want %v", pass, got, wantDelay)
				}
				before := lookup.calls
				if err := svc.ImportRetrier().RunDue(ctx); err != nil {
					t.Fatal(err)
				}
				if lookup.calls != before {
					t.Fatal("metadata lookup ran before its retry time")
				}
			}
			now = now.Add(24 * time.Hour)
			lookup.err = nil
			if err := svc.ImportRetrier().RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			if next.calls != 1 {
				t.Fatalf("imports after recovery = %d, want 1", next.calls)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			last := events[len(events)-1]["detail"].(map[string]any)
			if last["status"] != "applied" {
				t.Fatalf("recovered import outcome = %+v", last)
			}
		})
	}
}

func TestCitationLookupNotFoundReachesBundleValidation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs := &job.Store{S: db}
	id, err := jobs.CreateRequest(ctx, "citation-not-found", work.Work{DOI: "10.1000/absent"}, "", "", job.Policy{}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	svc := app.New(config.Default(), jobs, nil, nil)
	svc.Discovery = discovery.NewWithOptions(discovery.Options{BaseURL: server.URL, ContactEmail: "test@example.org"})
	next := &citationValidatingImporter{jobs: jobs}
	importer := citationEnrichingImporter{svc: svc, next: next}
	_, _, _, err = importer.PlanAndApply(ctx, id)
	if next.calls != 1 || err == nil || err.Error() != "no citation record for this paper" {
		t.Fatalf("absent citation: imports=%d error=%v, want bundle validation", next.calls, err)
	}
}
