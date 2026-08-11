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
		if got := r.URL.Query().Get("select"); got != "id,doi,ids,title,publication_year,authorships,open_access,locations" {
			t.Errorf("select = %q", got)
		}
		if got := r.URL.Query().Get("mailto"); got != "reader@example.org" {
			t.Errorf("mailto = %q", got)
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"https://openalex.org/W2741809807","doi":"https://doi.org/10.1002/pfi.4140340510","ids":{"openalex":"https://openalex.org/W2741809807","doi":"https://doi.org/10.1002/pfi.4140340510"},"title":"Evaluating training programs: the four levels","publication_year":2006,"authorships":[{"author":{"display_name":"Donald L. Kirkpatrick"}}],"open_access":{"is_oa":true},"locations":[{"is_oa":true,"landing_page_url":"https://example.org/paper"}]}]}`))
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
			body: `{"results":[{"id":"https://openalex.org/W12345","title":"A precise study","publication_year":2024,"authorships":[{"author":{"display_name":"John Brown"}}]}]}`,
		},
		{
			name: "wrong year",
			body: `{"results":[{"id":"https://openalex.org/W12345","title":"A precise study","publication_year":2023,"authorships":[{"author":{"display_name":"Jane Smith"}}]}]}`,
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

func TestOpenAlexEnrichRequiresOneToOneFullAuthorList(t *testing.T) {
	requested := work.Work{
		Title: "A precise study", Year: 2024,
		Authors: []string{"Jane Smith", "John Brown"},
	}
	for _, test := range []struct {
		name    string
		authors string
		matched bool
	}{
		{
			name:    "subset",
			authors: `{"author":{"display_name":"Jane Smith"}}`,
		},
		{
			name:    "superset",
			authors: `{"author":{"display_name":"Jane Smith"}},{"author":{"display_name":"John Brown"}},{"author":{"display_name":"Extra Author"}}`,
		},
		{
			name:    "duplicate cannot reuse one author",
			authors: `{"author":{"display_name":"Jane Smith"}},{"author":{"display_name":"Jane Smith"}}`,
		},
		{
			name:    "common surname does not corroborate",
			authors: `{"author":{"display_name":"Jane Smith"}},{"author":{"display_name":"Mark Jones"}}`,
		},
		{
			name:    "reordered exact authors",
			authors: `{"author":{"display_name":"John Brown"}},{"author":{"display_name":"Jane Smith"}}`,
			matched: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(openAlexTestResponse(test.authors,
					`{"is_oa":true}`,
					`{"is_oa":true,"landing_page_url":"https://example.org/paper"}`)))
			}))
			defer server.Close()
			enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{
				BaseURL: server.URL, ContactEmail: "reader@example.org",
			}).Enrich(context.Background(), requested)
			if err != nil {
				t.Fatal(err)
			}
			if matched != test.matched {
				t.Fatalf("enriched = %+v, matched = %v; want matched=%v", enriched, matched, test.matched)
			}
			if test.matched && enriched.OpenAlex != "W12345" {
				t.Fatalf("enriched = %+v; matched result lacks OpenAlex ID", enriched)
			}
			if !test.matched && enriched.OpenAlex != "" {
				t.Fatalf("enriched = %+v; rejected result must not carry OpenAlex ID", enriched)
			}
		})
	}
}

func TestOpenAlexEnrichRequiresExplicitOpenAccessLocation(t *testing.T) {
	requested := work.Work{Title: "A precise study", Year: 2024, Authors: []string{"Jane Smith"}}
	for _, test := range []struct {
		name      string
		open      string
		locations string
		matched   bool
	}{
		{
			name:      "closed record",
			open:      `{"is_oa":false}`,
			locations: `{"is_oa":true,"landing_page_url":"https://example.org/paper"}`,
		},
		{name: "no locations", open: `{"is_oa":true}`},
		{
			name:      "valid OA location",
			open:      `{"is_oa":true}`,
			locations: `{"is_oa":true,"pdf_url":"https://example.org/paper.pdf"}`,
			matched:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(openAlexTestResponse(
					`{"author":{"display_name":"Jane Smith"}}`, test.open, test.locations)))
			}))
			defer server.Close()
			enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{
				BaseURL: server.URL, ContactEmail: "reader@example.org",
			}).Enrich(context.Background(), requested)
			if err != nil {
				t.Fatal(err)
			}
			if matched != test.matched || (test.matched && enriched.OpenAlex != "W12345") || (!test.matched && enriched.OpenAlex != "") {
				t.Fatalf("enriched = %+v, matched = %v; want matched=%v", enriched, matched, test.matched)
			}
		})
	}
}

func openAlexTestResponse(authorships, openAccess, locations string) string {
	return `{"results":[{"id":"https://openalex.org/W12345","title":"A precise study","publication_year":2024,"authorships":[` +
		authorships + `],"open_access":` + openAccess + `,"locations":[` + locations + `]}]}`
}

func TestOpenAlexEnrichRejectsCommonSurnameOnlyMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"id":"https://openalex.org/W12345","title":"A precise study","publication_year":2024,"authorships":[{"author":{"display_name":"John Smith"}}]}
		]}`))
	}))
	defer server.Close()

	enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{
		BaseURL: server.URL, ContactEmail: "reader@example.org",
	}).Enrich(context.Background(), work.Work{
		Title: "A precise study", Year: 2024, Authors: []string{"Jane Smith"},
	})
	if err != nil || matched || enriched.OpenAlex != "" {
		t.Fatalf("common-surname enrichment = %+v, matched=%v, err=%v; unrelated given name must be refused", enriched, matched, err)
	}
}

