// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestArxivSearchMapsAtomAndBuildsRequest(t *testing.T) {
	fixture, err := os.ReadFile("testdata/arxiv_search.xml")
	if err != nil {
		t.Fatal(err)
	}
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/query"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	works, err := NewArxivWithOptions(ArxivOptions{
		Client: http.DefaultClient, BaseURL: server.URL,
	}).Search(context.Background(), SearchParams{
		Query: "resilient discovery", Limit: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotQuery.Get("search_query"), `all:"resilient" AND all:"discovery"`; got != want {
		t.Fatalf("search_query = %q, want %q", got, want)
	}
	if got, want := gotQuery.Get("max_results"), "50"; got != want {
		t.Fatalf("max_results = %q, want %q", got, want)
	}
	if got, want := gotQuery.Get("sortBy"), "submittedDate"; got != want {
		t.Fatalf("sortBy = %q, want %q", got, want)
	}
	if got, want := gotQuery.Get("sortOrder"), "descending"; got != want {
		t.Fatalf("sortOrder = %q, want %q", got, want)
	}
	if len(works) != 1 {
		t.Fatalf("works = %d, want one valid entry", len(works))
	}
	got := works[0]
	if got.Work.ArXiv != "2401.12345v2" || got.Work.Title != "A resilient arXiv discovery paper" || got.Work.Year != 2024 {
		t.Fatalf("work = %+v", got.Work)
	}
	if authors := strings.Join(got.Work.Authors, ", "); authors != "Ada Lovelace, Grace Hopper" {
		t.Fatalf("authors = %q", authors)
	}
	if !got.IsOA || got.OAURL != "https://arxiv.org/pdf/2401.12345" || got.Source != "arxiv" {
		t.Fatalf("discovered work = %+v", got)
	}
	if got.OpenAlexID != "" {
		t.Fatalf("OpenAlexID = %q, want empty for an arXiv result", got.OpenAlexID)
	}
}

func TestArxivYearRangeAddsSubmittedDateFilter(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`))
	}))
	defer server.Close()

	_, err := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL}).Search(
		context.Background(),
		SearchParams{Query: "trust calibration", YearFrom: 2020, YearTo: 2024},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := `all:"trust" AND all:"calibration" AND submittedDate:[202001010000 TO 202412312359]`
	if got := gotQuery.Get("search_query"); got != want {
		t.Fatalf("search_query = %q, want %q", got, want)
	}
}

func TestArxivQueryTermsAreBounded(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`))
	}))
	defer server.Close()

	longTerm := strings.Repeat("x", arxivMaxTermRunes+1)
	query := longTerm + " " + strings.Join([]string{
		"t01", "t02", "t03", "t04", "t05", "t06", "t07", "t08",
		"t09", "t10", "t11", "t12", "t13", "t14", "t15", "dropped",
	}, " ")
	_, err := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL}).
		Search(context.Background(), SearchParams{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	got := gotQuery.Get("search_query")
	if count := strings.Count(got, `all:"`); count != arxivMaxTerms {
		t.Fatalf("search_query has %d terms, want %d: %q", count, arxivMaxTerms, got)
	}
	if !strings.Contains(got, `all:"`+strings.Repeat("x", arxivMaxTermRunes)+`"`) {
		t.Fatalf("search_query did not cap the long term at %d runes: %q", arxivMaxTermRunes, got)
	}
	if strings.Contains(got, "dropped") || strings.Contains(got, longTerm) {
		t.Fatalf("search_query exceeded its term bounds: %q", got)
	}
}

func TestArxivCitationSnowballDeclinesWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	backend := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL})
	works, failures, err := NewMulti(backend).SearchPartial(context.Background(), SearchParams{Cites: "10.1000/seed"})
	if err == nil {
		t.Fatal("citation snowball succeeded, want backend refusal")
	}
	if works != nil {
		t.Fatalf("works = %+v, want nil", works)
	}
	if len(failures) != 1 || failures[0].Source != "arxiv" || !strings.Contains(failures[0].Message, "citation snowball") {
		t.Fatalf("failures = %+v, want one arxiv BackendFailure", failures)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestArxivOversizedResponseIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><id>https://arxiv.org/abs/2401.12345v1</id><title>` + strings.Repeat("x", 1024) + `</title></entry></feed>`))
	}))
	defer server.Close()

	_, err := NewArxivWithOptions(ArxivOptions{
		Client: http.DefaultClient, BaseURL: server.URL, MaxResponseBytes: 256,
	}).Search(context.Background(), SearchParams{Query: "bounded response"})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Search() error = %v, want size-limit rejection", err)
	}
}
