// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"papio/internal/work"
)

// PageState is one backend's completeness signal for one page of a paged
// search. It exists so that "this backend has nothing more" can never be
// confused with "this backend did not answer" or "this backend cannot page":
// a scheduled scan that reads all three as "no new results" quietly stops
// delivering papers while every run reports success.
type PageState string

const (
	// PageMore means the backend has further results; Next resumes after
	// this page.
	PageMore PageState = "more"
	// PageExhausted means the backend reported the end of its result set.
	// Next is empty.
	PageExhausted PageState = "exhausted"
	// PageTruncated means the backend holds further results but refuses to
	// page past a fixed result window (Semantic Scholar relevance search,
	// arXiv). Those results are unreachable by paging, so this is not
	// exhaustion. Next is empty.
	PageTruncated PageState = "truncated"
	// PageFailed means the backend did not answer this page. Only Multi
	// reports it, per source; a single backend's SearchPage returns an
	// error instead. Next echoes the token the caller passed, so persisting
	// it keeps the caller's position.
	PageFailed PageState = "failed"
	// PageUnsupported means the backend cannot page. Works holds its single
	// page and Next is empty. Only Multi reports it, for a Source that does
	// not implement PageSearcher.
	PageUnsupported PageState = "unsupported"
)

// ErrInvalidPageToken marks a page token that is malformed, was issued by a
// different backend, or was issued for a different search. Paging never
// silently restarts from the first page on a bad token: a caller that wants
// to start over must pass an empty token deliberately.
var ErrInvalidPageToken = errors.New("discovery: invalid page token")

// Page is one page of results from one backend.
type Page struct {
	Works []DiscoveredWork
	// State is PageMore, PageExhausted, or PageTruncated.
	State PageState
	// Next is the opaque continuation token, set only when State is PageMore.
	Next string
}

// PageSearcher is a Source that can resume a search from an opaque
// continuation token. An empty token requests the first page. The page size
// is params.Limit, clamped to MaxLimit like Search. Each call makes exactly
// one bounded search request through the backend's budget-gated HTTP client
// (OpenAlex adds one seed lookup per citation DOI on the first page only), so
// a budget refusal surfaces as an error rather than as an empty page.
//
// Tokens are serializable strings meant to be persisted by the caller. They
// bind the backend name and the result-selecting search parameters (query,
// year bounds, OA-only, citation seeds) but not the page size, so a caller may
// change the page size between calls. A token that fails validation returns an
// error wrapping ErrInvalidPageToken without any request being made.
type PageSearcher interface {
	SearchPage(ctx context.Context, params SearchParams, token string) (Page, error)
}

// Compile-time assertions: every shipped backend pages. Multi reaches them
// only by type-assertion, so a signature drift would otherwise silently
// downgrade a backend to PageUnsupported.
var (
	_ PageSearcher = (*Client)(nil)
	_ PageSearcher = (*Arxiv)(nil)
	_ PageSearcher = (*SemanticScholar)(nil)
)

const (
	// pageTokenPrefix versions the token encoding. A change to the payload
	// shape must change the prefix so an older persisted token fails closed.
	pageTokenPrefix = "dp1."
	// maxPageTokenBytes bounds the persisted token and what decoding accepts.
	maxPageTokenBytes = 2048
)

// pageToken is the decoded continuation. Offset-paged backends (arXiv,
// Semantic Scholar) use Offset; OpenAlex uses Cursor and carries its resolved
// citation seeds so later pages skip the DOI lookup.
type pageToken struct {
	Backend     string            `json:"b"`
	Fingerprint string            `json:"f"`
	Offset      int               `json:"o,omitempty"`
	Cursor      string            `json:"c,omitempty"`
	Seeds       map[string]string `json:"s,omitempty"`
}

func encodePageToken(token pageToken) string {
	// Marshal cannot fail: the payload holds only strings, an int, and a
	// string-keyed map of strings.
	data, _ := json.Marshal(token)
	return pageTokenPrefix + base64.RawURLEncoding.EncodeToString(data)
}

// decodePageToken validates the parts every backend shares: encoding, owner,
// and that the token belongs to this exact search. Backend-specific fields are
// validated by the caller.
func decodePageToken(raw, backend string, params SearchParams) (pageToken, error) {
	if len(raw) > maxPageTokenBytes {
		return pageToken{}, fmt.Errorf("%w: token exceeds %d bytes", ErrInvalidPageToken, maxPageTokenBytes)
	}
	encoded, ok := strings.CutPrefix(raw, pageTokenPrefix)
	if !ok {
		return pageToken{}, fmt.Errorf("%w: unrecognized token format", ErrInvalidPageToken)
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return pageToken{}, fmt.Errorf("%w: token is not base64url: %w", ErrInvalidPageToken, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var token pageToken
	if err := decoder.Decode(&token); err != nil {
		return pageToken{}, fmt.Errorf("%w: token payload: %w", ErrInvalidPageToken, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return pageToken{}, fmt.Errorf("%w: token payload has trailing data", ErrInvalidPageToken)
	}
	if token.Backend != backend {
		return pageToken{}, fmt.Errorf("%w: token was issued by %q, not %q", ErrInvalidPageToken, token.Backend, backend)
	}
	if token.Fingerprint != searchFingerprint(params) {
		return pageToken{}, fmt.Errorf("%w: token was issued for a different search", ErrInvalidPageToken)
	}
	return token, nil
}

// requireOffset validates an offset-paged token. window is the backend's
// fixed result window, or zero when it has none; a token at or past the window
// was never issued, because that page reports PageTruncated instead.
func (token pageToken) requireOffset(window int) error {
	if token.Cursor != "" || token.Seeds != nil {
		return fmt.Errorf("%w: %s tokens carry only an offset", ErrInvalidPageToken, token.Backend)
	}
	if token.Offset <= 0 || (window > 0 && token.Offset >= window) {
		return fmt.Errorf("%w: offset %d is out of range", ErrInvalidPageToken, token.Offset)
	}
	return nil
}

// offsetPageToken issues the continuation for an offset-paged backend.
func offsetPageToken(backend string, params SearchParams, offset int) string {
	return encodePageToken(pageToken{Backend: backend, Fingerprint: searchFingerprint(params), Offset: offset})
}

// searchFingerprint identifies the result set a token walks. It covers every
// parameter that changes which works match or their order, and deliberately
// excludes the page size, Slim, and the Source selector.
func searchFingerprint(params SearchParams) string {
	key := struct {
		Query     string `json:"q"`
		YearFrom  int    `json:"yf"`
		YearTo    int    `json:"yt"`
		OAOnly    bool   `json:"oa"`
		Cites     string `json:"c"`
		CitedBy   string `json:"cb"`
		RelatedTo string `json:"r"`
	}{
		Query:     strings.Join(strings.Fields(params.Query), " "),
		YearFrom:  params.YearFrom,
		YearTo:    params.YearTo,
		OAOnly:    params.OAOnly,
		Cites:     fingerprintDOI(params.Cites),
		CitedBy:   fingerprintDOI(params.CitedBy),
		RelatedTo: fingerprintDOI(params.RelatedTo),
	}
	data, _ := json.Marshal(key)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func fingerprintDOI(value string) string {
	if normalized, err := work.NormalizeDOI(value); err == nil {
		return normalized
	}
	return strings.TrimSpace(value)
}

// pageSize is the per-call page size: params.Limit clamped exactly as Search
// clamps it, so no single page request exceeds MaxLimit.
func pageSize(params SearchParams) int {
	return normalizeParams(params).Limit
}
