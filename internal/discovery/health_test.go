// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/work"
)

// The shared fakeSource in multi_test.go already models a backend that answers
// or fails on command; these tests reuse it rather than adding a second double.
func fakeWork(title string) DiscoveredWork {
	return DiscoveredWork{Work: work.Work{Title: title, DOI: "10.1000/" + strings.ReplaceAll(title, " ", "-")}}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// The regression: one backend failing while another answers used to return the
// survivor's results and silently drop the error, so a broken backend looked
// like a thin result set.
func TestSearchPartialReportsAFailedBackendAlongsideResults(t *testing.T) {
	healthy := &fakeSource{name: "openalex", works: []DiscoveredWork{fakeWork("a usable result")}}
	broken := &fakeSource{name: "semanticscholar", err: errors.New("backend is down")}
	multi := NewMulti(healthy, broken)
	multi.now = fixedClock(time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))

	works, failures, err := multi.SearchPartial(context.Background(), SearchParams{Query: "a usable result", Limit: 10})
	if err != nil {
		t.Fatalf("SearchPartial returned a hard error despite a usable backend: %v", err)
	}
	if len(works) != 1 {
		t.Fatalf("got %d works, want the healthy backend's 1", len(works))
	}
	if len(failures) != 1 {
		t.Fatalf("got %d reported failures, want 1", len(failures))
	}
	if failures[0].Source != "semanticscholar" {
		t.Fatalf("failure named %q, want semanticscholar", failures[0].Source)
	}
	if !strings.Contains(failures[0].Message, "backend is down") {
		t.Fatalf("failure message = %q, want it to carry the cause", failures[0].Message)
	}
	if failures[0].At.IsZero() {
		t.Fatal("failure carries no timestamp")
	}
}

// Search keeps the Source signature, so it cannot report partial failures — but
// it must still record them for diagnostics.
func TestSearchStillRecordsFailuresItCannotReturn(t *testing.T) {
	healthy := &fakeSource{name: "openalex", works: []DiscoveredWork{fakeWork("a usable result")}}
	broken := &fakeSource{name: "semanticscholar", err: errors.New("backend is down")}
	multi := NewMulti(healthy, broken)

	works, err := multi.Search(context.Background(), SearchParams{Query: "a usable result", Limit: 10})
	if err != nil || len(works) != 1 {
		t.Fatalf("Search = %d works, %v; want 1, nil", len(works), err)
	}
	failures := multi.LastFailures()
	if len(failures) != 1 || failures[0].Source != "semanticscholar" {
		t.Fatalf("LastFailures = %+v, want one entry for semanticscholar", failures)
	}
}

