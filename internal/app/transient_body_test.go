// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/landingmeta"
	"papio/internal/resolver"
	"papio/internal/resolvers/unpaywall"
)

type transientBodyClient func(*http.Request) (*http.Response, error)

func (f transientBodyClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

type transientBodyFailure struct{ err error }

func (b transientBodyFailure) Read([]byte) (int, error) { return 0, b.err }

func TestResolverBodyReadFailureParksForRetry(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	client := transientBodyClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(io.MultiReader(
				strings.NewReader(`{"doi":"10.1002/example",`),
				transientBodyFailure{err: io.ErrUnexpectedEOF},
			)),
			Request: req,
		}, nil
	})
	svc.Resolvers = []ResolverEntry{{
		Adapter: unpaywall.NewWithOptions(unpaywall.Options{Client: client, ContactEmail: "contact@example.org"}),
		Policy:  config.Source{Enabled: true},
	}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()
	id, err := svc.Submit(ctx, doiRequest("wr_resolver_body_failure"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait || fetches != 0 {
		t.Fatalf("state = %s, fetches = %d; want retry_wait without a candidate download", got.State, fetches)
	}
	detail := retryWaitDetail(t, jobs, id)
	if detail["reason"] != "resolver_temporarily_unavailable" || detail["retry_kind"] != "temporary" {
		t.Fatalf("retry detail = %+v, want a temporary resolver failure", detail)
	}
}

func TestLandingExpansionRetriesFirstReadFailure(t *testing.T) {
	for _, failure := range []struct {
		name   string
		status int
		err    error
		body   bool
	}{
		{name: "server error", status: http.StatusServiceUnavailable},
		{name: "rate limit", status: http.StatusTooManyRequests},
		{name: "request timeout", status: http.StatusRequestTimeout},
		{name: "transport timeout", err: context.DeadlineExceeded},
		{name: "body timeout", err: context.DeadlineExceeded, body: true},
		{name: "truncated body", err: io.ErrUnexpectedEOF, body: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			const deadURL = "https://blob.example/expired.pdf?sig=DEAD_SECRET"
			const landingURL = "https://publisher.example/article?sig=LANDING_SECRET"
			const pdfURL = "https://publisher.example/article.pdf?sig=PDF_SECRET"
			svc, jobs := newTestService(t)
			ctx := context.Background()
			svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
				Source: "fixture", URL: deadURL, Landing: landingURL, Version: resolver.VersionPublished,
				AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
			}}}, Policy: config.Source{Enabled: true}}}
			reads := 0
			client := transientBodyClient(func(req *http.Request) (*http.Response, error) {
				reads++
				response := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/html"}},
					Body:       io.NopCloser(strings.NewReader(`<html><head><meta name="citation_pdf_url" content="` + pdfURL + `"></head></html>`)),
					Request:    req,
				}
				if reads == 1 {
					switch {
					case failure.body:
						response.Body = io.NopCloser(io.MultiReader(strings.NewReader("<html><head>"), transientBodyFailure{err: failure.err}))
					case failure.err != nil:
						return nil, failure.err
					default:
						response.StatusCode = failure.status
						response.Header.Set("Retry-After", "120")
					}
				}
				return response, nil
			})
			svc.LandingReader = landingmeta.NewReader(client, 1<<20)
			fetches := 0
			download := landingExpansionFetch(pdfURL)
			svc.Fetch = func(ctx context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
				fetches++
				return download(ctx, candidate, path)
			}
			svc.Validate = passValidation()
			id, err := svc.Submit(ctx, doiRequest("wr_landing_first_read_failure"))
			if err != nil {
				t.Fatal(err)
			}
			for pass, want := range []string{job.StateRetryWait, job.StateReady} {
				row, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := svc.Process(ctx, row); err != nil {
					t.Fatal(err)
				}
				got, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != want {
					t.Fatalf("pass %d state = %s, want %s", pass+1, got.State, want)
				}
				if reads != pass+1 || fetches != pass+1 {
					t.Fatalf("pass %d: reads = %d, fetches = %d; want one landing read and one fetch per pass", pass+1, reads, fetches)
				}
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"DEAD_SECRET", "LANDING_SECRET", "PDF_SECRET"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("job events contain bearer data %q", secret)
				}
			}
		})
	}
}

func TestLandingExpansionSettlesCompletedRead(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.Config.Sources["flaky"] = config.Source{Enabled: true}
	svc.Resolvers = []ResolverEntry{
		{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
			Source: "fixture", URL: "https://files.example/dead.pdf", Landing: "https://publisher.example/article",
			Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true,
		}}}, Policy: config.Source{Enabled: true}},
		{Adapter: &fakeResolver{name: "flaky", err: &resolver.TemporaryError{Err: context.DeadlineExceeded}}, Policy: config.Source{Enabled: true}},
	}
	svc.RetryDelay = time.Millisecond
	svc.Fetch = landingExpansionFetch("https://publisher.example/never.pdf")
	svc.Validate = passValidation()
	reader := &fakeLandingReader{err: &resolver.TemporaryError{Err: io.ErrUnexpectedEOF}}
	svc.LandingReader = reader
	id, err := svc.Submit(ctx, doiRequest("wr_landing_read_settled"))
	if err != nil {
		t.Fatal(err)
	}
	for pass := range 3 {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Process(ctx, row); err != nil {
			t.Fatal(err)
		}
		reader.err = nil // The second read completes and advertises no PDF.
		got, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != job.StateRetryWait {
			t.Fatalf("pass %d state = %s, want retry_wait for the unrelated resolver", pass+1, got.State)
		}
	}
	if reader.calls != 2 {
		t.Fatalf("landing reads = %d, want one failed read, one complete read, and no third read", reader.calls)
	}
}
