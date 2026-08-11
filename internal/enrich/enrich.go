// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package enrich adds corroborated metadata to title-only work requests and
// answers typed version-relation questions about a DOI from the same Crossref
// metadata surface.
package enrich

import (
	"bytes"
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
	"unicode/utf8"

	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL = "https://api.crossref.org/works"
	defaultMaxBody = 1 << 20
	requestTimeout = 10 * time.Second
)

// HTTPClient is the injected HTTP dependency used to call Crossref.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures an Enricher. ContactEmail identifies papio to Crossref's
// polite pool; it is the same address every other source client already sends.
type Options struct {
	Client           HTTPClient
	ContactEmail     string
	BaseURL          string
	MaxResponseBytes int64
}

// Enricher searches Crossref metadata to add a DOI only when a result is
// independently corroborated by the submitted work metadata.
type Enricher struct {
	client  HTTPClient
	email   string
	baseURL string
	maxBody int64
}

// New constructs an enricher with default options.
func New(client HTTPClient) *Enricher {
	return NewWithOptions(Options{Client: client})
}

// NewWithOptions constructs an enricher with injectable dependencies.
func NewWithOptions(opts Options) *Enricher {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: requestTimeout}
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxBody
	}
	return &Enricher{
		client: opts.Client, email: strings.TrimSpace(opts.ContactEmail),
		baseURL: strings.TrimRight(opts.BaseURL, "/"), maxBody: opts.MaxResponseBytes,
	}
}

// Enrich searches Crossref for a title-only work. It adopts only an exact
// normalized title match with positive, equal publication year and author-family
// evidence; ISBN work is never promoted by title rescue.
func (e *Enricher) Enrich(ctx context.Context, requested work.Work) (work.Work, bool, error) {
	if strings.TrimSpace(requested.DOI) != "" || strings.TrimSpace(requested.ISBN) != "" || strings.TrimSpace(requested.Title) == "" {
		return requested, false, nil
	}
	if e == nil || e.client == nil {
		return requested, false, errors.New("enrich: HTTP client is not configured")
	}

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint, err := e.searchURL(requested.Title)
	if err != nil {
		return requested, false, err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return requested, false, errors.New("enrich: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: request deadline exceeded")}
			}
			return requested, false, requestCtx.Err()
		}
		return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: request failed")}
	}
	if resp == nil {
		return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: empty HTTP response")}
	}
	if resp.Body == nil {
		return requested, false, errors.New("enrich: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return requested, false, temporaryStatus(resp)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return requested, false, fmt.Errorf("enrich: Crossref returned HTTP %d", resp.StatusCode)
	}

	var payload response
	if err := decodeBoundedJSON(resp.Body, e.maxBody, &payload); err != nil {
		return requested, false, fmt.Errorf("enrich: invalid Crossref response: %w", err)
	}
	seen := make(map[string]record)
	for _, candidate := range payload.Message.Items {
		if !matches(candidate, requested) {
			continue
		}
		doi, err := work.NormalizeDOI(candidate.DOI)
		if err != nil {
			continue
		}
		if _, exists := seen[doi]; !exists {
			seen[doi] = candidate
		}
	}
	if len(seen) != 1 {
		return requested, false, nil
	}
	for doi, candidate := range seen {
		enriched := requested
		enriched.DOI = doi
		if enriched.Container == "" && len(candidate.ContainerTitle) > 0 {
			enriched.Container = strings.TrimSpace(candidate.ContainerTitle[0])
		}
		return enriched, true, nil
	}
	return requested, false, nil
}

// versionRelationTypes are the Crossref relation edges that name another
// registered version of the same work. Only these typed, depth-one edges are
// followed — never fuzzy similarity: a typed edge was asserted by a
// registrant, so following it cannot fetch a merely similar-looking work.
var versionRelationTypes = []string{"has-preprint", "is-preprint-of", "has-version", "is-version-of"}

// maxVersionSiblings caps how many related DOIs one lookup may return: the
// hop runs at the exhaustion boundary, where each sibling fans out over the
// enabled resolvers.
const maxVersionSiblings = 3

