// Copyright 2026 OrgMentem. Licensed under MIT.

package retraction

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/config"
)

func TestLibraryCollapseRecoveryWindow(t *testing.T) {
	for _, reducedCount := range []int{0, 1} {
		for _, restored := range []bool{false, true} {
			t.Run(fmt.Sprintf("count=%d/restored=%v", reducedCount, restored), func(t *testing.T) {
				ctx := context.Background()
				jobs := testStore(t)
				dataDir := t.TempDir()
				exportPath := filepath.Join(t.TempDir(), "library.bib")
				sources := []config.LibrarySource{{Name: "export", Kind: config.LibraryKindFile,
					Path: exportPath, Format: "bibtex", Claim: config.LibraryClaimRecordPresent}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					writeRetractionDataset(t, w, csvNotice{title: "Retracted paper", workDOI: "10.1234/paper-99", nature: "retraction"})
				}))
				defer server.Close()
				start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				check := func(hour, count int, complete bool) {
					t.Helper()
					body := ""
					for i := range count {
						body += fmt.Sprintf("@article{k%d, doi = {10.1234/paper-%d}}\n", i, i)
					}
					if err := os.WriteFile(exportPath, []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
					now := start.Add(time.Duration(hour) * time.Hour)
					sentinel := New(Options{Store: jobs, Budgets: &recordingBudget{}, Policy: config.Source{Enabled: true},
						Client: server.Client(), BaseURL: server.URL, DataDir: dataDir,
						Scope: config.RetractionScopeLibrary, LibraryDOIs: NewLibraryCatalog(sources, nil),
						Now: func() time.Time { return now }})
					if err := sentinel.RunDue(ctx); err != nil {
						t.Fatalf("hour %d: %v", hour, err)
					}
					sentinel.mu.Lock()
					cached, ok := sentinel.readCache()
					sentinel.mu.Unlock()
					if !ok || (cached.LastFetchError == "") != complete || cached.Truncated == complete {
						t.Fatalf("hour %d cache = %+v, want complete=%v", hour, cached, complete)
					}
					wantCount, wantNotices := 100, 1
					if complete {
						wantCount = count
						if count != 100 {
							wantNotices = 0
						}
					}
					if cached.LibraryCount != wantCount || len(cached.Notices) != wantNotices {
						t.Fatalf("hour %d cache = %+v, want baseline=%d notices=%d", hour, cached, wantCount, wantNotices)
					}
				}
				check(0, 100, true)
				check(24, reducedCount, false)
				check(48, reducedCount, false)
				if restored {
					check(72, 100, true)
					check(96, reducedCount, false)
					check(120, reducedCount, false)
					check(144, reducedCount, false)
					check(168, reducedCount, true)
				} else {
					check(72, reducedCount, false)
					check(96, reducedCount, true)
					check(120, reducedCount, true)
				}
			})
		}
	}
}

func TestLibraryCollapseResetSurvivesBudgetFailure(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	// Keep a ready work in the corpus so a failed library enumeration still
	// reaches the budget check before attempting the dataset lookup.
	addReadyDOI(t, jobs, "10.1234/acquired", 1)
	dataDir := t.TempDir()
	exportPath := filepath.Join(t.TempDir(), "library.bib")
	source := config.LibrarySource{Name: "export", Kind: config.LibraryKindFile,
		Path: exportPath, Format: "bibtex", Claim: config.LibraryClaimRecordPresent}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRetractionDataset(t, w, csvNotice{title: "Library paper",
			workDOI: "10.1234/library-only", nature: "retraction"})
	}))
	defer server.Close()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	budget := &recordingBudget{}
	newSentinel := func() *Sentinel {
		return New(Options{Store: jobs, Budgets: budget, Policy: config.Source{Enabled: true},
			Client: server.Client(), BaseURL: server.URL, DataDir: dataDir,
			Scope: config.RetractionScopeLibrary, LibraryDOIs: NewLibraryCatalog([]config.LibrarySource{source}, nil),
			Now: func() time.Time { return now }})
	}
	writeExport := func(body string) {
		t.Helper()
		if err := os.WriteFile(exportPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readCache := func() cache {
		t.Helper()
		sentinel := newSentinel()
		sentinel.mu.Lock()
		defer sentinel.mu.Unlock()
		cached, ok := sentinel.readCache()
		if !ok {
			t.Fatal("missing cache")
		}
		return cached
	}

	writeExport("@article{k, doi = {10.1234/library-only}}\n")
	if err := newSentinel().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if applied, err := newSentinel().AcknowledgeRetraction(ctx, "retraction:10.1234/library-only"); err != nil || !applied {
		t.Fatalf("acknowledge = %v, %v", applied, err)
	}
	// Hour zero of the collapse window follows the last good sweep.
	now = now.Add(24 * time.Hour)
	writeExport("")
	if err := newSentinel().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	first := readCache()
	if first.LibraryCollapse.Since == "" {
		t.Fatal("collapsed export did not start a confirmation window")
	}

	now = now.Add(48 * time.Hour)
	if err := os.Remove(exportPath); err != nil {
		t.Fatal(err)
	}
	budget.err = errors.New("budget exhausted")
	if err := newSentinel().RunDue(ctx); !errors.Is(err, budget.err) {
		t.Fatalf("failed enumeration and budget = %v, want budget error", err)
	}
	failed := readCache()
	if failed.LibraryCollapse.Since != "" {
		t.Fatalf("budget failure lost the reset: %+v", failed.LibraryCollapse)
	}
	if failed.LibraryCount != 1 || len(failed.Notices) != 1 {
		t.Fatalf("budget failure changed baseline or notices: %+v", failed)
	}

	now = now.Add(24 * time.Hour)
	budget.err = nil
	writeExport("")
	if err := newSentinel().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	retried := readCache()
	if retried.LastFetchError == "" || !retried.Truncated || retried.LibraryCount != 1 || len(retried.Notices) != 1 {
		t.Fatalf("hour 72 accepted the interrupted collapse: %+v", retried)
	}
	if retried.LibraryCollapse.Since == "" || retried.LibraryCollapse.Since == first.LibraryCollapse.Since {
		t.Fatalf("hour 72 did not start a fresh window: %+v", retried.LibraryCollapse)
	}
	var acks int
	if err := jobs.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM retraction_acks`).Scan(&acks); err != nil {
		t.Fatal(err)
	}
	if acks != 1 {
		t.Fatalf("acknowledgements = %d, want 1", acks)
	}
}
