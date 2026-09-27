// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// challengeDownloader serves bodies through the production saveResponse path
// with room for full challenge pages.
func challengeDownloader(t *testing.T, transport http.RoundTripper) *Downloader {
	t.Helper()
	p := DefaultPolicy()
	p.MaxBytes = 1 << 20
	p.Timeout = 10 * time.Second
	p.ConnectTimeout = 2 * time.Second
	p.HeaderTimeout = 2 * time.Second
	p.BodyTimeout = 5 * time.Second
	d, err := New(p, publicResolver(nil), transport)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func downloadBody(t *testing.T, d *Downloader) *Error {
	t.Helper()
	_, err := d.Download(context.Background(), "https://papers.example/file",
		filepath.Join(t.TempDir(), "q.tmp"))
	if err == nil {
		t.Fatal("want an error, got success")
	}
	return fetchError(t, err)
}

func TestBodyMarkersClassifySoftChallenges(t *testing.T) {
	pages := map[string]string{
		"fastly": `<html><head><title>Challenge</title><script src="/_fs-ch-abc123.js"></script></head>` +
			`<body><p>challenge</p></body></html>`,
		"anubis": `<html><head><title>Making sure</title><script>/* anubis proof of work */</script></head>` +
			`<body><p>making sure you are not a bot</p></body></html>`,
		"pow": `<html><head><title>Security check</title><script>solvePow()</script></head>` +
			`<body><p>please wait a moment</p></body></html>`,
		"pmc": `<html><head><title>Checking your browser - reCAPTCHA</title><script src="recaptcha/api.js"></script></head>` +
			`<body><p>checking your browser</p></body></html>`,
		"cloudflare": `<html><head><title>Just a moment...</title>` +
			`<script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"></script></head>` +
			`<body><p>verifying you are human</p></body></html>`,
		"gokuprops": `<html><head><title>Challenge</title><script>var gokuprops=1;</script></head>` +
			`<body><p>answer the captcha</p></body></html>`,
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			d := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, page, map[string]string{"Content-Type": "text/html"}), nil
			}))
			got := downloadBody(t, d)
			if got.Class != ClassInvalid || got.HTTPStatus != http.StatusOK || got.Msg != "anti-bot challenge response" {
				t.Fatalf("%s = %+v, want invalid 200 challenge", name, got)
			}
			if kind := got.FailureKind(); kind != FetchKindChallenge {
				t.Fatalf("%s kind = %q, want challenge", name, kind)
			}
		})
	}
}

func TestBodyMarkersNeedBothStructureAndText(t *testing.T) {
	pages := map[string]string{
		// A Turnstile comment widget on an ordinary page: vendor structure
		// with no challenge text must not read as a challenge.
		"turnstile-widget": `<html><head><title>Error: DOI Not Found</title>` +
			`<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script></head>` +
			`<body><div class="cf-turnstile"></div><p>doi not found</p></body></html>`,
		// An article about bot walls: challenge words with no vendor
		// structure must not read as a challenge.
		"article-about": `<html><head><title>On challenges</title></head>` +
			`<body><p>an essay about aws waf and the bots it stops</p></body></html>`,
		// The same article with a comment form: a form without a solver is
		// not a challenge form. (The measured variant fires here on the
		// bare form; the port narrows it.)
		"article-about-form": `<html><head><title>On challenges</title></head>` +
			`<body><p>an essay about aws waf and the bots it stops</p>` +
			`<form action="/comment"><input name="text"></form></body></html>`,
		// A vendor token with unrelated visible text.
		"gokuprops-textless": `<html><head><title>Release notes</title><script>var gokuprops=1;</script></head>` +
			`<body><p>version 2.1</p></body></html>`,
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			d := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, page, map[string]string{"Content-Type": "text/html"}), nil
			}))
			got := downloadBody(t, d)
			if got.Class != ClassInvalid || got.Msg != "response payload is not an acceptable PDF" {
				t.Fatalf("%s = %+v, want plain not-a-pdf", name, got)
			}
			if kind := got.FailureKind(); kind != FetchKindNotAPDF {
				t.Fatalf("%s kind = %q, want not-a-pdf", name, kind)
			}
		})
	}
}

func TestBodyMarkersNeverFireOnAPDF(t *testing.T) {
	// Marker-shaped strings inside real PDF bytes must not change the verdict.
	body := "%PDF-1.4\n%_fs-ch- recaptcha anubis challenge\n1 0 obj<</>>endobj\n"
	d := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, body, map[string]string{"Content-Type": "application/pdf"}), nil
	}))
	path := filepath.Join(t.TempDir(), "q.tmp")
	res, err := d.Download(context.Background(), "https://papers.example/file", path)
	if err != nil {
		t.Fatalf("pdf with marker-shaped bytes = %v, want success", err)
	}
	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.HTTPStatus)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("quarantine kept = %v", err)
	}
}

