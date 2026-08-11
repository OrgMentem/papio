// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
			name: "candidate year missing is rejected",
			body: `{"message":{"items":[{"DOI":"10.1234/example","title":["A Precise Study"],"author":[{"family":"Smith"}]}]}}`,
		},
		{
			name: "candidate author missing is rejected",
			body: `{"message":{"items":[{"DOI":"10.1234/example","title":["A Precise Study"],"published-print":{"date-parts":[[2024]]}}]}}`,
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
func TestEnrichRejectsISBNAmbiguityAndMissingEditionEvidence(t *testing.T) {
	const matching = `{"DOI":"10.1234/example","title":["A Precise Study"],"author":[{"family":"Smith"}],"published-print":{"date-parts":[[2024]]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"items":[` + matching + `,{"DOI":"10.1234/other","title":["A Precise Study"],"author":[{"family":"Smith"}],"published-print":{"date-parts":[[2024]]}}]}}`))
	}))
	defer server.Close()

	enriched, matched, err := NewWithOptions(Options{BaseURL: server.URL}).Enrich(context.Background(), work.Work{
		Title: "A Precise Study", Year: 2024, Authors: []string{"Smith"},
	})
	if err != nil || matched || enriched.DOI != "" {
		t.Fatalf("ambiguous enrichment = %+v, matched=%v, err=%v; ambiguity must be refused", enriched, matched, err)
	}

	for _, requested := range []work.Work{
		{Title: "A Precise Study", Authors: []string{"Smith"}},
		{Title: "A Precise Study", Year: 2024},
		{Title: "A Precise Study", Year: 2024, Authors: []string{"Smith"}, ISBN: "9781576753484"},
	} {
		enriched, matched, err := NewWithOptions(Options{BaseURL: server.URL}).Enrich(context.Background(), requested)
		if err != nil || matched || enriched.DOI != "" {
			t.Fatalf("requested=%+v enrichment=%+v matched=%v err=%v; missing/ISBN evidence must be refused", requested, enriched, matched, err)
		}
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

func versionServer(t *testing.T, status int, body string) (*Enricher, *string) {
	t.Helper()
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return NewWithOptions(Options{Client: http.DefaultClient, ContactEmail: "reader@example.test", BaseURL: server.URL + "/works"}), &gotURL
}

func TestVersionSiblingsFollowsTypedEdgesOnly(t *testing.T) {
	e, gotURL := versionServer(t, http.StatusOK, `{"message": {"relation": {
		"has-preprint": [
			{"id-type": "doi", "id": "https://doi.org/10.2139/ssrn.4020557"},
			{"id-type": "arxiv", "id": "2401.12345"},
			{"id-type": "doi", "id": "10.1145/3531146.3533202"}
		],
		"is-version-of": [{"id-type": "doi", "id": "10.5555/other.version"}],
		"is-review-of":  [{"id-type": "doi", "id": "10.9999/unrelated"}]
	}}}`)
	siblings, err := e.VersionSiblings(context.Background(), "10.1145/3531146.3533202")
	if err != nil {
		t.Fatal(err)
	}
	// The arXiv-typed target is skipped, the work's own DOI is excluded, and
	// a non-version relation (is-review-of) is never followed: a review is a
	// different work, not another version of this one.
	want := []string{"10.2139/ssrn.4020557", "10.5555/other.version"}
	if len(siblings) != len(want) || siblings[0] != want[0] || siblings[1] != want[1] {
		t.Fatalf("siblings = %v, want %v", siblings, want)
	}
	if !strings.Contains(*gotURL, "/works/10.1145/3531146.3533202") || !strings.Contains(*gotURL, "mailto=reader%40example.test") {
		t.Fatalf("request URL = %q, want the works path and the polite-pool mailto", *gotURL)
	}
}

func TestVersionSiblingsCapsAndDeduplicates(t *testing.T) {
	e, _ := versionServer(t, http.StatusOK, `{"message": {"relation": {
		"has-preprint": [
			{"id-type": "doi", "id": "10.1234/a"},
			{"id-type": "doi", "id": "10.1234/a"},
			{"id-type": "doi", "id": "10.1234/b"}
		],
		"has-version": [
			{"id-type": "doi", "id": "10.1234/c"},
			{"id-type": "doi", "id": "10.1234/d"}
		]
	}}}`)
	siblings, err := e.VersionSiblings(context.Background(), "10.1234/self")
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 3 {
		t.Fatalf("siblings = %v, want the duplicate collapsed and the count capped at 3", siblings)
	}
}

func TestVersionSiblingsClassifiesAbsenceAndFailures(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		body          string
		wantTemporary bool
		wantErr       bool
	}{
		{name: "unregistered DOI is absence", status: http.StatusNotFound},
		{name: "no relations is absence", status: http.StatusOK, body: `{"message": {}}`},
		{name: "429 is temporary", status: http.StatusTooManyRequests, wantTemporary: true, wantErr: true},
		{name: "500 is temporary", status: http.StatusInternalServerError, wantTemporary: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, _ := versionServer(t, test.status, test.body)
			siblings, err := e.VersionSiblings(context.Background(), "10.1234/self")
			if siblings != nil {
				t.Fatalf("siblings = %v, want nil", siblings)
			}
			if test.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %t", err, test.wantErr)
			}
			if _, temporary := resolver.Temporary(err); temporary != test.wantTemporary {
				t.Fatalf("Temporary(%v) = %t, want %t", err, temporary, test.wantTemporary)
			}
		})
	}
}

func TestVersionSiblingsRejectsDotSegmentDOIs(t *testing.T) {
	e := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: "https://api.example.test/works"})
	// doiCoreRE admits any non-whitespace suffix, so this is a "legal" DOI
	// whose path segments would escape /works/ on a Cleaning server. It must
	// be treated as absence without any request.
	siblings, err := e.VersionSiblings(context.Background(), "10.1234/../../../x")
	if siblings != nil || err != nil {
		t.Fatalf("VersionSiblings = (%v, %v), want (nil, nil)", siblings, err)
	}
}
