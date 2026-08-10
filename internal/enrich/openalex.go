// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"papio/internal/resolver"
	"papio/internal/work"
)

const openAlexEnrichmentBaseURL = "https://api.openalex.org/works"

// OpenAlexOptions configures an OpenAlex title-search enricher.
type OpenAlexOptions struct {
	Client           HTTPClient
	ContactEmail     string
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// OpenAlexEnricher adds a corroborated OpenAlex work ID, and its DOI when
// available, to a work that has no fetchable identifier. It is deliberately a
// separate enricher from Crossref so the app can account for each source and
// preserve the Crossref-first order.
type OpenAlexEnricher struct {
	client  HTTPClient
	email   string
	apiKey  string
	baseURL string
	maxBody int64
}

// NewOpenAlex constructs an OpenAlex metadata enricher with default options.
func NewOpenAlex(client HTTPClient, contactEmail string) *OpenAlexEnricher {
	return NewOpenAlexWithOptions(OpenAlexOptions{Client: client, ContactEmail: contactEmail})
}

// NewOpenAlexWithOptions constructs an OpenAlex metadata enricher with
// injectable dependencies.
func NewOpenAlexWithOptions(opts OpenAlexOptions) *OpenAlexEnricher {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: requestTimeout}
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		opts.BaseURL = openAlexEnrichmentBaseURL
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxBody
	}
	return &OpenAlexEnricher{
		client: opts.Client, email: strings.TrimSpace(opts.ContactEmail),
		apiKey:  strings.TrimSpace(opts.APIKey),
		baseURL: strings.TrimRight(opts.BaseURL, "/"), maxBody: opts.MaxResponseBytes,
	}
}

// Enrich performs one bounded OpenAlex title search. A match is accepted only
// when its normalized title, compatible publication year, and at least one
// requested author family agree, matching Crossref's corroboration policy.
func (e *OpenAlexEnricher) Enrich(ctx context.Context, requested work.Work) (work.Work, bool, error) {
	if requested.HasFetchableIdentifier() || strings.TrimSpace(requested.Title) == "" {
		return requested, false, nil
	}
	if e == nil || e.client == nil {
		return requested, false, errors.New("enrich: OpenAlex HTTP client is not configured")
	}
	if e.email == "" {
		return requested, false, errors.New("enrich: OpenAlex contact email is required")
	}

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint, err := e.searchURL(requested)
	if err != nil {
		return requested, false, err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return requested, false, errors.New("enrich: could not construct OpenAlex request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: OpenAlex request deadline exceeded")}
			}
			return requested, false, requestCtx.Err()
		}
		return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: OpenAlex request failed")}
	}
	if resp == nil {
		return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: empty OpenAlex HTTP response")}
	}
	if resp.Body == nil {
		return requested, false, errors.New("enrich: OpenAlex response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return requested, false, temporaryOpenAlexStatus(resp)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return requested, false, fmt.Errorf("enrich: OpenAlex returned HTTP %d", resp.StatusCode)
	}

	var payload openAlexSearchResponse
	if err := decodeBoundedJSON(resp.Body, e.maxBody, &payload); err != nil {
		return requested, false, fmt.Errorf("enrich: invalid OpenAlex response: %w", err)
	}
	for _, candidate := range payload.Results {
		if !matchesOpenAlex(candidate, requested) {
			continue
		}
		openAlexID, err := openAlexIdentifier(candidate)
		if err != nil {
			continue
		}
		enriched := requested
		enriched.OpenAlex = openAlexID
		if doi := openAlexDOI(candidate); doi != "" {
			enriched.DOI = doi
		}
		if enriched.Year == 0 && candidate.PublicationYear > 0 {
			enriched.Year = candidate.PublicationYear
		}
		return enriched, true, nil
	}
	return requested, false, nil
}

func (e *OpenAlexEnricher) searchURL(requested work.Work) (*url.URL, error) {
	endpoint, err := url.Parse(e.baseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("enrich: invalid configured OpenAlex endpoint")
	}
	filters := []string{"title.search:" + strings.TrimSpace(requested.Title)}
	if requested.Year != 0 {
		filters = append(filters, fmt.Sprintf("publication_year:%d", requested.Year))
	}
	query := endpoint.Query()
	query.Set("filter", strings.Join(filters, ","))
	query.Set("per-page", "5")
	if e.email != "" {
		query.Set("mailto", e.email)
	}
	if e.apiKey != "" {
		query.Set("api_key", e.apiKey)
	}
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

type openAlexSearchResponse struct {
	Results []openAlexRecord `json:"results"`
}

type openAlexRecord struct {
	ID              string               `json:"id"`
	DOI             string               `json:"doi"`
	IDs             openAlexIdentifiers  `json:"ids"`
	Title           string               `json:"title"`
	PublicationYear int                  `json:"publication_year"`
	Authorships     []openAlexAuthorship `json:"authorships"`
}

type openAlexIdentifiers struct {
	OpenAlex string `json:"openalex"`
	DOI      string `json:"doi"`
}

type openAlexAuthorship struct {
	Author struct {
		DisplayName string `json:"display_name"`
	} `json:"author"`
}

func matchesOpenAlex(candidate openAlexRecord, requested work.Work) bool {
	if normalizeTitle(candidate.Title) != normalizeTitle(requested.Title) {
		return false
	}
	if candidate.PublicationYear != 0 && requested.Year != 0 && candidate.PublicationYear != requested.Year {
		return false
	}
	if len(requested.Authors) == 0 {
		return true
	}
	for _, requestedAuthor := range requested.Authors {
		family := authorFamily(requestedAuthor)
		if family == "" {
			continue
		}
		for _, candidateAuthor := range candidate.Authorships {
			if family == authorFamily(candidateAuthor.Author.DisplayName) {
				return true
			}
		}
	}
	return false
}

func openAlexIdentifier(candidate openAlexRecord) (string, error) {
	for _, raw := range []string{candidate.ID, candidate.IDs.OpenAlex} {
		if normalized, err := work.NormalizeOpenAlex(raw); err == nil {
			return normalized, nil
		}
	}
	return "", errors.New("OpenAlex record has no valid work ID")
}

func openAlexDOI(candidate openAlexRecord) string {
	for _, raw := range []string{candidate.DOI, candidate.IDs.DOI} {
		if normalized, err := work.NormalizeDOI(raw); err == nil {
			return normalized
		}
	}
	return ""
}

func temporaryOpenAlexStatus(resp *http.Response) error {
	return &resolver.TemporaryError{
		Err:        fmt.Errorf("enrich: OpenAlex returned HTTP %d", resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}
