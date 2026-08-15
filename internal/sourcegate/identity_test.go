// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"net/http"
	"net/url"
	"testing"

	"papio/internal/config"
)

func openAlexReq(t *testing.T, raw string, header map[string]string) *http.Request {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{Method: http.MethodGet, URL: parsed, Header: make(http.Header)}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return req
}

func TestServedIdentityFromBearerHeader(t *testing.T) {
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	req := openAlexReq(t, "https://api.openalex.org/works/W1?mailto=a@b.c", map[string]string{
		"Authorization": "Bearer private-key",
	})
	served, ok := ServedIdentity(req, keyed)
	if !ok {
		t.Fatal("expected known identity")
	}
	if served.APIKey != "private-key" {
		t.Fatalf("APIKey = %q, want configured key", served.APIKey)
	}
}

func TestServedIdentityBearerWhitespaceCanonicalized(t *testing.T) {
	keyed := config.Source{Enabled: true, APIKey: " private-key "}
	req := openAlexReq(t, "https://api.openalex.org/works/W1", map[string]string{
		"Authorization": "Bearer  private-key  ",
	})
	served, ok := ServedIdentity(req, keyed)
	if !ok || served.APIKey != "private-key" {
		t.Fatalf("served = %+v ok=%v, want trimmed keyed identity", served, ok)
	}
}

func TestServedIdentityAnonymousOmitsCredential(t *testing.T) {
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	req := openAlexReq(t, "https://api.openalex.org/works/W1?mailto=a@b.c", nil)
	served, ok := ServedIdentity(req, keyed)
	if !ok || served.APIKey != "" {
		t.Fatalf("served = %+v, want anonymous", served)
	}
}

func TestServedIdentityRejectsUnknownCredential(t *testing.T) {
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	req := openAlexReq(t, "https://api.openalex.org/works/W1", map[string]string{
		"Authorization": "Bearer other-key",
	})
	if _, ok := ServedIdentity(req, keyed); ok {
		t.Fatal("unexpected identity for foreign bearer token")
	}
}

func TestServedIdentityLegacyQueryKeyStillWorks(t *testing.T) {
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	req := openAlexReq(t, "https://api.openalex.org/works/W1?api_key=private-key", nil)
	served, ok := ServedIdentity(req, keyed)
	if !ok || served.APIKey != "private-key" {
		t.Fatalf("served = %+v, want query api_key fallback during transition", served)
	}
}

func TestSetOpenAlexAuthorizationStripsQueryKey(t *testing.T) {
	req := openAlexReq(t, "https://api.openalex.org/works?api_key=stale&mailto=a@b.c", nil)
	SetOpenAlexAuthorization(req, "private-key")
	if req.URL.Query().Get("api_key") != "" {
		t.Fatalf("query still carries api_key: %s", req.URL.RawQuery)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer private-key" {
		t.Fatalf("Authorization = %q, want bearer", got)
	}
}

func TestClearOpenAlexAuthorization(t *testing.T) {
	req := openAlexReq(t, "https://api.openalex.org/works?api_key=stale", map[string]string{
		"Authorization": "Bearer private-key",
	})
	ClearOpenAlexAuthorization(req)
	if req.URL.Query().Get("api_key") != "" || req.Header.Get("Authorization") != "" {
		t.Fatalf("credential not cleared: query=%s auth=%q", req.URL.RawQuery, req.Header.Get("Authorization"))
	}
}
