// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalexyield

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestPlanComputesCostPreviewBeforeAnyRequest(t *testing.T) {
	plan := Plan(25)
	if len(plan.Shapes) != len(AllShapes) {
		t.Fatalf("Shapes has %d entries, want %d", len(plan.Shapes), len(AllShapes))
	}
	if plan.CreditsPerCall != CreditsPerTitleSearch {
		t.Errorf("CreditsPerCall = %d, want %d", plan.CreditsPerCall, CreditsPerTitleSearch)
	}
	wantCalls := len(AllShapes) * 25
	if plan.TotalCalls != wantCalls {
		t.Errorf("TotalCalls = %d, want %d", plan.TotalCalls, wantCalls)
	}
	wantCredits := wantCalls * CreditsPerTitleSearch
	if plan.TotalCredits != wantCredits {
		t.Errorf("TotalCredits = %d, want %d", plan.TotalCredits, wantCredits)
	}
}

// countingClient counts Do calls; a test asserting refusal must observe zero
// calls, since the whole point of the opt-in gate is that nothing is spent.
type countingClient struct{ calls int }

func (c *countingClient) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("countingClient: must not be called")
}

func TestRunRefusesWithoutConfirmation(t *testing.T) {
	client := &countingClient{}
	_, err := Run(context.Background(), ComparisonConfig{
		Confirm: false, Sample: 5, Client: client, ContactEmail: "ops@example.org",
	}, []TitleSample{{Title: "Example"}})
	if !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("err = %v, want ErrConfirmationRequired", err)
	}
	if client.calls != 0 {
		t.Fatalf("client.calls = %d, want 0 — refusal must spend nothing", client.calls)
	}
}

func TestRunRefusesOutOfBoundsSample(t *testing.T) {
	client := &countingClient{}
	titles := []TitleSample{{Title: "Example"}}
	for _, sample := range []int{0, -1, MaxSample + 1} {
		_, err := Run(context.Background(), ComparisonConfig{
			Confirm: true, Sample: sample, Client: client, ContactEmail: "ops@example.org",
		}, titles)
		if err == nil {
			t.Errorf("sample=%d: err = nil, want an out-of-bounds error", sample)
		}
	}
	if client.calls != 0 {
		t.Fatalf("client.calls = %d, want 0", client.calls)
	}
}

func TestRunRequiresClientAndContactEmail(t *testing.T) {
	titles := []TitleSample{{Title: "Example"}}
	if _, err := Run(context.Background(), ComparisonConfig{Confirm: true, Sample: 1, ContactEmail: "ops@example.org"}, titles); err == nil {
		t.Error("missing client: err = nil, want error")
	}
	client := &countingClient{}
	if _, err := Run(context.Background(), ComparisonConfig{Confirm: true, Sample: 1, Client: client}, titles); err == nil {
		t.Error("missing contact email: err = nil, want error")
	}
	if client.calls != 0 {
		t.Fatalf("client.calls = %d, want 0", client.calls)
	}
}

// shapeCapturingClient records every request's query string and returns an
// empty, well-formed result — a controlled fake, never a real network call,
// so shape correctness is verified without spending a credit.
type shapeCapturingClient struct{ queries []url.Values }

func (c *shapeCapturingClient) Do(req *http.Request) (*http.Response, error) {
	c.queries = append(c.queries, req.URL.Query())
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"results":[]}`)),
	}, nil
}

func TestRunBuildsAllThreeQueryShapes(t *testing.T) {
	client := &shapeCapturingClient{}
	titles := []TitleSample{{Title: "Attention Is All You Need"}}
	report, err := Run(context.Background(), ComparisonConfig{
		Confirm: true, Sample: 1, Client: client, ContactEmail: "ops@example.org", APIKey: "secret",
	}, titles)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != len(AllShapes) {
		t.Fatalf("Results has %d entries, want %d", len(report.Results), len(AllShapes))
	}
	if len(client.queries) != len(AllShapes) {
		t.Fatalf("issued %d requests, want %d (one per shape, sample size 1)", len(client.queries), len(AllShapes))
	}

	// search=<title>&per_page=10 (current shape)
	if got := client.queries[0].Get("search"); got != titles[0].Title {
		t.Errorf("shape 0 search = %q, want %q", got, titles[0].Title)
	}
	if got := client.queries[0].Get("per_page"); got != "10" {
		t.Errorf("shape 0 per_page = %q, want 10", got)
	}
	if client.queries[0].Has("filter") {
		t.Errorf("shape 0 must not set filter=")
	}

	// search=<title>&per_page=100
	if got := client.queries[1].Get("per_page"); got != "100" {
		t.Errorf("shape 1 per_page = %q, want 100", got)
	}

	// filter=title.search:<title>
	if got := client.queries[2].Get("filter"); got != "title.search:"+titles[0].Title {
		t.Errorf("shape 2 filter = %q, want %q", got, "title.search:"+titles[0].Title)
	}
	if client.queries[2].Has("search") {
		t.Errorf("shape 2 must not set search=")
	}

	// Every request must carry the contact email and API key regardless of shape.
	for i, q := range client.queries {
		if got := q.Get("mailto"); got != "ops@example.org" {
			t.Errorf("shape %d mailto = %q, want ops@example.org", i, got)
		}
		if got := q.Get("api_key"); got != "secret" {
			t.Errorf("shape %d api_key = %q, want secret", i, got)
		}
	}

	for i, res := range report.Results {
		if res.Requests != 1 {
			t.Errorf("shape %d Requests = %d, want 1", i, res.Requests)
		}
		if res.Credits != CreditsPerTitleSearch {
			t.Errorf("shape %d Credits = %d, want %d", i, res.Credits, CreditsPerTitleSearch)
		}
	}
}

func TestRunClampsToConfiguredSample(t *testing.T) {
	client := &shapeCapturingClient{}
	titles := []TitleSample{{Title: "A"}, {Title: "B"}, {Title: "C"}}
	report, err := Run(context.Background(), ComparisonConfig{
		Confirm: true, Sample: 2, Client: client, ContactEmail: "ops@example.org",
	}, titles)
	if err != nil {
		t.Fatal(err)
	}
	if report.Plan.SampleSize != 2 {
		t.Errorf("Plan.SampleSize = %d, want 2", report.Plan.SampleSize)
	}
	for i, res := range report.Results {
		if res.Requests != 2 {
			t.Errorf("shape %d Requests = %d, want 2 (clamped to Sample, not len(titles)=3)", i, res.Requests)
		}
	}
}

func TestSampleTitlesDrawsFromLocalLibrary(t *testing.T) {
	js, db := newFixtureStore(t)
	for i, id := range []string{"wr-sample-a", "wr-sample-b"} {
		_ = i
		createJob(t, js, id)
	}
	titles, err := SampleTitles(context.Background(), db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 2 {
		t.Fatalf("SampleTitles returned %d entries, want 2", len(titles))
	}
	for _, s := range titles {
		if s.Title == "" {
			t.Error("sampled title is empty")
		}
	}
}

func TestSampleTitlesRejectsNonPositiveN(t *testing.T) {
	_, db := newFixtureStore(t)
	if _, err := SampleTitles(context.Background(), db, 0); err == nil {
		t.Error("n=0: err = nil, want error")
	}
}
