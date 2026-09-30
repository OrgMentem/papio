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

// semanticScholarSearchWindow is how many relevance-ranked results
// /paper/search will page through (offset+limit may not exceed it).
const semanticScholarSearchWindow = 1000

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
	query, kind, doi, err := s.searchInputs(params)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if kind == "" {
		endpoint, err := s.searchURL(params, query, 0, semanticScholarLimitFor(params.Limit))
		if err != nil {
			return nil, err
		}
		payload, err := s.fetchSearch(requestCtx, endpoint)
		if err != nil {
			return nil, err
		}
		return mapSemanticScholarPapers(payload.Data), nil
	}
	endpoint, err := s.snowballURL(kind, doi, 0, semanticScholarSnowballFetchLimit(params))
	if err != nil {
		return nil, err
	}
	payload, err := s.fetchCitations(requestCtx, endpoint)
	if err != nil {
		return nil, err
	}
	return mapSemanticScholarPapers(filterSemanticScholarSnowballPapers(payload.papers(kind), params)), nil
}

// SearchPage is the paged form of Search, using Semantic Scholar's offset
// paging on both /paper/search and the citation snowball endpoints. The page
// size is capped at MaxLimit, below Search's own 100-row ceiling, so every
// backend shares one per-call bound.
//
// A snowball page filtered client-side by year or OA-only fetches a larger raw
// slice and keeps at most the page size; the next offset counts only the raw
// rows actually examined, so a row filtered out of one page is never skipped
// by the next.
//
// Ordering stability: /paper/search is relevance-ranked and reranks as the
// corpus changes, so a persisted offset can skip or repeat papers; it also
// stops at a 1,000-result window, reported as PageTruncated. The citation and
// reference lists have no documented order; in practice they are stable
// between runs for a given seed, and a new citing paper can shift a persisted
// offset in either direction. Callers treating a walk as coverage should
// restart from an empty token once it ends.
func (s *SemanticScholar) SearchPage(ctx context.Context, params SearchParams, token string) (Page, error) {
	query, kind, doi, err := s.searchInputs(params)
	if err != nil {
		return Page{}, err
	}
	window := 0
	if kind == "" {
		window = semanticScholarSearchWindow
	}
	offset := 0
	if token != "" {
		decoded, err := decodePageToken(token, s.Name(), params)
		if err != nil {
			return Page{}, err
		}
		if err := decoded.requireOffset(window); err != nil {
			return Page{}, err
		}
		offset = decoded.Offset
	}
	size := pageSize(params)
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var papers []semanticScholarPaper
	var returned, consumed int
	var hasNext bool
	total := -1
	if kind == "" {
		endpoint, err := s.searchURL(params, query, offset, min(size, window-offset))
		if err != nil {
			return Page{}, err
		}
		payload, err := s.fetchSearch(requestCtx, endpoint)
		if err != nil {
			return Page{}, err
		}
		papers, returned, consumed = payload.Data, len(payload.Data), len(payload.Data)
		hasNext, total = payload.Next != nil, payload.Total
	} else {
		fetch := size
		if semanticScholarSnowballFiltered(params) {
			fetch = semanticScholarMaxLimit
		}
		endpoint, err := s.snowballURL(kind, doi, offset, fetch)
		if err != nil {
			return Page{}, err
		}
		payload, err := s.fetchCitations(requestCtx, endpoint)
		if err != nil {
			return Page{}, err
		}
		raw := payload.papers(kind)
		returned, hasNext = len(raw), payload.Next != nil
		papers, consumed = filterSemanticScholarSnowballPage(raw, params, size)
	}
	page := Page{Works: mapSemanticScholarPapers(papers), State: PageExhausted}
	end := offset + consumed
	switch {
	case consumed < returned:
		// Rows already fetched but past the page size are still pending.
	case !hasNext:
		// The API omits next at the end of the list, and also at the search
		// window even when more matched; total tells the two apart.
		if window > 0 && end >= window && total > end {
			page.State = PageTruncated
		}
		return page, nil
	case returned == 0:
		return Page{}, errors.New("semanticscholar: pagination did not advance")
	case window > 0 && end >= window:
		page.State = PageTruncated
		return page, nil
	}
	page.State = PageMore
	page.Next = offsetPageToken(s.Name(), params, end)
	return page, nil
}

// searchInputs checks the client and parameters every search shares. It
// returns the trimmed query, or the snowball kind and normalized seed DOI.
func (s *SemanticScholar) searchInputs(params SearchParams) (string, string, string, error) {
	if s == nil || s.client == nil {
		return "", "", "", errors.New("semanticscholar: HTTP client is not configured")
	}
	query := strings.TrimSpace(params.Query)
	kind, doi, err := semanticScholarSnowball(params)
	if err != nil {
		return "", "", "", err
	}
	if kind != "" && query != "" {
		return "", "", "", errors.New("semanticscholar: text query cannot be combined with a citation snowball")
	}
	if query == "" && kind == "" {
		return "", "", "", errors.New("semanticscholar: query is required unless a citation snowball DOI is supplied")
	}
	return query, kind, doi, nil
}