// A backend that recovers must stop being reported, or every diagnostic surface
// would accuse it forever after one blip.
func TestSuccessfulBackendClearsItsRetainedFailure(t *testing.T) {
	flaky := &fakeSource{name: "semanticscholar", err: errors.New("temporary outage")}
	healthy := &fakeSource{name: "openalex", works: []DiscoveredWork{fakeWork("a usable result")}}
	multi := NewMulti(healthy, flaky)

	if _, err := multi.Search(context.Background(), SearchParams{Query: "a usable result", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if len(multi.LastFailures()) != 1 {
		t.Fatalf("expected the outage to be retained, got %+v", multi.LastFailures())
	}

	flaky.err = nil
	flaky.works = []DiscoveredWork{fakeWork("recovered result")}
	if _, err := multi.Search(context.Background(), SearchParams{Query: "a usable result", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if failures := multi.LastFailures(); len(failures) != 0 {
		t.Fatalf("recovered backend still reported as failing: %+v", failures)
	}
}

func TestLastFailuresIsOrderedBySourceForStableOutput(t *testing.T) {
	multi := NewMulti(
		&fakeSource{name: "zebra", err: errors.New("down")},
		&fakeSource{name: "alpha", err: errors.New("down")},
		&fakeSource{name: "mango", err: errors.New("down")},
	)
	// Every backend failing is a hard error; the failures are still retained.
	if _, err := multi.Search(context.Background(), SearchParams{Query: "anything at all", Limit: 10}); err == nil {
		t.Fatal("expected a hard error when every backend fails")
	}
	failures := multi.LastFailures()
	if len(failures) != 3 {
		t.Fatalf("got %d failures, want 3", len(failures))
	}
	for i, want := range []string{"alpha", "mango", "zebra"} {
		if failures[i].Source != want {
			t.Fatalf("failure %d = %q, want %q (order must be stable)", i, failures[i].Source, want)
		}
	}
}

// The security-relevant case. A transport failure is a *url.Error whose text
// embeds the request URL, and a discovery URL carries the configured contact
// email and, for key-bearing backends, an API key. Neither may reach a log.
func TestFailureMessageRedactsTheRequestURL(t *testing.T) {
	transport := &url.Error{
		Op:  "Get",
		URL: "https://api.openalex.org/works?search=trust&mailto=someone@example.edu&api_key=SECRETVALUE",
		Err: errors.New("dial tcp 203.0.113.7:443: connect: connection refused"),
	}
	message := SanitizeError(transport)
	for _, secret := range []string{"SECRETVALUE", "api_key", "someone@example.edu", "mailto"} {
		if strings.Contains(message, secret) {
			t.Fatalf("message leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "api.openalex.org") {
		t.Fatalf("message lost the host an operator needs: %s", message)
	}
	if !strings.Contains(message, "connection refused") {
		t.Fatalf("message lost the cause: %s", message)
	}
}

func TestFailureMessageIsBoundedAndSingleLine(t *testing.T) {
	message := SanitizeError(fmt.Errorf("backend said:\n%s", strings.Repeat("verbose ", 200)))
	if strings.ContainsAny(message, "\r\n") {
		t.Fatalf("message spans lines: %q", message)
	}
	if len(message) > maxFailureMessage+len("…") {
		t.Fatalf("message is %d bytes, want at most %d", len(message), maxFailureMessage+len("…"))
	}
}

// A named --source that fails is a hard error, and still worth remembering.
func TestNamedSourceFailureIsRecorded(t *testing.T) {
	broken := &fakeSource{name: "semanticscholar", err: errors.New("backend is down")}
	multi := NewMulti(&fakeSource{name: "openalex"}, broken)

	_, failures, err := multi.SearchPartial(context.Background(), SearchParams{Query: "anything at all", Limit: 10, Source: "semanticscholar"})
	if err == nil {
		t.Fatal("expected a hard error when the named source fails")
	}
	if len(failures) != 1 || failures[0].Source != "semanticscholar" {
		t.Fatalf("failures = %+v, want one entry for the named source", failures)
	}
	if len(multi.LastFailures()) != 1 {
		t.Fatalf("named-source failure was not retained: %+v", multi.LastFailures())
	}
}

// The gap: only the *url.Error branch scrubbed anything. Any other error
// shape passed through bound() untouched, so a credential embedded directly
// in error text — not inside a URL SanitizeError already knew how to parse —
// reached logs and diagnostics verbatim.
func TestFailureMessageRedactsCredentialsOutsideAURLError(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		denied []string
		want   []string
	}{
		{
			name:   "authorization bearer header",
			err:    fmt.Errorf("request failed: Authorization: Bearer sk-live-verysecrettoken"),
			denied: []string{"sk-live-verysecrettoken"},
			want:   []string{"request failed", "Authorization"},
		},
		{
			name:   "bare bearer token without an Authorization label",
			err:    fmt.Errorf("upstream rejected Bearer sk-live-verysecrettoken"),
			denied: []string{"sk-live-verysecrettoken"},
			want:   []string{"upstream rejected"},
		},
		{
			name:   "query-style credentials embedded in prose, not a parsed *url.Error",
			err:    fmt.Errorf("upstream said: https://host/?api_key=SECRETVALUE&mailto=someone@example.edu is unreachable"),
			denied: []string{"SECRETVALUE", "someone@example.edu"},
			want:   []string{"upstream said", "host", "is unreachable"},
		},
		{
			name:   "userinfo embedded in a scheme the url package still parses",
			err:    fmt.Errorf("dial failed for scheme://user:hunter2@internal.example/path"),
			denied: []string{"user:hunter2", "hunter2"},
			want:   []string{"internal.example"},
		},
		{
			name:   "bare key=value credential assignment",
			err:    fmt.Errorf("config rejected token=abcdef123456"),
			denied: []string{"abcdef123456"},
			want:   []string{"config rejected"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := SanitizeError(test.err)
			for _, secret := range test.denied {
				if strings.Contains(message, secret) {
					t.Fatalf("message leaked %q: %s", secret, message)
				}
			}
			for _, want := range test.want {
				if !strings.Contains(message, want) {
					t.Fatalf("message = %q, want it to contain %q", message, want)
				}
			}
		})
	}
}

// The *url.Error branch rebuilt the outer URL through redact.URL but
// interpolated urlErr.Err's text unscrubbed, so a credential quoted inside
// the inner error (e.g. a redirect target) still reached the message.
func TestFailureMessageRedactsTheInnerURLErrorText(t *testing.T) {
	transport := &url.Error{
		Op:  "Get",
		URL: "https://api.openalex.org/works?search=trust",
		Err: fmt.Errorf("redirected to https://api.openalex.org/works?api_key=SECRETVALUE&mailto=someone@example.edu: too many redirects"),
	}
	message := SanitizeError(transport)
	for _, secret := range []string{"SECRETVALUE", "someone@example.edu"} {
		if strings.Contains(message, secret) {
			t.Fatalf("message leaked %q via the inner error: %s", secret, message)
		}
	}
	if !strings.Contains(message, "too many redirects") {
		t.Fatalf("message lost the cause: %s", message)
	}
}

// SummarizeFailures backs the daemon's total-discovery-failure RPC message
// (internal/api's discovery.search handler): unlike SanitizeError on a
// joined error, it must name every backend, not just the first errors.As
// happens to find.
func TestSummarizeFailuresJoinsEveryBackendsSanitizedCause(t *testing.T) {
	if got := SummarizeFailures(nil); got != "" {
		t.Fatalf("SummarizeFailures(nil) = %q, want empty", got)
	}
	failures := []BackendFailure{
		{Source: "openalex", Message: "first cause"},
		{Source: "semanticscholar", Message: "second cause"},
	}
	got := SummarizeFailures(failures)
	for _, want := range []string{"openalex", "first cause", "semanticscholar", "second cause"} {
		if !strings.Contains(got, want) {
			t.Fatalf("SummarizeFailures = %q, want it to contain %q", got, want)
		}
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("SummarizeFailures spans lines: %q", got)
	}
}

// The regression: a caller that gave up mid-request was indistinguishable
// from a broken backend, so LastFailures accused a backend of failing when
// it never got a real chance to answer.
func TestSearchPartialDoesNotRecordACancelledCallerContextAsAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	healthy := &fakeSource{name: "openalex", works: []DiscoveredWork{fakeWork("a usable result")}}
	abandoned := &fakeSource{name: "semanticscholar", err: errors.New("backend is down")}
	multi := NewMulti(healthy, abandoned)

	works, failures, err := multi.SearchPartial(ctx, SearchParams{Query: "a usable result", Limit: 10})
	if err != nil {
		t.Fatalf("SearchPartial returned a hard error despite a usable backend: %v", err)
	}
	if len(works) != 1 {
		t.Fatalf("got %d works, want 1", len(works))
	}
	if len(failures) != 0 {
		t.Fatalf("SearchPartial reported %+v for a cancelled caller, want none", failures)
	}
	if got := multi.LastFailures(); len(got) != 0 {
		t.Fatalf("LastFailures retained a cancelled caller's context as a backend failure: %+v", got)
	}
}

// The other direction: a backend's own internal budget expiring independent
// of the caller is real signal and must still be recorded, even though the
// error value (context.DeadlineExceeded) is indistinguishable by type or by
// errors.Is from what a cancelled caller's own context would produce — the
// fix must key off ctx.Err(), not off inspecting err.
func TestSearchPartialStillRecordsABackendsOwnDeadlineDespiteAHealthyCallerContext(t *testing.T) {
	healthy := &fakeSource{name: "openalex", works: []DiscoveredWork{fakeWork("a usable result")}}
	slow := &fakeSource{name: "semanticscholar", err: context.DeadlineExceeded}
	multi := NewMulti(healthy, slow)

	_, failures, err := multi.SearchPartial(context.Background(), SearchParams{Query: "a usable result", Limit: 10})
	if err != nil {
		t.Fatalf("SearchPartial returned a hard error despite a usable backend: %v", err)
	}
	if len(failures) != 1 || failures[0].Source != "semanticscholar" {
		t.Fatalf("failures = %+v, want one entry for semanticscholar's own deadline", failures)
	}
	if got := multi.LastFailures(); len(got) != 1 {
		t.Fatalf("LastFailures did not retain the backend's own deadline: %+v", got)
	}
}

// The named-source branch has its own ctx.Err() check; a cancelled caller
// must not taint that backend's retained health either.
func TestNamedSourceSearchDoesNotRecordACancelledCallerContextAsAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	abandoned := &fakeSource{name: "semanticscholar", err: errors.New("backend is down")}
	multi := NewMulti(&fakeSource{name: "openalex"}, abandoned)

	_, failures, err := multi.SearchPartial(ctx, SearchParams{Query: "anything at all", Limit: 10, Source: "semanticscholar"})
	if err == nil {
		t.Fatal("expected the named source's own error to still reach the caller")
	}
	if len(failures) != 0 {
		t.Fatalf("SearchPartial reported %+v for a cancelled caller, want none", failures)
	}
	if got := multi.LastFailures(); len(got) != 0 {
		t.Fatalf("LastFailures retained a cancelled caller's context as a backend failure: %+v", got)
	}
}

// stubHTTPClient adapts a func to discovery.HTTPClient so
// TestMultiSearchPartialWithRealBackendsEndToEnd can drive the real OpenAlex
// and Semantic Scholar clients without a socket.
type stubHTTPClient func(*http.Request) (*http.Response, error)

func (f stubHTTPClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

// Every other test in this package exercises Multi through fakeSource, a
// hand-rolled double that satisfies Source only because it was written to
// — it cannot catch a real backend's Search method drifting out of sync
// with what Multi, PartialSearcher, or BackendHealth expect. This drives
// the actual discovery.Client and discovery.SemanticScholar — via a stub
// HTTPClient, no network — through SearchPartial's partial-failure path
// end to end instead.
func TestMultiSearchPartialWithRealBackendsEndToEnd(t *testing.T) {
	fixture, err := os.ReadFile("testdata/openalex_works.json")
	if err != nil {
		t.Fatal(err)
	}
	openAlex := NewWithOptions(Options{
		Client: stubHTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(fixture)),
			}, nil
		}),
		ContactEmail: "researcher@example.org",
	})
	semanticScholar := NewSemanticScholarWithOptions(SemanticScholarOptions{
		Client: stubHTTPClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset by peer")
		}),
	})

	multi := NewMulti(openAlex, semanticScholar)
	var partial PartialSearcher = multi
	works, failures, err := partial.SearchPartial(context.Background(), SearchParams{Query: "resilient discovery", Limit: 10})
	if err != nil {
		t.Fatalf("SearchPartial returned a hard error despite the real OpenAlex client answering: %v", err)
	}
	if len(works) == 0 {
		t.Fatal("expected the real OpenAlex client to contribute results")
	}
	if len(failures) != 1 || failures[0].Source != "semanticscholar" {
		t.Fatalf("failures = %+v, want one entry for semanticscholar", failures)
	}

	var health BackendHealth = multi
	if got := health.LastFailures(); len(got) != 1 || got[0].Source != "semanticscholar" {
		t.Fatalf("LastFailures = %+v, want one retained entry for semanticscholar", got)
	}
}

