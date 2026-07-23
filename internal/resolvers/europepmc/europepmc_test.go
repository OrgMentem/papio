// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package europepmc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

// oaResultJSON is a core-format result with a mix of OA and non-OA full-text
// URLs. The PDF OA url carries a query string to exercise redaction.
const oaResultJSON = `{
  "hitCount": 1,
  "resultList": {"result": [{
    "id": "PMC123", "source": "PMC", "pmid": "456", "doi": "10.1000/xyz",
    "title": "Some OA Paper", "authorString": "Smith J, Doe A", "pubYear": "2020",
    "isOpenAccess": "Y", "license": "cc by",
    "fullTextUrlList": {"fullTextUrl": [
      {"availability": "Open access", "availabilityCode": "OA", "documentStyle": "pdf", "site": "Europe_PMC", "url": "https://europepmc.org/articles/PMC123/pdf?token=SECRET_TOKEN#frag"},
      {"availability": "Open access", "availabilityCode": "OA", "documentStyle": "html", "site": "Europe_PMC", "url": "https://europepmc.org/article/MED/456"},
      {"availability": "Subscription required", "availabilityCode": "S", "documentStyle": "pdf", "site": "PubMedCentral", "url": "https://example.com/paywall.pdf"}
    ]}
  }]}
}`

func serveJSON(t *testing.T, status int, headers map[string]string, body string, capturedQuery *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capturedQuery != nil {
			*capturedQuery = r.URL.Query().Get("query")
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newResolver(srv *httptest.Server) *Resolver {
	return NewWithOptions(Options{Client: srv.Client(), BaseURL: srv.URL})
}

func TestResolveByDOIExactFields(t *testing.T) {
	var query string
	srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, &query)
	r := newResolver(srv)

	cands, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate (subscription PDF excluded), got %d: %+v", len(cands), cands)
	}
	if query != `DOI:"10.1000/xyz"` {
		t.Errorf("query = %q, want DOI-scoped query", query)
	}
	c := cands[0]
	liveURL := "https://europepmc.org/articles/PMC123/pdf?token=SECRET_TOKEN#frag"
	checks := map[string]struct{ got, want string }{
		"Source":       {c.Source, "europepmc"},
		"URL":          {c.URL, liveURL},
		"Landing":      {c.Landing, "https://europepmc.org/article/MED/456"},
		"Version":      {c.Version, resolver.VersionPublished},
		"AccessBasis":  {c.AccessBasis, resolver.AccessOpen},
		"ReuseLicense": {c.ReuseLicense, "cc by"},
		"ExpectedMIME": {c.ExpectedMIME, "application/pdf"},
	}
	for field, v := range checks {
		if v.got != v.want {
			t.Errorf("%s = %q, want %q", field, v.got, v.want)
		}
	}
	if !c.Direct {
		t.Error("Direct = false, want true")
	}
	if c.IdentityConfidence != 0.95 {
		t.Errorf("IdentityConfidence = %v, want 0.95", c.IdentityConfidence)
	}
	// Discovered bibliographic identity for the app to fill request fields.
	rw := c.ResolvedWork
	if rw.DOI != "10.1000/xyz" || rw.PMID != "456" || rw.Year != 2020 || rw.Title != "Some OA Paper" {
		t.Errorf("ResolvedWork = %+v", rw)
	}
	if strings.Join(rw.Authors, "|") != "Smith J|Doe A" {
		t.Errorf("ResolvedWork.Authors = %v", rw.Authors)
	}
	if err := resolver.ValidateCandidate(c); err != nil {
		t.Errorf("ValidateCandidate: %v", err)
	}
}

