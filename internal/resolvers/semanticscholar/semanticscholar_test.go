// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package semanticscholar

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

const fullRecord = `{
	"externalIds": {"DOI": "https://doi.org/10.5555/Example.", "ArXiv": "2401.12345", "PubMed": "12345678"},
	"title": "  Grounded resolving  ",
	"year": 2024,
	"authors": [{"name": "Ada Lovelace"}, {"name": " Grace Hopper "}, {"name": " "}],
	"venue": "Useful Journal",
	"url": "https://www.semanticscholar.org/paper/abc123",
	"isOpenAccess": true,
	"openAccessPdf": {"url": "https://example.org/paper.pdf", "status": "GREEN", "license": "CC-BY"}
}`

func TestResolveByDOIMapsCandidateAndRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		apiKey string
	}{
		{name: "keyless"},
		{name: "keyed", apiKey: "s2-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotPath, gotFields, gotKey string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotFields = r.URL.Query().Get("fields")
				gotKey = r.Header.Get("x-api-key")
				_, _ = w.Write([]byte(fullRecord))
			}))
			defer server.Close()

			cands, err := NewWithOptions(Options{
				Client: http.DefaultClient, APIKey: test.apiKey, BaseURL: server.URL,
			}).Resolve(context.Background(), work.Work{DOI: "10.5555/example"})
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != "/paper/DOI:10.5555/example" {
				t.Fatalf("lookup path = %q", gotPath)
			}
			if gotFields != paperFields {
				t.Fatalf("fields = %q, want %q", gotFields, paperFields)
			}
			if gotKey != test.apiKey {
				t.Fatalf("x-api-key = %q, want %q", gotKey, test.apiKey)
			}
			if len(cands) != 1 {
				t.Fatalf("candidates = %d, want 1", len(cands))
			}
			c := cands[0]
			if c.Source != "semanticscholar" || c.URL != "https://example.org/paper.pdf" {
				t.Fatalf("candidate = %+v", c)
			}
			if c.Landing != "https://www.semanticscholar.org/paper/abc123" {
				t.Fatalf("landing = %q", c.Landing)
			}
			if c.Version != resolver.VersionUnknown {
				t.Fatalf("version = %q, want unknown: openAccessPdf carries no typed version evidence", c.Version)
			}
			if c.AccessBasis != resolver.AccessOpen || !c.Direct || c.ExpectedMIME != "application/pdf" {
				t.Fatalf("candidate = %+v", c)
			}
			if c.ReuseLicense != "cc-by" {
				t.Fatalf("reuse license = %q, want the stated license lowercased", c.ReuseLicense)
			}
			if c.IdentityConfidence != 1 {
				t.Fatalf("identity confidence = %v", c.IdentityConfidence)
			}
			if c.ResolvedWork.Title != "Grounded resolving" || c.ResolvedWork.DOI != "10.5555/example" ||
				c.ResolvedWork.PMID != "12345678" || c.ResolvedWork.ArXiv != "2401.12345" ||
				len(c.ResolvedWork.Authors) != 2 || c.ResolvedWork.Container != "Useful Journal" {
				t.Fatalf("resolved work = %+v", c.ResolvedWork)
			}
			for _, line := range c.Evidence {
				if strings.Contains(line, "10.5555") && !strings.HasPrefix(line, "semanticscholar lookup=") {
					t.Fatalf("evidence leaks a raw identifier outside the lookup label: %q", line)
				}
			}
		})
	}
}

func TestResolvePrefersDOIThenArXivThenPMID(t *testing.T) {
	for _, test := range []struct {
		name      string
		requested work.Work
		wantPath  string
	}{
		{name: "arxiv", requested: work.Work{ArXiv: "2401.12345v2"}, wantPath: "/paper/arXiv:2401.12345v2"},
		{name: "pmid", requested: work.Work{PMID: "12345678"}, wantPath: "/paper/PMID:12345678"},
		{name: "doi wins over both", requested: work.Work{DOI: "10.1234/x", ArXiv: "2401.12345", PMID: "12345678"}, wantPath: "/paper/DOI:10.1234/x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = w.Write([]byte(`{"openAccessPdf": {"url": "https://example.org/p.pdf"}}`))
			}))
			defer server.Close()

			if _, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
				Resolve(context.Background(), test.requested); err != nil {
				t.Fatal(err)
			}
			if gotPath != test.wantPath {
				t.Fatalf("lookup path = %q, want %q", gotPath, test.wantPath)
			}
		})
	}
}

func TestResolveWithoutIdentifierMakesNoRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a work without DOI, arXiv, or PMID must not reach the network")
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{Title: "Only a title", ISBN: "9780306406157"})
	if err != nil || cands != nil {
		t.Fatalf("Resolve = (%v, %v), want (nil, nil): title search must never become a candidate", cands, err)
	}
}

