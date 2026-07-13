package unpaywall

import (
	"context"
	"errors"
	"io"
	"net/http"
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
	const best = `{"is_oa":true,"title":"A title","year":2024,"publication_year":1999,"z_authors":[{"given":"A.","family":"Author"}],"best_oa_location":{"url_for_pdf":"https://best.example/article.pdf","url_for_landing_page":"https://best.example/article","host_type":"publisher","version":"publishedVersion","license":"cc-by"},"oa_locations":[{"url_for_pdf":"https://later.example/article.pdf"}]}`
	const fallback = `{"is_oa":true,"best_oa_location":{"url":"https://landing.example/best"},"oa_locations":[{"url":"https://landing.example/first"},{"url_for_pdf":"https://pdf.example/first.pdf","version":"acceptedVersion"},{"url_for_pdf":"https://pdf.example/second.pdf"}]}`
	const landing = `{"is_oa":true,"best_oa_location":{"url":"https://landing.example/best"},"oa_locations":[{"url":"https://landing.example/first"}]}`
	const missing = `{"is_oa":true,"best_oa_location":{"url_for_pdf":"https://best.example/article.pdf"}}`
	const secret = `{"is_oa":true,"best_oa_location":{"url_for_pdf":"https://files.example/a.pdf?token=SECRET"}}`
	const invalidBest = `{"is_oa":true,"best_oa_location":{"url_for_pdf":"not a URL"},"oa_locations":[{"url_for_pdf":"https://pdf.example/fallback.pdf"}]}`
	const invalidBestLanding = `{"is_oa":true,"best_oa_location":{"url_for_landing_page":"not a URL","url":"https://landing.example/best"}}`
	const fallbackLanding = `{"is_oa":true,"best_oa_location":{"url_for_landing_page":"not a URL"},"oa_locations":[{"url":"https://landing.example/fallback"}]}`

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
		email       string
	}{
		{name: "best location", status: 200, body: best, wantURL: "https://best.example/article.pdf", wantVersion: resolver.VersionPublished, wantLicense: "cc-by", wantDirect: true},
		{name: "first PDF fallback", status: 200, body: fallback, wantURL: "https://pdf.example/first.pdf", wantVersion: resolver.VersionAccepted, wantLicense: "unknown", wantDirect: true},
		{name: "retain landing without PDF", status: 200, body: landing, wantURL: "https://landing.example/best", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: false},
		{name: "missing license and version", status: 200, body: missing, wantURL: "https://best.example/article.pdf", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: true},
		{name: "invalid best PDF falls back", status: 200, body: invalidBest, wantURL: "https://pdf.example/fallback.pdf", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: true},
		{name: "invalid preferred landing uses valid URL", status: 200, body: invalidBestLanding, wantURL: "https://landing.example/best", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: false},
		{name: "fallback landing is retained", status: 200, body: fallbackLanding, wantURL: "https://landing.example/fallback", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: false},
		{name: "non OA", status: 200, body: `{"is_oa":false}`, wantURL: ""},
		{name: "malformed JSON", status: 200, body: `{`, wantErr: true},
		{name: "not found", status: 404, wantURL: ""},
		{name: "unauthorized is permanent", status: 401, wantErr: true},
		{name: "forbidden is permanent", status: 403, wantErr: true},
		{name: "request timeout", status: 408, wantErr: true, temporary: true},
		{name: "rate limited", status: 429, headers: map[string]string{"Retry-After": "7"}, wantErr: true, temporary: true, retry: 7 * time.Second},
		{name: "upstream failure", status: 503, wantErr: true, temporary: true},
		{name: "secret URL redacted in evidence", status: 200, body: secret, wantURL: "https://files.example/a.pdf?token=SECRET", wantVersion: resolver.VersionUnknown, wantLicense: "unknown", wantDirect: true},
		{name: "missing contact", status: 200, body: best, wantErr: true, email: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email := tt.email
			if tt.name != "missing contact" {
				email = "contact@example.org"
			}
			client := clientFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.Query().Get("email") != email {
					t.Fatalf("unexpected request %s", req.URL.Redacted())
				}
				return responseFor(tt.status, tt.body, tt.headers), nil
			})
			r := NewWithOptions(Options{Client: client, ContactEmail: email, BaseURL: "https://api.test/v2"})
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

func TestResolveNetworkFailureIsTemporary(t *testing.T) {
	r := New(clientFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("network down") }), "contact@example.org")
	_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"})
	if _, ok := resolver.Temporary(err); !ok {
		t.Fatalf("error %v is not temporary", err)
	}
}

func TestResolveOversizedJSONFailsClosed(t *testing.T) {
	r := NewWithOptions(Options{Client: clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(200, `{"is_oa":true,"padding":"`+strings.Repeat("x", 64)+`"}`, nil), nil
	}), ContactEmail: "contact@example.org", MaxResponseBytes: 16})
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"}); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestResolveIncludesDiscoveredWork(t *testing.T) {
	r := New(clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(http.StatusOK, `{"doi":"10.1000/EXAMPLE","is_oa":true,"title":" Source title ","year":2024,"publication_year":1999,"z_authors":[{"given":" A.","family":"Author "},{"given":"B.","family":"Author"}],"best_oa_location":{"url_for_pdf":"https://files.example/article.pdf"}}`, nil), nil
	}), "contact@example.org")

	candidates, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/example"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("Resolve = %#v, %v", candidates, err)
	}
	got := candidates[0].ResolvedWork
	if got.DOI != "10.1000/example" || got.Title != "Source title" || got.Year != 2024 || strings.Join(got.Authors, ",") != "A. Author,B. Author" {
		t.Fatalf("ResolvedWork = %#v", got)
	}
}