func TestNoQueryOrFragmentSecretsInRedactedURL(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, nil)
	r := newResolver(srv)
	cands, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve: %v, %v", cands, err)
	}
	c := cands[0]
	// The live URL keeps its secret in memory only.
	if !strings.Contains(c.URL, "SECRET_TOKEN") {
		t.Fatal("expected the live candidate URL to retain the token")
	}
	// The redacted form and every evidence line must be secret-free.
	red := redact.URL(c.URL)
	if strings.Contains(red, "SECRET_TOKEN") || strings.ContainsAny(red, "?#") && strings.Contains(red, "frag") {
		t.Errorf("redacted URL leaked secret/fragment: %q", red)
	}
	for _, e := range c.Evidence {
		if strings.Contains(e, "SECRET_TOKEN") || strings.Contains(e, "frag") {
			t.Errorf("evidence leaked secret: %q", e)
		}
	}
}

func TestResolveByPMIDAndTitle(t *testing.T) {
	t.Run("pmid high confidence", func(t *testing.T) {
		var query string
		srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, &query)
		r := newResolver(srv)
		cands, err := r.Resolve(context.Background(), work.Work{PMID: "456"})
		if err != nil || len(cands) != 1 {
			t.Fatalf("Resolve: %v, %v", cands, err)
		}
		if query != "EXT_ID:456 AND SRC:MED" {
			t.Errorf("query = %q", query)
		}
		if cands[0].IdentityConfidence != 0.95 {
			t.Errorf("confidence = %v, want 0.95", cands[0].IdentityConfidence)
		}
	})
	t.Run("title lower confidence", func(t *testing.T) {
		var query string
		srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, &query)
		r := newResolver(srv)
		cands, err := r.Resolve(context.Background(), work.Work{Title: "Some OA Paper"})
		if err != nil || len(cands) != 1 {
			t.Fatalf("Resolve: %v, %v", cands, err)
		}
		if query != `TITLE:"Some OA Paper"` {
			t.Errorf("query = %q", query)
		}
		if cands[0].IdentityConfidence != 0.6 {
			t.Errorf("confidence = %v, want 0.6", cands[0].IdentityConfidence)
		}
	})
	t.Run("title mismatch rejected", func(t *testing.T) {
		srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, nil)
		r := newResolver(srv)
		cands, err := r.Resolve(context.Background(), work.Work{Title: "A Completely Different Title"})
		if err != nil || cands != nil {
			t.Fatalf("want nil,nil for title mismatch; got %v, %v", cands, err)
		}
	})
}

func TestSelectResultTitleRequiresMatchingBibliography(t *testing.T) {
	requested := work.Work{Title: "Some OA Paper", Authors: []string{"Smith J", "Doe A"}, Year: 2020}
	cases := []struct {
		name   string
		result epmcResult
		want   bool
	}{
		{"matching title, authors, and year", epmcResult{Title: requested.Title, AuthorString: "Smith J, Doe A", PubYear: "2020"}, true},
		{"incomplete author list", epmcResult{Title: requested.Title, AuthorString: "Smith J", PubYear: "2020"}, false},
		{"wrong year", epmcResult{Title: requested.Title, AuthorString: "Smith J, Doe A", PubYear: "2021"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectResult([]epmcResult{tc.result}, requested, matchTitle)
			if (got != nil) != tc.want {
				t.Fatalf("selectResult = %+v, want match=%v", got, tc.want)
			}
		})
	}
}

func TestSelectResultTitleAcceptsFullNameAuthors(t *testing.T) {
	requested := work.Work{
		Title:   "Some OA Paper",
		Authors: []string{"John Smith", "Jane Doe"},
		Year:    2020,
	}
	result := epmcResult{
		Title:        requested.Title,
		AuthorString: "Smith J, Doe J",
		PubYear:      "2020",
	}
	if got := selectResult([]epmcResult{result}, requested, matchTitle); got == nil {
		t.Fatal("selectResult rejected full author names matching Europe PMC wire format")
	}
}

func TestSameAuthorCanonicalizesNameOrder(t *testing.T) {
	for _, author := range []string{"Smith J", "J Smith", "John Smith", "Smith, John"} {
		if !sameAuthor(author, "John Smith") {
			t.Errorf("sameAuthor(%q, %q) = false, want true", author, "John Smith")
		}
	}
}

