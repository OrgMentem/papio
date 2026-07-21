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
