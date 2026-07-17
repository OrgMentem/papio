// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package arxiv resolves arXiv works to their direct open-access PDF. arXiv is
// an identifier-native open source: given an arXiv id (or the DataCite
// 10.48550/arXiv DOI form) the canonical PDF URL is known without a network
// call, so the export.arxiv.org Atom API is queried only to confirm the work
// exists and to gather bibliographic identity evidence. A requested version
// (vN) is preserved as identity evidence but stripped from the canonical
// abs/pdf URLs, which always point at the latest version.
package arxiv

import (
	"context"
	"encoding/xml"
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
	defaultBaseURL = "https://export.arxiv.org"
	defaultMaxBody = int64(1 << 20)

	// Canonical user-facing hosts for the emitted candidate. These are never
	// the (possibly test-injected) API host: the candidate URL is the live
	// arxiv.org file, the API base is only where metadata is fetched.
	absBase = "https://arxiv.org/abs/"
	pdfBase = "https://arxiv.org/pdf/"
)

// HTTPClient is the injected HTTP dependency used to call the arXiv Atom API.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. BaseURL overrides the export.arxiv.org root
// for tests or explicitly configured development endpoints.
type Options struct {
	Client           HTTPClient
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver for arXiv.
type Resolver struct {
	client  HTTPClient
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a resolver with the official export.arxiv.org endpoint.
func New(client HTTPClient) *Resolver {
	return NewWithOptions(Options{Client: client})
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
	return &Resolver{client: opts.Client, baseURL: baseURL, maxBody: maxBody}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "arxiv" }

// Resolve returns the direct OA PDF candidate for an arXiv work, or (nil, nil)
// when the request carries no arXiv identity or the work is not found.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil {
		return nil, errors.New("arxiv: HTTP client is not configured")
	}

	rawID := strings.TrimSpace(requested.ArXiv)
	if rawID == "" && strings.TrimSpace(requested.DOI) != "" {
		if doi, err := work.NormalizeDOI(requested.DOI); err == nil {
			rawID = work.ArXivFromDOI(doi)
		}
	}
	if rawID == "" {
		return nil, nil
	}
	id, err := work.NormalizeArXiv(rawID)
	if err != nil {
		return nil, nil
	}
	base, requestedVersion := splitVersion(id)

	endpoint, err := url.Parse(r.baseURL + "/api/query")
	if err != nil {
		return nil, errors.New("arxiv: invalid endpoint configuration")
	}
	query := endpoint.Query()
	query.Set("id_list", base)
	query.Set("max_results", "1")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("arxiv: could not construct request")
	}
	req.Header.Set("Accept", "application/atom+xml")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &resolver.TemporaryError{Err: errors.New("arxiv: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("arxiv: empty HTTP response")}
	}
	if resp.Body == nil {
		return nil, errors.New("arxiv: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, temporaryStatus("arxiv", resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		return nil, temporaryStatus("arxiv", resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("arxiv: unexpected HTTP status %d", resp.StatusCode)
	}

	var feed atomFeed
	if err := decodeBoundedXML(resp.Body, r.maxBody, &feed); err != nil {
		return nil, fmt.Errorf("arxiv: invalid response: %w", err)
	}

	entry := feed.firstResult(base)
	if entry == nil {
		return nil, nil
	}

	pdfURL := pdfBase + base
	landing := absBase + base

	// arXiv distributes an author copy. When the work carries a journal
	// reference or DOI it has been published elsewhere, so the arXiv file is at
	// best the accepted manuscript; it is never the publisher version of
	// record, so we never claim "published" for an arXiv PDF.
	version := resolver.VersionPreprint
	published := strings.TrimSpace(entry.DOI) != "" || strings.TrimSpace(entry.JournalRef) != ""
	if published {
		version = resolver.VersionAccepted
	}

	versionEvidence := "latest"
	if requestedVersion != "" {
		versionEvidence = requestedVersion
	}
	evidence := []string{
		"arxiv id=" + base,
		"arxiv requested_version=" + versionEvidence,
		"arxiv title=" + safeEvidenceValue(entry.Title),
		"arxiv url=" + redact.URL(pdfURL),
	}
	if published {
		ref := strings.TrimSpace(entry.JournalRef)
		if ref == "" {
			ref = strings.TrimSpace(entry.DOI)
		}
		evidence = append(evidence, "arxiv published_as="+safeEvidenceValue(ref))
	}

	candidate := resolver.Candidate{
		Source:             "arxiv",
		URL:                pdfURL,
		Landing:            landing,
		Version:            version,
		AccessBasis:        resolver.AccessOpen,
		ReuseLicense:       "unknown", // the arXiv Atom feed does not state a reuse license
		ExpectedMIME:       "application/pdf",
		ResolvedWork:       resolvedWork(base, entry),
		Direct:             true,
		IdentityConfidence: 0.98, // exact match on a canonical arXiv identifier
		Evidence:           evidence,
	}
	if err := resolver.ValidateCandidate(candidate); err != nil {
		return nil, nil
	}
	return []resolver.Candidate{candidate}, nil
}

// atomFeed is the minimal shape of the export.arxiv.org Atom response.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string       `xml:"id"`
	Title      string       `xml:"title"`
	Authors    []atomAuthor `xml:"author"`
	Published  string       `xml:"published"`
	DOI        string       `xml:"doi"`         // arxiv:doi (namespace-agnostic local match)
	JournalRef string       `xml:"journal_ref"` // arxiv:journal_ref
}

