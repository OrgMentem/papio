// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package landingmeta

import (
	"context"
	"errors"
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
		_, _ = io.WriteString(w, `<html><head><meta name="citation_pdf_url" content="paper.pdf"></head><body></body></html>`)
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

// cappedBody is a response body that both reports how many of its bytes were
// ever handed out and fails the test the instant a read pushes that total
// past the cap. It stands in for a decompression bomb: an unknown-length
// body (ContentLength -1, exactly what Go's transport reports for a response
// it transparently gunzipped) whose real size dwarfs maxBytes.
type cappedBody struct {
	t     *testing.T
	src   io.Reader
	limit int64
	read  int64
	over  bool
}

func (b *cappedBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	b.read += int64(n)
	if b.read > b.limit && !b.over {
		b.over = true
		b.t.Errorf("body handed out %d bytes with a %d-byte cap: the io.LimitReader bound in PDFURLFor is gone", b.read, b.limit)
		return n, errors.New("cappedBody: read past the cap")
	}
	return n, err
}

func (b *cappedBody) Close() error { return nil }

// TestPDFURLForTruncatesLargeBodyButStillFindsHeadTag defends the
// io.LimitReader cap: a body far larger than maxBytes must not be read in
// full, yet citation_pdf_url — which lives in <head>, ahead of the huge
// filler below — must still be found in the truncated prefix. The body is
// injected rather than served so the test can observe the bytes actually
// read, which is the half a URL-only assertion left undefended.
func TestPDFURLForTruncatesLargeBodyButStillFindsHeadTag(t *testing.T) {
	const (
		maxBytes = 512
		want     = "https://cap.example.test/head/paper.pdf"
	)
	head := `<html><head><meta name="citation_pdf_url" content="` + want + `"></head><body>`
	if int64(len(head)) > maxBytes {
		t.Fatalf("head prefix is %d bytes, it must fit inside the %d-byte cap", len(head), maxBytes)
	}

	body := &cappedBody{
		t:     t,
		limit: maxBytes,
		src:   strings.NewReader(head + strings.Repeat("x", 4<<20)), // 4 MiB, far past the cap
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		// Unknown length, so the declared-size fast path cannot fire: this
		// test is about the cap on the actual read, not that check.
		ContentLength: -1,
		Body:          body,
	}
	client := doFunc(func(req *http.Request) (*http.Response, error) {
		resp.Request = req
		return resp, nil
	})

	reader := NewReader(client, maxBytes)
	got, err := reader.PDFURLFor(context.Background(), "https://cap.example.test/landing")
	if err != nil {
		t.Fatalf("PDFURLFor: %v", err)
	}
	if got != want {
		t.Fatalf("PDFURLFor = %q, want %q", got, want)
	}
	// Equality, not an upper bound: `<=` also passes for a reader that
	// truncates EARLY (io.LimitReader(resp.Body, maxBytes-1)), because the
	// head tag fits well inside the cap either way. The whole legal prefix
	// must be consumed, and not one byte more.
	if body.read != maxBytes {
		t.Fatalf("PDFURLFor read %d body bytes, want exactly %d: the cap must bound the read without shortening the legal prefix", body.read, maxBytes)
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
		_, _ = io.WriteString(w, `<html><head><meta name="citation_pdf_url" content="https://cap.example.test/paper.pdf"></head></html>`)
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

func TestPDFURLForReturnsErrorOnNilResponse(t *testing.T) {
	client := doFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	})
	reader := NewReader(client, 1<<20)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PDFURLFor panicked on (nil, nil): %v", r)
		}
	}()
	_, err := reader.PDFURLFor(context.Background(), "https://example.test/landing")
	if err == nil {
		t.Fatal("want error for (nil, nil), got nil")
	}
	if !strings.Contains(err.Error(), "empty HTTP response") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "empty HTTP response")
	}
}

func TestPDFURLForReturnsErrorOnNilBodyWithoutPanicking(t *testing.T) {
	client := doFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: nil, Header: make(http.Header)}, nil
	})
	reader := NewReader(client, 1<<20)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PDFURLFor panicked on nil Body: %v", r)
		}
	}()
	_, err := reader.PDFURLFor(context.Background(), "https://example.test/landing")
	if err == nil {
		t.Fatal("want error for nil response body, got nil")
	}
	if !strings.Contains(err.Error(), "response body is missing") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "response body is missing")
	}
}
