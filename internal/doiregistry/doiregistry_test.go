// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doiregistry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestTraversalSegmentInADOIIsRejectedWithoutARequest(t *testing.T) {
	// doiCoreRE is `^10\.[0-9]{4,9}/\S{1,200}$`, and RE2's \S excludes only the
	// five ASCII whitespace bytes — so a "." segment is a legal DOI. Building
	// the path with path.Join used to Clean those segments away and send the
	// request to https://doi.org/<whatever>, doi.org's own resolver root, which
	// 302s wherever that DOI's registrant points. The request must never leave.
	for _, doi := range []string{
		"10.1234/../../../10.1000/182",
		"10.1234/a/../../../../example.com/x",
		"10.1234/a/./b",
	} {
		client, paths := handleProxy(t, http.StatusOK, `{"responseCode":1}`)
		if _, err := client.Registered(context.Background(), doi); err == nil {
			t.Errorf("Registered(%q) = nil error, want a rejection", doi)
		}
		if len(*paths) != 0 {
			t.Errorf("Registered(%q) issued %v, want no request at all", doi, *paths)
		}
	}
}

func TestRepeatedSlashInADOIIsPreserved(t *testing.T) {
	// 10.48612//monograph-2025-2 and 10.48612/monograph-2025-2 are two
	// separately registered DataCite works with different titles (AGENTS.md;
	// pinned as a pair in ownership_test.go's TestNormalizeIdentifier).
	// path.Join collapsed the run, so the probe answered for the wrong work.
	client, paths := handleProxy(t, http.StatusOK, `{"responseCode":1}`)

	if _, err := client.Registered(context.Background(), "10.48612//monograph-2025-2"); err != nil {
		t.Fatalf("Registered: %v", err)
	}
	if want := "/api/handles/10.48612//monograph-2025-2?type=URL"; (*paths)[0] != want {
		t.Fatalf("request path = %q, want %q", (*paths)[0], want)
	}
}

func TestAnswersAreMemoizedAndErrorsAreNot(t *testing.T) {
	// The maintenance pass sweeps every parked job once a minute, so without
	// memoization a stuck queue becomes a registry request per job per tick.
	// An outage must NOT be latched the same way: it would disable the gate
	// for as long as the entry lived.
	var codes []int
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		code := codes[0]
		if len(codes) > 1 {
			codes = codes[1:]
		}
		if code == 100 {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = fmt.Fprintf(w, `{"responseCode":%d}`, code)
	}))
	t.Cleanup(server.Close)
	client := New(Options{Client: server.Client(), BaseURL: server.URL})

	codes = []int{2, 1}
	if _, err := client.Registered(context.Background(), "10.1234/x"); err == nil {
		t.Fatal("want an error from responseCode 2")
	}
	for range 3 {
		ok, err := client.Registered(context.Background(), "10.1234/x")
		if err != nil || !ok {
			t.Fatalf("Registered = %v, %v; want true, nil", ok, err)
		}
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 — the error must not be cached and the answer must be", hits)
	}

	codes = []int{100}
	for range 3 {
		if ok, err := client.Registered(context.Background(), "10.1234/gone"); err != nil || ok {
			t.Fatalf("Registered = %v, %v; want false, nil", ok, err)
		}
	}
	if hits != 3 {
		t.Fatalf("upstream hits = %d, want 3 — a negative answer must be cached too", hits)
	}
}

func TestCachedAnswersExpire(t *testing.T) {
	client, paths := handleProxy(t, http.StatusOK, `{"responseCode":1}`)
	now := time.Now()
	client.now = func() time.Time { return now }

	if _, err := client.Registered(context.Background(), "10.1234/x"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(positiveTTL - time.Second)
	if _, err := client.Registered(context.Background(), "10.1234/x"); err != nil {
		t.Fatal(err)
	}
	if len(*paths) != 1 {
		t.Fatalf("requests = %d, want 1 while the entry is live", len(*paths))
	}
	now = now.Add(2 * time.Second)
	if _, err := client.Registered(context.Background(), "10.1234/x"); err != nil {
		t.Fatal(err)
	}
	if len(*paths) != 2 {
		t.Fatalf("requests = %d, want 2 once the entry expired", len(*paths))
	}
}

func TestNilClientFailsClosedInsteadOfBuildingAnUnguardedOne(t *testing.T) {
	// A plain http.Client would honour HTTP_PROXY, follow ten redirects, dial
	// loopback, and apply no body cap — every property the daemon's secure
	// client exists to provide. Erroring here reaches the caller as "unknown",
	// which fails open; silently downgrading the transport would not be
	// visible at all.
	if _, err := New(Options{}).Registered(context.Background(), "10.1234/x"); err == nil {
		t.Fatal("want an error when no HTTP client was injected")
	}
}
