// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type publicTestResolver struct{}

func (publicTestResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func metadataPolicy() Policy {
	return Policy{
		MaxBytes:       16,
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		HeaderTimeout:  time.Second,
		BodyTimeout:    time.Second,
		MaxRedirects:   2,
	}
}

func TestSecureHTTPClientBlocksPrivateRedirectBeforeSecondRequest(t *testing.T) {
	calls := 0
	client, err := NewSecureHTTPClient(metadataPolicy(), publicTestResolver{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://127.0.0.1/secret"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test/start", nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("private redirect accepted")
	}
	if calls != 1 {
		t.Fatalf("round trips = %d, want 1", calls)
	}
}

func TestSecureHTTPClientStripsCallerHeadersAcrossHosts(t *testing.T) {
	calls := 0
	client, err := NewSecureHTTPClient(metadataPolicy(), publicTestResolver{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://other.test/final"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization leaked across host: %q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("pdf")), ContentLength: 3}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test/start", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "pdf" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
}

func TestSecureHTTPClientRejectsOversizedBodyWithoutContentLength(t *testing.T) {
	policy := metadataPolicy()
	policy.MaxBytes = 3
	client, err := NewSecureHTTPClient(policy, publicTestResolver{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("four")), ContentLength: -1}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test/data", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("oversized streaming body accepted")
	}
}
func TestSecureHTTPClientGETOnlyRejectsPOST(t *testing.T) {
	client, err := NewSecureHTTPClient(metadataPolicy(), publicTestResolver{}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("GET-only client sent a POST")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://example.test/submit", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || err.Error() != "fetch invalid: only GET requests are supported" {
		t.Fatalf("POST error = %v, want existing GET-only error", err)
	}
}

func TestSecureHTTPClientWithPOSTPreservesBody(t *testing.T) {
	const wantBody = `{"title":"A grounded result"}`
	client, err := NewSecureHTTPClientWithPOST(metadataPolicy(), publicTestResolver{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if string(got) != wantBody {
			t.Fatalf("request body = %q, want %q", got, wantBody)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(`{"ok":true}`)),
			ContentLength: 11,
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://example.test/submit", strings.NewReader(wantBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("response body = %q", got)
	}
}

func TestSecureHTTPClientWithPOSTBlocksPrivateDestination(t *testing.T) {
	calls := 0
	client, err := NewSecureHTTPClientWithPOST(metadataPolicy(), publicTestResolver{}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://127.0.0.1/submit", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	bodyClosed := false
	req.Body = closeTrackingBody{Reader: strings.NewReader("body"), closed: &bodyClosed}
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "destination address is not permitted") {
		t.Fatalf("blocked POST error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("round trips = %d, want 0", calls)
	}
	if !bodyClosed {
		t.Fatal("blocked POST request body was not closed")
	}
}

type closeTrackingBody struct {
	io.Reader
	closed *bool
}

func (b closeTrackingBody) Close() error {
	*b.closed = true
	return nil
}

func TestSecureHTTPClientWithPOSTRefusesRedirectWithoutReplay(t *testing.T) {
	calls := 0
	client, err := NewSecureHTTPClientWithPOST(metadataPolicy(), publicTestResolver{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("POST body was replayed after redirect")
		}
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": {"https://example.test/final"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://example.test/submit", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "redirects are not followed for non-GET requests") {
		t.Fatalf("redirect POST error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("round trips = %d, want 1", calls)
	}
}
