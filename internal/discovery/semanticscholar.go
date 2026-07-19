// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"papio/internal/work"
)

const (
	defaultSemanticScholarBaseURL = "https://api.semanticscholar.org/graph/v1"
	semanticScholarMaxLimit       = 100
)

const semanticScholarFields = "externalIds,title,year,authors,isOpenAccess,openAccessPdf,citationCount,venue"

// SemanticScholarOptions configures a bounded Semantic Scholar client.
type SemanticScholarOptions struct {
	Client           HTTPClient
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// SemanticScholar searches Semantic Scholar without creating acquisition jobs.
type SemanticScholar struct {
	client  HTTPClient
	apiKey  string
	baseURL string
	maxBody int64
}

// NewSemanticScholarWithOptions constructs a client with a bounded ten-second
// default HTTP client. BaseURL is intended for an explicitly configured
// loopback development endpoint.
func NewSemanticScholarWithOptions(opts SemanticScholarOptions) *SemanticScholar {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultSemanticScholarBaseURL
	}
	maxBody := opts.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &SemanticScholar{
		client: client, apiKey: strings.TrimSpace(opts.APIKey), baseURL: baseURL, maxBody: maxBody,
	}
}

// Name identifies the Semantic Scholar backend.
func (s *SemanticScholar) Name() string {
	return "semanticscholar"
}

// Search performs a bounded Semantic Scholar query and maps returned papers.
func (s *SemanticScholar) Search(ctx context.Context, params SearchParams) ([]DiscoveredWork, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("semanticscholar: HTTP client is not configured")
	}
	query := strings.TrimSpace(params.Query)
	kind, doi, err := semanticScholarSnowball(params)
	if err != nil {
		return nil, err
	}
	if query == "" && kind == "" {
		return nil, errors.New("semanticscholar: query is required unless a citation snowball DOI is supplied")
	}

	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if kind == "" {
		endpoint, err := s.searchURL(params, query)
		if err != nil {
			return nil, err
		}
		resp, err := s.do(requestCtx, endpoint)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("semanticscholar: returned HTTP %d", resp.StatusCode)
		}
		var payload semanticScholarSearchResponse
		if err := decodeBoundedJSON(resp.Body, s.maxBody, &payload); err != nil {
			return nil, fmt.Errorf("semanticscholar: invalid response: %w", err)
		}
		return mapSemanticScholarPapers(payload.Data), nil
	}

	endpoint, err := s.snowballURL(kind, doi, params)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(requestCtx, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("semanticscholar: returned HTTP %d", resp.StatusCode)
	}
	var payload semanticScholarCitationResponse
	if err := decodeBoundedJSON(resp.Body, s.maxBody, &payload); err != nil {
		return nil, fmt.Errorf("semanticscholar: invalid response: %w", err)
	}
	papers := make([]semanticScholarPaper, 0, len(payload.Data))
	for _, citation := range payload.Data {
		if kind == "citations" {
			papers = append(papers, citation.CitingPaper)
		} else {
			papers = append(papers, citation.CitedPaper)
		}
	}
	return mapSemanticScholarPapers(papers), nil
}

func semanticScholarSnowball(params SearchParams) (string, string, error) {
	seeds := []struct {
		kind string
		doi  string
	}{
		{kind: "citations", doi: params.Cites},
		{kind: "references", doi: params.CitedBy},
		{kind: "related", doi: params.RelatedTo},
	}
	var selected struct {
		kind string
		doi  string
	}
	for _, seed := range seeds {
		if strings.TrimSpace(seed.doi) == "" {
			continue
		}
		if selected.kind != "" {
			return "", "", errors.New("semanticscholar: exactly one citation snowball parameter may be supplied")
		}
		selected = seed
	}
	if selected.kind == "" {
		return "", "", nil
	}
	if selected.kind == "related" {
		return "", "", fmt.Errorf("semanticscholar: related-to snowball is not supported")
	}
	doi, err := work.NormalizeDOI(selected.doi)
	if err != nil {
		return "", "", fmt.Errorf("semanticscholar: invalid DOI for %s: %w", selected.kind, err)
	}
	return selected.kind, doi, nil
}

