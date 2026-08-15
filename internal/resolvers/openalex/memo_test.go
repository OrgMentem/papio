// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"papio/internal/resolver"
	"papio/internal/work"
)

// countingResolver serves the paywalled canonical record to singleton lookups
// and one OA sibling to searches, counting every request that reaches the wire.
func countingResolver(t *testing.T, searchBody string) (*Resolver, *int) {
	t.Helper()
	requests := 0
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Query().Get("search") != "" {
			return responseFor(200, searchBody, nil), nil
		}
		return responseFor(200, canonicalRecord, nil), nil
	})
	r := NewWithOptions(Options{
		Client: client, ContactEmail: "contact@example.org", APIKey: "key", BaseURL: "https://api.test/works",
	})
	return r, &requests
}

// The memo exists for the sibling hop only. Resolve must stay a live read:
// serving a candidate URL from a remembered record is how a job acquires a dead
// bearer link.
func TestResolveNeverReadsMemo(t *testing.T) {
	r, requests := countingResolver(t, `{"results":[]}`)
	ctx := context.Background()
	for range 2 {
		if _, err := r.Resolve(ctx, work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
			t.Fatal(err)
		}
	}
	if *requests != 2 {
		t.Fatalf("requests = %d, want 2: Resolve never reuses the memo", *requests)
	}
}

func TestSiblingReusesResolveRecord(t *testing.T) {
	r, requests := countingResolver(t, siblingSearchBody(
		"How Explanations Shape Trust", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf"))
	ctx := context.Background()
	if _, err := r.Resolve(ctx, work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if *requests != 1 {
		t.Fatalf("requests after Resolve = %d, want 1", *requests)
	}
	candidates, err := r.ResolveSiblings(ctx, work.Work{DOI: "10.1145/3531146.3533202"})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v, want one sibling matched against the memoized record", candidates)
	}
	if *requests != 2 {
		t.Fatalf("requests = %d, want 2 (one Resolve GET + one title search): the canonical GET must not repeat", *requests)
	}
}

func TestSiblingColdMemoSearchesCallerMetadata(t *testing.T) {
	r, requests := countingResolver(t, siblingSearchBody(
		"How Explanations Shape Trust", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf"))
	candidates, err := r.ResolveSiblings(context.Background(), canonicalWork())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v, want the caller's own metadata to drive matching", candidates)
	}
	if *requests != 1 {
		t.Fatalf("requests = %d, want exactly the title search", *requests)
	}
}

// With a DOI but no basis anywhere, the hop re-earns one with the SINGLETON
// lookup (one credit) rather than skipping discovery. Only a DOI the provider
// does not know leaves nothing to search on.
func TestSiblingWithoutBasisReearnsItCheaply(t *testing.T) {
	r, requests := countingResolver(t, `{"results":[]}`)
	if _, err := r.ResolveSiblings(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if *requests != 2 {
		t.Fatalf("requests = %d, want 2 (one singleton to re-earn the basis + one search)", *requests)
	}
}
func TestSiblingNoSearchBasisAfterUnknownDOI(t *testing.T) {
	// The provider does not know this DOI, so re-earning a basis is impossible:
	// the hop must report the singleton it spent and then decline, rather than
	// paying for a search it cannot aim.
	client := clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(404, "", nil), nil
	})
	r := New(client, "contact@example.org", "private-key")
	candidates, err := r.ResolveSiblings(context.Background(), work.Work{DOI: "10.9999/unknown.work"})
	if !errors.Is(err, resolver.ErrNoSearchBasis) {
		t.Fatalf("err = %v, want resolver.ErrNoSearchBasis", err)
	}
	if candidates != nil {
		t.Fatalf("candidates = %#v, want none", candidates)
	}
}

// A stale memo entry must not silently cancel discovery: the basis is re-earned
// with the one-credit singleton, never assumed absent.
func TestSiblingStaleMemoReearnsBasis(t *testing.T) {
	r, requests := countingResolver(t, `{"results":[]}`)
	ctx := context.Background()
	if _, err := r.Resolve(ctx, work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	entry := r.records["doi:10.1145/3531146.3533202"]
	entry.at = time.Now().Add(-recordMemoTTL - time.Second)
	r.records["doi:10.1145/3531146.3533202"] = entry
	r.mu.Unlock()
	before := *requests
	if _, err := r.ResolveSiblings(ctx, work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if got := *requests - before; got != 2 {
		t.Fatalf("requests = %d, want 2 (singleton re-earn + search) after the memo went stale", got)
	}
}

// The cap must evict what is expired, not everything: a live entry surviving a
// cap-crossing write is what keeps a DOI-only job's search basis available.
func TestMemoCapEvictsOnlyExpiredEntries(t *testing.T) {
	r, _ := countingResolver(t, `{"results":[]}`)
	fresh := workRecord{Title: "Shape Trust", DOI: "https://doi.org/10.1145/3531146.3533202"}
	r.writeMemo("10.1145/3531146.3533202", fresh, true)
	r.mu.Lock()
	for i := range recordMemoCap {
		r.records[fmt.Sprintf("doi:10.0000/filler.%d", i)] = recordMemo{
			found: true, at: time.Now().Add(-recordMemoTTL - time.Second),
		}
	}
	r.mu.Unlock()
	r.writeMemo("10.0000/trigger", workRecord{}, true)
	if _, ok := r.recordFor("10.1145/3531146.3533202"); !ok {
		t.Fatal("a fresh entry was discarded by a cap-crossing write; expired entries should have gone first")
	}
}

func TestAnonymousCredentialsOmitAPIKey(t *testing.T) {
	var seen []*http.Request
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req)
		if req.URL.Query().Get("search") != "" {
			return responseFor(200, `{"results":[]}`, nil), nil
		}
		return responseFor(200, canonicalRecord, nil), nil
	})
	r := NewWithOptions(Options{
		Client: client, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works",
	})
	ctx := resolver.WithAnonymousCredentials(context.Background())
	if _, err := r.Resolve(ctx, work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveSiblings(ctx, canonicalWork()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want the singleton lookup and the sibling search", len(seen))
	}
	for _, req := range seen {
		query := req.URL.Query()
		if query.Get("api_key") != "" {
			t.Fatalf("anonymous request carried api_key: %s", req.URL.RawQuery)
		}
		if query.Get("mailto") != "contact@example.org" {
			t.Fatalf("anonymous request dropped mailto: %s", req.URL.RawQuery)
		}
	}
}

// A dev base URL may already carry an api_key. Without an unconditional Del an
// "anonymous" request would stay keyed on the wire while the observer and the
// budget manager both metered it as keyless.
func TestAnonymousStripsBaseURLKey(t *testing.T) {
	var seen []*http.Request
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req)
		return responseFor(200, canonicalRecord, nil), nil
	})
	r := NewWithOptions(Options{
		Client: client, ContactEmail: "contact@example.org", APIKey: "private-key",
		BaseURL: "https://api.test/works?api_key=stale",
	})
	if _, err := r.Resolve(resolver.WithAnonymousCredentials(context.Background()),
		work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want two", len(seen))
	}
	if got := seen[0].URL.Query().Get("api_key"); got != "" {
		t.Fatalf("anonymous request api_key = %q, want it stripped", got)
	}
	if got := seen[1].URL.Query()["api_key"]; len(got) != 1 || got[0] != "private-key" {
		t.Fatalf("keyed request api_key = %#v, want exactly the configured key once", got)
	}
	if strings.Contains(seen[1].URL.RawQuery, "stale") {
		t.Fatalf("keyed request kept the stale base-URL key: %s", seen[1].URL.RawQuery)
	}
}
