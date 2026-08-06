// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package landingmeta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// doFunc adapts a plain function to the HTTPClient interface for tests that
// need to assert a call was (or wasn't) made, without a real network hop.
type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestPDFURLForResolvesAgainstFinalURLNotRedirector is the motivating
// incident in miniature: /doi 302s to /publisher/article, whose page
// carries a RELATIVE citation_pdf_url. Resolving that against the
// redirector (/doi, which has no path segment to drop) would silently
// produce a different, wrong URL instead of the publisher's actual PDF path.
func TestPDFURLForResolvesAgainstFinalURLNotRedirector(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/doi", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/publisher/article", http.StatusFound)
	})
	mux.HandleFunc("/publisher/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><meta name="citation_pdf_url" content="paper.pdf"></head><body></body></html>`)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	reader := NewReader(server.Client(), 1<<20)
	got, err := reader.PDFURLFor(context.Background(), server.URL+"/doi")
	if err != nil {
		t.Fatalf("PDFURLFor: %v", err)
	}
	if want := server.URL + "/publisher/paper.pdf"; got != want {
		t.Fatalf("PDFURLFor = %q, want %q (resolved against the redirector instead of the final URL)", got, want)
	}
}

// TestPDFURLForSkipsNonHTMLContentTypeWithoutReadingBody covers a fetch
// landing on the PDF itself (or an image) rather than an HTML page. The
// handler flushes headers and then blocks on the response body forever; if
// PDFURLFor ever tried to read that body, the request context deadline
// below — not "no PDF found" — would be what ends the test.
func TestPDFURLForSkipsNonHTMLContentTypeWithoutReadingBody(t *testing.T) {
	block := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/report.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block // never sent to until the deferred close below
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	defer close(block)

	reader := NewReader(server.Client(), 1<<20)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := reader.PDFURLFor(ctx, server.URL+"/report.pdf")
	if err != nil || got != "" {
		t.Fatalf("PDFURLFor = (%q, %v), want (\"\", nil) — a non-HTML Content-Type must short-circuit before the body is read", got, err)
	}
}

// TestPDFURLForTruncatesLargeBodyButStillFindsHeadTag defends the
// io.LimitReader cap: a body far larger than maxBytes must not be read in
// full, yet citation_pdf_url — which lives in <head>, ahead of the huge
// filler below — must still be found in the truncated prefix.
func TestPDFURLForTruncatesLargeBodyButStillFindsHeadTag(t *testing.T) {
	const want = "https://cap.example.test/head/paper.pdf"
	mux := http.NewServeMux()
	mux.HandleFunc("/landing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `<html><head><meta name="citation_pdf_url" content="`+want+`"></head><body>`)
		if f, ok := w.(http.Flusher); ok {
			// Force chunked transfer (no Content-Length) so the fast-path
			// declared-size check below never fires — this test is about
			// the io.LimitReader cap on the actual read, not that check.
			f.Flush()
		}
		io.WriteString(w, strings.Repeat("x", 4<<20)) // 4 MiB, far past the 512-byte cap
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	reader := NewReader(server.Client(), 512)
	got, err := reader.PDFURLFor(context.Background(), server.URL+"/landing")
	if err != nil {
		t.Fatalf("PDFURLFor: %v", err)
	}
	if got != want {
		t.Fatalf("PDFURLFor = %q, want %q", got, want)
	}
}

// TestPDFURLForRejectsNonHTTPSLandingURL asserts the guard rail short-
// circuits before any network I/O: a stub client that would fail the test
// if it were ever invoked proves a plain http:// URL never reaches Do.
func TestPDFURLForRejectsNonHTTPSLandingURL(t *testing.T) {
	client := doFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Do called for a non-https landing URL")
		return nil, nil
	})
	reader := NewReader(client, 1<<20)

	got, err := reader.PDFURLFor(context.Background(), "http://example.test/landing")
	if err != nil || got != "" {
		t.Fatalf("PDFURLFor = (%q, %v), want (\"\", nil)", got, err)
	}
}

// TestPDFURLForSendsNoCallerCredentialHeaders checks the outgoing request
// PDFURLFor actually sends. There is no parameter through which a caller
// could inject Authorization/Cookie/Proxy-Authorization in the first place
// (the guarantee is the method signature), but this pins that the two
// headers PDFURLFor does set — User-Agent and Accept — never grow into a
// credential leak by accident.
func TestPDFURLForSendsNoCallerCredentialHeaders(t *testing.T) {
	var captured http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/landing", func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><meta name="citation_pdf_url" content="https://cap.example.test/paper.pdf"></head></html>`)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	reader := NewReader(server.Client(), 1<<20)
	if _, err := reader.PDFURLFor(context.Background(), server.URL+"/landing"); err != nil {
		t.Fatalf("PDFURLFor: %v", err)
	}

	for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
		if v := captured.Get(h); v != "" {
			t.Fatalf("outgoing request carried %s: %q", h, v)
		}
	}
}

// TestPDFURLForReturnsNilOnNotFound covers the ordinary dead-link case: a
// 404 is not a transport failure, just a page with nothing to advertise.
func TestPDFURLForReturnsNilOnNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	reader := NewReader(server.Client(), 1<<20)
	got, err := reader.PDFURLFor(context.Background(), server.URL+"/gone")
	if err != nil || got != "" {
		t.Fatalf("PDFURLFor = (%q, %v), want (\"\", nil)", got, err)
	}
}
