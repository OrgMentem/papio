// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package openaire

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/resolver"
	"papio/internal/work"
)

// probeRecord mirrors the live Graph API shape verified against
// api.openaire.eu on 2026-08-07: pids carry doi/pmid, bestAccessRight labels
// the aggregate, and instances list urls with an optional license and no
// file marker.
const probeRecord = `{
	"header": {"numFound": 1},
	"results": [{
		"mainTitle": "  The perils of plurality rule  ",
		"publicationDate": "2022-01-20",
		"bestAccessRight": {"label": "OPEN"},
		"pids": [
			{"scheme": "doi", "value": "10.1371/journal.pone.0262026"},
			{"scheme": "pmid", "value": "35051190"}
		],
		"authors": [{"fullName": "Joshua Holzer"}, {"fullName": " "}],
		"container": {"name": "PLOS ONE"},
		"instances": [
			{"license": "CC BY", "urls": ["https://doi.org/10.1371/journal.pone.0262026"], "refereed": "peerReviewed"},
			{"license": "CC BY", "urls": ["https://journals.plos.org/plosone/article/file?id=10.1371/journal.pone.0262026&type=printable"]},
			{"urls": ["https://pubmed.ncbi.nlm.nih.gov/35051190"]}
		]
	}]
}`

func TestResolveMapsOpenInstancesAsLandingCandidates(t *testing.T) {
	var gotPath, gotPID, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPID = r.URL.Query().Get("pid")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(probeRecord))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, APIKey: "token", BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0262026"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/researchProducts" || gotPID != "10.1371/journal.pone.0262026" {
		t.Fatalf("request = %q pid=%q", gotPath, gotPID)
	}
	if gotAuth != "Bearer token" {
		t.Fatalf("authorization = %q, want the bearer token", gotAuth)
	}
	// The pubmed instance has no license and no OPEN access right: skipped.
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want the two licensed instances", len(cands))
	}
	for _, c := range cands {
		if c.Source != "openaire" || c.AccessBasis != resolver.AccessOpen {
			t.Fatalf("candidate = %+v", c)
		}
		if c.Direct || c.ExpectedMIME != "" {
			t.Fatalf("candidate = %+v, want a landing observation: OpenAIRE marks no URL as the file itself", c)
		}
		if c.ReuseLicense != "cc by" {
			t.Fatalf("reuse license = %q", c.ReuseLicense)
		}
		if c.ResolvedWork.Title != "The perils of plurality rule" || c.ResolvedWork.PMID != "35051190" ||
			c.ResolvedWork.Year != 2022 || c.ResolvedWork.Container != "PLOS ONE" || len(c.ResolvedWork.Authors) != 1 {
			t.Fatalf("resolved work = %+v", c.ResolvedWork)
		}
	}
}

func TestResolveSkipsClosedRecordsAndConflicts(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "closed aggregate record", body: `{"results": [{"bestAccessRight": {"label": "RESTRICTED"},
			"pids": [{"scheme": "doi", "value": "10.1371/journal.pone.0262026"}],
			"instances": [{"license": "CC BY", "urls": ["https://example.org/p"]}]}]}`},
		{name: "echoed DOI names a different work", body: `{"results": [{"bestAccessRight": {"label": "OPEN"},
			"pids": [{"scheme": "doi", "value": "10.9999/other"}],
			"instances": [{"license": "CC BY", "urls": ["https://example.org/p"]}]}]}`},
		{name: "no results", body: `{"results": []}`},
		{name: "open record with only unlicensed closed instances", body: `{"results": [{"bestAccessRight": {"label": "OPEN"},
			"pids": [{"scheme": "doi", "value": "10.1371/journal.pone.0262026"}],
			"instances": [{"urls": ["https://example.org/p"]}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
				Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0262026"})
			if err != nil || cands != nil {
				t.Fatalf("Resolve = (%v, %v), want (nil, nil)", cands, err)
			}
		})
	}
}

func TestResolveCapsInstancesAndDeduplicatesURLs(t *testing.T) {
	instances := []string{}
	for _, u := range []string{"https://a.example/p", "https://a.example/p", "https://b.example/p", "https://c.example/p", "https://d.example/p"} {
		instances = append(instances, `{"license": "cc0", "urls": ["`+u+`"]}`)
	}
	body := `{"results": [{"bestAccessRight": {"label": "OPEN"},
		"pids": [{"scheme": "doi", "value": "10.1371/journal.pone.0262026"}],
		"instances": [` + strings.Join(instances, ",") + `]}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0262026"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want the duplicate collapsed and the count capped at 3", len(cands))
	}
}

func TestResolveWithoutDOIOrPMIDMakesNoRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a work without DOI or PMID must not reach the network")
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{Title: "Only a title", ArXiv: "2401.12345"})
	if err != nil || cands != nil {
		t.Fatalf("Resolve = (%v, %v), want (nil, nil)", cands, err)
	}
}

