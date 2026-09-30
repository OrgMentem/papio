// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"papio/internal/work"
)

func pageTitles(works []DiscoveredWork) string {
	titles := make([]string, len(works))
	for i, discovered := range works {
		titles[i] = discovered.Work.Title
	}
	return strings.Join(titles, ", ")
}

func TestOpenAlexSearchPageFollowsCursorToExhaustion(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		switch cursor {
		case "*":
			_, _ = w.Write([]byte(`{"meta":{"next_cursor":"c2"},"results":[{"id":"https://openalex.org/W1","title":"First"}]}`))
		case "c2":
			_, _ = w.Write([]byte(`{"meta":{"next_cursor":null},"results":[{"id":"https://openalex.org/W2","title":"Second"}]}`))
		default:
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := NewWithOptions(Options{Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL + "/works"})
	params := SearchParams{Query: "resilient discovery", Limit: 1}

	first, err := client.SearchPage(context.Background(), params, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != PageMore || first.Next == "" || pageTitles(first.Works) != "First" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.SearchPage(context.Background(), params, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != PageExhausted || second.Next != "" || pageTitles(second.Works) != "Second" {
		t.Fatalf("second page = %+v", second)
	}
	if got := strings.Join(cursors, ","); got != "*,c2" {
		t.Fatalf("cursors = %q, want *,c2", got)
	}
}

// A cursor must not cost a seed lookup per page: the resolved work ID travels
// in the token, and the filter on page two is the one page one used.
func TestOpenAlexSearchPageCarriesResolvedSeeds(t *testing.T) {
	seedFixture, err := os.ReadFile("testdata/openalex_work_by_doi.json")
	if err != nil {
		t.Fatal(err)
	}
	var lookups int
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/works/doi:") {
			lookups++
			_, _ = w.Write(seedFixture)
			return
		}
		filters = append(filters, r.URL.Query().Get("filter"))
		if r.URL.Query().Get("cursor") == "*" {
			_, _ = w.Write([]byte(`{"meta":{"next_cursor":"c2"},"results":[{"id":"https://openalex.org/W1","title":"First"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"next_cursor":null},"results":[]}`))
	}))
	defer server.Close()
	client := NewWithOptions(Options{Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL + "/works"})
	params := SearchParams{Cites: "10.1000/seed"}

	first, err := client.SearchPage(context.Background(), params, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.SearchPage(context.Background(), params, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != PageExhausted || len(second.Works) != 0 {
		t.Fatalf("second page = %+v, want an empty exhausted page", second)
	}
	if lookups != 1 {
		t.Fatalf("seed lookups = %d, want 1 across both pages", lookups)
	}
	if got, want := strings.Join(filters, " | "), "cites:W2741809807 | cites:W2741809807"; got != want {
		t.Fatalf("filters = %q, want %q", got, want)
	}
}

func arxivPageFeed(total int, ids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom" xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/">`)
	fmt.Fprintf(&b, `<opensearch:totalResults>%d</opensearch:totalResults>`, total)
	for _, id := range ids {
		fmt.Fprintf(&b, `<entry><id>https://arxiv.org/abs/%s</id><published>2024-01-01T00:00:00Z</published><title>Paper %s</title></entry>`, id, id)
	}
	b.WriteString(`</feed>`)
	return b.String()
}

func TestArxivSearchPageWalksOffsetsToExhaustion(t *testing.T) {
	var requests []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Query())
		switch r.URL.Query().Get("start") {
		case "":
			_, _ = w.Write([]byte(arxivPageFeed(3, "2401.00001", "2401.00002")))
		case "2":
			_, _ = w.Write([]byte(arxivPageFeed(3, "2401.00003")))
		default:
			http.Error(w, "unexpected start", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL})
	params := SearchParams{Query: "resilient discovery", Limit: 2}

	first, err := client.SearchPage(context.Background(), params, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != PageMore || pageTitles(first.Works) != "Paper 2401.00001, Paper 2401.00002" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.SearchPage(context.Background(), params, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != PageExhausted || second.Next != "" || pageTitles(second.Works) != "Paper 2401.00003" {
		t.Fatalf("second page = %+v", second)
	}
	if got := requests[1].Get("max_results"); got != "2" {
		t.Fatalf("max_results = %q, want 2", got)
	}
}

// arXiv answers a deep page transiently empty; that must not end the walk as
// though the results ran out.
func TestArxivSearchPageEmptyPageBeforeTotalIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(arxivPageFeed(5)))
	}))
	defer server.Close()
	_, err := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL}).
		SearchPage(context.Background(), SearchParams{Query: "resilient"}, "")
	if err == nil || !strings.Contains(err.Error(), "empty page") {
		t.Fatalf("err = %v, want an empty-page failure", err)
	}
}

