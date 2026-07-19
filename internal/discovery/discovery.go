// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package discovery searches OpenAlex for works without creating acquisition jobs.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"papio/internal/work"
)

const (
	defaultBaseURL   = "https://api.openalex.org/works"
	defaultVersion   = "0.1.0-dev"
	defaultLimit     = 20
	maxLimit         = 50
	defaultMaxBody   = int64(1 << 20)
	maxAbstractWords = 20_000
)

const slimFields = "id,doi,title,display_name,publication_year,authorships,primary_location,open_access,cited_by_count"

// HTTPClient is the injected HTTP dependency used to call OpenAlex.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Client. ContactEmail is required for OpenAlex's polite
// pool. BaseURL exists only for an explicitly configured loopback development
// endpoint.
type Options struct {
	Client           HTTPClient
	ContactEmail     string
	BaseURL          string
	Version          string
	MaxResponseBytes int64
}

// SearchParams selects works from configured sources. Slim is an internal-only
// mode for callers that do not need abstract_inverted_index and must keep
// response sizes bounded on broad scheduled searches.
type SearchParams struct {
	Query     string `json:"query,omitempty"`
	Limit     int    `json:"limit"`
	YearFrom  int    `json:"year_from,omitempty"`
	YearTo    int    `json:"year_to,omitempty"`
	OAOnly    bool   `json:"oa_only,omitempty"`
	Cites     string `json:"cites,omitempty"`
	CitedBy   string `json:"cited_by,omitempty"`
	RelatedTo string `json:"related_to,omitempty"`
	Source    string `json:"source,omitempty"`
	Slim      bool   `json:"-"`
}

// HasCitationSnowball reports whether the parameters select a citation-based
// search, which can run without a free-text query.
func (params SearchParams) HasCitationSnowball() bool {
	return strings.TrimSpace(params.Cites) != "" ||
		strings.TrimSpace(params.CitedBy) != "" ||
		strings.TrimSpace(params.RelatedTo) != ""
}

// DiscoveredWork is the durable, acquisition-neutral description returned by
// a discovery search. It intentionally contains no request credentials.
type DiscoveredWork struct {
	Work         work.Work `json:"work"`
	OpenAlexID   string    `json:"openalex_id"`
	IsOA         bool      `json:"is_oa"`
	OAURL        string    `json:"oa_url"`
	CitedBy      int       `json:"cited_by"`
	Abstract     string    `json:"abstract"`
	Owned        bool      `json:"owned"`
	OwnedItemKey string    `json:"owned_item_key,omitempty"`
	Source       string    `json:"source,omitempty"`
}

// Client searches OpenAlex works.
type Client struct {
	client  HTTPClient
	email   string
	baseURL string
	version string
	maxBody int64
}

// Name identifies the OpenAlex backend.
func (c *Client) Name() string {
	return "openalex"
}

// NewWithOptions constructs a client with a bounded ten-second default HTTP
// client. Tests may inject a transport through Options.Client.
func NewWithOptions(opts Options) *Client {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = defaultVersion
	}
	maxBody := opts.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		client: client, email: strings.TrimSpace(opts.ContactEmail), baseURL: baseURL,
		version: version, maxBody: maxBody,
	}
}

// Search performs bounded OpenAlex requests and returns the mapped works. It
// never creates or mutates an acquisition job.
func (c *Client) Search(ctx context.Context, params SearchParams) ([]DiscoveredWork, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("discovery: HTTP client is not configured")
	}
	if c.email == "" {
		return nil, errors.New("discovery: contact email is required for the OpenAlex polite pool")
	}
	query := strings.TrimSpace(params.Query)
	if query == "" && !params.HasCitationSnowball() {
		return nil, errors.New("discovery: query is required unless a citation snowball DOI is supplied")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	citationFilters, err := c.citationFilters(requestCtx, params)
	if err != nil {
		return nil, err
	}
	endpoint, err := c.searchURL(params, query, citationFilters)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(requestCtx, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("discovery: OpenAlex returned HTTP %d", resp.StatusCode)
	}

	var payload searchResponse
	if err := decodeBoundedJSON(resp.Body, c.maxBody, &payload); err != nil {
		return nil, fmt.Errorf("discovery: invalid OpenAlex response: %w", err)
	}
	works := make([]DiscoveredWork, 0, len(payload.Results))
	for _, record := range payload.Results {
		works = append(works, discoveredWork(record))
	}
	return works, nil
}

