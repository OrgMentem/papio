// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package arxiv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

// atomEntryXML renders a single-entry Atom feed. idInFeed is the value placed in
// <id>; extra is optional trailing arxiv:* elements (doi/journal_ref).
func atomEntryXML(idInFeed, title, extra string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <entry>
    <id>http://arxiv.org/abs/%s</id>
    <published>2017-06-12T00:00:00Z</published>
    <title>%s</title>
    <author><name>Ashish Vaswani</name></author>
    <author><name>Noam Shazeer</name></author>
    %s
  </entry>
</feed>`, idInFeed, title, extra)
}

// serveAtom returns an httptest server that records the last id_list query and
// replies with status/headers/body.
func serveAtom(t *testing.T, status int, headers map[string]string, body string, capturedIDList *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capturedIDList != nil {
			*capturedIDList = r.URL.Query().Get("id_list")
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newResolver(srv *httptest.Server) *Resolver {
	return NewWithOptions(Options{Client: srv.Client(), BaseURL: srv.URL})
}

func TestResolveVersionForms(t *testing.T) {
	cases := []struct {
		name        string
		in          work.Work
		wantBase    string
		wantVersion string // requested_version evidence value
	}{
		{"new style", work.Work{ArXiv: "2101.00001"}, "2101.00001", "latest"},
		{"new style versioned", work.Work{ArXiv: "2101.00001v3"}, "2101.00001", "v3"},
		{"old style", work.Work{ArXiv: "hep-th/9901001"}, "hep-th/9901001", "latest"},
		{"old style versioned", work.Work{ArXiv: "hep-th/9901001v2"}, "hep-th/9901001", "v2"},
		{"datacite doi", work.Work{DOI: "10.48550/arXiv.2101.00001"}, "2101.00001", "latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var idList string
			// Echo the requested id in the feed so firstResult accepts it.
			srv := serveAtom(t, http.StatusOK, nil, "", &idList)
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				idList = r.URL.Query().Get("id_list")
				_, _ = w.Write([]byte(atomEntryXML(idList+"v1", "Attention Is All You Need", "")))
			})
			r := newResolver(srv)
			cands, err := r.Resolve(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(cands) != 1 {
				t.Fatalf("want 1 candidate, got %d", len(cands))
			}
			// Canonical API query and PDF/landing URLs strip the version.
			if idList != tc.wantBase {
				t.Errorf("id_list = %q, want %q (version must be stripped)", idList, tc.wantBase)
			}
			c := cands[0]
			if c.URL != pdfBase+tc.wantBase {
				t.Errorf("URL = %q, want %q", c.URL, pdfBase+tc.wantBase)
			}
			if c.Landing != absBase+tc.wantBase {
				t.Errorf("Landing = %q, want %q", c.Landing, absBase+tc.wantBase)
			}
			if c.ResolvedWork.ArXiv != tc.wantBase {
				t.Errorf("ResolvedWork.ArXiv = %q, want %q", c.ResolvedWork.ArXiv, tc.wantBase)
			}
			if !hasEvidence(c.Evidence, "arxiv requested_version="+tc.wantVersion) {
				t.Errorf("evidence missing requested_version=%q: %v", tc.wantVersion, c.Evidence)
			}
			if err := resolver.ValidateCandidate(c); err != nil {
				t.Errorf("ValidateCandidate: %v", err)
			}
		})
	}
}

func TestResolveExactCandidateFields(t *testing.T) {
	// A published work: arxiv:doi + journal_ref present -> accepted-manuscript
	// semantics and a discovered DOI in ResolvedWork.
	extra := `<arxiv:doi>10.1000/xyz123</arxiv:doi><arxiv:journal_ref>NIPS 2017</arxiv:journal_ref>`
	body := atomEntryXML("2101.00001v1", "Attention Is All You Need", extra)
	srv := serveAtom(t, http.StatusOK, nil, body, nil)
	r := newResolver(srv)

	cands, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	c := cands[0]
	checks := map[string]struct{ got, want string }{
		"Source":       {c.Source, "arxiv"},
		"URL":          {c.URL, "https://arxiv.org/pdf/2101.00001"},
		"Landing":      {c.Landing, "https://arxiv.org/abs/2101.00001"},
		"Version":      {c.Version, resolver.VersionAccepted},
		"AccessBasis":  {c.AccessBasis, resolver.AccessOpen},
		"ReuseLicense": {c.ReuseLicense, "unknown"},
		"ExpectedMIME": {c.ExpectedMIME, "application/pdf"},
	}
	for field, v := range checks {
		if v.got != v.want {
			t.Errorf("%s = %q, want %q", field, v.got, v.want)
		}
	}
	if !c.Direct {
		t.Error("Direct = false, want true")
	}
	if c.IdentityConfidence != 0.98 {
		t.Errorf("IdentityConfidence = %v, want 0.98", c.IdentityConfidence)
	}
	if c.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", c.CostUSD)
	}
	// Discovered metadata.
	if c.ResolvedWork.DOI != "10.1000/xyz123" {
		t.Errorf("ResolvedWork.DOI = %q, want 10.1000/xyz123", c.ResolvedWork.DOI)
	}
	if c.ResolvedWork.Year != 2017 {
		t.Errorf("ResolvedWork.Year = %d, want 2017", c.ResolvedWork.Year)
	}
	if got := strings.Join(c.ResolvedWork.Authors, ","); got != "Ashish Vaswani,Noam Shazeer" {
		t.Errorf("ResolvedWork.Authors = %q", got)
	}
	if !hasEvidence(c.Evidence, "arxiv published_as=NIPS 2017") {
		t.Errorf("evidence missing published_as: %v", c.Evidence)
	}
	// ResolvedWork is not persisted as evidence: the discovered DOI must not
	// leak into an evidence line (a url= line may legitimately embed it).
	for _, e := range c.Evidence {
		if strings.Contains(e, "10.1000/xyz123") && !strings.Contains(e, "url=") {
			t.Errorf("DOI leaked into evidence line: %q", e)
		}
	}
}

func TestPreprintSemanticsWithoutJournalRef(t *testing.T) {
	body := atomEntryXML("2101.00002v1", "A Preprint", "")
	srv := serveAtom(t, http.StatusOK, nil, body, nil)
	r := newResolver(srv)
	cands, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00002"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(cands) != 1 || cands[0].Version != resolver.VersionPreprint {
		t.Fatalf("want single preprint candidate, got %+v", cands)
	}
	if cands[0].ResolvedWork.DOI != "" {
		t.Errorf("ResolvedWork.DOI = %q, want empty (no DOI in feed)", cands[0].ResolvedWork.DOI)
	}
}

func TestEmptyResults(t *testing.T) {
	emptyFeed := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`
	errorFeed := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><id>http://arxiv.org/api/errors#missing</id><title>Error</title></entry></feed>`
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"no entries", http.StatusOK, emptyFeed},
		{"error entry", http.StatusOK, errorFeed},
		{"http 404", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveAtom(t, tc.status, nil, tc.body, nil)
			r := newResolver(srv)
			cands, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
			if err != nil {
				t.Fatalf("want nil error, got %v", err)
			}
			if cands != nil {
				t.Fatalf("want nil candidates, got %v", cands)
			}
		})
	}
}

