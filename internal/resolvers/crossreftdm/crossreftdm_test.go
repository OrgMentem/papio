package crossreftdm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/resolver"
	"papio/internal/work"
)

func TestResolvePDFLinkAndNoMetadataAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected undocumented authorization header")
		}
		if r.Header.Get("Crossref-Plus-API-Token") != "Bearer tdm-secret" {
			t.Errorf("missing Crossref Plus token")
		}
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test","link":[{"URL":"https://download.example/article?Signature=signed-secret","content-type":"application/pdf","content-version":"vor","intended-application":"text-mining"},{"URL":"https://download.example/xml","content-type":"application/xml"}]}}`))
	}))
	defer server.Close()
	r := NewWithOptions(Options{Client: server.Client(), APIKey: "tdm-secret", BaseURL: server.URL})
	got, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("candidate count = %d", len(got))
	}
	candidate := got[0]
	if candidate.AccessBasis != resolver.AccessLicensedAPI || candidate.ExpectedMIME != "application/pdf" || candidate.Version != resolver.VersionPublished {
		t.Fatalf("unexpected candidate: %#v", candidate)
	}
	if strings.Contains(strings.Join(candidate.Evidence, " "), "signed-secret") || strings.Contains(strings.Join(candidate.Evidence, " "), "tdm-secret") {
		t.Fatalf("signed URL or credential leaked in evidence: %#v", candidate.Evidence)
	}
	if err := resolver.ValidateCandidate(candidate); err != nil {
		t.Fatalf("invalid candidate: %v", err)
	}
}

func TestResolveDisabledNoLinkAndFailures(t *testing.T) {
	called := false
	client := tdmRoundTrip(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })
	if got, err := New(client, "").Resolve(context.Background(), work.Work{DOI: "10.1000/test"}); err != nil || len(got) != 0 || called {
		t.Fatalf("disabled = %#v, %v, called=%v", got, err, called)
	}
	if got, err := NewConfigured(client, config.Source{Enabled: false, APIKey: "token"}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"}); err != nil || len(got) != 0 || called {
		t.Fatalf("policy disabled = %#v, %v, called=%v", got, err, called)
	}

	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "2")
				}
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test","link":[{"URL":"https://example.test/file.pdf","content-type":"text/html"}]}}`))
					return
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "token", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
			if status == http.StatusOK {
				if err != nil || len(got) != 0 {
					t.Fatalf("non-PDF link: %#v, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			wait, temporary := resolver.Temporary(err)
			if temporary != (status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500) {
				t.Fatalf("temporary = %v for %v", temporary, err)
			}
			if status == http.StatusTooManyRequests && wait != 2*time.Second {
				t.Fatalf("retry after = %v", wait)
			}
		})
	}
}

func TestResolveRejectsMalformedOversizedAndCrossHostCredentialRedirect(t *testing.T) {
	for name, body := range map[string]string{"malformed": "{", "oversized": `{"message":` + strings.Repeat("x", 200)} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, err := NewWithOptions(Options{Client: server.Client(), APIKey: "token", BaseURL: server.URL, MaxResponseBytes: 32}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
			if err == nil {
				t.Fatal("expected decode failure")
			}
		})
	}

	var redirectedAuthorization, redirectedToken string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuthorization, redirectedToken = r.Header.Get("Authorization"), r.Header.Get("Crossref-Plus-API-Token")
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test"}}`))
	}))
	defer destination.Close()
	target, err := url.Parse(destination.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.String(), http.StatusFound) }))
	defer origin.Close()
	_, err = NewWithOptions(Options{Client: origin.Client(), APIKey: "must-not-leak", BaseURL: origin.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil {
		t.Fatal(err)
	}
	if redirectedAuthorization != "" || redirectedToken != "" {
		t.Fatalf("credential leaked on redirect: authorization=%q token=%q", redirectedAuthorization, redirectedToken)
	}
}

type tdmRoundTrip func(*http.Request) (*http.Response, error)

func (f tdmRoundTrip) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveEscapesDOIAndScopesCredentialHeaders(t *testing.T) {
	var escapedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escapedPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/a/b","title":["Resolved title"],"author":[{"given":"Ada","family":"Lovelace"}],"published-online":{"date-parts":[[2025,1,1]]},"link":[{"URL":"http://` + r.Host + `","content-type":"application/pdf","content-version":"vor","intended-application":"text-mining"}]}}`))
	}))
	defer server.Close()

	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "tdm-secret", BaseURL: server.URL + "/works"}).Resolve(context.Background(), work.Work{DOI: "10.1000/a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if escapedPath != "/works/10.1000%2Fa%2Fb" {
		t.Fatalf("escaped DOI path = %q", escapedPath)
	}
	if len(got) != 1 {
		t.Fatalf("candidate count = %d", len(got))
	}
	if got[0].RequestHeaders != nil {
		t.Fatalf("publisher candidate must not receive metadata credentials: %#v", got[0].RequestHeaders)
	}
	if got[0].ResolvedWork.Title != "Resolved title" || got[0].ResolvedWork.Year != 2025 || strings.Join(got[0].ResolvedWork.Authors, " ") != "Ada Lovelace" {
		t.Fatalf("resolved work = %#v", got[0].ResolvedWork)
	}
	if strings.Contains(strings.Join(got[0].Evidence, " "), "tdm-secret") {
		t.Fatalf("credential leaked in evidence: %#v", got[0].Evidence)
	}
}

