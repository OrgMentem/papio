// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package unpaywall resolves DOI records from the Unpaywall v2 API.
package unpaywall

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
	defaultBaseURL = "https://api.unpaywall.org/v2"
	defaultMaxBody = int64(1 << 20)
)

// HTTPClient is the injected HTTP dependency used to call Unpaywall.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. BaseURL is the v2 endpoint root and is
// intended for tests or explicitly configured development endpoints.
type Options struct {
	Client           HTTPClient
	ContactEmail     string
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver using Unpaywall's DOI-only API.
type Resolver struct {
	client  HTTPClient
	email   string
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a resolver with the official v2 endpoint.
func New(client HTTPClient, contactEmail string) *Resolver {
	return NewWithOptions(Options{Client: client, ContactEmail: contactEmail})
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
		email:   strings.TrimSpace(opts.ContactEmail),
		baseURL: baseURL,
		maxBody: maxBody,
	}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "unpaywall" }

// Resolve returns the best Unpaywall OA location, falling back to the first
// OA location with a PDF. It keeps a legal landing URL when the chosen
// location has no direct PDF URL.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil {
		return nil, errors.New("unpaywall: HTTP client is not configured")
	}
	if r.email == "" {
		return nil, errors.New("unpaywall: contact email is required; configure an address accepted by Unpaywall")
	}
	if !validHTTPURL(r.baseURL) {
		return nil, errors.New("unpaywall: invalid endpoint configuration")
	}
	if strings.TrimSpace(requested.DOI) == "" {
		return nil, nil
	}
	doi, err := work.NormalizeDOI(requested.DOI)
	if err != nil {
		return nil, nil
	}

	endpoint, err := r.endpointURL(doi)
	if err != nil {
		return nil, errors.New("unpaywall: invalid endpoint configuration")
	}
	query := endpoint.Query()
	query.Set("email", r.email)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("unpaywall: could not construct request")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &resolver.TemporaryError{Err: errors.New("unpaywall: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("unpaywall: empty HTTP response")}
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("unpaywall: request was rejected (check the configured contact email and API access)")
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return nil, temporaryStatus("unpaywall", resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		return nil, temporaryStatus("unpaywall", resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("unpaywall: unexpected HTTP status %d", resp.StatusCode)
	}
	if resp.Body == nil {
		return nil, errors.New("unpaywall: response body is missing")
	}

	var record response
	if err := decodeBoundedJSON(resp.Body, r.maxBody, &record); err != nil {
		return nil, fmt.Errorf("unpaywall: invalid response: %w", err)
	}
	if !record.IsOA {
		return nil, nil
	}

	location, source, direct, candidateURL := chooseLocation(record.BestOALocation, record.OALocations)
	if location == nil {
		return nil, nil
	}
	landing := firstValidURL(location.URLForLandingPage, location.URL, record.DOIURL)
	if !direct {
		// A legal landing page is the candidate URL when no PDF is available.
		landing = candidateURL
	}

	candidate := resolver.Candidate{
		Source:             "unpaywall",
		URL:                candidateURL,
		Landing:            landing,
		Version:            mapVersion(location.Version),
		AccessBasis:        resolver.AccessOpen,
		ReuseLicense:       reuseLicense(location.License),
		ExpectedMIME:       expectedMIME(candidateURL, direct),
		Direct:             direct,
		IdentityConfidence: 1,
		ResolvedWork:       resolvedWork(record),
		Evidence: []string{
			"unpaywall location=" + source,
			"unpaywall host_type=" + safeEvidenceValue(location.HostType),
			"unpaywall url=" + redact.URL(candidateURL),
		},
	}
	if err := resolver.ValidateCandidate(candidate); err != nil {
		return nil, nil
	}
	return []resolver.Candidate{candidate}, nil
}

func (r *Resolver) endpointURL(doi string) (*url.URL, error) {
	base, err := url.Parse(r.baseURL)
	if err != nil || !validHTTPURL(base.String()) {
		return nil, errors.New("invalid endpoint")
	}
	// The v2 API takes the DOI as the path tail. Preserve its slash, which is
	// part of the DOI and accepted by Unpaywall's wildcard route.
	base.Path = strings.TrimRight(base.Path, "/") + "/" + doi
	return base, nil
}

type response struct {
	DOI             string     `json:"doi"`
	DOIURL          string     `json:"doi_url"`
	IsOA            bool       `json:"is_oa"`
	BestOALocation  *location  `json:"best_oa_location"`
	OALocations     []location `json:"oa_locations"`
	Title           string     `json:"title"`
	Year            int        `json:"year"`
	PublicationYear int        `json:"publication_year"`
	ZAuthors        []author   `json:"z_authors"`
}

type author struct {
	Given  string `json:"given"`
	Family string `json:"family"`
	Name   string `json:"name"` // compatibility with historical responses
}

type location struct {
	URLForPDF         string `json:"url_for_pdf"`
	URLForLandingPage string `json:"url_for_landing_page"`
	URL               string `json:"url"`
	HostType          string `json:"host_type"`
	Version           string `json:"version"`
	License           string `json:"license"`
}

func resolvedWork(record response) work.Work {
	resolved := work.Work{
		Title: strings.TrimSpace(record.Title),
		Year:  record.Year,
	}
	if resolved.Year < 1 {
		resolved.Year = record.PublicationYear
	}
	if resolved.Year < 1 {
		resolved.Year = 0
	}
	for _, raw := range []string{record.DOI, record.DOIURL} {
		if doi, err := work.NormalizeDOI(raw); err == nil {
			resolved.DOI = doi
			break
		}
	}
	for _, author := range record.ZAuthors {
		name := strings.TrimSpace(strings.Join([]string{author.Given, author.Family}, " "))
		if name == "" {
			name = strings.TrimSpace(author.Name)
		}
		if name != "" {
			resolved.Authors = append(resolved.Authors, name)
		}
	}
	return resolved
}

func chooseLocation(best *location, locations []location) (*location, string, bool, string) {
	if best != nil && validHTTPURL(best.URLForPDF) {
		return best, "best", true, strings.TrimSpace(best.URLForPDF)
	}
	for i := range locations {
		if validHTTPURL(locations[i].URLForPDF) {
			return &locations[i], "fallback_pdf", true, strings.TrimSpace(locations[i].URLForPDF)
		}
	}
	if best != nil {
		if landing := firstValidURL(best.URLForLandingPage, best.URL); landing != "" {
			return best, "best_landing", false, landing
		}
	}
	for i := range locations {
		if landing := firstValidURL(locations[i].URLForLandingPage, locations[i].URL); landing != "" {
			return &locations[i], "fallback_landing", false, landing
		}
	}
	return nil, "", false, ""
}

func firstValidURL(values ...string) string {
	for _, value := range values {
		if validHTTPURL(value) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func expectedMIME(candidateURL string, direct bool) string {
	if !direct || candidateURL == "" {
		return ""
	}
	return "application/pdf"
}

func mapVersion(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "publishedversion", "published", "version of record":
		return resolver.VersionPublished
	case "acceptedversion", "accepted", "accepted manuscript":
		return resolver.VersionAccepted
	case "submittedversion", "submitted", "preprint":
		return resolver.VersionPreprint
	default:
		return resolver.VersionUnknown
	}
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
		return errors.New("response exceeds size limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("response contains multiple JSON values")
		}
		return err
	}
	return nil
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