type atomAuthor struct {
	Name string `xml:"name"`
}

// firstResult returns the entry that corresponds to base, or nil. arXiv reports
// a lookup miss as an entry whose id points at /api/errors; such entries and
// entries for a different id are treated as "not found".
func (f *atomFeed) firstResult(base string) *atomEntry {
	for i := range f.Entries {
		id := f.Entries[i].ID
		if strings.Contains(id, "/api/errors") {
			return nil
		}
		if strings.Contains(id, base) {
			return &f.Entries[i]
		}
	}
	return nil
}

// resolvedWork carries the source-discovered bibliographic identity for the app
// to fill missing request fields after its own consistency checks. Only fields
// the arXiv Atom entry actually supports are set; nothing is fabricated.
func resolvedWork(base string, entry *atomEntry) work.Work {
	resolved := work.Work{ArXiv: base, Title: safeEvidenceValue(entry.Title)}
	if doi, err := work.NormalizeDOI(entry.DOI); err == nil {
		resolved.DOI = doi
	}
	for _, a := range entry.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			resolved.Authors = append(resolved.Authors, name)
		}
	}
	if len(entry.Published) >= 4 {
		if year, err := strconv.Atoi(entry.Published[:4]); err == nil {
			resolved.Year = year
		}
	}
	return resolved
}

// splitVersion separates a trailing vN version suffix from an arXiv id. Old and
// new style ids never end with a lowercase 'v' followed by digits except for
// the version suffix, so this is unambiguous.
func splitVersion(id string) (base, version string) {
	i := strings.LastIndexByte(id, 'v')
	if i > 0 && i < len(id)-1 && isDigits(id[i+1:]) {
		return id[:i], id[i:]
	}
	return id, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func decodeBoundedXML(body io.Reader, max int64, destination any) error {
	payload, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return err
	}
	if int64(len(payload)) > max {
		return errors.New("response exceeds size limit")
	}
	return xml.Unmarshal(payload, destination)
}

func safeEvidenceValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return strings.Join(strings.Fields(value), " ")
}

func temporaryStatus(source string, resp *http.Response) error {
	return &resolver.TemporaryError{
		Err:        fmt.Errorf("%s: upstream HTTP status %d", source, resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		const maxDuration = time.Duration(1<<63 - 1)
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return deadline.Sub(now)
	}
	return 0
}
