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

// Enrich performs one bounded OpenAlex title search. A match requires exact
// title, positive equal publication year, one-to-one full-author-list
// corroboration, and an explicit usable open-access location. ISBN work is
// never promoted because OpenAlex provides no edition-level ISBN data.
func (e *OpenAlexEnricher) Enrich(ctx context.Context, requested work.Work) (work.Work, bool, error) {
	// A pre-wire decline: no request is made, so the caller must not charge
	// this as a performed call (resolver.ErrNotApplicable).
	if requested.HasFetchableIdentifier() || strings.TrimSpace(requested.ISBN) != "" ||
		strings.TrimSpace(requested.Title) == "" || requested.Year <= 0 || len(requested.Authors) == 0 {
		return requested, false, resolver.ErrNotApplicable
	}
	if e == nil || e.client == nil {
		return requested, false, errors.New("enrich: OpenAlex HTTP client is not configured")
	}
	if e.email == "" {
		return requested, false, errors.New("enrich: OpenAlex contact email is required")
	}

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint, err := e.searchURL(requestCtx, requested)
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
	seen := make(map[string]string)
	ambiguous := false
	for _, candidate := range payload.Results {
		if !matchesOpenAlex(candidate, requested) {
			continue
		}
		openAlexID, err := openAlexIdentifier(candidate)
		if err != nil {
			continue
		}
		doi := openAlexDOI(candidate)
		if previous, exists := seen[openAlexID]; exists {
			if previous != "" && doi != "" && previous != doi {
				ambiguous = true
			} else if previous == "" {
				seen[openAlexID] = doi
			}
			continue
		}
		seen[openAlexID] = doi
	}
	if ambiguous || len(seen) != 1 {
		return requested, false, nil
	}
	for openAlexID, doi := range seen {
		enriched := requested
		enriched.OpenAlex = openAlexID
		enriched.DOI = doi
		return enriched, true, nil
	}
	return requested, false, nil
}

// searchURL builds the bounded title search. It takes a context because the
// keyed identity's own daily-quota signal can send this call to OpenAlex's
// keyless tier instead (resolver.WithAnonymousCredentials), and the api_key
// must then be absent from the wire — a configured dev base URL may carry a
// stale one, so it is deleted unconditionally before the key is re-applied.
func (e *OpenAlexEnricher) searchURL(ctx context.Context, requested work.Work) (*url.URL, error) {
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
	query.Set("select", "id,doi,ids,title,publication_year,authorships,open_access,locations")
	if e.email != "" {
		query.Set("mailto", e.email)
	}
	query.Del("api_key")
	if e.apiKey != "" && !resolver.AnonymousCredentials(ctx) {
		query.Set("api_key", e.apiKey)
	}
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

type openAlexSearchResponse struct {
	Results []openAlexRecord `json:"results"`
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

type openAlexOpenAccess struct {
	IsOA bool `json:"is_oa"`
}

type openAlexLocation struct {
	IsOA           bool   `json:"is_oa"`
	LandingPageURL string `json:"landing_page_url"`
	PDFURL         string `json:"pdf_url"`
}

type openAlexRecord struct {
	ID              string               `json:"id"`
	DOI             string               `json:"doi"`
	IDs             openAlexIdentifiers  `json:"ids"`
	Title           string               `json:"title"`
	PublicationYear int                  `json:"publication_year"`
	Authorships     []openAlexAuthorship `json:"authorships"`
	OpenAccess      openAlexOpenAccess   `json:"open_access"`
	Locations       []openAlexLocation   `json:"locations"`
}

func matchesOpenAlex(candidate openAlexRecord, requested work.Work) bool {
	if normalizeTitle(candidate.Title) != normalizeTitle(requested.Title) {
		return false
	}
	if requested.Year <= 0 || candidate.PublicationYear <= 0 || candidate.PublicationYear != requested.Year {
		return false
	}
	if !hasUsableOpenAlexLocation(candidate) {
		return false
	}
	if len(requested.Authors) == 0 || len(candidate.Authorships) == 0 {
		return false
	}
	if len(requested.Authors) != len(candidate.Authorships) {
		return false
	}
	used := make([]bool, len(candidate.Authorships))
	for _, requestedAuthor := range requested.Authors {
		match := -1
		for index, candidateAuthor := range candidate.Authorships {
			if used[index] || !authorsCorroborate(requestedAuthor, candidateAuthor.Author.DisplayName) {
				continue
			}
			if match != -1 {
				// A requested name with multiple candidate matches is not a
				// unique corroboration, even when a different ordering could
				// produce a complete matching.
				return false
			}
			match = index
		}
		if match == -1 {
			return false
		}
		used[match] = true
	}
	return true
}

func hasUsableOpenAlexLocation(candidate openAlexRecord) bool {
	if !candidate.OpenAccess.IsOA {
		return false
	}
	for _, location := range candidate.Locations {
		if !location.IsOA {
			continue
		}
		for _, raw := range []string{location.PDFURL, location.LandingPageURL} {
			locationURL, err := url.Parse(strings.TrimSpace(raw))
			if err == nil && (locationURL.Scheme == "http" || locationURL.Scheme == "https") && locationURL.Host != "" {
				return true
			}
		}
	}
	return false
}

// authorsCorroborate requires more than a shared family name. A family name
// alone is not enough to identify a work: common surnames routinely appear on
// unrelated works with the same title and year. OpenAlex supplies display
// names, so compare the given-name evidence when it is available, accepting
// the usual full-name/initial presentations in either direction.
func authorsCorroborate(requested, candidate string) bool {
	if authorFamily(requested) == "" || authorFamily(requested) != authorFamily(candidate) {
		return false
	}
	requestedGiven := authorGivenNames(requested)
	candidateGiven := authorGivenNames(candidate)
	if len(requestedGiven) == 0 || len(candidateGiven) == 0 {
		return false
	}
	return givenNamesCorroborate(requestedGiven, candidateGiven)
}

func authorGivenNames(value string) []string {
	value = strings.TrimSpace(normalizeTitle(value))
	if comma := strings.IndexRune(value, ','); comma >= 0 {
		value = strings.TrimSpace(value[comma+1:])
	} else {
		parts := strings.Fields(value)
		if len(parts) <= 1 {
			return nil
		}
		// authorFamily treats a trailing initial as given-name evidence only
		// when it follows the family in a comma-formatted citation. For the
		// display-name form, the final token is the family name.
		value = strings.Join(parts[:len(parts)-1], " ")
	}
	var given []string
	for _, part := range strings.Fields(value) {
		part = strings.Trim(part, ".,")
		if part != "" {
			given = append(given, part)
		}
	}
	return given
}

func givenNamesCorroborate(requested, candidate []string) bool {
	// The first given name is the stable cross-source signal. Compare either
	// spelling or an initial so "D. L. Kirkpatrick" and "Donald L.
	// Kirkpatrick" corroborate, while "Jane Smith" and "John Smith" do not.
	firstRequested := strings.Trim(requested[0], ".")
	firstCandidate := strings.Trim(candidate[0], ".")
	if firstRequested == "" || firstCandidate == "" {
		return false
	}
	if firstRequested == firstCandidate {
		return true
	}
	return len(firstRequested) == 1 && strings.HasPrefix(firstCandidate, firstRequested) ||
		len(firstCandidate) == 1 && strings.HasPrefix(firstRequested, firstCandidate)
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