func TestResolveRejectsExplicitNonTDMMetadataLink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test","link":[{"URL":"https://publisher.example/article.pdf","content-type":"application/pdf","intended-application":"similarity-checking"}]}}`))
	}))
	defer server.Close()

	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "token", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("non-TDM metadata link became authorized candidate: %#v", got)
	}
}

func TestResolveKeepsCredentialsStrippedAcrossForeignRedirects(t *testing.T) {
	var finalAuthorization, finalToken string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalAuthorization = r.Header.Get("Authorization")
		finalToken = r.Header.Get("Crossref-Plus-API-Token")
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test"}}`))
	}))
	defer final.Close()
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusFound)
	}))
	defer origin.Close()

	_, err := NewWithOptions(Options{Client: origin.Client(), APIKey: "must-not-leak", BaseURL: origin.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil {
		t.Fatal(err)
	}
	if finalAuthorization != "" || finalToken != "" {
		t.Fatalf("credential leaked after foreign redirect: authorization=%q token=%q", finalAuthorization, finalToken)
	}
}

func TestResolveMatchesLicenseToPDFVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test","license":[{"URL":"https://licenses.example/vor","content-version":"vor"},{"URL":"https://licenses.example/am","content-version":"am"}],"link":[{"URL":"https://publisher.example/vor.pdf","content-type":"application/pdf","content-version":"vor","intended-application":"text-mining"},{"URL":"https://publisher.example/am.pdf","content-type":"application/pdf","content-version":"am","intended-application":"text-mining"}]}}`))
	}))
	defer server.Close()

	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "token", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil || len(got) != 2 {
		t.Fatalf("licensed PDF results = %#v, %v", got, err)
	}
	licenses := map[string]string{}
	for _, candidate := range got {
		licenses[candidate.Version] = candidate.ReuseLicense
	}
	if licenses[resolver.VersionPublished] != "https://licenses.example/vor" || licenses[resolver.VersionAccepted] != "https://licenses.example/am" {
		t.Fatalf("licenses by version = %#v", licenses)
	}
}

func TestResolveRejectsUnsafeClient(t *testing.T) {
	_, err := New(tdmRoundTrip(func(*http.Request) (*http.Response, error) { return nil, nil }), "token").Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err == nil {
		t.Fatal("opaque client must be rejected before an authenticated request")
	}
	if _, temporary := resolver.Temporary(err); temporary {
		t.Fatalf("unsafe client error must be permanent: %v", err)
	}
}

func TestResolveRequiresTextMiningMarkerAndLeavesUnknownVersionUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"DOI":"10.1000/test","link":[
			{"URL":"https://publisher.example/unmarked.pdf","content-type":"application/pdf","content-version":"publisher-layout"},
			{"URL":"https://publisher.example/unknown.pdf","content-type":"application/pdf","content-version":"publisher-layout","intended-application":"text-mining"}
		]}}`))
	}))
	defer server.Close()

	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "token", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil || len(got) != 1 {
		t.Fatalf("strict TDM PDF results = %#v, %v", got, err)
	}
	if got[0].Version != resolver.VersionUnknown {
		t.Fatalf("unknown content version mapped to %q", got[0].Version)
	}
}