func TestSemanticScholarSearchPageWalksOffsetsToExhaustion(t *testing.T) {
	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		switch r.URL.Query().Get("offset") {
		case "":
			_, _ = w.Write([]byte(`{"total":3,"offset":0,"next":2,"data":[{"title":"A"},{"title":"B"}]}`))
		case "2":
			_, _ = w.Write([]byte(`{"total":3,"offset":2,"data":[{"title":"C"}]}`))
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := NewSemanticScholarWithOptions(SemanticScholarOptions{Client: http.DefaultClient, BaseURL: server.URL})
	params := SearchParams{Query: "resilient discovery", Limit: 2}

	first, err := client.SearchPage(context.Background(), params, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != PageMore || pageTitles(first.Works) != "A, B" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.SearchPage(context.Background(), params, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != PageExhausted || second.Next != "" || pageTitles(second.Works) != "C" {
		t.Fatalf("second page = %+v", second)
	}
	if got := strings.Join(offsets, ","); got != ",2" {
		t.Fatalf("offsets = %q, want first page unset then 2", got)
	}
}

// Relevance search stops at a 1,000-row window. Reaching it with more matched
// is truncation, not exhaustion, and the last request must not ask past it.
func TestSemanticScholarSearchPageReportsWindowTruncation(t *testing.T) {
	var limit string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit = r.URL.Query().Get("limit")
		rows := make([]string, 10)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"title":"Row %d"}`, i)
		}
		_, _ = w.Write([]byte(`{"total":5000,"offset":990,"data":[` + strings.Join(rows, ",") + `]}`))
	}))
	defer server.Close()
	params := SearchParams{Query: "broad topic", Limit: 20}
	page, err := NewSemanticScholarWithOptions(SemanticScholarOptions{Client: http.DefaultClient, BaseURL: server.URL}).
		SearchPage(context.Background(), params, offsetPageToken("semanticscholar", params, 990))
	if err != nil {
		t.Fatal(err)
	}
	if limit != "10" {
		t.Fatalf("limit = %q, want 10 so offset+limit stays inside the window", limit)
	}
	if page.State != PageTruncated || page.Next != "" || len(page.Works) != 10 {
		t.Fatalf("page = %+v, want 10 works and truncated", page)
	}
}

// A filtered snowball page keeps at most the page size from a larger raw
// fetch; the next page must resume right after the last row examined, or the
// rows fetched but not yet delivered would be skipped.
func TestSemanticScholarSnowballPageResumesAfterLastExaminedRow(t *testing.T) {
	var requests []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Query())
		switch r.URL.Query().Get("offset") {
		case "":
			_, _ = w.Write([]byte(`{"offset":0,"data":[
				{"citingPaper":{"title":"Closed","isOpenAccess":false}},
				{"citingPaper":{"title":"Open B","isOpenAccess":true}},
				{"citingPaper":{"title":"Open C","isOpenAccess":true}}
			]}`))
		case "2":
			_, _ = w.Write([]byte(`{"offset":2,"data":[{"citingPaper":{"title":"Open C","isOpenAccess":true}}]}`))
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := NewSemanticScholarWithOptions(SemanticScholarOptions{Client: http.DefaultClient, BaseURL: server.URL})
	params := SearchParams{Cites: "10.1000/seed", Limit: 1, OAOnly: true}

	first, err := client.SearchPage(context.Background(), params, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != PageMore || pageTitles(first.Works) != "Open B" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.SearchPage(context.Background(), params, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != PageExhausted || pageTitles(second.Works) != "Open C" {
		t.Fatalf("second page = %+v", second)
	}
	if got := requests[0].Get("limit"); got != "100" {
		t.Fatalf("filtered snowball limit = %q, want 100", got)
	}
}

func rawPageToken(payload string) string {
	return pageTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func TestSearchPageRejectsInvalidTokensWithoutARequest(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"meta":{"next_cursor":null},"results":[]}`))
	}))
	defer server.Close()
	arxiv := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL})
	openAlex := NewWithOptions(Options{Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL + "/works"})
	params := SearchParams{Query: "resilient discovery"}
	snowball := SearchParams{Cites: "10.1000/seed"}
	fingerprint := searchFingerprint(params)

	for _, test := range []struct {
		name   string
		source PageSearcher
		params SearchParams
		token  string
	}{
		{name: "garbage", source: arxiv, params: params, token: "page-2"},
		{name: "not base64", source: arxiv, params: params, token: pageTokenPrefix + "!!!"},
		{name: "not JSON", source: arxiv, params: params, token: rawPageToken("offset=2")},
		{name: "unknown field", source: arxiv, params: params, token: rawPageToken(`{"b":"arxiv","f":"` + fingerprint + `","o":2,"x":1}`)},
		{name: "trailing data", source: arxiv, params: params, token: rawPageToken(`{"b":"arxiv","f":"` + fingerprint + `","o":2}{}`)},
		{name: "oversized", source: arxiv, params: params, token: pageTokenPrefix + strings.Repeat("A", maxPageTokenBytes)},
		{name: "another backend's token", source: arxiv, params: params, token: offsetPageToken("semanticscholar", params, 2)},
		{name: "another search's token", source: arxiv, params: params, token: offsetPageToken("arxiv", SearchParams{Query: "other topic"}, 2)},
		{name: "zero offset", source: arxiv, params: params, token: rawPageToken(`{"b":"arxiv","f":"` + fingerprint + `"}`)},
		{name: "negative offset", source: arxiv, params: params, token: offsetPageToken("arxiv", params, -5)},
		{name: "offset past the window", source: arxiv, params: params, token: offsetPageToken("arxiv", params, arxivResultWindow)},
		{name: "cursor on an offset backend", source: arxiv, params: params, token: encodePageToken(pageToken{Backend: "arxiv", Fingerprint: fingerprint, Offset: 2, Cursor: "c"})},
		{name: "openalex offset token", source: openAlex, params: params, token: offsetPageToken("openalex", params, 2)},
		{name: "openalex initial cursor", source: openAlex, params: params, token: encodePageToken(pageToken{Backend: "openalex", Fingerprint: fingerprint, Cursor: "*"})},
		{name: "openalex missing seed", source: openAlex, params: snowball, token: encodePageToken(pageToken{Backend: "openalex", Fingerprint: searchFingerprint(snowball), Cursor: "c2"})},
		{name: "openalex malformed seed", source: openAlex, params: snowball, token: encodePageToken(pageToken{Backend: "openalex", Fingerprint: searchFingerprint(snowball), Cursor: "c2", Seeds: map[string]string{"cites": "W1,cites:W2"}})},
		{name: "openalex extra seed", source: openAlex, params: snowball, token: encodePageToken(pageToken{Backend: "openalex", Fingerprint: searchFingerprint(snowball), Cursor: "c2", Seeds: map[string]string{"cites": "W1", "cited_by": "W2"}})},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests = 0
			_, err := test.source.SearchPage(context.Background(), test.params, test.token)
			if !errors.Is(err, ErrInvalidPageToken) {
				t.Fatalf("err = %v, want ErrInvalidPageToken", err)
			}
			if requests != 0 {
				t.Fatalf("requests = %d, want none for a rejected token", requests)
			}
		})
	}
}

