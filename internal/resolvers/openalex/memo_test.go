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

// With a DOI but no basis anywhere the hop makes NO request and says so. Paying
// a singleton to re-earn a basis was tried and reverted: it broke this
// sentinel's contract (the caller reads it as "no request happened" and skips
// charging) and let one admission cover two HTTP requests.
func TestSiblingWithoutBasisMakesNoRequest(t *testing.T) {
	r, requests := countingResolver(t, `{"results":[]}`)
	_, err := r.ResolveSiblings(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"})
	if !errors.Is(err, resolver.ErrNoSearchBasis) {
		t.Fatalf("err = %v, want resolver.ErrNoSearchBasis", err)
	}
	if *requests != 0 {
		t.Fatalf("requests = %d, want zero: the sentinel promises no request happened", *requests)
	}
}

// A title with no readable author cannot yield a candidate: every result is
// required to share an author surname, and that check fails whenever either list
// is empty. Buying the ten-credit search anyway is spending on a foregone
// conclusion.
func TestSiblingRefusesABasisThatCannotAccept(t *testing.T) {
	for _, test := range []struct {
		name  string
		basis work.Work
		buys  bool
	}{
		{name: "title only", basis: work.Work{DOI: "10.1145/3531146.3533202", Title: "Shape Trust"}},
		{
			name:  "title and unreadable author",
			basis: work.Work{DOI: "10.1145/3531146.3533202", Title: "Shape Trust", Authors: []string{"  "}},
		},
		{
			name:  "title and author",
			basis: work.Work{DOI: "10.1145/3531146.3533202", Title: "Shape Trust", Authors: []string{"Andrea Ferrario"}},
			buys:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, requests := countingResolver(t, `{"results":[]}`)
			_, err := r.ResolveSiblings(context.Background(), test.basis)
			if test.buys {
				if err != nil {
					t.Fatalf("err = %v, want the search to run on a usable basis", err)
				}
				if *requests != 1 {
					t.Fatalf("requests = %d, want exactly the one search", *requests)
				}
				return
			}
			if !errors.Is(err, resolver.ErrNoSearchBasis) {
				t.Fatalf("err = %v, want resolver.ErrNoSearchBasis", err)
			}
			if *requests != 0 {
				t.Fatalf("requests = %d, want zero: no response could have been accepted", *requests)
			}
		})
	}
}

// A negative DOI memo proves only that the provider does not resolve THAT DOI.
// It says nothing about whether the caller's own bibliography can find a sibling
// indexed under a different DOI, so it must not suppress a usable caller basis.
func TestNegativeMemoDoesNotSuppressACallerBasis(t *testing.T) {
	var searched bool
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("search") != "" {
			searched = true
			return responseFor(200, `{"results":[]}`, nil), nil
		}
		return responseFor(404, "", nil), nil
	})
	r := New(client, "contact@example.org", "private-key")
	ctx := context.Background()
	if _, err := r.Resolve(ctx, work.Work{DOI: "10.9999/unknown.work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveSiblings(ctx, work.Work{
		DOI:     "10.9999/unknown.work",
		Title:   "A Title The Caller Supplied",
		Authors: []string{"Andrea Ferrario"},
	}); err != nil {
		t.Fatal(err)
	}
	if !searched {
		t.Fatal("a usable caller basis was suppressed by an unrelated negative DOI memo")
	}
}

// The record must be about the DOI that was asked for, and an internally
// inconsistent record — one identity field naming the requested work, another
// naming a different one — must be rejected rather than accepted on whichever
// field parses first.
func TestEchoedDOIRejectsInconsistentIdentities(t *testing.T) {
	want := "10.1145/3531146.3533202"
	for _, test := range []struct {
		name   string
		record workRecord
		ok     bool
	}{
		{name: "both agree", record: workRecord{DOI: "https://doi.org/" + want, IDs: identifiers{DOI: "https://doi.org/" + want}}, ok: true},
		{name: "only one present", record: workRecord{DOI: "https://doi.org/" + want}, ok: true},
		{name: "second names another work", record: workRecord{DOI: "https://doi.org/" + want, IDs: identifiers{DOI: "https://doi.org/10.9999/other"}}},
		{name: "first names another work", record: workRecord{DOI: "https://doi.org/10.9999/other", IDs: identifiers{DOI: "https://doi.org/" + want}}},
		{name: "nothing legible", record: workRecord{}},
		{name: "present but unparseable", record: workRecord{DOI: "not-a-doi"}},
		{name: "version suffix is a different work", record: workRecord{DOI: "https://doi.org/" + want + "v2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := echoesDOI(test.record, want); got != test.ok {
				t.Fatalf("echoesDOI = %v, want %v", got, test.ok)
			}
		})
	}
}