func TestSameAuthorRejectsDistinctUTF8Initials(t *testing.T) {
	if sameAuthor("Émile Smith", "Östen Smith") {
		t.Fatal("sameAuthor accepted distinct non-ASCII initials")
	}
}

func TestSameAuthorMatchesMononyms(t *testing.T) {
	if !sameAuthor("Madonna", "Madonna") {
		t.Fatal("sameAuthor rejected identical mononyms")
	}
	if sameAuthor("Madonna", "Prince") {
		t.Fatal("sameAuthor accepted distinct mononyms")
	}
	if sameAuthor("Madonna", "John Smith") {
		t.Fatal("sameAuthor accepted mononym against full name")
	}
}

func TestLandingOnlyWhenNoPDF(t *testing.T) {
	body := `{"hitCount":1,"resultList":{"result":[{
      "id":"PMC9","source":"PMC","doi":"10.1000/only-html","title":"HTML Only","isOpenAccess":"Y",
      "fullTextUrlList":{"fullTextUrl":[
        {"availabilityCode":"OA","documentStyle":"html","site":"Europe_PMC","url":"https://europepmc.org/article/PMC9"}
      ]}}]}}`
	srv := serveJSON(t, http.StatusOK, nil, body, nil)
	r := newResolver(srv)
	cands, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/only-html"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve: %v, %v", cands, err)
	}
	c := cands[0]
	if c.Direct {
		t.Error("landing-only candidate must not be Direct")
	}
	if c.ExpectedMIME != "text/html" {
		t.Errorf("ExpectedMIME = %q, want text/html", c.ExpectedMIME)
	}
}

func TestNonOAAndEmptyAndMismatch(t *testing.T) {
	nonOA := strings.Replace(oaResultJSON, `"isOpenAccess": "Y"`, `"isOpenAccess": "N"`, 1)
	wrongDOI := strings.Replace(oaResultJSON, `"doi": "10.1000/xyz"`, `"doi": "10.9999/other"`, 1)
	empty := `{"hitCount":0,"resultList":{"result":[]}}`
	cases := []struct {
		name   string
		status int
		body   string
		in     work.Work
	}{
		{"non-oa result", http.StatusOK, nonOA, work.Work{DOI: "10.1000/xyz"}},
		{"empty result list", http.StatusOK, empty, work.Work{DOI: "10.1000/xyz"}},
		{"http 404", http.StatusNotFound, "", work.Work{DOI: "10.1000/xyz"}},
		{"doi mismatch", http.StatusOK, wrongDOI, work.Work{DOI: "10.1000/xyz"}},
		{"no usable identifier", http.StatusOK, oaResultJSON, work.Work{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveJSON(t, tc.status, nil, tc.body, nil)
			r := newResolver(srv)
			cands, err := r.Resolve(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("want nil error, got %v", err)
			}
			if cands != nil {
				t.Fatalf("want nil candidates, got %+v", cands)
			}
		})
	}
}

func TestMalformedAndOversizedPayload(t *testing.T) {
	t.Run("malformed json", func(t *testing.T) {
		srv := serveJSON(t, http.StatusOK, nil, `{"resultList": {`, nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		if err == nil {
			t.Fatal("want error for malformed JSON")
		}
		if _, temp := resolver.Temporary(err); temp {
			t.Error("malformed payload must not be temporary")
		}
	})
	t.Run("trailing json rejected", func(t *testing.T) {
		srv := serveJSON(t, http.StatusOK, nil, oaResultJSON+`{"extra":1}`, nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		if err == nil {
			t.Fatal("want error for multiple JSON values")
		}
	})
	t.Run("oversized bounded read", func(t *testing.T) {
		srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, nil)
		r := NewWithOptions(Options{Client: srv.Client(), BaseURL: srv.URL, MaxResponseBytes: 64})
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("want size-limit error, got %v", err)
		}
		if _, temp := resolver.Temporary(err); temp {
			t.Error("oversized payload must not be temporary")
		}
	})
}

