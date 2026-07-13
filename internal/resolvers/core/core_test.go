package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/resolver"
	"papio/internal/work"
)

func TestResolveDOISuccessAndTitleFallback(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer core-secret" {
			t.Errorf("authorization = %q", got)
		}
		query := r.URL.Query().Get("q")
		queries = append(queries, query)
		w.Header().Set("Content-Type", "application/json")
		if query == "10.1000/missing" {
			_, _ = w.Write([]byte(`{"results":[] }`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"doi":"10.1000/missing","title":"A Precise Title","downloadUrl":"https://files.example/article.pdf","version":"accepted","license":"CC-BY"}]}`))
	}))
	defer server.Close()

	r := NewWithOptions(Options{Client: server.Client(), APIKey: "core-secret", BaseURL: server.URL})
	got, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/missing", Title: "A Precise Title"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("candidate count = %d", len(got))
	}
	if want := []string{"10.1000/missing", "A Precise Title"}; fmt.Sprint(queries) != fmt.Sprint(want) {
		t.Fatalf("queries = %v, want %v", queries, want)
	}
	if got[0].AccessBasis != resolver.AccessLicensedAPI || got[0].ExpectedMIME != "application/pdf" || got[0].Version != resolver.VersionAccepted {
		t.Fatalf("unexpected candidate: %#v", got[0])
	}
	if err := resolver.ValidateCandidate(got[0]); err != nil {
		t.Fatalf("invalid candidate: %v", err)
	}
}

func TestResolveDisabledAndNoFullText(t *testing.T) {
	called := false
	client := roundTripFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("should not call") })
	if got, err := New(client, "").Resolve(context.Background(), work.Work{DOI: "10.1000/test"}); err != nil || len(got) != 0 || called {
		t.Fatalf("disabled = %#v, %v, called=%v", got, err, called)
	}
	if got, err := NewConfigured(client, config.Source{Enabled: false, APIKey: "key"}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"}); err != nil || len(got) != 0 || called {
		t.Fatalf("policy disabled = %#v, %v, called=%v", got, err, called)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"doi":"10.1000/test","title":"Test","landingPageUrl":"https://publisher.example/landing"}]}`))
	}))
	defer server.Close()
	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "key", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil || len(got) != 0 {
		t.Fatalf("metadata link became authorized candidate: %#v, %v", got, err)
	}
}

func TestResolveHTTPFailuresAndRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "3")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := NewWithOptions(Options{Client: server.Client(), APIKey: "key", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
			if err == nil {
				t.Fatal("expected error")
			}
			wait, temporary := resolver.Temporary(err)
			if temporary != (status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500) {
				t.Fatalf("temporary = %v for %v", temporary, err)
			}
			if status == http.StatusTooManyRequests && wait != 3*time.Second {
				t.Fatalf("retry after = %v", wait)
			}
			if (status == http.StatusUnauthorized || status == http.StatusForbidden) && strings.Contains(err.Error(), "key") && strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("credential leaked: %v", err)
			}
		})
	}
}

func TestResolveRejectsMalformedAndOversizedJSON(t *testing.T) {
	for name, body := range map[string]string{"malformed": "{", "oversized": `{"results": ["` + strings.Repeat("x", 200) + `"]}`} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, err := NewWithOptions(Options{Client: server.Client(), APIKey: "key", BaseURL: server.URL, MaxResponseBytes: 32}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
			if err == nil {
				t.Fatal("expected decode failure")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolvePreservesBibliographicIdentityAndStripsCrossHostBearer(t *testing.T) {
	var redirectedAuthorization string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[{"doi":"10.1000/test","title":"Resolved title","authors":[{"firstName":"Ada","lastName":"Lovelace"}],"yearPublished":2025,"downloadUrl":"https://files.example/article.pdf","version":"published"}]}`))
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()

	got, err := NewWithOptions(Options{Client: origin.Client(), APIKey: "core-secret", BaseURL: origin.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil {
		t.Fatal(err)
	}
	if redirectedAuthorization != "" {
		t.Fatalf("CORE bearer leaked on cross-host redirect: %q", redirectedAuthorization)
	}
	if len(got) != 1 {
		t.Fatalf("candidate count = %d", len(got))
	}
	if resolved := got[0].ResolvedWork; resolved.DOI != "10.1000/test" || resolved.Title != "Resolved title" || resolved.Year != 2025 || fmt.Sprint(resolved.Authors) != "[Ada Lovelace]" {
		t.Fatalf("resolved work = %#v", resolved)
	}
}

func TestResolveRejectsConflictingTitleAuthorsAndNonFullTextLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"doi":"10.1000/other","title":"Same title","authors":["Different Author"],"downloadUrl":"https://files.example/conflict.pdf"},
			{"doi":"10.1000/other2","title":"Same title","authors":["Expected Author"],"links":[{"url":"https://publisher.example/metadata","type":"landing"}]}
		]}`))
	}))
	defer server.Close()

	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "key", BaseURL: server.URL}).Resolve(context.Background(), work.Work{Title: "Same title", Authors: []string{"Expected Author"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("identity-conflicting or metadata records became candidates: %#v", got)
	}
}

func TestResolveAcceptsParameterizedPDFAndRejectsUnsafeClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"doi":"10.1000/test","links":[{"url":"https://files.example/download","contentType":"application/pdf; charset=binary"}]}]}`))
	}))
	defer server.Close()

	got, err := NewWithOptions(Options{Client: server.Client(), APIKey: "key", BaseURL: server.URL}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err != nil || len(got) != 1 || got[0].ExpectedMIME != "application/pdf" {
		t.Fatalf("parameterized PDF result = %#v, %v", got, err)
	}

	_, err = New(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }), "key").Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if err == nil {
		t.Fatal("opaque client must be rejected before an authenticated request")
	}
	if _, temporary := resolver.Temporary(err); temporary {
		t.Fatalf("unsafe client error must be permanent: %v", err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveClassifiesNetworkAndDeadlineFailuresAsTemporary(t *testing.T) {
	networkClient := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	_, err := NewWithOptions(Options{Client: networkClient, APIKey: "core-secret"}).Resolve(context.Background(), work.Work{DOI: "10.1000/test"})
	if _, temporary := resolver.Temporary(err); !temporary {
		t.Fatalf("network error must be temporary: %v", err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = NewWithOptions(Options{Client: http.DefaultClient, APIKey: "core-secret"}).Resolve(ctx, work.Work{DOI: "10.1000/test"})
	if _, temporary := resolver.Temporary(err); !temporary {
		t.Fatalf("deadline error must be temporary: %v", err)
	}
}
