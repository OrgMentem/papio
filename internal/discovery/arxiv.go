// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"bytes"
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

	resolverarxiv "papio/internal/resolvers/arxiv"
	"papio/internal/work"
)

const (
	defaultArxivBaseURL = "https://export.arxiv.org"
	arxivMaxTerms       = 16
	arxivMaxTermRunes   = 80
	arxivPDFBase        = "https://arxiv.org/pdf/"
)

// arxivResultWindow is the arXiv API's documented ceiling on how deep a single
// query can be paged; results past it are unreachable.
const arxivResultWindow = 30000

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
	query, err := a.searchQuery(params)
	if err != nil {
		return nil, err
	}
	endpoint, err := a.searchURL(params, query, 0, pageSize(params))
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := a.do(requestCtx, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	entries, err := resolverarxiv.DecodeBoundedAtomEntries(resp.Body, a.maxBody)
	if err != nil {
		return nil, fmt.Errorf("arxiv discovery: invalid response: %w", err)
	}
	return arxivWorks(entries), nil
}

// SearchPage is the paged form of Search, using the arXiv API's start offset.
// Completeness comes from the feed's opensearch:totalResults and its raw entry
// count, not the mapped works, because malformed entries are dropped from Works
// but still occupy a position.
//
// Ordering stability: results are sorted by submittedDate descending, so a new
// submission lands on page one and pushes every older result one position
// deeper. A persisted offset therefore re-delivers rows rather than skipping
// them as the corpus grows; only a withdrawn entry, which shifts rows
// shallower, can make a resumed walk skip one. Request pacing is the gated
// HTTP client's job, as for Search.
func (a *Arxiv) SearchPage(ctx context.Context, params SearchParams, token string) (Page, error) {
	query, err := a.searchQuery(params)
	if err != nil {
		return Page{}, err
	}
	offset := 0
	if token != "" {
		decoded, err := decodePageToken(token, a.Name(), params)
		if err != nil {
			return Page{}, err
		}
		if err := decoded.requireOffset(arxivResultWindow); err != nil {
			return Page{}, err
		}
		offset = decoded.Offset
	}
	size := min(pageSize(params), arxivResultWindow-offset)
	endpoint, err := a.searchURL(params, query, offset, size)
	if err != nil {
		return Page{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := a.do(requestCtx, endpoint)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, a.maxBody+1))
	if err != nil {
		return Page{}, fmt.Errorf("arxiv discovery: reading response: %w", err)
	}
	if int64(len(data)) > a.maxBody {
		return Page{}, errors.New("arxiv discovery: invalid response: response exceeds configured limit")
	}
	entries, err := resolverarxiv.DecodeBoundedAtomEntries(bytes.NewReader(data), a.maxBody)
	if err != nil {
		return Page{}, fmt.Errorf("arxiv discovery: invalid response: %w", err)
	}
	var counts arxivFeedCounts
	if err := xml.Unmarshal(data, &counts); err != nil {
		return Page{}, fmt.Errorf("arxiv discovery: invalid response: %w", err)
	}
	total := -1
	if raw := strings.TrimSpace(counts.TotalResults); raw != "" {
		if total, err = strconv.Atoi(raw); err != nil || total < 0 {
			return Page{}, fmt.Errorf("arxiv discovery: invalid totalResults %q", raw)
		}
	}
	page := Page{Works: arxivWorks(entries), State: PageExhausted}
	returned := len(counts.Entries)
	end := offset + returned
	switch {
	case returned == 0 && total > offset:
		// arXiv is known to answer a deep page transiently empty. Reporting
		// that as exhaustion would end the walk short, so it is a failure.
		return Page{}, fmt.Errorf("arxiv discovery: empty page at offset %d before the reported %d results", offset, total)
	case returned == 0, total >= 0 && end >= total, total < 0 && returned < size:
		return page, nil
	case end >= arxivResultWindow:
		page.State = PageTruncated
		return page, nil
	}
	page.State = PageMore
	page.Next = offsetPageToken(a.Name(), params, end)
	return page, nil
}

// arxivFeedCounts reads the parts of an Atom feed SearchPage needs to judge
// completeness. Unprefixed tags match the opensearch namespace too.
type arxivFeedCounts struct {
	TotalResults string     `xml:"totalResults"`
	Entries      []struct{} `xml:"entry"`
}

// searchQuery checks the client and parameters every search shares and
// returns the trimmed free-text query.
func (a *Arxiv) searchQuery(params SearchParams) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("arxiv discovery: HTTP client is not configured")
	}
	if params.HasCitationSnowball() {
		return "", errors.New("arxiv discovery: citation snowball is not supported")
	}
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return "", errors.New("arxiv discovery: query is required")
	}
	return query, nil
}

// do sends the request and returns a successful response whose body the
// caller must close.
func (a *Arxiv) do(ctx context.Context, endpoint *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
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
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("arxiv discovery: returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func arxivWorks(entries []resolverarxiv.AtomEntry) []DiscoveredWork {
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
	return works
}

// searchURL builds the query URL. start is omitted at zero so the first page
// matches Search's request exactly.
func (a *Arxiv) searchURL(params SearchParams, query string, start, size int) (*url.URL, error) {
	base, err := url.Parse(a.baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("arxiv discovery: invalid endpoint configuration")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/query"
	values := base.Query()
	values.Set("search_query", arxivSearchQuery(query, params))
	values.Set("sortBy", "submittedDate")
	values.Set("sortOrder", "descending")
	if start > 0 {
		values.Set("start", strconv.Itoa(start))
	}
	values.Set("max_results", strconv.Itoa(size))
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