func TestRetryClasses(t *testing.T) {
	t.Run("429 with Retry-After", func(t *testing.T) {
		srv := serveJSON(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "5"}, "", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		d, temp := resolver.Temporary(err)
		if !temp {
			t.Fatalf("429 must be temporary; err=%v", err)
		}
		if d.Seconds() != 5 {
			t.Errorf("RetryAfter = %v, want 5s", d)
		}
	})
	t.Run("500 temporary", func(t *testing.T) {
		srv := serveJSON(t, http.StatusInternalServerError, nil, "", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		if _, temp := resolver.Temporary(err); !temp {
			t.Fatalf("500 must be temporary; err=%v", err)
		}
	})
	t.Run("403 permanent", func(t *testing.T) {
		srv := serveJSON(t, http.StatusForbidden, nil, "", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		if err == nil {
			t.Fatal("403 must produce an error")
		}
		if _, temp := resolver.Temporary(err); temp {
			t.Error("403 must not be temporary")
		}
	})
	t.Run("network failure temporary", func(t *testing.T) {
		srv := serveJSON(t, http.StatusOK, nil, oaResultJSON, nil)
		client := srv.Client()
		base := srv.URL
		srv.Close()
		r := NewWithOptions(Options{Client: client, BaseURL: base})
		_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/xyz"})
		if _, temp := resolver.Temporary(err); !temp {
			t.Fatalf("network failure must be temporary; err=%v", err)
		}
	})
}

// TestSelectResultRequiresPositiveIdentity pins that an identifier-scoped query
// only selects a result whose DOI/PMID field positively equals the request. A
// result whose identifier field is empty cannot be verified and is rejected, so
// a wrong (or unverifiable) work is never emitted.
func TestSelectResultRequiresPositiveIdentity(t *testing.T) {
	t.Run("pmid empty field not selected", func(t *testing.T) {
		requested := work.Work{PMID: "12345"}
		results := []epmcResult{{ID: "a", PMID: ""}}
		if got := selectResult(results, requested, matchPMID); got != nil {
			t.Errorf("selectResult with empty PMID field = %+v, want nil", got)
		}
	})
	t.Run("pmid matching field selected", func(t *testing.T) {
		requested := work.Work{PMID: "12345"}
		results := []epmcResult{{ID: "a", PMID: ""}, {ID: "b", PMID: "12345"}}
		got := selectResult(results, requested, matchPMID)
		if got == nil || got.ID != "b" {
			t.Errorf("selectResult matching PMID = %+v, want result b", got)
		}
	})
	t.Run("doi empty field not selected", func(t *testing.T) {
		requested := work.Work{DOI: "10.1000/test"}
		results := []epmcResult{{ID: "a", DOI: ""}}
		if got := selectResult(results, requested, matchDOI); got != nil {
			t.Errorf("selectResult with empty DOI field = %+v, want nil", got)
		}
	})
	t.Run("doi matching field selected", func(t *testing.T) {
		requested := work.Work{DOI: "10.1000/test"}
		results := []epmcResult{{ID: "a", DOI: ""}, {ID: "b", DOI: "10.1000/test"}}
		got := selectResult(results, requested, matchDOI)
		if got == nil || got.ID != "b" {
			t.Errorf("selectResult matching DOI = %+v, want result b", got)
		}
	})
}

// TestParseRetryAfterClampsHugeValues pins the overflow-safe Retry-After
// parsing: a header large enough to overflow the nanosecond multiply must clamp
// to the max duration rather than wrap to a garbage (possibly negative) value.
func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	now := time.Now()
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"garbage", "not-a-number", 0},
		{"normal seconds", "5", 5 * time.Second},
		{"overflow multiply clamps to max", "99999999999", maxDuration},
		{"beyond int64 range falls through", "9999999999999999999", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRetryAfter(c.value, now)
			if got != c.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", c.value, got, c.want)
			}
			if got < 0 {
				t.Errorf("parseRetryAfter(%q) = %v, must never be negative", c.value, got)
			}
		})
	}
}