// VersionSiblings returns the DOIs Crossref records as typed version
// relations of doi (preprint and version edges, depth one), normalized,
// deduplicated, capped, and never including doi itself. An unregistered or
// relation-less DOI is (nil, nil); rate limits and upstream faults are
// *resolver.TemporaryError like every other source client.
func (e *Enricher) VersionSiblings(ctx context.Context, doi string) ([]string, error) {
	if e == nil || e.client == nil {
		return nil, errors.New("enrich: HTTP client is not configured")
	}
	normalized, err := work.NormalizeDOI(doi)
	if err != nil {
		return nil, nil
	}
	// A "." or ".." path segment inside an otherwise-legal DOI would escape
	// the /works/ prefix when a server resolves the path. No real DOI has
	// one (the same guard internal/doiregistry documents).
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "." || segment == ".." {
			return nil, nil
		}
	}

	endpoint, err := url.Parse(e.baseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("enrich: invalid configured endpoint")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + normalized
	query := endpoint.Query()
	if e.email != "" {
		query.Set("mailto", e.email)
	}
	endpoint.RawQuery = query.Encode()

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("enrich: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				return nil, &resolver.TemporaryError{Err: errors.New("enrich: request deadline exceeded")}
			}
			return nil, requestCtx.Err()
		}
		return nil, &resolver.TemporaryError{Err: errors.New("enrich: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("enrich: empty HTTP response")}
	}
	if resp.Body == nil {
		return nil, errors.New("enrich: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, temporaryStatus(resp)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return nil, fmt.Errorf("enrich: Crossref returned HTTP %d", resp.StatusCode)
	}

	var payload relationResponse
	if err := decodeBoundedJSON(resp.Body, e.maxBody, &payload); err != nil {
		return nil, fmt.Errorf("enrich: invalid Crossref response: %w", err)
	}
	seen := map[string]bool{normalized: true}
	siblings := make([]string, 0, maxVersionSiblings)
	for _, relationType := range versionRelationTypes {
		for _, target := range payload.Message.Relation[relationType] {
			if !strings.EqualFold(strings.TrimSpace(target.IDType), "doi") {
				continue
			}
			sibling, err := work.NormalizeDOI(target.ID)
			if err != nil || seen[sibling] {
				continue
			}
			seen[sibling] = true
			siblings = append(siblings, sibling)
			if len(siblings) == maxVersionSiblings {
				return siblings, nil
			}
		}
	}
	if len(siblings) == 0 {
		return nil, nil
	}
	return siblings, nil
}

type relationResponse struct {
	Message struct {
		Relation map[string][]relationTarget `json:"relation"`
	} `json:"message"`
}

type relationTarget struct {
	ID     string `json:"id"`
	IDType string `json:"id-type"`
}

func (e *Enricher) searchURL(title string) (*url.URL, error) {
	endpoint, err := url.Parse(e.baseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("enrich: invalid configured endpoint")
	}
	query := endpoint.Query()
	query.Set("query.title", strings.TrimSpace(title))
	query.Set("rows", "5")
	if e.email != "" {
		query.Set("mailto", e.email)
	}
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

type response struct {
	Message struct {
		Items []record `json:"items"`
	} `json:"message"`
}

type record struct {
	DOI             string    `json:"DOI"`
	Title           []string  `json:"title"`
	Author          []author  `json:"author"`
	ContainerTitle  []string  `json:"container-title"`
	PublishedPrint  dateParts `json:"published-print"`
	PublishedOnline dateParts `json:"published-online"`
	Issued          dateParts `json:"issued"`
}

type author struct {
	Family string `json:"family"`
}

type dateParts struct {
	DateParts [][]int `json:"date-parts"`
}

func (d dateParts) year() int {
	if len(d.DateParts) == 0 || len(d.DateParts[0]) == 0 || d.DateParts[0][0] < 1 {
		return 0
	}
	return d.DateParts[0][0]
}

func (r record) year() int {
	for _, date := range []dateParts{r.PublishedPrint, r.PublishedOnline, r.Issued} {
		if year := date.year(); year != 0 {
			return year
		}
	}
	return 0
}

func matches(candidate record, requested work.Work) bool {
	if len(candidate.Title) == 0 || normalizeTitle(candidate.Title[0]) != normalizeTitle(requested.Title) {
		return false
	}
	candidateYear := candidate.year()
	if requested.Year <= 0 || candidateYear <= 0 || candidateYear != requested.Year {
		return false
	}
	if len(requested.Authors) == 0 {
		return false
	}
	for _, requestedAuthor := range requested.Authors {
		family := authorFamily(requestedAuthor)
		if family == "" {
			continue
		}
		for _, candidateAuthor := range candidate.Author {
			if family == normalizeTitle(candidateAuthor.Family) {
				return true
			}
		}
	}
	return false
}

// normalizeTitle folds the differences that never distinguish two works: case,
// runs of whitespace, and a single trailing full stop. The last one is not
// cosmetic. APA and several other publishers deposit article titles with a
// closing period ("...and well-being."), while citations and reference managers
// almost never carry one, so an exact comparison rejected a perfect match on one
// character. It cost Ryan & Deci 2000 — the most-cited work in its literature —
// which papio reported as no_identifier despite Crossref returning it at rank 0
// with corroborating authors and year.
//
// Only the period is folded. Replaying one cohort's twenty-six clean-title
// failures showed every recoverable case was a deposited full stop, so folding
// "?" or "!" too would buy nothing measurable while making "Who?" and "Who!"
// equal — and matches() adopts on title alone when the request carries no
// authors or year. This helper also normalizes author family names, so any
// widening here loosens author corroboration as well.
func normalizeTitle(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Join(strings.Fields(value), " ")), ".")
}

func authorFamily(value string) string {
	value = strings.TrimSpace(value)
	if comma := strings.IndexRune(value, ','); comma >= 0 {
		return normalizeTitle(value[:comma])
	}
	parts := strings.Fields(normalizeTitle(value))
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		// A single-word author is a bare family name or mononym; corroborate
		// on it directly instead of silently skipping the author check.
		return parts[0]
	}
	if isAuthorInitial(parts[len(parts)-1]) {
		return strings.Join(parts[:len(parts)-1], " ")
	}
	return parts[len(parts)-1]
}

func isAuthorInitial(value string) bool {
	value = strings.Trim(value, ".")
	_, size := utf8.DecodeRuneInString(value)
	return size > 0 && size == len(value)
}

func decodeBoundedJSON(body io.Reader, maximum int64, destination any) error {
	if maximum <= 0 {
		return errors.New("invalid response limit")
	}
	data, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maximum {
		return fmt.Errorf("response exceeds %d-byte limit", maximum)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func temporaryStatus(resp *http.Response) error {
	return &resolver.TemporaryError{Err: fmt.Errorf("enrich: Crossref returned HTTP %d", resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
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
