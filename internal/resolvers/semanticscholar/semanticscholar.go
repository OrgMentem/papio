// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package semanticscholar resolves open-access PDF locations from the
// Semantic Scholar Graph API. It consumes the same provider the discovery
// backend queries, but through the acquisition resolver contract: exact
// identifier lookup only, and a candidate exists only when the record
// carries a usable openAccessPdf URL — isOpenAccess alone is metadata,
// not a candidate.
package semanticscholar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL = "https://api.semanticscholar.org/graph/v1"
	defaultMaxBody = int64(1 << 20)

	// paperFields is the bounded field set the lookup requests: identity
	// evidence, the OA PDF location, and the paper page for a landing URL.
	paperFields = "externalIds,title,year,authors,venue,isOpenAccess,openAccessPdf,url"
)

// HTTPClient is the injected HTTP dependency used to call Semantic Scholar.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. BaseURL is the Graph API root and is
// intended for tests or explicitly configured development endpoints.
type Options struct {
	Client           HTTPClient
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver using Semantic Scholar's exact
// identifier lookup (DOI, arXiv, or PMID — never a title search: a weak
// title match must not become an automatic candidate).
type Resolver struct {
	client  HTTPClient
	apiKey  string
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a resolver with the official Graph API endpoint.
func New(client HTTPClient, apiKey string) *Resolver {
	return NewWithOptions(Options{Client: client, APIKey: apiKey})
}

// NewWithOptions constructs a resolver with injected dependencies.
func NewWithOptions(opts Options) *Resolver {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	maxBody := opts.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	return &Resolver{
		client:  opts.Client,
		apiKey:  strings.TrimSpace(opts.APIKey),
		baseURL: baseURL,
		maxBody: maxBody,
	}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "semanticscholar" }

// Resolve looks the work up by its strongest exact identifier and returns at
// most one open-access candidate. Absence of an identifier, an unknown work,
// and a record without a usable OA PDF URL are all (nil, nil).
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil {
		return nil, errors.New("semanticscholar: HTTP client is not configured")
	}
	ref, ok := paperRef(requested)
	if !ok {
		return nil, nil
	}
	endpoint, err := r.endpointURL(ref)
	if err != nil {
		return nil, errors.New("semanticscholar: invalid endpoint configuration")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("semanticscholar: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("x-api-key", r.apiKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &resolver.TemporaryError{Err: errors.New("semanticscholar: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("semanticscholar: empty HTTP response")}
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("semanticscholar: request was rejected (check the configured api_key)")
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return nil, temporaryStatus(resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		return nil, temporaryStatus(resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("semanticscholar: unexpected HTTP status %d", resp.StatusCode)
	}
	if resp.Body == nil {
		return nil, errors.New("semanticscholar: response body is missing")
	}

	var record paper
	if err := decodeBoundedJSON(resp.Body, r.maxBody, &record); err != nil {
		return nil, fmt.Errorf("semanticscholar: invalid response: %w", err)
	}
	if conflictsWithRequest(record, requested) {
		return nil, nil
	}
	pdfURL := ""
	if record.OpenAccessPDF != nil {
		pdfURL = strings.TrimSpace(record.OpenAccessPDF.URL)
	}
	if !validHTTPURL(pdfURL) {
		// A publicly reachable record without a PDF URL is metadata only.
		return nil, nil
	}

	landing := strings.TrimSpace(record.URL)
	if !validHTTPURL(landing) {
		landing = pdfURL
	}
	license := "unknown"
	status := "unknown"
	if record.OpenAccessPDF != nil {
		license = reuseLicense(record.OpenAccessPDF.License)
		if s := strings.TrimSpace(record.OpenAccessPDF.Status); s != "" {
			status = safeEvidenceValue(s)
		}
	}

	candidate := resolver.Candidate{
		Source:       "semanticscholar",
		URL:          pdfURL,
		Landing:      landing,
		Version:      resolver.VersionUnknown,
		AccessBasis:  resolver.AccessOpen,
		ReuseLicense: license,
		ExpectedMIME: "application/pdf",
		Direct:       true,
		// The lookup was by exact identifier and the echoed identifiers do
		// not conflict, so identity confidence is the identifier's.
		IdentityConfidence: 1,
		ResolvedWork:       resolvedWork(record),
		Evidence: []string{
			"semanticscholar lookup=" + ref.evidence,
			"semanticscholar oa_status=" + status,
			"semanticscholar url=" + redact.URL(pdfURL),
		},
	}
	if err := resolver.ValidateCandidate(candidate); err != nil {
		return nil, nil
	}
	return []resolver.Candidate{candidate}, nil
}

// ref names one exact-identifier lookup path.
type ref struct {
	path     string // URL path segment after /paper/
	evidence string // scheme-only evidence label; never the raw value
}

// paperRef picks the strongest exact identifier the Graph API can look up.
func paperRef(requested work.Work) (ref, bool) {
	if doi, err := work.NormalizeDOI(requested.DOI); err == nil {
		return ref{path: "DOI:" + doi, evidence: "doi"}, true
	}
	if arxiv, err := work.NormalizeArXiv(requested.ArXiv); err == nil {
		return ref{path: "arXiv:" + arxiv, evidence: "arxiv"}, true
	}
	if pmid, err := work.NormalizePMID(requested.PMID); err == nil {
		return ref{path: "PMID:" + pmid, evidence: "pmid"}, true
	}
	return ref{}, false
}

func (r *Resolver) endpointURL(lookup ref) (*url.URL, error) {
	base, err := url.Parse(r.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("invalid base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/paper/" + lookup.path
	values := base.Query()
	values.Set("fields", paperFields)
	base.RawQuery = values.Encode()
	return base, nil
}

type paper struct {
	ExternalIDs struct {
		DOI    string `json:"DOI"`
		ArXiv  string `json:"ArXiv"`
		PubMed string `json:"PubMed"`
	} `json:"externalIds"`
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Venue         string `json:"venue"`
	URL           string `json:"url"`
	IsOpenAccess  bool   `json:"isOpenAccess"`
	OpenAccessPDF *struct {
		URL     string `json:"url"`
		Status  string `json:"status"`
		License string `json:"license"`
	} `json:"openAccessPdf"`
}

// conflictsWithRequest reports whether the record's echoed identifiers name a
// different work than the request. Only a definite mismatch counts: an absent
// value on either side is not a conflict, and the arXiv comparison collapses
// the version suffix because Semantic Scholar echoes the versionless id.
func conflictsWithRequest(record paper, requested work.Work) bool {
	if reqDOI, err := work.NormalizeDOI(requested.DOI); err == nil {
		if gotDOI, err := work.NormalizeDOI(record.ExternalIDs.DOI); err == nil && gotDOI != reqDOI {
			return true
		}
	}
	if reqArXiv, err := work.NormalizeArXiv(requested.ArXiv); err == nil {
		if gotArXiv, err := work.NormalizeArXiv(record.ExternalIDs.ArXiv); err == nil &&
			stripArXivVersion(gotArXiv) != stripArXivVersion(reqArXiv) {
			return true
		}
	}
	if reqPMID, err := work.NormalizePMID(requested.PMID); err == nil {
		if gotPMID, err := work.NormalizePMID(record.ExternalIDs.PubMed); err == nil && gotPMID != reqPMID {
			return true
		}
	}
	return false
}

// stripArXivVersion collapses an explicit vN suffix for the conflict check
// only. Acquisition identifiers stay version-preserving everywhere else.
func stripArXivVersion(id string) string {
	i := strings.LastIndexByte(id, 'v')
	if i <= 0 {
		return id
	}
	suffix := id[i+1:]
	if suffix == "" {
		return id
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return id
		}
	}
	return id[:i]
}

func resolvedWork(record paper) work.Work {
	authors := make([]string, 0, len(record.Authors))
	for _, author := range record.Authors {
		if name := strings.TrimSpace(author.Name); name != "" {
			authors = append(authors, name)
		}
	}
	resolved := work.Work{
		Title:     strings.TrimSpace(record.Title),
		Authors:   authors,
		Year:      record.Year,
		Container: strings.TrimSpace(record.Venue),
	}
	if doi, err := work.NormalizeDOI(record.ExternalIDs.DOI); err == nil {
		resolved.DOI = doi
	}
	if arxiv, err := work.NormalizeArXiv(record.ExternalIDs.ArXiv); err == nil {
		resolved.ArXiv = arxiv
	}
	if pmid, err := work.NormalizePMID(record.ExternalIDs.PubMed); err == nil {
		resolved.PMID = pmid
	}
	return resolved
}

func reuseLicense(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	return value
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func safeEvidenceValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
}

func decodeBoundedJSON(body io.Reader, max int64, destination any) error {
	payload, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return err
	}
	if int64(len(payload)) > max {
		return errors.New("response exceeds the configured size limit")
	}
	return json.Unmarshal(payload, destination)
}

func temporaryStatus(resp *http.Response) error {
	wait := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return &resolver.TemporaryError{
		Err:        fmt.Errorf("semanticscholar: returned HTTP %d", resp.StatusCode),
		RetryAfter: wait,
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		const maxDuration = time.Duration(1<<63 - 1)
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return time.Until(when)
	}
	return 0
}
