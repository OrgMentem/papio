package openalex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"papio/internal/resolver"
	"papio/internal/work"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func responseFor(status int, body string, headers map[string]string) *http.Response {
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestResolveVectors(t *testing.T) {
	const best = `{"open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://best.example/article.pdf","landing_page_url":"https://best.example/article","license":"cc-by","version":"publishedVersion"},"locations":[{"is_oa":true,"pdf_url":"https://later.example/article.pdf"}]}`
	const fallback = `{"open_access":{"is_oa":true,"oa_status":"green"},"best_oa_location":null,"locations":[{"is_oa":false,"pdf_url":"https://paywall.example/no.pdf"},{"is_oa":true,"pdf_url":"https://pdf.example/first.pdf","version":"acceptedVersion"},{"is_oa":true,"pdf_url":"https://pdf.example/second.pdf"}]}`
	const missing = `{"open_access":{"is_oa":true,"oa_status":"bronze"},"best_oa_location":{"is_oa":true,"pdf_url":"https://best.example/article.pdf"}}`
	const secret = `{"open_access":{"is_oa":true,"oa_status":"green"},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/a.pdf?token=SECRET"}}`
	const linkOnly = `{"open_access":{"is_oa":true,"oa_status":"green"},"best_oa_location":{"is_oa":false,"pdf_url":"https://paywall.example/no.pdf"}}`
	const topLevelOnly = `{"is_oa":true,"best_oa_location":{"is_oa":true,"pdf_url":"https://best.example/article.pdf"}}`
	const invalidBest = `{"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"not a URL"},"locations":[{"is_oa":true,"pdf_url":"https://pdf.example/fallback.pdf"}]}`
	const bestLanding = `{"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"landing_page_url":"https://landing.example/best"}}`
	const fallbackLanding = `{"open_access":{"is_oa":true},"best_oa_location":{"is_oa":false,"landing_page_url":"https://paywall.example/no"},"locations":[{"is_oa":false,"landing_page_url":"https://paywall.example/also-no"},{"is_oa":true,"landing_page_url":"https://landing.example/fallback"}]}`

	tests := []struct {
		name        string
		status      int
		body        string
		headers     map[string]string
		wantURL     string
		wantVersion string
		wantLicense string
		wantDirect  bool
		wantErr     bool
		temporary   bool
		retry       time.Duration
	}{
		{name: "best location", status: 200, body: best, wantURL: "https://best.example/article.pdf", wantVersion: resolver.VersionPublished, wantLicense: "cc-by", wantDirect: true},
		{name: "null best fallback PDF", status: 200, body: fallback, wantURL: "https://pdf.example/first.pdf", wantVersion: resolver.VersionAccepted, wantLicense: "unknown", wantDirect: true},
		{name: "missing license and version", status: 200, body: missing, wantURL: "https://best.example/article.pdf", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: true},
		{name: "invalid best PDF falls back", status: 200, body: invalidBest, wantURL: "https://pdf.example/fallback.pdf", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: true},
		{name: "best legal landing", status: 200, body: bestLanding, wantURL: "https://landing.example/best", wantVersion: resolver.VersionUnknown, wantLicense: "unknown"},
		{name: "fallback legal landing", status: 200, body: fallbackLanding, wantURL: "https://landing.example/fallback", wantVersion: resolver.VersionUnknown, wantLicense: "unknown"},
		{name: "link alone is not legal", status: 200, body: linkOnly},
		{name: "top-level OA is not authoritative", status: 200, body: topLevelOnly},
		{name: "malformed JSON", status: 200, body: `{`, wantErr: true},
		{name: "not found", status: 404},
		{name: "unauthorized is permanent", status: 401, wantErr: true},
		{name: "forbidden is permanent", status: 403, wantErr: true},
		{name: "request timeout", status: 408, wantErr: true, temporary: true},
		{name: "rate limited", status: 429, headers: map[string]string{"Retry-After": "9"}, wantErr: true, temporary: true, retry: 9 * time.Second},
		{name: "upstream failure", status: 502, wantErr: true, temporary: true},
		{name: "secret URL redacted in evidence", status: 200, body: secret, wantURL: "https://files.example/a.pdf?token=SECRET", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := clientFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Query().Get("mailto") != "contact@example.org" || req.URL.Query().Get("api_key") != "private-key" {
					t.Fatalf("polite-pool credentials missing")
				}
				return responseFor(tt.status, tt.body, tt.headers), nil
			})
			r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
			candidates, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				retry, temporary := resolver.Temporary(err)
				if temporary != tt.temporary || (tt.temporary && tt.retry != 0 && retry != tt.retry) {
					t.Fatalf("temporary = %v/%v, want %v/%v", temporary, retry, tt.temporary, tt.retry)
				}
				return
			}
			if tt.wantURL == "" {
				if len(candidates) != 0 {
					t.Fatalf("candidates = %#v, want empty", candidates)
				}
				return
			}
			if len(candidates) != 1 {
				t.Fatalf("candidates = %#v, want one", candidates)
			}
			got := candidates[0]
			if got.URL != tt.wantURL || got.Version != tt.wantVersion || got.ReuseLicense != tt.wantLicense || got.Direct != tt.wantDirect {
				t.Fatalf("candidate = %#v", got)
			}
			if strings.Contains(strings.Join(got.Evidence, " "), "SECRET") {
				t.Fatalf("secret leaked in evidence: %#v", got.Evidence)
			}
		})
	}
}