// A misrouted record must not be published as an exact-DOI match, and must not
// reach the memo where a later sibling hop would trust its bibliography.
// papio's worst outcome is a wrong paper filed under a right citation.
func TestMisroutedRecordIsNeitherPublishedNorMemoized(t *testing.T) {
	client := clientFunc(func(*http.Request) (*http.Response, error) {
		return responseFor(200, `{"id":"https://openalex.org/W9","doi":"https://doi.org/10.9999/other.work","title":"An Entirely Different Paper","publication_year":2019,"open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/other.pdf","version":"publishedVersion"}}`, nil), nil
	})
	r := New(client, "contact@example.org", "private-key")
	candidates, err := r.Resolve(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"})
	if err != nil {
		t.Fatal(err)
	}
	if candidates != nil {
		t.Fatalf("candidates = %#v, want none: the record is about another work", candidates)
	}
	if _, ok := r.recordFor("10.1145/3531146.3533202"); ok {
		t.Fatal("the misrouted record was memoized and would be trusted by a later sibling hop")
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

// The harder case, and the one that shipped broken: nothing has expired, so
// expiry frees nothing and the old code replaced the entire map. A job's only
// canonical basis then vanished because 512 unrelated works were looked up
// between its own Resolve and its sibling hop. Exactly one victim may go, and it
// must be the oldest.
//
// Oldest-first is a bound, not a guarantee: a basis that genuinely is the oldest
// live entry can still be evicted, one entry at a time instead of 512 at once.
// Item 7's re-earned basis is the real repair; this only stops the cliff.
func TestMemoCapEvictsOneOldestEntryWhenNothingHasExpired(t *testing.T) {
	r, _ := countingResolver(t, `{"results":[]}`)
	r.mu.Lock()
	// All fresh, so expiry frees nothing - and all OLDER than the basis written
	// below, so the oldest entry is an unrelated filler rather than the entry a
	// live job depends on.
	for i := range recordMemoCap - 1 {
		r.records[fmt.Sprintf("doi:10.0000/filler.%d", i)] = recordMemo{
			found: true, at: time.Now().Add(-recordMemoTTL / 2).Add(time.Duration(i) * time.Millisecond),
		}
	}
	r.mu.Unlock()
	basis := workRecord{Title: "Shape Trust", DOI: "https://doi.org/10.1145/3531146.3533202"}
	r.writeMemo("10.1145/3531146.3533202", basis, true)
	r.mu.Lock()
	before := len(r.records)
	r.mu.Unlock()

	r.writeMemo("10.0000/trigger", workRecord{}, true)

	if _, ok := r.recordFor("10.1145/3531146.3533202"); !ok {
		t.Fatal("an all-fresh cap-crossing write discarded a live job's basis; only the oldest entry may go")
	}
	r.mu.Lock()
	after := len(r.records)
	r.mu.Unlock()
	if after > before {
		t.Fatalf("memo grew past the cap: %d entries, was %d", after, before)
	}
	if after < recordMemoCap/2 {
		t.Fatalf("memo was emptied wholesale: %d entries left of %d", after, before)
	}
}

// fetch must wrap a client error, never replace it. The injected client is a
// sourcegate.Client whose admission refusals carry the reset a caller has to
// park until; substituting a fresh error left the caller with an
// undifferentiated transport failure and generic retry behaviour.
func TestFetchPreservesTheClientCause(t *testing.T) {
	sentinel := errors.New("deferred until 2026-08-16T00:00:00Z")
	r := NewWithOptions(Options{
		Client: clientFunc(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		}),
		ContactEmail: "contact@example.org", BaseURL: "https://api.test/works",
	})

	_, err := r.Resolve(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"})
	if err == nil {
		t.Fatal("a refused request resolved successfully")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the client's own cause to survive", err)
	}
	if _, temporary := resolver.Temporary(err); !temporary {
		t.Fatalf("err = %v, want it still classified temporary", err)
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

// Both exact endpoints publish at IdentityConfidence 1.0, so both must prove the
// response describes the identity that was requested. Only the DOI path checked.
func TestExactOpenAlexIDLookupRequiresAnEchoedID(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		accepts bool
	}{
		{
			name:    "echoes the requested id",
			body:    `{"id":"https://openalex.org/W2741809807","ids":{"openalex":"https://openalex.org/W2741809807"},"title":"Shape Trust","open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/a.pdf","version":"publishedVersion"}}`,
			accepts: true,
		},
		{
			name: "answers about another work",
			body: `{"id":"https://openalex.org/W2741809808","ids":{"openalex":"https://openalex.org/W2741809808"},"title":"Another Paper","open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/b.pdf","version":"publishedVersion"}}`,
		},
		{
			name: "identities disagree with each other",
			body: `{"id":"https://openalex.org/W2741809807","ids":{"openalex":"https://openalex.org/W2741809808"},"title":"Shape Trust","open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/a.pdf","version":"publishedVersion"}}`,
		},
		{
			name: "echoes nothing legible",
			body: `{"title":"Shape Trust","open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/a.pdf","version":"publishedVersion"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := clientFunc(func(*http.Request) (*http.Response, error) {
				return responseFor(200, test.body, nil), nil
			})
			r := New(client, "contact@example.org", "private-key")
			candidates, err := r.Resolve(context.Background(), work.Work{OpenAlex: "W2741809807"})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(candidates) > 0; got != test.accepts {
				t.Fatalf("accepted = %v, want %v", got, test.accepts)
			}
		})
	}
}

// A positive memo wins only if it yields a usable basis. Replacing caller
// metadata wholesale let a canonical record with no legible authors cancel the
// hop — the negative-memo defect arriving by the opposite route.
func TestIncompletePositiveMemoDoesNotSuppressACallerBasis(t *testing.T) {
	var searched bool
	client := clientFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("search") != "" {
			searched = true
			return responseFor(200, `{"results":[]}`, nil), nil
		}
		// Canonical record: a title, but no authors the canonicalizer can read.
		return responseFor(200, `{"id":"https://openalex.org/W2741809807","doi":"https://doi.org/10.1145/3531146.3533202","title":"Shape Trust","publication_year":2022,"authorships":[]}`, nil), nil
	})
	r := New(client, "contact@example.org", "private-key")
	ctx := context.Background()
	if _, err := r.Resolve(ctx, work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveSiblings(ctx, work.Work{
		DOI:     "10.1145/3531146.3533202",
		Title:   "Shape Trust",
		Authors: []string{"Andrea Ferrario"},
	}); err != nil {
		t.Fatal(err)
	}
	if !searched {
		t.Fatal("an incomplete positive memo suppressed a usable caller basis")
	}
}
