// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package core resolves licensed full text from the CORE v3 API.
package core

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

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL   = "https://api.core.ac.uk/v3/search/works"
	defaultMaxBody   = 1 << 20
	defaultResultCap = 5
)

var errUnsafeHTTPClient = errors.New("core: configured HTTP client cannot enforce credential-safe redirects")

// HTTPClient is the injected HTTP dependency used to call CORE.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a licensed CORE resolver. A resolver is inert without an
// API key, regardless of the configured endpoint.
type Options struct {
	Client           HTTPClient
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver using the CORE v3 search API.
type Resolver struct {
	client  HTTPClient
	apiKey  string
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a CORE resolver for the official v3 search endpoint.
func New(client HTTPClient, apiKey string) *Resolver {
	return NewWithOptions(Options{Client: client, APIKey: apiKey})
}

// NewConfigured constructs a resolver from a source policy. Both enabling the
// source and providing its credential are required for it to make requests.
func NewConfigured(client HTTPClient, source config.Source) *Resolver {
	if !source.Enabled {
		return NewWithOptions(Options{Client: client})
	}
	return NewWithOptions(Options{Client: client, APIKey: source.APIKey, BaseURL: source.BaseURLForDev})
}

// NewWithOptions constructs a resolver with injected dependencies.
func NewWithOptions(opts Options) *Resolver {
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.BaseURL == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxBody
	}
	return &Resolver{client: opts.Client, apiKey: strings.TrimSpace(opts.APIKey), baseURL: opts.BaseURL, maxBody: opts.MaxResponseBytes}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "core" }

// Resolve searches by DOI before attempting an exact-title search. It only
// returns direct full-text URLs supplied by CORE; a URL is never treated as
// proof of licensed entitlement.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.apiKey == "" {
		return nil, nil
	}
	if doi := canonicalDOI(requested.DOI); doi != "" {
		records, err := r.search(ctx, doi)
		if err != nil {
			return nil, err
		}
		if candidates := r.candidates(records, requested, doi, false); len(candidates) != 0 {
			return candidates, nil
		}
	}
	if strings.TrimSpace(requested.Title) == "" {
		return nil, nil
	}
	records, err := r.search(ctx, requested.Title)
	if err != nil {
		return nil, err
	}
	return r.candidates(records, requested, canonicalDOI(requested.DOI), true), nil
}

