// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/resolver"
	"papio/internal/sourcegate"
	"papio/internal/work"
)

type seqHTTPClient struct {
	handlers []func(*http.Request) (*http.Response, error)
	calls    int
}

func (c *seqHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c.calls >= len(c.handlers) {
		return nil, errors.New("unexpected request")
	}
	h := c.handlers[c.calls]
	c.calls++
	return h(req)
}

type countingEgress struct {
	commits int
}

func (c *countingEgress) CommitEgress(context.Context, budget.EgressRequest) error {
	c.commits++
	return nil
}

func (c *countingEgress) Defer(context.Context, string, config.Source, time.Time) error { return nil }
func (c *countingEgress) LatchQuota(string, string, time.Time)                          {}
func (c *countingEgress) QuotaLatchedUntil(string, string) (time.Time, bool) {
	return time.Time{}, false
}

type guardedWithCounter struct {
	HTTPClient
	egress *countingEgress
}

func newGuardedClient(t *testing.T, inner HTTPClient) (*guardedWithCounter, *countingEgress) {
	t.Helper()
	egress := &countingEgress{}
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	guarded, err := sourcegate.NewGuarded(egress, config.SourceOpenAlex, keyed, sourcegate.OpenAlexCreditCost, inner)
	if err != nil {
		t.Fatal(err)
	}
	return &guardedWithCounter{HTTPClient: guarded, egress: egress}, egress
}

const mergedRecordBody = `{"id":"https://openalex.org/W2741809808","ids":{"openalex":"https://openalex.org/W2741809808"},"title":"Shape Trust","publication_year":2022,"authorships":[{"author":{"display_name":"Andrea Ferrario"}}],"open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/merged.pdf","version":"publishedVersion","license":"cc-by"}}`

func TestBearerCredentialNeverInQuery(t *testing.T) {
	var seen []*http.Request
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req)
		return responseFor(200, mergedRecordBody, nil), nil
	})
	r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
	if _, err := r.Resolve(context.Background(), work.Work{OpenAlex: "W2741809808"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	if seen[0].URL.Query().Get("api_key") != "" {
		t.Fatalf("api_key leaked in query: %s", seen[0].URL.RawQuery)
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer private-key" {
		t.Fatalf("Authorization = %q, want bearer private-key", got)
	}

	seen = nil
	if _, err := r.Resolve(resolver.WithAnonymousCredentials(context.Background()), work.Work{OpenAlex: "W2741809808"}); err != nil {
		t.Fatal(err)
	}
	if seen[0].URL.Query().Get("api_key") != "" || seen[0].Header.Get("Authorization") != "" {
		t.Fatal("anonymous request carried a credential")
	}
}

func TestMergeRedirectTwoAdmissions(t *testing.T) {
	seq := &seqHTTPClient{
		handlers: []func(*http.Request) (*http.Response, error){
			func(*http.Request) (*http.Response, error) {
				return responseFor(http.StatusMovedPermanently, "", map[string]string{
					"Location": "https://api.test/works/W2741809808",
				}), nil
			},
			func(*http.Request) (*http.Response, error) {
				return responseFor(http.StatusOK, mergedRecordBody, nil), nil
			},
		},
	}
	guardedClient, egress := newGuardedClient(t, seq)
	r := NewWithOptions(Options{Client: guardedClient, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
	candidates, err := r.Resolve(context.Background(), work.Work{OpenAlex: "W2741809807"})
	if err != nil {
		t.Fatal(err)
	}
	if seq.calls != 2 {
		t.Fatalf("physical requests = %d, want 2", seq.calls)
	}
	if egress.commits != 2 {
		t.Fatalf("egress commits = %d, want 2", egress.commits)
	}
	if len(candidates) != 1 || candidates[0].Authority != resolver.AuthorityExactEcho {
		t.Fatalf("candidates = %#v, want one exact-echo merge acceptance", candidates)
	}
}

func TestCrossHostEntityRedirectRefused(t *testing.T) {
	client := clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(http.StatusMovedPermanently, "", map[string]string{
			"Location": "https://evil.example/works/W2741809808",
		}), nil
	})
	r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
	_, err := r.Resolve(context.Background(), work.Work{OpenAlex: "W2741809807"})
	if err == nil || !strings.Contains(err.Error(), "cross-host") {
		t.Fatalf("err = %v, want cross-host refusal", err)
	}
}

func TestUnrelatedRecordWithoutAliasStillRefused(t *testing.T) {
	body := `{"id":"https://openalex.org/W9999999999","ids":{"openalex":"https://openalex.org/W9999999999"},"title":"Other","open_access":{"is_oa":true},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/x.pdf"}}`
	client := clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(http.StatusOK, body, nil), nil
	})
	r := New(client, "contact@example.org", "private-key")
	candidates, err := r.Resolve(context.Background(), work.Work{OpenAlex: "W2741809807"})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %#v, want refusal without alias chain", candidates)
	}
}

func TestEntityRedirectDepthBounded(t *testing.T) {
	loop := func(*http.Request) (*http.Response, error) {
		return responseFor(http.StatusMovedPermanently, "", map[string]string{
			"Location": "/works/W2741809808",
		}), nil
	}
	handlers := make([]func(*http.Request) (*http.Response, error), maxOpenAlexEntityRedirects+2)
	for i := range handlers {
		handlers[i] = loop
	}
	seq := &seqHTTPClient{handlers: handlers}
	r := NewWithOptions(Options{Client: seq, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
	_, err := r.Resolve(context.Background(), work.Work{OpenAlex: "W2741809807"})
	if err == nil || !strings.Contains(err.Error(), "redirect depth exceeded") {
		t.Fatalf("err = %v, want depth bound", err)
	}
	if seq.calls != maxOpenAlexEntityRedirects+1 {
		t.Fatalf("calls = %d, want %d hops before giving up", seq.calls, maxOpenAlexEntityRedirects+1)
	}
}

func TestEntityMergeAlias_guardRequired(t *testing.T) {
	var record workRecord
	if err := decodeBoundedJSON(io.NopCloser(strings.NewReader(mergedRecordBody)), defaultMaxBody, &record); err != nil {
		t.Fatal(err)
	}
	if (entityMergeAlias{}).accepts(record, "W2741809807") {
		t.Fatal("without alias evidence a merged id must not pass as Wold")
	}
	alias := entityMergeAlias{from: "W2741809807", to: "W2741809808"}
	if !alias.accepts(record, "W2741809807") {
		t.Fatal("authoritative Wold->Wnew alias must accept the merged record")
	}
}
