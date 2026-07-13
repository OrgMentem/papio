// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package europepmc resolves works to legal open-access full text via the
// Europe PMC REST search API. It queries by DOI, PMID, or title, parses the
// fullTextUrlList and publication metadata, and emits candidates only for
// results Europe PMC marks open access, filtering the URL list to its OA
// entries. Non-OA hits, empty results, and 404s are (nil, nil); 429/5xx and
// network failures are retryable resolver.TemporaryError.
package europepmc

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
	defaultBaseURL  = "https://www.ebi.ac.uk/europepmc/webservices/rest"
	defaultMaxBody  = int64(4 << 20)
	defaultPageSize = 10
)

// HTTPClient is the injected HTTP dependency used to call Europe PMC.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. BaseURL overrides the REST root for tests or
// explicitly configured development endpoints.
type Options struct {
	Client           HTTPClient
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver using Europe PMC's REST search.
type Resolver struct {
	client  HTTPClient
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a resolver with the official Europe PMC endpoint.
func New(client HTTPClient) *Resolver {
	return NewWithOptions(Options{Client: client})
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
	return &Resolver{client: opts.Client, baseURL: baseURL, maxBody: maxBody}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "europepmc" }

// Resolve searches Europe PMC and returns legal OA full-text candidates.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil {
		return nil, errors.New("europepmc: HTTP client is not configured")
	}

	queryString, mode := buildQuery(requested)
	if queryString == "" {
		return nil, nil
	}

	endpoint, err := url.Parse(r.baseURL + "/search")
	if err != nil {
		return nil, errors.New("europepmc: invalid endpoint configuration")
	}
	params := endpoint.Query()
	params.Set("query", queryString)
	params.Set("format", "json")
	params.Set("resultType", "core")
	params.Set("pageSize", strconv.Itoa(defaultPageSize))
	endpoint.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("europepmc: could not construct request")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &resolver.TemporaryError{Err: errors.New("europepmc: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("europepmc: empty HTTP response")}
	}
	if resp.Body == nil {
		return nil, errors.New("europepmc: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, temporaryStatus("europepmc", resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		return nil, temporaryStatus("europepmc", resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("europepmc: unexpected HTTP status %d", resp.StatusCode)
	}

	var record searchResponse
	if err := decodeBoundedJSON(resp.Body, r.maxBody, &record); err != nil {
		return nil, fmt.Errorf("europepmc: invalid response: %w", err)
	}

	result := selectResult(record.ResultList.Result, requested, mode)
	if result == nil {
		return nil, nil
	}
	// Legal-OA gate: only Europe PMC's own open-access flag authorizes emitting
	// a full-text candidate. "Free to read" is not a reuse/redistribution basis.
	if !strings.EqualFold(strings.TrimSpace(result.IsOpenAccess), "Y") {
		return nil, nil
	}

	confidence := 0.6
	if mode == matchDOI || mode == matchPMID {
		confidence = 0.95
	}
	license := reuseLicense(result.License)
	sourceID := safeEvidenceValue(strings.TrimSpace(result.Source) + "/" + strings.TrimSpace(result.ID))
	resolved := resolvedWork(result)

	var pdfEntries []fullTextURL
	var htmlLanding string
	for _, ft := range result.FullTextUrlList.FullTextURL {
		if !strings.EqualFold(strings.TrimSpace(ft.AvailabilityCode), "OA") {
			continue
		}
		if !validHTTPURL(ft.URL) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(ft.DocumentStyle)) {
		case "pdf":
			pdfEntries = append(pdfEntries, ft)
		case "html", "doi":
			if htmlLanding == "" {
				htmlLanding = strings.TrimSpace(ft.URL)
			}
		}
	}

	var candidates []resolver.Candidate
	for _, ft := range pdfEntries {
		candidate := resolver.Candidate{
			Source:             "europepmc",
			URL:                strings.TrimSpace(ft.URL),
			Landing:            htmlLanding,
			Version:            resolver.VersionPublished, // Europe PMC OA full text is the version of record
			AccessBasis:        resolver.AccessOpen,
			ReuseLicense:       license,
			ExpectedMIME:       "application/pdf",
			ResolvedWork:       resolved,
			Direct:             true,
			IdentityConfidence: confidence,
			Evidence: []string{
				"europepmc match=" + string(mode),
				"europepmc source_id=" + sourceID,
				"europepmc site=" + safeEvidenceValue(ft.Site),
				"europepmc url=" + redact.URL(ft.URL),
			},
		}
		if err := resolver.ValidateCandidate(candidate); err != nil {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 && htmlLanding != "" {
		// No direct PDF, but an OA landing page exists: emit it as a
		// non-direct candidate rather than dropping the OA result.
		candidate := resolver.Candidate{
			Source:             "europepmc",
			URL:                htmlLanding,
			Landing:            "",
			Version:            resolver.VersionPublished,
			AccessBasis:        resolver.AccessOpen,
			ReuseLicense:       license,
			ExpectedMIME:       "text/html",
			ResolvedWork:       resolved,
			Direct:             false,
			IdentityConfidence: confidence,
			Evidence: []string{
				"europepmc match=" + string(mode),
				"europepmc source_id=" + sourceID,
				"europepmc url=" + redact.URL(htmlLanding),
			},
		}
		if err := resolver.ValidateCandidate(candidate); err == nil {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates, nil
}

type matchMode string

const (
	matchDOI   matchMode = "doi"
	matchPMID  matchMode = "pmid"
	matchTitle matchMode = "title"
)

// buildQuery builds the Europe PMC query string and records how the work was
// matched. DOI wins over PMID over title (strongest identity first).
func buildQuery(requested work.Work) (string, matchMode) {
	if doi := strings.TrimSpace(requested.DOI); doi != "" {
		if normalized, err := work.NormalizeDOI(doi); err == nil {
			return `DOI:"` + normalized + `"`, matchDOI
		}
	}
	if pmid := strings.TrimSpace(requested.PMID); pmid != "" {
		if normalized, err := work.NormalizePMID(pmid); err == nil {
			return "EXT_ID:" + normalized + " AND SRC:MED", matchPMID
		}
	}
	if title := strings.TrimSpace(requested.Title); title != "" {
		return `TITLE:"` + sanitizeTitle(title) + `"`, matchTitle
	}
	return "", ""
}

// selectResult picks the result that matches the requested identity, rejecting
// clear mismatches so a wrong work is never emitted.
func selectResult(results []epmcResult, requested work.Work, mode matchMode) *epmcResult {
	if len(results) == 0 {
		return nil
	}
	switch mode {
	case matchDOI:
		want, _ := work.NormalizeDOI(requested.DOI)
		for i := range results {
			got := strings.TrimSpace(results[i].DOI)
			if got == "" || strings.EqualFold(got, want) {
				return &results[i]
			}
		}
		return nil
	case matchPMID:
		want, _ := work.NormalizePMID(requested.PMID)
		for i := range results {
			got := strings.TrimSpace(results[i].PMID)
			if got == "" || got == want {
				return &results[i]
			}
		}
		return nil
	case matchTitle:
		want := normalizeTitle(requested.Title)
		for i := range results {
			if normalizeTitle(results[i].Title) == want {
				return &results[i]
			}
		}
		return nil
	}
	return nil
}

type searchResponse struct {
	HitCount   int `json:"hitCount"`
	ResultList struct {
		Result []epmcResult `json:"result"`
	} `json:"resultList"`
}

type epmcResult struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	PMID            string `json:"pmid"`
	DOI             string `json:"doi"`
	Title           string `json:"title"`
	AuthorString    string `json:"authorString"`
	PubYear         string `json:"pubYear"`
	IsOpenAccess    string `json:"isOpenAccess"`
	License         string `json:"license"`
	FullTextUrlList struct {
		FullTextURL []fullTextURL `json:"fullTextUrl"`
	} `json:"fullTextUrlList"`
}

type fullTextURL struct {
	Availability     string `json:"availability"`
	AvailabilityCode string `json:"availabilityCode"`
	DocumentStyle    string `json:"documentStyle"`
	Site             string `json:"site"`
	URL              string `json:"url"`
}

// resolvedWork carries the source-discovered bibliographic identity so the app
// can fill missing request fields after its own consistency checks. Only fields
// the Europe PMC record actually supports are set; nothing is fabricated.
func resolvedWork(result *epmcResult) work.Work {
	resolved := work.Work{Title: safeEvidenceValue(result.Title)}
	if doi, err := work.NormalizeDOI(result.DOI); err == nil {
		resolved.DOI = doi
	}
	if pmid, err := work.NormalizePMID(result.PMID); err == nil {
		resolved.PMID = pmid
	}
	for _, name := range strings.Split(result.AuthorString, ",") {
		if name = strings.TrimSpace(name); name != "" {
			resolved.Authors = append(resolved.Authors, name)
		}
	}
	if year, err := strconv.Atoi(strings.TrimSpace(result.PubYear)); err == nil {
		resolved.Year = year
	}
	return resolved
}

func reuseLicense(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	return value
}

func normalizeTitle(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// sanitizeTitle removes characters that would break the Lucene-style query.
func sanitizeTitle(value string) string {
	replacer := strings.NewReplacer(`"`, " ", `\`, " ", "\n", " ", "\r", " ")
	return strings.Join(strings.Fields(replacer.Replace(value)), " ")
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func safeEvidenceValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return strings.Join(strings.Fields(value), " ")
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
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return deadline.Sub(now)
	}
	return 0
}
