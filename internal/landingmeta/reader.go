// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package landingmeta

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
)

// readerUserAgent identifies this fetch to the publisher landing page. It is
// not configurable per call: PDFURLFor never forwards a caller's own headers
// (see the comment on that method), so this is the only identity a landing
// server ever sees from it.
const readerUserAgent = "papio/0.1 (legitimate research acquisition; mailto:unset)"

// HTTPClient is the subset of an HTTP client PDFURLFor needs. It matches
// both *fetch.SecureHTTPClient and *http.Client — the same shim doiregistry,
// discovery, core and crossreftdm already use — so a caller wires the SAME
// SSRF-guarded client instance used for metadata resolution into this
// reader instead of standing up a second one.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Reader fetches a landing page over HTTPS and hands its bytes to PDFURL.
// It owns exactly the guard rails that make fetching attacker-influenced
// HTML from the open internet safe to feed into a download pipeline:
// HTTPS-only, no caller credentials forwarded, a hard cap on bytes actually
// read regardless of what the server claims to be sending, and resolving
// against the post-redirect URL rather than the one the caller asked for.
type Reader struct {
	client   HTTPClient
	maxBytes int64
}

// NewReader constructs a Reader. client is expected to already carry the
// project's SSRF-guarded transport and redirect policy (fetch.SecureHTTPClient,
// or a plain *http.Client in tests) — Reader does not install its own
// CheckRedirect and relies entirely on the injected client for that.
// maxBytes bounds how much of a landing page's (decompressed) body is ever
// held in memory.
func NewReader(client HTTPClient, maxBytes int64) *Reader {
	return &Reader{client: client, maxBytes: maxBytes}
}

// PDFURLFor fetches landingURL and returns the citation_pdf_url it
// advertises. Returns ("", nil) when the page advertises none, when
// landingURL itself is unusable (not https, unparseable, non-HTML,
// non-2xx), or when the page could only be read up to the byte cap — none
// of those are the caller's problem, they just mean "no PDF found here." A
// genuine transport failure (DNS, TLS, connection refused, ...) returns a
// non-nil error; a conflicting pair of citation_pdf_url tags on the page
// returns ErrConflictingPDFURL, unchanged from PDFURL.
func (r *Reader) PDFURLFor(ctx context.Context, landingURL string) (string, error) {
	parsed, err := url.Parse(landingURL)
	if err != nil || parsed.Scheme != "https" {
		// Candidate rows in this system carry landing_redacted URLs from
		// Unpaywall/OpenAlex/Crossref, not user input, but "not an error
		// worth propagating" is the whole point: a malformed or plaintext
		// landing URL just means expansion has nothing to offer, the same
		// as a page with no citation_pdf_url tag at all.
		return "", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, landingURL, nil)
	if err != nil {
		return "", nil
	}
	// No caller headers, ever. The candidate that produced this landing URL
	// may carry a bearer token, a signed-URL query string, or other
	// source-specific credentials (that's exactly how the motivating
	// incident's Azure blob link died — a SAS signature, se=2021-02-16,
	// expired). Forwarding any of that to whatever origin the landing page
	// happens to live on would be a cross-origin credential leak. This
	// request carries only the two headers below and nothing the caller
	// supplied.
	req.Header.Set("User-Agent", readerUserAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("landingmeta: fetch %s: %w", landingURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
			// A PDF or an image is not a landing page — and not reading the
			// body at all is what lets the "PDF Content-Type" test assert
			// the server never streamed one to us just to have it thrown
			// away.
			return "", nil
		}
	}

	// Reject on the declared size before reading anything: a landing page
	// that already claims to be bigger than maxBytes isn't worth the round
	// trip. This is a fast path, not the real defense — a gzip-compressing
	// origin reports the compressed Content-Length (or none at all, with Go's
	// transport transparently inflating the body and reporting -1), so a
	// decompression bomb sails straight past this check. The io.LimitReader
	// below is what actually bounds memory in that case.
	if resp.ContentLength > 0 && resp.ContentLength > r.maxBytes {
		return "", nil
	}

	// citation_pdf_url lives in <head>, and PDFURL stops scanning at </head>
	// regardless, so truncating here at maxBytes never costs us the tag —
	// it only ever cuts off body content PDFURL wasn't going to look at.
	// Read errors (a connection dropped mid-response) are treated the same
	// way as hitting the cap: parse whatever bytes arrived and return
	// whatever they advertise, rather than failing a fetch that may have
	// already delivered the one tag we needed.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, r.maxBytes))

	// Resolve against the FINAL URL, not landingURL. resp.Request.URL is the
	// URL of the last request actually sent, i.e. post-redirect. This is
	// the detail the motivating incident hinges on: landing_redacted is a
	// https://doi.org/10.3389/... URL, and doi.org 302s to the publisher's
	// actual article page. A relative citation_pdf_url on that page means
	// "relative to the Frontiers article path" — resolving it against
	// doi.org instead would silently produce a doi.org URL that 404s.
	base := landingURL
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL.String()
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		baseURL = parsed
	}

	return PDFURL(body, baseURL)
}