func (s *SemanticScholar) do(ctx context.Context, endpoint *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("semanticscholar: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	if s.apiKey != "" {
		req.Header.Set("x-api-key", s.apiKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semanticscholar: request failed: %w", err)
	}
	if resp == nil {
		return nil, errors.New("semanticscholar: returned an empty response")
	}
	if resp.Body == nil {
		return nil, errors.New("semanticscholar: response body is missing")
	}
	return resp, nil
}

func (s *SemanticScholar) searchURL(params SearchParams, query string) (*url.URL, error) {
	base, err := s.base()
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/paper/search"
	values := base.Query()
	values.Set("query", query)
	values.Set("limit", strconv.Itoa(semanticScholarLimitFor(params.Limit)))
	values.Set("fields", semanticScholarFields)
	if year := semanticScholarYear(params); year != "" {
		values.Set("year", year)
	}
	if params.OAOnly {
		values.Set("openAccessPdf", "true")
	}
	base.RawQuery = values.Encode()
	return base, nil
}

func (s *SemanticScholar) snowballURL(kind, doi string, params SearchParams) (*url.URL, error) {
	base, err := s.base()
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/paper/DOI:" + doi + "/" + kind
	values := base.Query()
	prefix := "citingPaper"
	if kind == "references" {
		prefix = "citedPaper"
	}
	fields := strings.Split(semanticScholarFields, ",")
	for i, field := range fields {
		fields[i] = prefix + "." + field
	}
	values.Set("fields", strings.Join(fields, ","))
	values.Set("limit", strconv.Itoa(semanticScholarLimitFor(params.Limit)))
	base.RawQuery = values.Encode()
	return base, nil
}

func (s *SemanticScholar) base() (*url.URL, error) {
	base, err := url.Parse(s.baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("semanticscholar: invalid endpoint configuration")
	}
	return base, nil
}

func semanticScholarLimitFor(limit int) int {
	if limit == 0 {
		return defaultLimit
	}
	if limit < 1 {
		return 1
	}
	if limit > semanticScholarMaxLimit {
		return semanticScholarMaxLimit
	}
	return limit
}

func semanticScholarYear(params SearchParams) string {
	if params.YearFrom == 0 && params.YearTo == 0 {
		return ""
	}
	from := ""
	if params.YearFrom != 0 {
		from = strconv.Itoa(params.YearFrom)
	}
	to := ""
	if params.YearTo != 0 {
		to = strconv.Itoa(params.YearTo)
	}
	return from + "-" + to
}

type semanticScholarSearchResponse struct {
	Data []semanticScholarPaper `json:"data"`
}

type semanticScholarCitationResponse struct {
	Data []struct {
		CitingPaper semanticScholarPaper `json:"citingPaper"`
		CitedPaper  semanticScholarPaper `json:"citedPaper"`
	} `json:"data"`
}

type semanticScholarPaper struct {
	ExternalIDs struct {
		DOI   string `json:"DOI"`
		ArXiv string `json:"ArXiv"`
	} `json:"externalIds"`
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	IsOpenAccess  bool `json:"isOpenAccess"`
	OpenAccessPDF *struct {
		URL string `json:"url"`
	} `json:"openAccessPdf"`
	CitationCount int    `json:"citationCount"`
	Venue         string `json:"venue"`
}

func mapSemanticScholarPapers(papers []semanticScholarPaper) []DiscoveredWork {
	works := make([]DiscoveredWork, 0, len(papers))
	for _, paper := range papers {
		authors := make([]string, 0, len(paper.Authors))
		for _, author := range paper.Authors {
			if name := strings.TrimSpace(author.Name); name != "" {
				authors = append(authors, name)
			}
		}
		doi := strings.TrimSpace(paper.ExternalIDs.DOI)
		if normalized, err := work.NormalizeDOI(doi); err == nil {
			doi = normalized
		}
		oaURL := ""
		if paper.OpenAccessPDF != nil {
			oaURL = strings.TrimSpace(paper.OpenAccessPDF.URL)
		}
		works = append(works, DiscoveredWork{
			Work: work.Work{
				DOI: doi, ArXiv: strings.TrimSpace(paper.ExternalIDs.ArXiv),
				Title: strings.TrimSpace(paper.Title), Authors: authors,
				Year: paper.Year, Container: strings.TrimSpace(paper.Venue),
			},
			IsOA: paper.IsOpenAccess, OAURL: oaURL, CitedBy: paper.CitationCount,
			Source: "semanticscholar",
		})
	}
	return works
}