func (c *Client) do(ctx context.Context, endpoint *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("discovery: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("papio/%s (mailto:%s)", c.version, c.email))
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: OpenAlex request failed: %w", err)
	}
	if resp == nil {
		return nil, errors.New("discovery: OpenAlex returned an empty response")
	}
	if resp.Body == nil {
		return nil, errors.New("discovery: OpenAlex response body is missing")
	}
	return resp, nil
}

type citationSeed struct {
	filter string
	doi    string
}

func (c *Client) citationFilters(ctx context.Context, params SearchParams) ([]string, error) {
	seeds := []citationSeed{
		{filter: "cites", doi: params.Cites},
		{filter: "cited_by", doi: params.CitedBy},
		{filter: "related_to", doi: params.RelatedTo},
	}
	resolved := make(map[string]string, len(seeds))
	filters := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if strings.TrimSpace(seed.doi) == "" {
			continue
		}
		doi, err := work.NormalizeDOI(seed.doi)
		if err != nil {
			return nil, fmt.Errorf("discovery: invalid DOI for %s: %w", seed.filter, err)
		}
		workID, ok := resolved[doi]
		if !ok {
			workID, err = c.resolveDOI(ctx, doi)
			if err != nil {
				return nil, err
			}
			resolved[doi] = workID
		}
		filters = append(filters, seed.filter+":"+workID)
	}
	return filters, nil
}

func (c *Client) resolveDOI(ctx context.Context, doi string) (string, error) {
	endpoint, err := c.seedURL(doi)
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, endpoint)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("discovery: OpenAlex seed DOI %q was not found", doi)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("discovery: OpenAlex returned HTTP %d resolving DOI %q", resp.StatusCode, doi)
	}
	var seed workRecord
	if err := decodeBoundedJSON(resp.Body, c.maxBody, &seed); err != nil {
		return "", fmt.Errorf("discovery: invalid OpenAlex seed response: %w", err)
	}
	workID := openAlexWorkID(seed.ID)
	if workID == "" {
		return "", fmt.Errorf("discovery: OpenAlex returned no work ID for DOI %q", doi)
	}
	return workID, nil
}

func (c *Client) seedURL(doi string) (*url.URL, error) {
	base, err := c.openAlexBaseURL()
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/doi:" + doi
	base.RawPath = ""
	values := base.Query()
	values.Set("mailto", c.email)
	base.RawQuery = values.Encode()
	return base, nil
}

func openAlexWorkID(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "https://openalex.org/")
	value = strings.TrimPrefix(value, "http://openalex.org/")
	if len(value) < 2 || value[0] != 'W' {
		return ""
	}
	return value
}

func (c *Client) searchURL(params SearchParams, query string, citationFilters []string) (*url.URL, error) {
	base, err := c.openAlexBaseURL()
	if err != nil {
		return nil, err
	}
	normalized := normalizeParams(params)
	values := base.Query()
	if query != "" {
		values.Set("search", query)
	}
	values.Set("per-page", strconv.Itoa(normalized.Limit))
	values.Set("mailto", c.email)
	if normalized.Slim {
		values.Set("select", slimFields)
	}
	filters := make([]string, 0, 3+len(citationFilters))
	if normalized.YearFrom > 0 {
		filters = append(filters, fmt.Sprintf("from_publication_date:%04d-01-01", normalized.YearFrom))
	}
	if normalized.YearTo > 0 {
		filters = append(filters, fmt.Sprintf("to_publication_date:%04d-12-31", normalized.YearTo))
	}
	if normalized.OAOnly {
		filters = append(filters, "open_access.is_oa:true")
	}
	filters = append(filters, citationFilters...)
	if len(filters) > 0 {
		values.Set("filter", strings.Join(filters, ","))
	}
	base.RawQuery = values.Encode()
	return base, nil
}