// Page size and whitespace do not change the result set, so a token survives
// them; a caller may shrink or grow its page between runs.
func TestPageTokenIgnoresPageSizeAndQueryWhitespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(arxivPageFeed(3, "2401.00003")))
	}))
	defer server.Close()
	token := offsetPageToken("arxiv", SearchParams{Query: "resilient discovery", Limit: 2}, 2)
	page, err := NewArxivWithOptions(ArxivOptions{Client: http.DefaultClient, BaseURL: server.URL}).
		SearchPage(context.Background(), SearchParams{Query: "  resilient   discovery ", Limit: 40}, token)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != PageExhausted {
		t.Fatalf("page = %+v, want exhausted", page)
	}
}

type fakePageSource struct {
	name   string
	page   Page
	err    error
	tokens []string
}

func (s *fakePageSource) Name() string { return s.name }

func (s *fakePageSource) Search(context.Context, SearchParams) ([]DiscoveredWork, error) {
	return nil, errors.New("fakePageSource: Search must not be called by a paged search")
}

func (s *fakePageSource) SearchPage(_ context.Context, _ SearchParams, token string) (Page, error) {
	s.tokens = append(s.tokens, token)
	return s.page, s.err
}

func TestMultiSearchPagesReportsEachSourceIndependently(t *testing.T) {
	failing := &fakePageSource{name: "failing", err: errors.New("failing unavailable")}
	paging := &fakePageSource{name: "paging", page: Page{
		Works: []DiscoveredWork{{Work: work.Work{DOI: "10.1000/paged", Title: "Paged"}}},
		State: PageMore, Next: "paging-next",
	}}
	single := &fakeSource{name: "single", works: []DiscoveredWork{{Work: work.Work{DOI: "10.1000/single", Title: "Single"}}}}
	multi := NewMulti(failing, paging, single)

	result, err := multi.SearchPages(context.Background(), SearchParams{Query: "test"}, map[string]string{
		"failing": "failing-token", "paging": "paging-token", "removed": "stale-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 3 {
		t.Fatalf("sources = %+v, want failing, paging, single", result.Sources)
	}
	failed, paged, unsupported := result.Sources[0], result.Sources[1], result.Sources[2]
	if failed.Source != "failing" || failed.State != PageFailed || failed.Next != "failing-token" || failed.Failure == nil || failed.Failure.Message != "failing unavailable" {
		t.Fatalf("failing source page = %+v, want failed keeping its token", failed)
	}
	if paged.Source != "paging" || paged.State != PageMore || paged.Next != "paging-next" || pageTitles(paged.Works) != "Paged" {
		t.Fatalf("paging source page = %+v", paged)
	}
	if got := strings.Join(paging.tokens, ","); got != "paging-token" {
		t.Fatalf("paging source got tokens %q, want its own", got)
	}
	if unsupported.Source != "single" || unsupported.State != PageUnsupported || unsupported.Next != "" || pageTitles(unsupported.Works) != "Single" {
		t.Fatalf("non-paging source page = %+v, want its single page marked unsupported", unsupported)
	}
	if got := pageTitles(result.Works); got != "Paged, Single" {
		t.Fatalf("merged works = %q", got)
	}
	if got := multi.LastFailures(); len(got) != 1 || got[0].Source != "failing" {
		t.Fatalf("retained failures = %+v, want the failing backend", got)
	}
}

func TestMultiSearchPagesFailsClosedOnBadPaging(t *testing.T) {
	for _, test := range []struct {
		name   string
		source Source
		tokens map[string]string
	}{
		{
			name:   "token for a source that cannot page",
			source: &fakeSource{name: "single"},
			tokens: map[string]string{"single": "stale-token"},
		},
		{
			name:   "backend token rejected",
			source: &fakePageSource{name: "paging", err: fmt.Errorf("%w: test", ErrInvalidPageToken)},
			tokens: map[string]string{"paging": "bad-token"},
		},
		{
			name:   "more results without a token",
			source: &fakePageSource{name: "paging", page: Page{State: PageMore}},
		},
		{
			name:   "exhausted with a token",
			source: &fakePageSource{name: "paging", page: Page{State: PageExhausted, Next: "stray"}},
		},
		{
			name:   "unknown state",
			source: &fakePageSource{name: "paging", page: Page{State: "done"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			multi := NewMulti(test.source)
			result, err := multi.SearchPages(context.Background(), SearchParams{Query: "test"}, test.tokens)
			if err == nil {
				t.Fatal("err = nil, want every-source-failed error")
			}
			if len(result.Sources) != 1 || result.Sources[0].State != PageFailed {
				t.Fatalf("sources = %+v, want one failed page", result.Sources)
			}
			if plain, ok := test.source.(*fakeSource); ok && plain.calls != 0 {
				t.Fatalf("non-paging source searched %d times with a token it never issued", plain.calls)
			}
			tokenFault := errors.Is(result.Sources[0].Err, ErrInvalidPageToken)
			if retained := len(multi.LastFailures()); tokenFault && retained != 0 {
				t.Fatalf("retained failures = %d, want none for the caller's own bad token", retained)
			}
		})
	}
}