func TestResolveLookupsAndTemporaryNetworkFailure(t *testing.T) {
	var requested []*http.Request
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.Clone(req.Context()))
		if req.URL.Query().Get("search") != "" {
			return responseFor(200, `{"results":[]}`, nil), nil
		}
		return responseFor(404, "", nil), nil
	})
	r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
	for _, requestedWork := range []work.Work{{DOI: "10.1000/example"}, {OpenAlex: "W123456789"}, {Title: "A precise title"}} {
		if _, err := r.Resolve(context.Background(), requestedWork); err != nil {
			t.Fatal(err)
		}
	}
	if len(requested) != 3 || !strings.Contains(requested[0].URL.Path, "doi.org") || !strings.HasSuffix(requested[1].URL.Path, "/W123456789") || requested[2].URL.Query().Get("search") != "A precise title" || requested[2].URL.Query().Get("per_page") != "10" {
		t.Fatalf("lookup requests = %#v", requested)
	}

	network := New(clientFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("network down") }), "contact@example.org", "private-key")
	_, err := network.Resolve(context.Background(), work.Work{DOI: "10.1000/example"})
	if _, ok := resolver.Temporary(err); !ok {
		t.Fatalf("error %v is not temporary", err)
	}
}

func TestResolveOversizedJSONFailsClosed(t *testing.T) {
	r := NewWithOptions(Options{Client: clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(200, `{"open_access":{"is_oa":true},"padding":"`+strings.Repeat("x", 64)+`"}`, nil), nil
	}), ContactEmail: "contact@example.org", APIKey: "private-key", MaxResponseBytes: 16})
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"}); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestResolveIncludesDiscoveredWork(t *testing.T) {
	r := New(clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(http.StatusOK, `{"id":"https://openalex.org/W123456789","ids":{"doi":"https://doi.org/10.1000/EXAMPLE","pmid":"https://pubmed.ncbi.nlm.nih.gov/0012345","arxiv":"https://arxiv.org/abs/2101.00001v2","openalex":"https://openalex.org/W123456789"},"title":" Source title ","publication_year":2024,"authorships":[{"author":{"display_name":" A. Author "}},{"author":{"display_name":"B. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/article.pdf"}}`, nil), nil
	}), "contact@example.org", "private-key")

	candidates, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Resolve = %#v, %v", candidates, err)
	}
	got := candidates[0].ResolvedWork
	if got.DOI != "10.1000/example" || got.PMID != "12345" || got.ArXiv != "2101.00001v2" || got.OpenAlex != "W123456789" || got.Title != "Source title" || got.Year != 2024 || strings.Join(got.Authors, ",") != "A. Author,B. Author" {
		t.Fatalf("ResolvedWork = %#v", got)
	}
}

