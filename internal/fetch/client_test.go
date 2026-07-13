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
	if _, err := client.Do(req); err == nil {
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
