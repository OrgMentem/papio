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

func TestServedIdentity(t *testing.T) {
	bearer := func(v string) map[string]string { return map[string]string{"Authorization": v} }
	for _, test := range []struct {
		name    string
		key     string
		rawURL  string
		headers map[string]string
		wantOK  bool
		wantKey string
	}{
		{name: "bearer header carries the configured key", key: "private-key", rawURL: "https://api.openalex.org/works/W1?mailto=a@b.c", headers: bearer("Bearer private-key"), wantOK: true, wantKey: "private-key"},
		{name: "bearer whitespace canonicalized", key: " private-key ", rawURL: "https://api.openalex.org/works/W1", headers: bearer("Bearer  private-key  "), wantOK: true, wantKey: "private-key"},
		{name: "anonymous request omits credential", key: "private-key", rawURL: "https://api.openalex.org/works/W1?mailto=a@b.c", wantOK: true, wantKey: ""},
		{name: "foreign bearer token is not an identity", key: "private-key", rawURL: "https://api.openalex.org/works/W1", headers: bearer("Bearer other-key"), wantOK: false},
		{name: "legacy query api_key still works during transition", key: "private-key", rawURL: "https://api.openalex.org/works/W1?api_key=private-key", wantOK: true, wantKey: "private-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			keyed := config.Source{Enabled: true, APIKey: test.key}
			served, ok := ServedIdentity(openAlexReq(t, test.rawURL, test.headers), keyed)
			if ok != test.wantOK {
				t.Fatalf("ServedIdentity ok = %t, want %t (served = %+v)", ok, test.wantOK, served)
			}
			if test.wantOK && served.APIKey != test.wantKey {
				t.Fatalf("served = %+v, want APIKey %q", served, test.wantKey)
			}
		})
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