func TestResolveTitleRequiresMatchingBibliography(t *testing.T) {
	const matched = `{"results":[{"title":"Exact title","publication_year":2024,"authorships":[{"author":{"display_name":"A. Author"}},{"author":{"display_name":"B. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/article.pdf"}}]}`
	const nearTitle = `{"results":[{"title":"Exact title and related work","publication_year":2024,"authorships":[{"author":{"display_name":"A. Author"}},{"author":{"display_name":"B. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/article.pdf"}}]}`
	const wrongYear = `{"results":[{"title":"Exact title","publication_year":2023,"authorships":[{"author":{"display_name":"A. Author"}},{"author":{"display_name":"B. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/article.pdf"}}]}`
	const incompleteAuthors = `{"results":[{"title":"Exact title","publication_year":2024,"authorships":[{"author":{"display_name":"A. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/article.pdf"}}]}`
	requested := work.Work{Title: "Exact title", Authors: []string{"A. Author", "B. Author"}, Year: 2024}
	for name, body := range map[string]string{
		"matching bibliography":   matched,
		"near-title first result": nearTitle,
		"wrong year":              wrongYear,
		"incomplete author list":  incompleteAuthors,
	} {
		t.Run(name, func(t *testing.T) {
			r := New(clientFunc(func(*http.Request) (*http.Response, error) {
				return responseFor(http.StatusOK, body, nil), nil
			}), "contact@example.org", "private-key")
			candidates, err := r.Resolve(context.Background(), requested)
			if err != nil {
				t.Fatal(err)
			}
			if name == "matching bibliography" {
				if len(candidates) != 1 {
					t.Fatalf("candidates = %#v, want one", candidates)
				}
				return
			}
			if candidates != nil {
				t.Fatalf("candidates = %#v, want nil", candidates)
			}
		})
	}
}

func TestResolveTitleSelectsMatchingSecondResult(t *testing.T) {
	const body = `{"results":[
		{"title":"Exact title and related work","publication_year":2024,"authorships":[{"author":{"display_name":"A. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/near.pdf"}},
		{"title":"Exact title","publication_year":2024,"authorships":[{"author":{"display_name":"A. Author"}}],"open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://files.example/match.pdf"}}
	]}`
	r := New(clientFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Query().Get("per_page"); got != "10" {
			t.Fatalf("per_page = %q, want 10", got)
		}
		return responseFor(http.StatusOK, body, nil), nil
	}), "contact@example.org", "private-key")

	candidates, err := r.Resolve(context.Background(), work.Work{
		Title:   "Exact title",
		Authors: []string{"A. Author"},
		Year:    2024,
	})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Resolve = %#v, %v; want one candidate", candidates, err)
	}
	if candidates[0].URL != "https://files.example/match.pdf" {
		t.Fatalf("candidate URL = %q, want exact second result", candidates[0].URL)
	}
}

func TestSameAuthorRejectsDistinctUTF8Initials(t *testing.T) {
	if sameAuthor("Émile Smith", "Östen Smith") {
		t.Fatal("sameAuthor accepted distinct non-ASCII initials")
	}
}

// The OpenAlex works API is free in the polite pool: a contact email is the
// only requirement and the api_key parameter is optional premium capacity —
// the same stance as the discovery client.
func TestKeylessPolitePoolSendsMailtoAndOmitsAPIKey(t *testing.T) {
	var gotQuery url.Values
	r := New(clientFunc(func(req *http.Request) (*http.Response, error) {
		gotQuery = req.URL.Query()
		return responseFor(http.StatusNotFound, "", nil), nil
	}), "contact@example.org")
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"}); err != nil {
		t.Fatalf("keyless polite-pool resolve errored: %v", err)
	}
	if got := gotQuery.Get("mailto"); got != "contact@example.org" {
		t.Fatalf("mailto = %q, want the polite-pool contact", got)
	}
	if got := gotQuery.Get("api_key"); got != "" {
		t.Fatalf("api_key = %q, want omitted when unconfigured", got)
	}
}

func TestIdentifierTail(t *testing.T) {
	tests := map[string]string{
		"bare value":        "12345",
		"identifier URL":    "https://pubmed.ncbi.nlm.nih.gov/0012345/",
		"host only URL":     "https://pubmed.ncbi.nlm.nih.gov",
		"root path URL":     "https://pubmed.ncbi.nlm.nih.gov/",
		"unparseable value": "%",
	}
	want := map[string]string{
		"bare value":        "12345",
		"identifier URL":    "0012345",
		"host only URL":     "https://pubmed.ncbi.nlm.nih.gov",
		"root path URL":     "https://pubmed.ncbi.nlm.nih.gov/",
		"unparseable value": "%",
	}
	for name, raw := range tests {
		if got := identifierTail(raw); got != want[name] {
			t.Errorf("%s: identifierTail(%q) = %q, want %q", name, raw, got, want[name])
		}
	}
}

func TestLoopbackEndpointAllowsMissingAPIKey(t *testing.T) {
	r := NewWithOptions(Options{
		Client: clientFunc(func(*http.Request) (*http.Response, error) {
			return responseFor(http.StatusNotFound, "", nil), nil
		}),
		ContactEmail: "contact@example.org",
		BaseURL:      "http://127.0.0.1:8080/works",
	})
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"}); err != nil {
		t.Fatalf("loopback resolver rejected missing key: %v", err)
	}
}
