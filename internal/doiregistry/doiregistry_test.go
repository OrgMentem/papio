// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doiregistry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// handleProxy serves the DOI Handle System's REST shape: the response code in
// the body, mirrored in the HTTP status. Both live fixtures are captured from
// https://doi.org/api/handles/… on 2026-08-06.
func handleProxy(t *testing.T, status int, body string) (*Client, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return New(Options{Client: server.Client(), BaseURL: server.URL}), &paths
}

func TestRegisteredDOIResolves(t *testing.T) {
	client, paths := handleProxy(t, http.StatusOK,
		`{"responseCode":1,"handle":"10.1016/j.cedpsych.2020.101860","values":[{"index":1,"type":"URL","data":{"format":"string","value":"https://linkinghub.elsevier.com/retrieve/pii/S0361476X20300254"}}]}`)

	ok, err := client.Registered(context.Background(), "10.1016/j.cedpsych.2020.101860")
	if err != nil {
		t.Fatalf("Registered: %v", err)
	}
	if !ok {
		t.Fatal("registered DOI reported as unregistered")
	}
	// The DOI's own slash is a path separator, not an escaped segment: the
	// proxy 404s on %2F, which would make every real DOI look unregistered.
	if want := "/api/handles/10.1016/j.cedpsych.2020.101860?type=URL"; (*paths)[0] != want {
		t.Fatalf("request path = %q, want %q", (*paths)[0], want)
	}
}

func TestUnregisteredDOIIsReportedNotRegistered(t *testing.T) {
	client, _ := handleProxy(t, http.StatusNotFound,
		`{"responseCode":100,"handle":"10.1016/j.cedpsych.2020.101816"}`)

	ok, err := client.Registered(context.Background(), "10.1016/j.cedpsych.2020.101816")
	if err != nil {
		t.Fatalf("Registered: %v — code 100 is a definite answer, not a fault", err)
	}
	if ok {
		t.Fatal("unregistered DOI reported as registered")
	}
}

func TestHandleWithoutURLValueIsStillRegistered(t *testing.T) {
	// responseCode 200 means the handle exists but carries no value of the
	// requested type. That is a registrant metadata gap, not a missing DOI, and
	// reading it as unregistered would kill real jobs.
	client, _ := handleProxy(t, http.StatusOK, `{"responseCode":200,"handle":"10.1234/values-missing"}`)

	ok, err := client.Registered(context.Background(), "10.1234/values-missing")
	if err != nil {
		t.Fatalf("Registered: %v", err)
	}
	if !ok {
		t.Fatal("handle with no URL value must still count as registered")
	}
}

func TestProxyFaultIsAnErrorNotAnAnswer(t *testing.T) {
	// Every non-nil error here must reach the caller as "unknown". The app gate
	// fails open on it; a false "unregistered" would terminate good jobs during
	// an outage.
	for name, test := range map[string]struct {
		status int
		body   string
	}{
		"server error":     {http.StatusInternalServerError, `{"responseCode":2}`},
		"proxy error code": {http.StatusOK, `{"responseCode":2,"handle":"10.1234/x"}`},
		"unknown code":     {http.StatusOK, `{"responseCode":42}`},
		"invalid json":     {http.StatusOK, `not json`},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := handleProxy(t, test.status, test.body)
			if _, err := client.Registered(context.Background(), "10.1234/x"); err == nil {
				t.Fatal("want an error so the caller treats the answer as unknown")
			}
		})
	}
}

func TestMalformedDOIIsRejectedWithoutARequest(t *testing.T) {
	client, paths := handleProxy(t, http.StatusOK, `{"responseCode":1}`)

	if _, err := client.Registered(context.Background(), "not-a-doi"); err == nil {
		t.Fatal("want an error for a string that is not shaped like a DOI")
	}
	if len(*paths) != 0 {
		t.Fatalf("paths = %v, want no request for an unparseable DOI", *paths)
	}
}

func TestDOIURLFormIsNormalizedBeforeProbing(t *testing.T) {
	client, paths := handleProxy(t, http.StatusOK, `{"responseCode":1}`)

	if _, err := client.Registered(context.Background(), "https://doi.org/10.1234/Mixed.Case"); err != nil {
		t.Fatalf("Registered: %v", err)
	}
	if got := (*paths)[0]; !strings.HasPrefix(got, "/api/handles/10.1234/mixed.case") {
		t.Fatalf("request path = %q, want the normalized bare DOI", got)
	}
}
