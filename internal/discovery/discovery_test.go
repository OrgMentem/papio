// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestSearchMapsRecordedOpenAlexWorks(t *testing.T) {
	fixture, err := os.ReadFile("testdata/openalex_works.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotQuery url.Values
	var gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotUserAgent = r.UserAgent()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	client := NewWithOptions(Options{
		Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL,
		Version: "9.8.7",
	})
	works, err := client.Search(context.Background(), SearchParams{
		Query: "resilient discovery", Limit: 99, YearFrom: 2020, YearTo: 2024, OAOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotQuery.Get("search"), "resilient discovery"; got != want {
		t.Fatalf("search = %q, want %q", got, want)
	}
	if got, want := gotQuery.Get("per-page"), "50"; got != want {
		t.Fatalf("per-page = %q, want %q", got, want)
	}
	if got, want := gotQuery.Get("mailto"), "researcher@example.org"; got != want {
		t.Fatalf("mailto = %q, want %q", got, want)
	}
	if got := gotQuery.Get("api_key"); got != "" {
		t.Fatalf("api_key = %q, want omitted", got)
	}
	if got, want := gotQuery.Get("filter"), "from_publication_date:2020-01-01,to_publication_date:2024-12-31,open_access.is_oa:true"; got != want {
		t.Fatalf("filter = %q, want %q", got, want)
	}
	if got, want := gotUserAgent, "papio/9.8.7 (mailto:researcher@example.org)"; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
	if len(works) != 3 {
		t.Fatalf("works = %d, want 3", len(works))
	}
	first := works[0]
	if first.Work.DOI != "10.1000/example.doi" || first.Work.Title != "A resilient discovery paper" ||
		first.Work.Year != 2023 || first.Work.Container != "Journal of Useful Results" {
		t.Fatalf("first work = %+v", first.Work)
	}
	if got, want := strings.Join(first.Work.Authors, ", "), "Ada Lovelace, Grace Hopper"; got != want {
		t.Fatalf("authors = %q, want %q", got, want)
	}
	if first.OpenAlexID != "https://openalex.org/W2741809807" || !first.IsOA ||
		first.OAURL != "https://repository.example.org/paper.pdf" || first.CitedBy != 42 ||
		first.Abstract != "A resilient abstract" {
		t.Fatalf("first discovery result = %+v", first)
	}
	if works[1].Work.DOI != "10.5555/second" || works[1].IsOA || works[1].Abstract != "" {
		t.Fatalf("second discovery result = %+v", works[1])
	}
	if works[2].Work.DOI != "" || works[2].Work.Container != "" || len(works[2].Work.Authors) != 0 {
		t.Fatalf("third discovery result = %+v", works[2])
	}
}

func TestSearchClampsLimitsAndRequiresQueryWithoutSnowball(t *testing.T) {
	var limits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits = append(limits, r.URL.Query().Get("per-page"))
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := NewWithOptions(Options{Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL})

	for _, limit := range []int{-1, 0, 1, 51} {
		if _, err := client.Search(context.Background(), SearchParams{Query: "test", Limit: limit}); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if got, want := strings.Join(limits, ","), "1,20,1,50"; got != want {
		t.Fatalf("limits = %q, want %q", got, want)
	}
	if _, err := client.Search(context.Background(), SearchParams{Query: " \t "}); err == nil {
		t.Fatal("blank query without a citation snowball succeeded")
	}
}

func TestSearchCitationSnowballsResolveDOIsAndBuildExactFilters(t *testing.T) {
	seedFixture, err := os.ReadFile("testdata/openalex_work_by_doi.json")
	if err != nil {
		t.Fatal(err)
	}
	searchFixture, err := os.ReadFile("testdata/openalex_works.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		params SearchParams
		filter string
	}{
		{
			name:   "forward citations use cites",
			params: SearchParams{Cites: "10.1000/seed"},
			filter: "cites:W2741809807",
		},
		{
			name:   "backward references use cited_by",
			params: SearchParams{CitedBy: "10.1000/seed"},
			filter: "cited_by:W2741809807",
		},
		{
			name:   "related works use related_to",
			params: SearchParams{RelatedTo: "10.1000/seed"},
			filter: "related_to:W2741809807",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var lookupPaths []string
			var searchQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/works/doi:"):
					lookupPaths = append(lookupPaths, r.URL.Path)
					_, _ = w.Write(seedFixture)
				case r.URL.Path == "/works":
					searchQuery = r.URL.Query()
					_, _ = w.Write(searchFixture)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := NewWithOptions(Options{
				Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL + "/works",
			})
			if _, err := client.Search(context.Background(), test.params); err != nil {
				t.Fatal(err)
			}
			if got, want := strings.Join(lookupPaths, ","), "/works/doi:10.1000/seed"; got != want {
				t.Fatalf("DOI resolution path = %q, want %q", got, want)
			}
			if got, want := searchQuery.Get("filter"), test.filter; got != want {
				t.Fatalf("filter = %q, want %q", got, want)
			}
			if _, present := searchQuery["search"]; present {
				t.Fatalf("search query = %q, want omitted without free text", searchQuery.Get("search"))
			}
		})
	}
}

func TestSearchCitationSnowballsCacheSeedResolution(t *testing.T) {
	seedFixture, err := os.ReadFile("testdata/openalex_work_by_doi.json")
	if err != nil {
		t.Fatal(err)
	}
	var lookupCount int
	var filter string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/works/doi:") {
			lookupCount++
			_, _ = w.Write(seedFixture)
			return
		}
		filter = r.URL.Query().Get("filter")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := NewWithOptions(Options{
		Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL + "/works",
	})
	if _, err := client.Search(context.Background(), SearchParams{
		Cites: "10.1000/seed", CitedBy: "10.1000/seed", RelatedTo: "10.1000/seed",
	}); err != nil {
		t.Fatal(err)
	}
	if lookupCount != 1 {
		t.Fatalf("DOI resolution requests = %d, want 1", lookupCount)
	}
	if got, want := filter, "cites:W2741809807,cited_by:W2741809807,related_to:W2741809807"; got != want {
		t.Fatalf("filter = %q, want %q", got, want)
	}
}

func TestSearchCitationSnowballReportsMissingSeedDOI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewWithOptions(Options{
		Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL + "/works",
	})
	_, err := client.Search(context.Background(), SearchParams{Cites: "10.1000/not-found"})
	if err == nil || !strings.Contains(err.Error(), `10.1000/not-found`) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing seed error = %v", err)
	}
}

func TestInvertAbstractSkipsOversizedIndexes(t *testing.T) {
	positions := make([]int, maxAbstractWords+1)
	for i := range positions {
		positions[i] = i
	}
	if got := invertAbstract(map[string][]int{"word": positions}); got != "" {
		t.Fatalf("oversized abstract = %q", got)
	}
}