func TestResolveClassifiesHTTPFailures(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		retryAfter    string
		wantNilNil    bool
		wantTemporary bool
		wantWait      time.Duration
	}{
		{name: "not found is absence", status: http.StatusNotFound, wantNilNil: true},
		{name: "429 is temporary with wait", status: http.StatusTooManyRequests, retryAfter: "60", wantTemporary: true, wantWait: time.Minute},
		{name: "503 is temporary", status: http.StatusServiceUnavailable, wantTemporary: true},
		{name: "401 is a hard failure", status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
			}))
			defer server.Close()

			cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
				Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0262026"})
			if test.wantNilNil {
				if err != nil || cands != nil {
					t.Fatalf("Resolve = (%v, %v), want (nil, nil)", cands, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			wait, temporary := resolver.Temporary(err)
			if temporary != test.wantTemporary {
				t.Fatalf("Temporary(%v) = %v, want %v", err, temporary, test.wantTemporary)
			}
			if test.wantWait != 0 && wait != test.wantWait {
				t.Fatalf("retry wait = %v, want %v", wait, test.wantWait)
			}
		})
	}
}

func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	for _, value := range []string{"9223372037", "9999999999"} {
		if got := parseRetryAfter(value, time.Now()); got < 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, want a non-negative clamped duration", value, got)
		}
	}
}

func TestResolveRejectsOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results": [{"mainTitle": "` + strings.Repeat("x", 512) + `"}]}`))
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, MaxResponseBytes: 128}).
		Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0262026"})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Resolve = %v, want a size-limit rejection", err)
	}
	if _, temporary := resolver.Temporary(err); temporary {
		t.Fatalf("an oversized body is malformed, not retryable: %v", err)
	}
}

func TestResolveByPMIDUsesThePIDFilter(t *testing.T) {
	var gotPID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPID = r.URL.Query().Get("pid")
		_, _ = w.Write([]byte(`{"results": [{"bestAccessRight": {"label": "OPEN"},
			"pids": [{"scheme": "pmid", "value": "35051190"}],
			"instances": [{"license": "CC BY", "urls": ["https://example.org/p"]}]}]}`))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{PMID: "35051190"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve = (%v, %v)", cands, err)
	}
	if gotPID != "35051190" {
		t.Fatalf("pid query = %q", gotPID)
	}
}

func TestResolveRejectsRestrictedLicensesAsOpenAccess(t *testing.T) {
	// Live-observed shape: a dedup'd OPEN record carrying the publisher's
	// paywalled instance under a contractual "Springer TDM" license beside
	// a genuinely open repository copy. Only the open copy may survive.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results": [{"bestAccessRight": {"label": "OPEN"},
			"pids": [{"scheme": "doi", "value": "10.1038/s41586-020-2649-2"}],
			"instances": [
				{"license": "Springer TDM", "urls": ["https://doi.org/10.1038/nature12373"]},
				{"license": "CC BY", "urls": ["https://repository.example.edu/handle/1/1234"]}
			]}]}`))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.1038/s41586-020-2649-2"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve = (%v, %v), want exactly the repository copy", cands, err)
	}
	if cands[0].URL != "https://repository.example.edu/handle/1/1234" {
		t.Fatalf("candidate URL = %q: a contractual TDM license is not open access", cands[0].URL)
	}
}

func TestResolveAcceptsDedupRecordsMatchedByANonFirstDOI(t *testing.T) {
	// Live-observed shape: OpenAIRE returns the same dedup'd record for any
	// of its five DOIs, in a fixed order unrelated to the query. A request
	// by the repository DOI must not be rejected because the publisher DOI
	// happens to sit first.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results": [{"bestAccessRight": {"label": "OPEN"},
			"pids": [
				{"scheme": "doi", "value": "10.1038/s41586-020-2649-2"},
				{"scheme": "doi", "value": "10.17863/cam.62701"}
			],
			"instances": [{"license": "CC BY", "urls": ["https://repository.example.edu/p"]}]}]}`))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.17863/cam.62701"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve = (%v, %v), want the record accepted via its non-first DOI", cands, err)
	}
}

func TestResolveMarksObviousPDFPathsDirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results": [{"bestAccessRight": {"label": "OPEN"},
			"pids": [{"scheme": "doi", "value": "10.1371/journal.pone.0262026"}],
			"instances": [
				{"license": "CC BY", "urls": ["https://repository.example.edu/download/paper.PDF"]},
				{"license": "CC BY", "urls": ["https://repository.example.edu/record/1234"]}
			]}]}`))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0262026"})
	if err != nil || len(cands) != 2 {
		t.Fatalf("Resolve = (%v, %v)", cands, err)
	}
	if !cands[0].Direct || cands[0].ExpectedMIME != "application/pdf" {
		t.Fatalf("file-path candidate = %+v, want Direct with a PDF MIME: landing expansion parses HTML and can never read a file", cands[0])
	}
	if cands[1].Direct {
		t.Fatalf("record-page candidate = %+v, want a landing observation", cands[1])
	}
}
