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

func TestSearchClampsLimitsAndRejectsBlankQuery(t *testing.T) {
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
		t.Fatal("blank query succeeded")
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
