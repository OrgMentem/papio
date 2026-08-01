// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"papio/internal/resolver"
	"papio/internal/work"
)

func TestEnrichCorroboratesCrossrefSearchResults(t *testing.T) {
	requested := work.Work{Title: "A Precise Study", Year: 2024, Authors: []string{"Jane Smith", "R. Jones"}}
	tests := []struct {
		name    string
		body    string
		matched bool
		wantDOI string
	}{
		{
			name:    "confident match fills normalized DOI and metadata",
			body:    `{"message":{"items":[{"DOI":"https://doi.org/10.1234/EXAMPLE.","title":["  A   Precise Study  "],"author":[{"family":"Smith"}],"published-print":{"date-parts":[[2024]]},"container-title":["Journal of Tests"]}]}}`,
			matched: true, wantDOI: "10.1234/example",
		},
		{
			name: "near miss title is rejected",
			body: `{"message":{"items":[{"DOI":"10.1234/example","title":["A Nearly Precise Study"],"author":[{"family":"Smith"}],"published-print":{"date-parts":[[2024]]}}]}}`,
		},
		{
			name: "year mismatch is rejected",
			body: `{"message":{"items":[{"DOI":"10.1234/example","title":["A Precise Study"],"author":[{"family":"Smith"}],"published-print":{"date-parts":[[2023]]}}]}}`,
		},
		{
			name: "author mismatch is rejected",
			body: `{"message":{"items":[{"DOI":"10.1234/example","title":["A Precise Study"],"author":[{"family":"Brown"}],"published-print":{"date-parts":[[2024]]}}]}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("query.title"); got != requested.Title {
					t.Errorf("query.title = %q, want %q", got, requested.Title)
				}
				if got := r.URL.Query().Get("rows"); got != "5" {
					t.Errorf("rows = %q, want 5", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			enriched, matched, err := NewWithOptions(Options{BaseURL: server.URL}).Enrich(context.Background(), requested)
			if err != nil {
				t.Fatal(err)
			}
			if matched != test.matched {
				t.Fatalf("matched = %v, want %v", matched, test.matched)
			}
			if enriched.DOI != test.wantDOI {
				t.Errorf("DOI = %q, want %q", enriched.DOI, test.wantDOI)
			}
			if matched && (enriched.Year != 2024 || enriched.Container != "Journal of Tests") {
				t.Errorf("enriched metadata = %+v", enriched)
			}
		})
	}
}

// APA and several other publishers deposit article titles with a closing full
// stop; citations and reference managers almost never carry it. Exact-equality
// normalization therefore rejected a perfect Crossref hit on one character.
// The real case: Ryan & Deci 2000, the most-cited work in its literature,
// returned by Crossref at rank 0 with corroborating authors and year, reported
// by papio as no_identifier. Measured over one cohort's 26 clean-title
// failures, 3 were recoverable this way and every one was an APA DOI.
func TestEnrichAdoptsATitleDepositedWithATrailingPeriod(t *testing.T) {
	const bare = "Self-determination theory and the facilitation of intrinsic motivation, social development, and well-being"
	requested := work.Work{Title: bare, Year: 2000, Authors: []string{"Ryan"}}
	body := `{"message":{"items":[{"DOI":"10.1037/0003-066x.55.1.68","title":["` + bare + `."],` +
		`"author":[{"family":"Ryan"},{"family":"Deci"}],"published-print":{"date-parts":[[2000]]}}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	enriched, matched, err := NewWithOptions(Options{BaseURL: server.URL}).Enrich(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || enriched.DOI != "10.1037/0003-066x.55.1.68" {
		t.Fatalf("matched = %v, DOI = %q; a trailing full stop must not reject an otherwise exact match", matched, enriched.DOI)
	}
}

// A title that differs by more than punctuation must still be rejected: the
// stripping folds terminal punctuation, it does not loosen title matching.
func TestEnrichStillRejectsATitleThatDiffersBeyondPunctuation(t *testing.T) {
	requested := work.Work{Title: "A Precise Study", Year: 2024, Authors: []string{"Smith"}}
	body := `{"message":{"items":[{"DOI":"10.1234/example","title":["A Precise Study of Something."],` +
		`"author":[{"family":"Smith"}],"published-print":{"date-parts":[[2024]]}}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	if _, matched, err := NewWithOptions(Options{BaseURL: server.URL}).Enrich(context.Background(), requested); err != nil || matched {
		t.Fatalf("matched = %v, err = %v; want rejection", matched, err)
	}
}

// Every other papio source client identifies itself to its provider's polite
// pool, and doctor reports that identity as configured. The Crossref enricher
// was the one client omitting it.
func TestEnrichIdentifiesItselfToCrossrefsPolitePool(t *testing.T) {
	got := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.Query().Get("mailto")
		_, _ = w.Write([]byte(`{"message":{"items":[]}}`))
	}))
	defer server.Close()

	enricher := NewWithOptions(Options{BaseURL: server.URL, ContactEmail: "researcher@example.org"})
	if _, _, err := enricher.Enrich(context.Background(), work.Work{Title: "Any Title"}); err != nil {
		t.Fatal(err)
	}
	if mailto := <-got; mailto != "researcher@example.org" {
		t.Fatalf("mailto = %q, want the configured contact address", mailto)
	}
}

func TestEnrichResponseFailures(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		limit     int64
		temporary bool
	}{
		{name: "rate limit is temporary", status: http.StatusTooManyRequests, temporary: true},
		{name: "malformed JSON fails", status: http.StatusOK, body: `{`, limit: 1024},
		{name: "oversized body is bounded", status: http.StatusOK, body: `{"message":{"items":[]}}` + string(make([]byte, 128)), limit: 16},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, matched, err := NewWithOptions(Options{BaseURL: server.URL, MaxResponseBytes: test.limit}).Enrich(context.Background(), work.Work{Title: "A Precise Study"})
			if matched {
				t.Fatal("matched = true, want false")
			}
			if err == nil {
				t.Fatal("error = nil")
			}
			_, temporary := resolver.Temporary(err)
			if temporary != test.temporary {
				t.Errorf("temporary = %v, want %v (error %v)", temporary, test.temporary, err)
			}
		})
	}
}

func TestAuthorFamilyAcceptsSingleWordAuthors(t *testing.T) {
	if got := authorFamily("Smith"); got != "smith" {
		t.Fatalf("authorFamily(Smith) = %q, want smith", got)
	}
	if got := authorFamily("  "); got != "" {
		t.Fatalf("authorFamily(blank) = %q, want empty", got)
	}
	if got := authorFamily("John Smith"); got != "smith" {
		t.Fatalf("authorFamily(John Smith) = %q, want smith", got)
	}
}