func (r *Resolver) search(ctx context.Context, query string) ([]record, error) {
	u, err := url.Parse(r.baseURL)
	if err != nil {
		return nil, errors.New("core: invalid configured endpoint")
	}
	values := u.Query()
	values.Set("q", query)
	values.Set("limit", strconv.Itoa(defaultResultCap))
	values.Set("offset", "0")
	values.Set("sort", "relevance")
	u.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("core: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	resp, err := r.do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextError(ctx, "core")
		}
		if errors.Is(err, errUnsafeHTTPClient) {
			return nil, err
		}
		// Do not expose transport errors: custom transports can include request
		// headers in their error text.
		return nil, &resolver.TemporaryError{Err: errors.New("core: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("core: empty HTTP response")}
	}
	if resp.Body == nil {
		return nil, errors.New("core: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("core: authentication failed (401); verify the configured CORE API key")
	case resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("core: access forbidden (403); verify the configured CORE entitlement")
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, temporaryStatus("core", resp)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("core: API returned HTTP %d", resp.StatusCode)
	}

	var payload searchResponse
	if err := decodeBoundedJSON(resp.Body, r.maxBody, &payload); err != nil {
		return nil, fmt.Errorf("core: invalid response: %w", err)
	}
	return payload.Results, nil
}

// do permits only clients that protect CORE's bearer credential across
// cross-origin redirects. fetch.SecureHTTPClient clears all caller-supplied
// headers outside the origin, while http.Client is copied with a redirect policy
// that strips Authorization; opaque HTTPClient implementations are rejected.
func (r *Resolver) do(req *http.Request) (*http.Response, error) {
	switch client := r.client.(type) {
	case *fetch.SecureHTTPClient:
		return client.Do(req)
	case *http.Client:
		copyClient := *client
		previous := client.CheckRedirect
		copyClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if !sameAuthority(next.URL, req.URL) {
				next.Header.Del("Authorization")
			}
			if previous != nil {
				return previous(next, via)
			}
			return nil
		}
		return copyClient.Do(req)
	default:
		return nil, errUnsafeHTTPClient
	}
}

func sameAuthority(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func contextError(ctx context.Context, source string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &resolver.TemporaryError{Err: fmt.Errorf("%s: request deadline exceeded", source)}
	}
	return ctx.Err()
}

func (r *Resolver) candidates(records []record, requested work.Work, requestedDOI string, titleSearch bool) []resolver.Candidate {
	seen := make(map[string]struct{})
	var candidates []resolver.Candidate
	for _, record := range records {
		recordDOI := canonicalDOI(record.DOI)
		match := ""
		confidence := 0.0
		switch {
		case requestedDOI != "":
			// A DOI-bearing request can only emit an exact DOI match. Title
			// fallback helps locate that record; it cannot relax identity.
			if recordDOI != requestedDOI {
				continue
			}
			match, confidence = "doi_match", 1
		case titleSearch && matchesTitleSearch(record.Title, record.authorNames(), record.publicationYear(), requested):
			match, confidence = "title_match", 0.8
		default:
			continue
		}
		for _, link := range record.fullTextLinks() {
			if !validHTTPURL(link.URL) {
				continue
			}
			if _, ok := seen[link.URL]; ok {
				continue
			}
			seen[link.URL] = struct{}{}
			landing := doiLanding(recordDOI)
			if landing == "" && validHTTPURL(record.LandingPageURL) {
				landing = record.LandingPageURL
			}
			candidates = append(candidates, resolver.Candidate{
				Source:             "core",
				URL:                link.URL,
				Landing:            landing,
				Version:            mapVersion(record.Version),
				AccessBasis:        resolver.AccessLicensedAPI,
				ReuseLicense:       reuseLicense(record.License),
				ExpectedMIME:       expectedMIME(link),
				RequestHeaders:     r.requestHeaders(link.URL),
				ResolvedWork:       record.resolvedWork(),
				Direct:             true,
				IdentityConfidence: confidence,
				Evidence:           []string{"core " + match},
			})
		}
	}
	return candidates
}

type searchResponse struct {
	Results []record `json:"results"`
}

type record struct {
	DOI                string          `json:"doi"`
	Title              string          `json:"title"`
	Authors            json.RawMessage `json:"authors"`
	YearPublished      int             `json:"yearPublished"`
	PublishedDate      string          `json:"publishedDate"`
	DownloadURL        string          `json:"downloadUrl"`
	SourceFulltextURLs []string        `json:"sourceFulltextUrls"`
	LandingPageURL     string          `json:"landingPageUrl"`
	License            string          `json:"license"`
	Version            string          `json:"version"`
	Links              []link          `json:"links"`
}

type link struct {
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
	Type        string `json:"type"`
}

func (l link) isFullText() bool {
	kind := strings.ToLower(strings.TrimSpace(l.Type))
	return expectedMIME(l) == "application/pdf" ||
		strings.Contains(kind, "fulltext") ||
		strings.Contains(kind, "full-text") ||
		strings.Contains(kind, "download")
}

func (r record) fullTextLinks() []link {
	var links []link
	for _, value := range r.Links {
		if value.isFullText() {
			links = append(links, value)
		}
	}
	if r.DownloadURL != "" {
		links = append(links, link{URL: r.DownloadURL})
	}
	for _, value := range r.SourceFulltextURLs {
		links = append(links, link{URL: value})
	}
	return links
}

func canonicalDOI(value string) string {
	doi, err := work.NormalizeDOI(value)
	if err != nil {
		return ""
	}
	return doi
}
func doiLanding(doi string) string {
	if doi == "" {
		return ""
	}
	return "https://doi.org/" + url.PathEscape(doi)
}
func sameTitle(left, right string) bool {
	return normalizeTitle(left) != "" && normalizeTitle(left) == normalizeTitle(right)
}
func matchesTitleSearch(recordTitle string, recordAuthors []string, recordYear int, requested work.Work) bool {
	if !sameTitle(recordTitle, requested.Title) {
		return false
	}
	if requested.Year != 0 && recordYear != requested.Year {
		return false
	}
	return sameAuthorLists(recordAuthors, requested.Authors)
}

func sameAuthorLists(recordAuthors, requestedAuthors []string) bool {
	if len(requestedAuthors) == 0 {
		return true
	}
	if len(recordAuthors) != len(requestedAuthors) {
		return false
	}
	matched := make([]bool, len(recordAuthors))
	for _, requestedAuthor := range requestedAuthors {
		found := false
		for i, recordAuthor := range recordAuthors {
			if !matched[i] && sameAuthor(recordAuthor, requestedAuthor) {
				matched[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func sameAuthor(left, right string) bool {
	left, right = normalizeTitle(left), normalizeTitle(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	leftParts, rightParts := strings.Fields(left), strings.Fields(right)
	if len(leftParts) == 0 || len(rightParts) == 0 || leftParts[len(leftParts)-1] != rightParts[len(rightParts)-1] {
		return false
	}
	// A shared surname alone is not enough to accept a title-only result.
	return leftParts[0][0] == rightParts[0][0]
}
func normalizeTitle(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func (r record) authorNames() []string {
	var raw []json.RawMessage
	if len(r.Authors) == 0 || json.Unmarshal(r.Authors, &raw) != nil {
		return nil
	}
	var names []string
	for _, item := range raw {
		var name string
		if json.Unmarshal(item, &name) == nil && strings.TrimSpace(name) != "" {
			names = append(names, strings.TrimSpace(name))
			continue
		}
		var person struct {
			Name      string `json:"name"`
			FullName  string `json:"fullName"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		}
		if json.Unmarshal(item, &person) == nil {
			name = strings.TrimSpace(person.Name)
			if name == "" {
				name = strings.TrimSpace(person.FullName)
			}
			if name == "" {
				name = strings.TrimSpace(person.FirstName + " " + person.LastName)
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

func (r record) resolvedWork() work.Work {
	return work.Work{DOI: canonicalDOI(r.DOI), Title: strings.TrimSpace(r.Title), Authors: r.authorNames(), Year: r.publicationYear()}
}

func (r record) publicationYear() int {
	year := r.YearPublished
	if year == 0 && len(r.PublishedDate) >= 4 {
		if parsed, err := strconv.Atoi(r.PublishedDate[:4]); err == nil {
			year = parsed
		}
	}
	return year
}

func mapVersion(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "published", "published version", "version of record", "vor":
		return resolver.VersionPublished
	case "accepted", "acceptedversion", "accepted manuscript", "author accepted manuscript":
		return resolver.VersionAccepted
	case "preprint", "submitted", "submitted version":
		return resolver.VersionPreprint
	default:
		return resolver.VersionUnknown
	}
}
func reuseLicense(value string) string {
	if value = strings.TrimSpace(value); value == "" {
		return "unknown"
	}
	return value
}
func expectedMIME(value link) string {
	if isPDF(value.ContentType) || isPDF(value.Type) || strings.HasSuffix(strings.ToLower(strings.Split(value.URL, "?")[0]), ".pdf") {
		return "application/pdf"
	}
	return ""
}
func isPDF(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "application/pdf" || strings.HasPrefix(value, "application/pdf;")
}

// requestHeaders only attaches CORE credentials to a same-host download
// endpoint. Publisher URLs and bearer-signed URLs receive no extra secret.
func (r *Resolver) requestHeaders(address string) map[string]string {
	service, serviceErr := url.Parse(r.baseURL)
	target, targetErr := url.Parse(address)
	if serviceErr != nil || targetErr != nil || !sameAuthority(service, target) {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + r.apiKey}
}
func validHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.IsAbs() && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
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

func temporaryStatus(source string, resp *http.Response) error {
	return &resolver.TemporaryError{Err: fmt.Errorf("%s: API returned HTTP %d", source, resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
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