// concurrentFakeSource is like fakeSource but safe under concurrent Search
// calls: fakeSource's call counter is a plain, unguarded field, since
// nothing else in this package drives it from more than one goroutine.
// concurrentFakeSource has no mutable state at all, so concurrent reads of
// it are race-free by construction.
type concurrentFakeSource struct {
	name  string
	works []DiscoveredWork
	err   error
}

func (s concurrentFakeSource) Name() string { return s.name }

func (s concurrentFakeSource) Search(context.Context, SearchParams) ([]DiscoveredWork, error) {
	return s.works, s.err
}

// Multi is genuinely shared between interactive searches and the periodic
// watch runner (see recordFailure's doc comment), so this drives concurrent
// SearchPartial and LastFailures calls against one shared instance under
// go test -race. It does not assert which goroutine's write wins — that is
// the documented last-writer-wins limitation on recordFailure/clearFailure
// — only that concurrent access never corrupts the retained-failures map or
// trips the race detector.
func TestConcurrentSearchesDoNotRaceOnRetainedFailures(t *testing.T) {
	healthy := concurrentFakeSource{name: "openalex", works: []DiscoveredWork{fakeWork("a usable result")}}
	broken := concurrentFakeSource{name: "semanticscholar", err: errors.New("backend is down")}
	multi := NewMulti(healthy, broken)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = multi.SearchPartial(context.Background(), SearchParams{Query: "a usable result", Limit: 10})
			_ = multi.LastFailures()
		}()
	}
	wg.Wait()

	failures := multi.LastFailures()
	if len(failures) != 1 || failures[0].Source != "semanticscholar" {
		t.Fatalf("after concurrent searches, LastFailures = %+v, want one entry for semanticscholar", failures)
	}
}