func (s *SemanticScholar) fetchSearch(ctx context.Context, endpoint *url.URL) (semanticScholarSearchResponse, error) {
	var payload semanticScholarSearchResponse
	err := s.fetchJSON(ctx, endpoint, &payload)
	return payload, err
}

func (s *SemanticScholar) fetchCitations(ctx context.Context, endpoint *url.URL) (semanticScholarCitationResponse, error) {
	var payload semanticScholarCitationResponse
	err := s.fetchJSON(ctx, endpoint, &payload)
	return payload, err
}

func (s *SemanticScholar) fetchJSON(ctx context.Context, endpoint *url.URL, into any) error {
	resp, err := s.do(ctx, endpoint)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("semanticscholar: returned HTTP %d", resp.StatusCode)
	}
	if err := decodeBoundedJSON(resp.Body, s.maxBody, into); err != nil {
		return fmt.Errorf("semanticscholar: invalid response: %w", err)
	}
	return nil
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

// searchURL builds a /paper/search URL. offset is omitted at zero so the first
// page matches Search's request exactly.
func (s *SemanticScholar) searchURL(params SearchParams, query string, offset, limit int) (*url.URL, error) {
	base, err := s.base()
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/paper/search"
	values := base.Query()
	values.Set("query", query)
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	values.Set("limit", strconv.Itoa(limit))
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

func (s *SemanticScholar) snowballURL(kind, doi string, offset, limit int) (*url.URL, error) {
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
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	values.Set("limit", strconv.Itoa(limit))
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

func semanticScholarSnowballFetchLimit(params SearchParams) int {
	if semanticScholarSnowballFiltered(params) {
		return semanticScholarMaxLimit
	}
	return semanticScholarLimitFor(params.Limit)
}

// semanticScholarSnowballFiltered reports whether snowball rows are filtered
// client-side, which needs a larger raw fetch to fill a page.
func semanticScholarSnowballFiltered(params SearchParams) bool {
	return params.YearFrom != 0 || params.YearTo != 0 || params.OAOnly
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

func filterSemanticScholarSnowballPapers(papers []semanticScholarPaper, params SearchParams) []semanticScholarPaper {
	filtered, _ := filterSemanticScholarSnowballPage(papers, params, semanticScholarLimitFor(params.Limit))
	return filtered
}

// filterSemanticScholarSnowballPage keeps up to limit papers that pass the
// client-side filters and reports how many raw papers it examined, so a paged
// caller resumes right after the last one rather than after the whole slice.
func filterSemanticScholarSnowballPage(papers []semanticScholarPaper, params SearchParams, limit int) ([]semanticScholarPaper, int) {
	filtered := make([]semanticScholarPaper, 0, min(len(papers), limit))
	for i, paper := range papers {
		if paper.Year == 0 && (params.YearFrom != 0 || params.YearTo != 0) {
			continue
		}
		if params.YearFrom != 0 && paper.Year < params.YearFrom {
			continue
		}
		if params.YearTo != 0 && paper.Year > params.YearTo {
			continue
		}
		if params.OAOnly && !paper.IsOpenAccess {
			continue
		}
		filtered = append(filtered, paper)
		if len(filtered) == limit {
			return filtered, i + 1
		}
	}
	return filtered, len(papers)
}

// semanticScholarSearchResponse is a /paper/search page. Next is absent on the
// last page and at the relevance window.
type semanticScholarSearchResponse struct {
	Total int                    `json:"total"`
	Next  *int                   `json:"next"`
	Data  []semanticScholarPaper `json:"data"`
}

// semanticScholarCitationResponse is a citations or references page. Next is
// absent on the last page.
type semanticScholarCitationResponse struct {
	Next *int `json:"next"`
	Data []struct {
		CitingPaper semanticScholarPaper `json:"citingPaper"`
		CitedPaper  semanticScholarPaper `json:"citedPaper"`
	} `json:"data"`
}

// papers selects the side of each citation edge the snowball kind asks for.
func (payload semanticScholarCitationResponse) papers(kind string) []semanticScholarPaper {
	papers := make([]semanticScholarPaper, 0, len(payload.Data))
	for _, citation := range payload.Data {
		if kind == "citations" {
			papers = append(papers, citation.CitingPaper)
		} else {
			papers = append(papers, citation.CitedPaper)
		}
	}
	return papers
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
		arxivRaw := strings.TrimSpace(paper.ExternalIDs.ArXiv)
		arxiv := ""
		if arxivRaw != "" {
			if normalized, err := work.NormalizeArXiv(arxivRaw); err == nil {
				arxiv = normalized
			}
		}
		oaURL := ""
		if paper.OpenAccessPDF != nil {
			oaURL = strings.TrimSpace(paper.OpenAccessPDF.URL)
		}
		works = append(works, DiscoveredWork{
			Work: work.Work{
				DOI: doi, ArXiv: arxiv,
				Title: strings.TrimSpace(paper.Title), Authors: authors,
				Year: paper.Year, Container: strings.TrimSpace(paper.Venue),
			},
			IsOA: paper.IsOpenAccess, OAURL: oaURL, CitedBy: paper.CitationCount,
			Source: "semanticscholar",
		})
	}
	return works
}