func TestBodyMarkersIgnoreBytesPastThePrefixCap(t *testing.T) {
	// A marker past bodyChallengePrefixBytes must not fire: the scan only
	// reads the bounded head the daemon already streams.
	page := strings.Repeat(" ", bodyChallengePrefixBytes+1024) +
		`<html><head><title>Challenge</title><script src="/_fs-ch-late.js"></script></head></html>`
	d := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, page, map[string]string{"Content-Type": "text/html"}), nil
	}))
	got := downloadBody(t, d)
	if got.Msg != "response payload is not an acceptable PDF" {
		t.Fatalf("late marker = %+v, want plain not-a-pdf", got)
	}
}

func TestErrorStatusBodiesKeepTheirStatusVerdict(t *testing.T) {
	// A 403 carrying challenge markup still reports the status verdict: the
	// classifier runs before any body is read, so error bodies are never
	// scanned.
	page := `<html><head><title>Challenge</title><script src="/_fs-ch-abc.js"></script></head></html>`
	d := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusForbidden, page, map[string]string{"Content-Type": "text/html"}), nil
	}))
	got := downloadBody(t, d)
	if got.Class != ClassInvalid || got.HTTPStatus != http.StatusForbidden ||
		got.Msg != "permanent HTTP response" {
		t.Fatalf("403 with markers = %+v, want permanent 403", got)
	}
	if kind := got.FailureKind(); kind != "http-403" {
		t.Fatalf("kind = %q, want http-403", kind)
	}
}

func TestFailureKindVocabulary(t *testing.T) {
	cases := []struct {
		name   string
		err    *Error
		kind   string
		detail string
	}{
		{"header-challenge", &Error{Class: ClassInvalid, HTTPStatus: 202, Msg: "anti-bot challenge response"},
			"challenge", "*fetch.Error(challenge) host=papers.example"},
		{"body-challenge", &Error{Class: ClassInvalid, HTTPStatus: 200, Msg: "anti-bot challenge response"},
			"challenge", "*fetch.Error(challenge) host=papers.example"},
		{"forbidden", &Error{Class: ClassInvalid, HTTPStatus: 403, Msg: "permanent HTTP response"},
			"http-403", "*fetch.Error(http-403) host=papers.example"},
		{"not-found", &Error{Class: ClassInvalid, HTTPStatus: 404, Msg: "permanent HTTP response"},
			"http-404", "*fetch.Error(http-404) host=papers.example"},
		{"rate-limited", &Error{Class: ClassRetryable, HTTPStatus: 429, Msg: "temporary HTTP response"},
			"http-429", "*fetch.Error(http-429) host=papers.example"},
		{"server-error", &Error{Class: ClassRetryable, HTTPStatus: 503, Msg: "temporary HTTP response"},
			"http-5xx", "*fetch.Error(http-5xx) host=papers.example"},
		{"bad-request", &Error{Class: ClassInvalid, HTTPStatus: 400, Msg: "permanent HTTP response"},
			"http-400", "*fetch.Error(http-400) host=papers.example"},
		{"not-a-pdf", &Error{Class: ClassInvalid, Msg: "response payload is not an acceptable PDF"},
			"not-a-pdf", "*fetch.Error(not-a-pdf) host=papers.example"},
		{"size-limit", &Error{Class: ClassInvalid, Msg: "response body exceeds configured size limit"},
			"size-limit", "*fetch.Error(size-limit) host=papers.example"},
		{"network", &Error{Class: ClassRetryable, Msg: "network request failed"},
			"network", "*fetch.Error(network) host=papers.example"},
		{"timeout", &Error{Class: ClassRetryable, Msg: "request timed out"},
			"timeout", "*fetch.Error(timeout) host=papers.example"},
		{"blocked", &Error{Class: ClassBlocked, Msg: "destination scheme is not permitted"},
			"blocked", "*fetch.Error(blocked) host=papers.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if kind := tc.err.FailureKind(); kind != tc.kind {
				t.Fatalf("kind = %q, want %q", kind, tc.kind)
			}
			if detail := tc.err.PersistedDetail("https://papers.example/file?token=secret"); detail != tc.detail {
				t.Fatalf("detail = %q, want %q", detail, tc.detail)
			}
			kind, host, final := ParsePersistedDetail(tc.detail)
			if kind != tc.kind || host != "papers.example" || final != "" {
				t.Fatalf("parse = %q/%q/%q, want %q/papers.example/empty", kind, host, final, tc.kind)
			}
		})
	}
}

