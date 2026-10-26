// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/resolver"
)

type tokenTestResolver struct {
	address netip.Addr
}

func (r tokenTestResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{r.address}, nil
}

type tokenTestTransport func(*http.Request) (*http.Response, error)

func (f tokenTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAIRETokensSecureExchange(t *testing.T) {
	for _, tc := range []struct {
		name        string
		address     string
		redirect    bool
		oversized   bool
		wantCalls   int32
		wantRefusal string
	}{
		{name: "token", address: "93.184.216.34", wantCalls: 1},
		{name: "redirect", address: "93.184.216.34", redirect: true, wantCalls: 1, wantRefusal: "redirects are not followed"},
		{name: "private address", address: "127.0.0.1", wantRefusal: "fetch blocked"},
		{name: "oversized response", address: "93.184.216.34", oversized: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.Method != http.MethodPost || req.URL.Path != "/oidc/token" {
					t.Errorf("request = %s %s, want POST /oidc/token", req.Method, req.URL.Path)
				}
				id, secret, ok := req.BasicAuth()
				if !ok || id != "test-client" || secret != "test-secret" {
					t.Error("token request has incorrect client credentials")
				}
				if err := req.ParseForm(); err != nil || req.Form.Get("grant_type") != "client_credentials" {
					t.Errorf("token grant is incorrect: %v", err)
				}
				if tc.redirect {
					w.Header().Set("Location", "https://other.example/token")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				if tc.oversized {
					w.Header().Set("Content-Length", "1025")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(strings.Repeat("x", 1025)))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"test-access-token","token_type":"Bearer","expires_in":3600}`))
			}))
			defer srv.Close()
			endpoint, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			// Keep the production AAI URL and secure-client admission checks.
			// Only the final transport routes the admitted request to the fixture.
			transport := tokenTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "aai.openaire.eu" {
					t.Errorf("token host = %q, want aai.openaire.eu", req.URL.Host)
				}
				local := req.Clone(req.Context())
				local.URL.Scheme = endpoint.Scheme
				local.URL.Host = endpoint.Host
				return srv.Client().Transport.RoundTrip(local)
			})
			policy := fetch.DefaultPolicy()
			policy.MaxBytes = 1024
			client, err := fetch.NewSecureHTTPClient(policy, tokenTestResolver{netip.MustParseAddr(tc.address)}, transport)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			source := cfg.Sources[config.SourceOpenAIRE]
			source.ClientID = "test-client"
			source.ClientSecret = "test-secret"
			cfg.Sources[config.SourceOpenAIRE] = source
			tokens := openAIRETokens(cfg, client)
			if tokens == nil {
				t.Fatal("client credentials did not produce a token source")
			}
			token, err := tokens.Token(context.Background())
			if tc.wantRefusal != "" {
				var temporary *resolver.TemporaryError
				if err == nil || errors.As(err, &temporary) || !strings.Contains(err.Error(), tc.wantRefusal) {
					t.Fatalf("token error = %v, want permanent refusal containing %q", err, tc.wantRefusal)
				}
			} else if tc.oversized {
				var temporary *resolver.TemporaryError
				if !errors.As(err, &temporary) {
					t.Fatalf("token error = %v, want temporary oversized-response failure", err)
				}
			} else {
				if err != nil || token != "test-access-token" {
					t.Fatalf("token = %q, error = %v", token, err)
				}
				if cached, err := tokens.Token(context.Background()); err != nil || cached != token {
					t.Fatalf("cached token = %q, error = %v", cached, err)
				}
			}
			// Opting the exchange into POST must not widen metadata access.
			req, err := http.NewRequest(http.MethodPost, "https://aai.openaire.eu/oidc/token", nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
				t.Fatal("metadata client accepted POST")
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("token requests = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}