func (c *Client) openAlexBaseURL() (*url.URL, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("discovery: invalid OpenAlex endpoint configuration")
	}
	return base, nil
}

func normalizeParams(params SearchParams) SearchParams {
	if params.Limit == 0 {
		params.Limit = defaultLimit
	} else if params.Limit < 1 {
		params.Limit = 1
	} else if params.Limit > maxLimit {
		params.Limit = maxLimit
	}
	return params
}

type searchResponse struct {
	Results []workRecord `json:"results"`
}

type workRecord struct {
	ID                    string           `json:"id"`
	DOI                   string           `json:"doi"`
	Title                 string           `json:"title"`
	PublicationYear       int              `json:"publication_year"`
	Authorships           []authorship     `json:"authorships"`
	PrimaryLocation       *primaryLocation `json:"primary_location"`
	OpenAccess            openAccess       `json:"open_access"`
	CitedByCount          int              `json:"cited_by_count"`
	AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
}

type authorship struct {
	Author struct {
		DisplayName string `json:"display_name"`
	} `json:"author"`
}

type primaryLocation struct {
	Source *source `json:"source"`
}

type source struct {
	DisplayName string `json:"display_name"`
}

type openAccess struct {
	IsOA  bool   `json:"is_oa"`
	OAURL string `json:"oa_url"`
}

func discoveredWork(record workRecord) DiscoveredWork {
	authors := make([]string, 0, len(record.Authorships))
	for _, authorship := range record.Authorships {
		if name := strings.TrimSpace(authorship.Author.DisplayName); name != "" {
			authors = append(authors, name)
		}
	}
	container := ""
	if record.PrimaryLocation != nil && record.PrimaryLocation.Source != nil {
		container = strings.TrimSpace(record.PrimaryLocation.Source.DisplayName)
	}
	doi := ""
	if normalized, err := work.NormalizeDOI(record.DOI); err == nil {
		doi = normalized
	}
	return DiscoveredWork{
		Work: work.Work{
			DOI: doi, Title: strings.TrimSpace(record.Title), Authors: authors,
			Year: record.PublicationYear, Container: container,
		},
		OpenAlexID: strings.TrimSpace(record.ID),
		IsOA:       record.OpenAccess.IsOA,
		OAURL:      strings.TrimSpace(record.OpenAccess.OAURL),
		CitedBy:    record.CitedByCount,
		Abstract:   invertAbstract(record.AbstractInvertedIndex),
		Source:     "openalex",
	}
}

type abstractToken struct {
	position int
	word     string
}

func invertAbstract(index map[string][]int) string {
	if len(index) == 0 {
		return ""
	}
	count := 0
	for _, positions := range index {
		count += len(positions)
		if count > maxAbstractWords {
			return ""
		}
	}
	tokens := make([]abstractToken, 0, count)
	for word, positions := range index {
		for _, position := range positions {
			if position >= 0 {
				tokens = append(tokens, abstractToken{position: position, word: word})
			}
		}
	}
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].position == tokens[j].position {
			return tokens[i].word < tokens[j].word
		}
		return tokens[i].position < tokens[j].position
	})
	words := make([]string, len(tokens))
	for i, token := range tokens {
		words[i] = token.word
	}
	return strings.Join(words, " ")
}

func decodeBoundedJSON(body io.Reader, maxBody int64, into any) error {
	reader := io.LimitReader(body, maxBody+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBody {
		return errors.New("response exceeds configured limit")
	}
	return json.Unmarshal(data, into)
}
