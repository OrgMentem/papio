// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
