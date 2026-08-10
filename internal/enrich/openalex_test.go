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

func TestOpenAlexEnrichMeasuredBookRescue(t *testing.T) {
	requested := work.Work{
		Title:   "Evaluating training programs: the four levels",
		Year:    2006,
		Authors: []string{"Kirkpatrick, D. L."},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("filter"); got != "title.search:Evaluating training programs: the four levels,publication_year:2006" {
			t.Errorf("filter = %q", got)
		}
		if got := r.URL.Query().Get("per-page"); got != "5" {
			t.Errorf("per-page = %q, want 5", got)
		}
		if got := r.URL.Query().Get("mailto"); got != "reader@example.org" {
			t.Errorf("mailto = %q", got)
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"https://openalex.org/W2741809807","doi":"https://doi.org/10.1002/pfi.4140340510","ids":{"openalex":"https://openalex.org/W2741809807","doi":"https://doi.org/10.1002/pfi.4140340510"},"title":"Evaluating training programs: the four levels","publication_year":2006,"authorships":[{"author":{"display_name":"Donald L. Kirkpatrick"}}]}]}`))
	}))
	defer server.Close()

	enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{
		BaseURL: server.URL, ContactEmail: "reader@example.org",
	}).Enrich(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("matched = false, want true")
	}
	if enriched.OpenAlex != "W2741809807" {
		t.Fatalf("OpenAlex = %q", enriched.OpenAlex)
	}
	if enriched.DOI != "10.1002/pfi.4140340510" {
		t.Fatalf("DOI = %q", enriched.DOI)
	}
}

func TestOpenAlexEnrichStrictCorroboration(t *testing.T) {
	requested := work.Work{Title: "A precise study", Year: 2024, Authors: []string{"Jane Smith"}}
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "wrong authors",
			body: `{"results":[{"id":"https://openalex.org/W1","title":"A precise study","publication_year":2024,"authorships":[{"author":{"display_name":"John Brown"}}]}]}`,
		},
		{
			name: "wrong year",
			body: `{"results":[{"id":"https://openalex.org/W1","title":"A precise study","publication_year":2023,"authorships":[{"author":{"display_name":"Jane Smith"}}]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "reader@example.org"}).Enrich(context.Background(), requested)
			if err != nil {
				t.Fatal(err)
			}
			if matched || enriched.OpenAlex != "" || enriched.DOI != "" {
				t.Fatalf("enriched = %+v, matched = %v; wrong corroboration must be refused", enriched, matched)
			}
		})
	}
}

func TestOpenAlexEnrichAdoptsIDWithoutDOI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"https://openalex.org/W12345","ids":{"openalex":"https://openalex.org/W12345"},"title":"A title","publication_year":2024}]}`))
	}))
	defer server.Close()

	enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "reader@example.org"}).Enrich(context.Background(), work.Work{Title: "A title", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	if !matched || enriched.OpenAlex != "W12345" || enriched.DOI != "" {
		t.Fatalf("enriched = %+v, matched = %v; want OpenAlex ID without DOI", enriched, matched)
	}
}

func TestOpenAlexEnrichAPIErrorsAreTemporary(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
			}))
			defer server.Close()

			_, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{
				BaseURL: server.URL, ContactEmail: "reader@example.org",
			}).Enrich(context.Background(), work.Work{Title: "A title"})
			if matched || err == nil {
				t.Fatalf("matched = %v, err = %v; want temporary API failure", matched, err)
			}
			if _, temporary := resolver.Temporary(err); !temporary {
				t.Fatalf("Temporary(%v) = false, want true", err)
			}
		})
	}
}

func TestOpenAlexEnrichSkipsFetchableWork(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer server.Close()

	requested := work.Work{Title: "A title", DOI: "10.1234/existing"}
	_, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL}).Enrich(context.Background(), requested)
	if err != nil || matched || called {
		t.Fatalf("result matched=%v err=%v called=%v; fetchable work must skip", matched, err, called)
	}
}