func TestOpenAlexEnrichAdoptsIDWithoutDOI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"https://openalex.org/W12345","ids":{"openalex":"https://openalex.org/W12345"},"title":"A title","publication_year":2024,"authorships":[{"author":{"display_name":"Jane Smith"}}],"open_access":{"is_oa":true},"locations":[{"is_oa":true,"pdf_url":"https://example.org/paper.pdf"}]}]}`))
	}))
	defer server.Close()

	enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "reader@example.org"}).Enrich(context.Background(), work.Work{Title: "A title", Year: 2024, Authors: []string{"Jane Smith"}})
	if err != nil {
		t.Fatal(err)
	}
	if !matched || enriched.OpenAlex != "W12345" || enriched.DOI != "" {
		t.Fatalf("enriched = %+v, matched = %v; want OpenAlex ID without DOI", enriched, matched)
	}
}
func TestOpenAlexEnrichRejectsISBNAndAmbiguousMatches(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"results":[
			{"id":"https://openalex.org/W12345","doi":"10.1000/one","title":"A title","publication_year":2024,"authorships":[{"author":{"display_name":"Jane Smith"}}],"open_access":{"is_oa":true},"locations":[{"is_oa":true,"landing_page_url":"https://example.org/one"}]},
			{"id":"https://openalex.org/W23456","doi":"10.1000/two","title":"A title","publication_year":2024,"authorships":[{"author":{"display_name":"Jane Smith"}}],"open_access":{"is_oa":true},"locations":[{"is_oa":true,"landing_page_url":"https://example.org/two"}]}
		]}`))
	}))
	defer server.Close()

	enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "reader@example.org"}).Enrich(context.Background(), work.Work{
		Title: "A title", Year: 2024, Authors: []string{"Jane Smith"}, ISBN: "9781576753484",
	})
	if err != nil || matched || enriched.OpenAlex != "" || called {
		t.Fatalf("ISBN enrichment = %+v, matched=%v, err=%v, called=%v; ISBN must not be promoted", enriched, matched, err, called)
	}

	enriched, matched, err = NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "reader@example.org"}).Enrich(context.Background(), work.Work{
		Title: "A title", Year: 2024, Authors: []string{"Jane Smith"},
	})
	if err != nil || matched || enriched.OpenAlex != "" || enriched.DOI != "" {
		t.Fatalf("ambiguous enrichment = %+v, matched=%v, err=%v; ambiguity must be refused", enriched, matched, err)
	}
}

func TestOpenAlexEnrichRequiresEditionEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"https://openalex.org/W12345","title":"A title","publication_year":2024,"authorships":[{"author":{"display_name":"Jane Smith"}}],"open_access":{"is_oa":true},"locations":[{"is_oa":true,"landing_page_url":"https://example.org/paper"}]}]}`))
	}))
	defer server.Close()
	for _, requested := range []work.Work{
		{Title: "A title", Authors: []string{"Jane Smith"}},
		{Title: "A title", Year: 2024},
	} {
		enriched, matched, err := NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "reader@example.org"}).Enrich(context.Background(), requested)
		if err != nil || matched || enriched.OpenAlex != "" {
			t.Fatalf("requested=%+v enrichment=%+v matched=%v err=%v; missing evidence must be refused", requested, enriched, matched, err)
		}
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
			}).Enrich(context.Background(), work.Work{Title: "A title", Year: 2024, Authors: []string{"Jane Smith"}})
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