func TestNoArxivIdentitySkipsNetwork(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	t.Cleanup(srv.Close)
	r := newResolver(srv)
	cands, err := r.Resolve(context.Background(), work.Work{DOI: "10.1000/not-arxiv"})
	if err != nil || cands != nil {
		t.Fatalf("want nil,nil for non-arxiv work; got %v, %v", cands, err)
	}
	if hit {
		t.Error("resolver contacted the API for a work with no arXiv identity")
	}
}

func TestMalformedAndOversizedPayload(t *testing.T) {
	t.Run("malformed xml", func(t *testing.T) {
		srv := serveAtom(t, http.StatusOK, nil, "<feed><entry>unterminated", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
		if err == nil {
			t.Fatal("want error for malformed XML")
		}
		if _, temp := resolver.Temporary(err); temp {
			t.Error("malformed payload must not be a temporary error")
		}
	})
	t.Run("oversized bounded read", func(t *testing.T) {
		big := atomEntryXML("2101.00001v1", strings.Repeat("A", 8192), "")
		srv := serveAtom(t, http.StatusOK, nil, big, nil)
		r := NewWithOptions(Options{Client: srv.Client(), BaseURL: srv.URL, MaxResponseBytes: 256})
		_, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
		if err == nil {
			t.Fatal("want error when response exceeds the size limit")
		}
		if !strings.Contains(err.Error(), "size limit") {
			t.Errorf("error = %v, want a size-limit rejection", err)
		}
		if _, temp := resolver.Temporary(err); temp {
			t.Error("oversized payload must not be a temporary error")
		}
	})
}

func TestRetryClasses(t *testing.T) {
	t.Run("429 with Retry-After seconds", func(t *testing.T) {
		srv := serveAtom(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "12"}, "", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
		d, temp := resolver.Temporary(err)
		if !temp {
			t.Fatalf("429 must be temporary; err=%v", err)
		}
		if d.Seconds() != 12 {
			t.Errorf("RetryAfter = %v, want 12s", d)
		}
	})
	t.Run("503 temporary", func(t *testing.T) {
		srv := serveAtom(t, http.StatusServiceUnavailable, nil, "", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
		if _, temp := resolver.Temporary(err); !temp {
			t.Fatalf("503 must be temporary; err=%v", err)
		}
	})
	t.Run("403 permanent", func(t *testing.T) {
		srv := serveAtom(t, http.StatusForbidden, nil, "", nil)
		r := newResolver(srv)
		_, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
		if err == nil {
			t.Fatal("403 must produce an error")
		}
		if _, temp := resolver.Temporary(err); temp {
			t.Error("403 must not be temporary")
		}
	})
	t.Run("network failure temporary", func(t *testing.T) {
		srv := serveAtom(t, http.StatusOK, nil, "", nil)
		client := srv.Client()
		base := srv.URL
		srv.Close() // force a connection error
		r := NewWithOptions(Options{Client: client, BaseURL: base})
		_, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
		if _, temp := resolver.Temporary(err); !temp {
			t.Fatalf("network failure must be temporary; err=%v", err)
		}
	})
}

func TestRedactedURLHasNoQueryOrFragment(t *testing.T) {
	body := atomEntryXML("2101.00001v1", "T", "")
	srv := serveAtom(t, http.StatusOK, nil, body, nil)
	r := newResolver(srv)
	cands, err := r.Resolve(context.Background(), work.Work{ArXiv: "2101.00001"})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Resolve: %v, %v", cands, err)
	}
	red := redact.URL(cands[0].URL)
	if strings.ContainsAny(red, "?#") {
		t.Errorf("redacted URL %q contains query/fragment", red)
	}
	if !hasEvidence(cands[0].Evidence, "arxiv url="+red) {
		t.Errorf("evidence should carry the redacted URL; got %v", cands[0].Evidence)
	}
}

func hasEvidence(evidence []string, want string) bool {
	for _, e := range evidence {
		if e == want {
			return true
		}
	}
	return false
}

// TestParseRetryAfterClampsHugeValues pins the overflow-safe Retry-After
// parsing: a header large enough to overflow the nanosecond multiply must clamp
// to the max duration rather than wrap to a garbage (possibly negative) value.
func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	now := time.Now()
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"garbage", "not-a-number", 0},
		{"normal seconds", "5", 5 * time.Second},
		{"overflow multiply clamps to max", "99999999999", maxDuration},
		{"beyond int64 range falls through", "9999999999999999999", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRetryAfter(c.value, now)
			if got != c.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", c.value, got, c.want)
			}
			if got < 0 {
				t.Errorf("parseRetryAfter(%q) = %v, must never be negative", c.value, got)
			}
		})
	}
}
