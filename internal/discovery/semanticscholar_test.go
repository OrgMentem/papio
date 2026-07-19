// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSemanticScholarSearchMapsPapersAndRequests(t *testing.T) {
	const fixture = `{"data":[{"externalIds":{"DOI":"https://doi.org/10.5555/Example.","ArXiv":"2401.12345v2"},"title":"  Semantic discovery  ","year":2024,"authors":[{"name":"Ada Lovelace"},{"name":" Grace Hopper "},{"name":" "}],"isOpenAccess":true,"openAccessPdf":{"url":"https://example.org/paper.pdf"},"citationCount":19,"venue":"Useful Journal"}]}`

	for _, test := range []struct {
		name   string
		apiKey string
	}{
		{name: "sends configured API key", apiKey: "secret-key"},
		{name: "omits absent API key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotQuery url.Values
			var gotAPIKey string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.URL.Path, "/paper/search"; got != want {
					t.Errorf("path = %q, want %q", got, want)
				}
				gotQuery = r.URL.Query()
				gotAPIKey = r.Header.Get("x-api-key")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fixture))
			}))
			defer server.Close()

			client := NewSemanticScholarWithOptions(SemanticScholarOptions{
				Client: http.DefaultClient, APIKey: test.apiKey, BaseURL: server.URL,
			})
			works, err := client.Search(context.Background(), SearchParams{
				Query: "resilient discovery", Limit: 101, YearFrom: 2020, YearTo: 2024, OAOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gotQuery.Get("query"), "resilient discovery"; got != want {
				t.Fatalf("query = %q, want %q", got, want)
			}
			if got, want := gotQuery.Get("limit"), "100"; got != want {
				t.Fatalf("limit = %q, want %q", got, want)
			}
			if got, want := gotQuery.Get("year"), "2020-2024"; got != want {
				t.Fatalf("year = %q, want %q", got, want)
			}
			if got, want := gotQuery.Get("openAccessPdf"), "true"; got != want {
				t.Fatalf("openAccessPdf = %q, want %q", got, want)
			}
			if got, want := gotQuery.Get("fields"), semanticScholarFields; got != want {
				t.Fatalf("fields = %q, want %q", got, want)
			}
			if got, want := gotAPIKey, test.apiKey; got != want {
				t.Fatalf("x-api-key = %q, want %q", got, want)
			}
			if len(works) != 1 {
				t.Fatalf("works = %d, want 1", len(works))
			}
			got := works[0]
			if got.Work.DOI != "10.5555/example" || got.Work.ArXiv != "2401.12345v2" ||
				got.Work.Title != "Semantic discovery" || got.Work.Year != 2024 || got.Work.Container != "Useful Journal" {
				t.Fatalf("work = %+v", got.Work)
			}
			if authors := strings.Join(got.Work.Authors, ", "); authors != "Ada Lovelace, Grace Hopper" {
				t.Fatalf("authors = %q", authors)
			}
			if !got.IsOA || got.OAURL != "https://example.org/paper.pdf" || got.CitedBy != 19 || got.Source != "semanticscholar" {
				t.Fatalf("discovered work = %+v", got)
			}
		})
	}
}

func TestSemanticScholarSearchSnowballs(t *testing.T) {
	for _, test := range []struct {
		name       string
		params     SearchParams
		path       string
		response   string
		wantTitle  string
		wantFields string
	}{
		{
			name:      "citations maps citing papers",
			params:    SearchParams{Cites: "10.1000/seed"},
			path:      "/paper/DOI:10.1000/seed/citations",
			response:  `{"data":[{"citingPaper":{"title":"Forward work"}}]}`,
			wantTitle: "Forward work", wantFields: "citingPaper.title",
		},
		{
			name:      "references maps cited papers",
			params:    SearchParams{CitedBy: "10.1000/seed"},
			path:      "/paper/DOI:10.1000/seed/references",
			response:  `{"data":[{"citedPaper":{"title":"Backward work"}}]}`,
			wantTitle: "Backward work", wantFields: "citedPaper.title",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.URL.Path, test.path; got != want {
					t.Errorf("path = %q, want %q", got, want)
				}
				gotQuery = r.URL.Query()
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()

			works, err := NewSemanticScholarWithOptions(SemanticScholarOptions{Client: http.DefaultClient, BaseURL: server.URL}).Search(context.Background(), test.params)
			if err != nil {
				t.Fatal(err)
			}
			if len(works) != 1 || works[0].Work.Title != test.wantTitle {
				t.Fatalf("works = %+v", works)
			}
			if got := gotQuery.Get("fields"); !strings.Contains(got, test.wantFields) {
				t.Fatalf("fields = %q, want %q", got, test.wantFields)
			}
		})
	}
}

func TestSemanticScholarYearFiltersSupportOpenBounds(t *testing.T) {
	for _, test := range []struct {
		params SearchParams
		want   string
	}{
		{params: SearchParams{YearFrom: 2020}, want: "2020-"},
		{params: SearchParams{YearTo: 2024}, want: "-2024"},
		{params: SearchParams{YearFrom: 2020, YearTo: 2024}, want: "2020-2024"},
	} {
		if got := semanticScholarYear(test.params); got != test.want {
			t.Fatalf("semanticScholarYear(%+v) = %q, want %q", test.params, got, test.want)
		}
	}
}

func TestSemanticScholarRejectsUnsupportedOrMultipleSnowballs(t *testing.T) {
	client := NewSemanticScholarWithOptions(SemanticScholarOptions{Client: http.DefaultClient})
	for _, params := range []SearchParams{
		{RelatedTo: "10.1000/seed"},
		{Cites: "10.1000/seed", CitedBy: "10.1000/other"},
	} {
		if _, err := client.Search(context.Background(), params); err == nil {
			t.Fatalf("params %+v succeeded", params)
		}
	}
}