func TestParsePersistedDetailKeepsLegacyRowsReadable(t *testing.T) {
	kind, host, final := ParsePersistedDetail("*fetch.Error")
	if kind != "" || host != "" || final != "" {
		t.Fatalf("legacy = %q/%q/%q, want unknown/empty/empty", kind, host, final)
	}
	kind, host, final = ParsePersistedDetail("*budget.ErrExceeded")
	if kind != "" || host != "" || final != "" {
		t.Fatalf("non-fetch = %q/%q/%q, want unknown/empty/empty", kind, host, final)
	}
	kind, host, final = ParsePersistedDetail("*fetch.Error(challenge)")
	if kind != "challenge" || host != "" || final != "" {
		t.Fatalf("hostless = %q/%q/%q, want challenge/empty/empty", kind, host, final)
	}
	kind, host, final = ParsePersistedDetail("*fetch.Error(http-403) host=doi.org final=publisher.example")
	if kind != "http-403" || host != "doi.org" || final != "publisher.example" {
		t.Fatalf("redirected = %q/%q/%q, want http-403/doi.org/publisher.example", kind, host, final)
	}
	if got := SuppressionHost(kind, host, final); got != "publisher.example" {
		t.Fatalf("suppression host = %q, want the responder", got)
	}
	if got := SuppressionHost("http-403", "papers.example", ""); got != "papers.example" {
		t.Fatalf("suppression host without final = %q, want the candidate", got)
	}
}

// A failure the response decided names the host that answered after
// redirects; a failure with no answer names none.
func TestFailuresRecordTheRespondingHost(t *testing.T) {
	d := challengeDownloader(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "doi.example" {
			return response(http.StatusFound, "", map[string]string{"Location": "https://publisher.example/pdf"}), nil
		}
		return response(http.StatusForbidden, "", nil), nil
	}))
	_, err := d.Download(context.Background(), "https://doi.example/10.1/x", filepath.Join(t.TempDir(), "q"))
	got := fetchError(t, err)
	if got.FinalHost != "publisher.example" {
		t.Fatalf("final host = %q, want the publisher that answered 403", got.FinalHost)
	}
	if detail := got.PersistedDetail("https://doi.example/10.1/x?token=secret"); detail != "*fetch.Error(http-403) host=doi.example final=publisher.example" {
		t.Fatalf("detail = %q", detail)
	}

	offline := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	}))
	_, err = offline.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "q"))
	if got := fetchError(t, err); got.FinalHost != "" {
		t.Fatalf("network failure final host = %q, want none: nothing answered", got.FinalHost)
	}
}

// The host gate runs before every hop: a redirect into a refused host stops
// before any request reaches it, and the refusal names that host.
func TestHostGateRefusesARedirectHop(t *testing.T) {
	var reached []string
	d := challengeDownloader(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.URL.Host)
		return response(http.StatusFound, "", map[string]string{"Location": "https://walled.example/pdf"}), nil
	}))
	d.SetHostGate(func(_ context.Context, host string) error {
		if host == "walled.example" {
			return Suppressed(host)
		}
		return nil
	})
	_, err := d.Download(context.Background(), "https://doi.example/10.1/x", filepath.Join(t.TempDir(), "q"))
	host, ok := IsSuppressed(err)
	if !ok || host != "walled.example" {
		t.Fatalf("err = %v, want a suppression naming walled.example", err)
	}
	if got := fetchError(t, err); got.HTTPStatus != 0 || got.FinalHost != "" {
		t.Fatalf("refusal = %+v, want no status and no responder", got)
	}
	if len(reached) != 1 || reached[0] != "doi.example" {
		t.Fatalf("requests = %v, want only the redirector", reached)
	}
}

// HTTP 429 outranks a challenge header: the response is a rate limit
// (retryable, Retry-After kept, kind http-429), never a challenge wall.
func TestRateLimitOutranksChallengeHeader(t *testing.T) {
	d := challengeDownloader(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusTooManyRequests, "", map[string]string{
			"X-Amzn-Waf-Action": "challenge", "Retry-After": "7", "Content-Type": "text/html",
		}), nil
	}))
	_, err := d.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "q"))
	got := fetchError(t, err)
	if got.Class != ClassRetryable || got.HTTPStatus != http.StatusTooManyRequests || got.RetryAfter != 7*time.Second {
		t.Fatalf("429 + challenge header = %+v, want retryable 429 keeping Retry-After", got)
	}
	if kind := got.FailureKind(); kind != "http-429" {
		t.Fatalf("kind = %q, want http-429", kind)
	}
	// A 429 that some older path labelled a challenge still persists as 429.
	legacy := &Error{Class: ClassInvalid, HTTPStatus: http.StatusTooManyRequests, Msg: "anti-bot challenge response"}
	if kind := legacy.FailureKind(); kind != "http-429" {
		t.Fatalf("challenge-labelled 429 kind = %q, want http-429", kind)
	}
}
