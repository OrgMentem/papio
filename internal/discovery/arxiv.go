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

	resolverarxiv "papio/internal/resolvers/arxiv"
	"papio/internal/work"
)

const (
	defaultArxivBaseURL = "https://export.arxiv.org"
	arxivMaxTerms       = 16
	arxivMaxTermRunes   = 80
	arxivPDFBase        = "https://arxiv.org/pdf/"
)

var arxivTermEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// ArxivOptions configures a bounded arXiv discovery client.
type ArxivOptions struct {
	Client           HTTPClient
	BaseURL          string
	MaxResponseBytes int64
}

// Arxiv searches the arXiv Atom API without creating acquisition jobs.
type Arxiv struct {
	client  HTTPClient
	baseURL string
	maxBody int64
}

// NewArxivWithOptions constructs an arXiv discovery client.
func NewArxivWithOptions(opts ArxivOptions) *Arxiv {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultArxivBaseURL
	}
	maxBody := opts.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Arxiv{client: client, baseURL: baseURL, maxBody: maxBody}
}

// Name identifies the arXiv discovery backend.
func (*Arxiv) Name() string { return "arxiv" }

// Search performs a bounded arXiv Atom query and maps valid entries.
func (a *Arxiv) Search(ctx context.Context, params SearchParams) ([]DiscoveredWork, error) {
	if a == nil || a.client == nil {
		return nil, errors.New("arxiv discovery: HTTP client is not configured")
	}
	if params.HasCitationSnowball() {
		return nil, errors.New("arxiv discovery: citation snowball is not supported")
	}
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return nil, errors.New("arxiv discovery: query is required")
	}
	endpoint, err := a.searchURL(params, query)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("arxiv discovery: could not construct request")
	}
	req.Header.Set("Accept", "application/atom+xml")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("arxiv discovery: request failed: %w", err)
	}
	if resp == nil {
		return nil, errors.New("arxiv discovery: returned an empty response")
	}
	if resp.Body == nil {
		return nil, errors.New("arxiv discovery: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("arxiv discovery: returned HTTP %d", resp.StatusCode)
	}
	entries, err := resolverarxiv.DecodeBoundedAtomEntries(resp.Body, a.maxBody)
	if err != nil {
		return nil, fmt.Errorf("arxiv discovery: invalid response: %w", err)
	}
	works := make([]DiscoveredWork, 0, len(entries))
	for _, entry := range entries {
		works = append(works, DiscoveredWork{
			Work: work.Work{
				ArXiv:   entry.ArXivID,
				Title:   entry.Title,
				Authors: entry.Authors,
				Year:    entry.Year,
			},
			IsOA:      true,
			OAURL:     arxivPDFBase + entry.BaseID,
			Source:    "arxiv",
			MatchKind: MatchUnscored,
		})
	}
	return works, nil
}

func (a *Arxiv) searchURL(params SearchParams, query string) (*url.URL, error) {
	base, err := url.Parse(a.baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("arxiv discovery: invalid endpoint configuration")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/query"
	values := base.Query()
	values.Set("search_query", arxivSearchQuery(query, params))
	values.Set("sortBy", "submittedDate")
	values.Set("sortOrder", "descending")
	values.Set("max_results", strconv.Itoa(normalizeParams(params).Limit))
	base.RawQuery = values.Encode()
	return base, nil
}

func arxivSearchQuery(query string, params SearchParams) string {
	terms := strings.Fields(query)
	if len(terms) > arxivMaxTerms {
		terms = terms[:arxivMaxTerms]
	}
	clauses := make([]string, 0, len(terms)+1)
	for _, term := range terms {
		term = truncateArxivTerm(term)
		term = arxivTermEscaper.Replace(term)
		clauses = append(clauses, `all:"`+term+`"`)
	}
	if params.YearFrom > 0 || params.YearTo > 0 {
		from := "000001010000"
		if params.YearFrom > 0 {
			from = fmt.Sprintf("%04d01010000", params.YearFrom)
		}
		to := "999912312359"
		if params.YearTo > 0 {
			to = fmt.Sprintf("%04d12312359", params.YearTo)
		}
		clauses = append(clauses, "submittedDate:["+from+" TO "+to+"]")
	}
	return strings.Join(clauses, " AND ")
}

func truncateArxivTerm(term string) string {
	runes := 0
	for index := range term {
		if runes == arxivMaxTermRunes {
			return term[:index]
		}
		runes++
	}
	return term
}
