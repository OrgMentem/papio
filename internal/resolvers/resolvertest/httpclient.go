// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package resolvertest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"papio/internal/fetch"
)

// HTTPClient is the minimal resolver HTTP dependency. It mirrors the
// HTTPClient interfaces defined in each resolver package (core, crossreftdm,
// etc.) without importing them, avoiding an import cycle.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type httpClientTestResolver struct{}

func (httpClientTestResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type testTransportFunc func(*http.Request) (*http.Response, error)

func (f testTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type opaqueDoer func(*http.Request) (*http.Response, error)

func (f opaqueDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

// CheckDoRejectsOpaqueClient verifies the security contract shared by resolver
// packages: a vetting SecureHTTPClient is accepted, while an opaque HTTPClient
// that does not enforce credential-safe redirects is rejected with unsafeErr.
// The caller supplies its package-private errUnsafeHTTPClient sentinel and a
// closure that wires the supplied client into its own Resolver and calls do.
func CheckDoRejectsOpaqueClient(t *testing.T, unsafeErr error, doWithClient func(client HTTPClient) (*http.Response, error)) {
	t.Helper()

	secureClient, err := fetch.NewSecureHTTPClient(fetch.Policy{
		MaxBytes:       1024,
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		HeaderTimeout:  time.Second,
		BodyTimeout:    time.Second,
	}, httpClientTestResolver{}, testTransportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}))
	if err != nil {
		t.Fatalf("NewSecureHTTPClient: %v", err)
	}

	cases := []struct {
		name       string
		client     HTTPClient
		wantUnsafe bool
	}{
		{name: "secure client", client: secureClient},
		{name: "opaque client", client: opaqueDoer(func(*http.Request) (*http.Response, error) { return nil, errors.New("must not call") }), wantUnsafe: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := doWithClient(tc.client)
			if got := errors.Is(err, unsafeErr); got != tc.wantUnsafe {
				t.Fatalf("errUnsafeHTTPClient = %v, err = %v", got, err)
			}
			if tc.wantUnsafe {
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for secure client: %v", err)
			}
			if resp == nil {
				t.Fatal("secure client returned nil response")
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("Body.Close: %v", err)
			}
		})
	}
}