func TestResolveTreatsMetadataOnlyRecordsAsAbsent(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "no openAccessPdf", body: `{"isOpenAccess": true}`},
		{name: "empty pdf url", body: `{"isOpenAccess": true, "openAccessPdf": {"url": "  "}}`},
		{name: "non-http pdf url", body: `{"openAccessPdf": {"url": "ftp://example.org/p.pdf"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
				Resolve(context.Background(), work.Work{DOI: "10.5555/example"})
			if err != nil || cands != nil {
				t.Fatalf("Resolve = (%v, %v), want (nil, nil): isOpenAccess without a usable PDF URL is metadata", cands, err)
			}
		})
	}
}

func TestResolveRejectsConflictingIdentifiers(t *testing.T) {
	for _, test := range []struct {
		name      string
		requested work.Work
		body      string
		wantCands int
	}{
		{
			name:      "different DOI is a conflict",
			requested: work.Work{DOI: "10.5555/example"},
			body:      `{"externalIds": {"DOI": "10.9999/other"}, "openAccessPdf": {"url": "https://example.org/p.pdf"}}`,
		},
		{
			name:      "different PMID is a conflict",
			requested: work.Work{DOI: "10.5555/example", PMID: "111"},
			body:      `{"externalIds": {"DOI": "10.5555/example", "PubMed": "222"}, "openAccessPdf": {"url": "https://example.org/p.pdf"}}`,
		},
		{
			name:      "arXiv version suffix is not a conflict",
			requested: work.Work{ArXiv: "2401.12345v2"},
			body:      `{"externalIds": {"ArXiv": "2401.12345"}, "openAccessPdf": {"url": "https://example.org/p.pdf"}}`,
			wantCands: 1,
		},
		{
			name:      "absent echoed identifiers are not a conflict",
			requested: work.Work{DOI: "10.5555/example"},
			body:      `{"openAccessPdf": {"url": "https://example.org/p.pdf"}}`,
			wantCands: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
				Resolve(context.Background(), test.requested)
			if err != nil {
				t.Fatal(err)
			}
			if len(cands) != test.wantCands {
				t.Fatalf("candidates = %d, want %d", len(cands), test.wantCands)
			}
		})
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
		{name: "429 is temporary with wait", status: http.StatusTooManyRequests, retryAfter: "7", wantTemporary: true, wantWait: 7 * time.Second},
		{name: "500 is temporary", status: http.StatusInternalServerError, wantTemporary: true},
		{name: "403 is a hard failure", status: http.StatusForbidden},
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
				Resolve(context.Background(), work.Work{DOI: "10.5555/example"})
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

func TestResolveMissingLicenseIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openAccessPdf": {"url": "https://example.org/p.pdf", "status": "BRONZE"}}`))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.5555/example"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve = (%v, %v)", cands, err)
	}
	if cands[0].ReuseLicense != "unknown" {
		t.Fatalf("reuse license = %q, want unknown: a reachable PDF is not a redistribution license", cands[0].ReuseLicense)
	}
}

func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	for _, value := range []string{"9223372037", "9999999999", "922337203685477581"} {
		if got := parseRetryAfter(value, time.Now()); got < 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, want a non-negative clamped duration: a negative wait inverts Retry-After into an immediate retry storm", value, got)
		}
	}
	if got := parseRetryAfter("7", time.Now()); got != 7*time.Second {
		t.Fatalf("parseRetryAfter(7) = %v", got)
	}
}

func TestResolveRejectsOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openAccessPdf": {"url": "https://example.org/p.pdf"}, "title": "` + strings.Repeat("x", 512) + `"}`))
	}))
	defer server.Close()

	_, err := NewWithOptions(Options{
		Client: http.DefaultClient, BaseURL: server.URL, MaxResponseBytes: 128,
	}).Resolve(context.Background(), work.Work{DOI: "10.5555/example"})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Resolve = %v, want a size-limit rejection", err)
	}
	if _, temporary := resolver.Temporary(err); temporary {
		t.Fatalf("an oversized body is a malformed response, not a retryable condition: %v", err)
	}
}

func TestResolvedWorkNormalizesEchoedArXivID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"externalIds": {"ArXiv": " arXiv:2401.12345v2 "}, "openAccessPdf": {"url": "https://example.org/p.pdf"}}`))
	}))
	defer server.Close()

	cands, err := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL}).
		Resolve(context.Background(), work.Work{DOI: "10.5555/example"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve = (%v, %v)", cands, err)
	}
	if got := cands[0].ResolvedWork.ArXiv; got != "2401.12345v2" {
		t.Fatalf("resolved arXiv = %q, want the normalized, version-preserving id", got)
	}
}
