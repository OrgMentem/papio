// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package openalex resolves open-access work locations from the OpenAlex API.
package openalex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL        = "https://api.openalex.org/works"
	defaultMaxBody        = int64(1 << 20)
	defaultSearchPageSize = 10
)

// HTTPClient is the injected HTTP dependency used to call OpenAlex.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. ContactEmail is required for OpenAlex's
// polite pool. APIKey is required when calling the official API and is sent
// only to OpenAlex as an api_key query parameter. BaseURL is the works endpoint root.
type Options struct {
	Client           HTTPClient
	ContactEmail     string
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver using OpenAlex work records.
type Resolver struct {
	client  HTTPClient
	email   string
	apiKey  string
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a resolver with the official works endpoint. An API key is
// required when Resolve calls the official endpoint.
func New(client HTTPClient, contactEmail string, apiKey ...string) *Resolver {
	key := ""
	if len(apiKey) > 0 {
		key = apiKey[0]
	}
	return NewWithOptions(Options{Client: client, ContactEmail: contactEmail, APIKey: key})
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
		client: opts.Client, email: strings.TrimSpace(opts.ContactEmail),
		apiKey: strings.TrimSpace(opts.APIKey), baseURL: baseURL, maxBody: maxBody,
	}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "openalex" }

// Resolve looks up a DOI, OpenAlex work ID, or title. A URL alone is never
// sufficient: the result must explicitly mark both the work and its selected
// location as open access.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil {
		return nil, errors.New("openalex: HTTP client is not configured")
	}
	if r.email == "" {
		return nil, errors.New("openalex: contact email is required; configure an address for the OpenAlex polite pool")
	}

	endpoint, lookup, search, err := r.lookupURL(requested)
	if err != nil {
		return nil, err
	}
	if lookup == "" {
		return nil, nil
	}
	body, err := r.fetch(ctx, endpoint)
	if err != nil || body == nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	var record workRecord
	if search {
		var results searchResponse
		if err := decodeBoundedJSON(body, r.maxBody, &results); err != nil {
			return nil, fmt.Errorf("openalex: invalid response: %w", err)
		}
		matched := false
		for _, result := range results.Results {
			if matchesTitleSearch(result, requested) {
				record, matched = result, true
				break
			}
		}
		if !matched {
			return nil, nil
		}
	} else if err := decodeBoundedJSON(body, r.maxBody, &record); err != nil {
		return nil, fmt.Errorf("openalex: invalid response: %w", err)
	}

	if !record.isOpenAccess() {
		return nil, nil
	}
	location, source, direct := chooseLocation(record.BestOALocation, record.Locations)
	if location == nil {
		return nil, nil
	}
	candidateURL := location.PDFURL
	if !direct {
		candidateURL = landingURL(location)
	}
	landing := landingURL(location)
	confidence := 1.0
	if search {
		confidence = 0.75
	}
	candidate := resolver.Candidate{
		Source: "openalex", URL: candidateURL, Landing: landing,
		Version: mapVersion(location.Version), AccessBasis: resolver.AccessOpen,
		ReuseLicense: reuseLicense(location.License), ExpectedMIME: expectedMIME(direct),
		Direct: direct, IdentityConfidence: confidence, ResolvedWork: resolvedWork(record),
		Evidence: []string{
			"openalex lookup=" + lookup,
			"openalex location=" + source,
			"openalex oa_status=" + safeEvidenceValue(record.oaStatus()),
			"openalex url=" + redact.URL(candidateURL),
		},
	}
	if err := resolver.ValidateCandidate(candidate); err != nil {
		return nil, nil
	}
	return []resolver.Candidate{candidate}, nil
}

func (r *Resolver) lookupURL(requested work.Work) (*url.URL, string, bool, error) {
	base, err := url.Parse(r.baseURL)
	if err != nil || !validHTTPURL(base.String()) {
		return nil, "", false, errors.New("openalex: invalid endpoint configuration")
	}
	if r.apiKey == "" && !isLoopbackHost(base.Hostname()) {
		return nil, "", false, errors.New("openalex: API key is required except for an explicit loopback endpoint")
	}
	lookup, search := "", false
	switch {
	case strings.TrimSpace(requested.DOI) != "":
		doi, err := work.NormalizeDOI(requested.DOI)
		if err != nil {
			return nil, "", false, nil
		}
		base.Path = strings.TrimRight(base.Path, "/") + "/https://doi.org/" + doi
		lookup = "doi"
	case strings.TrimSpace(requested.OpenAlex) != "":
		id, err := work.NormalizeOpenAlex(requested.OpenAlex)
		if err != nil {
			return nil, "", false, nil
		}
		base.Path = strings.TrimRight(base.Path, "/") + "/" + url.PathEscape(id)
		lookup = "openalex"
	case strings.TrimSpace(requested.Title) != "":
		lookup, search = "title", true
		query := base.Query()
		query.Set("search", strings.TrimSpace(requested.Title))
		query.Set("per_page", strconv.Itoa(defaultSearchPageSize))
		base.RawQuery = query.Encode()
	default:
		return base, "", false, nil
	}
	query := base.Query()
	query.Set("mailto", r.email)
	if r.apiKey != "" {
		query.Set("api_key", r.apiKey)
	}
	base.RawQuery = query.Encode()
	return base, lookup, search, nil
}

// fetch issues one authenticated OpenAlex GET and maps HTTP statuses to the
// resolver error taxonomy. A nil ReadCloser with nil error means "not found".
func (r *Resolver) fetch(ctx context.Context, endpoint *url.URL) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("openalex: could not construct request")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &resolver.TemporaryError{Err: errors.New("openalex: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("openalex: empty HTTP response")}
	}
	closeBody := func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		closeBody()
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		closeBody()
		return nil, errors.New("openalex: request was rejected (check polite-pool contact and API credentials)")
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		closeBody()
		return nil, temporaryStatus("openalex", resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		closeBody()
		return nil, temporaryStatus("openalex", resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		closeBody()
		return nil, fmt.Errorf("openalex: unexpected HTTP status %d", resp.StatusCode)
	}
	if resp.Body == nil {
		return nil, errors.New("openalex: response body is missing")
	}
	return resp.Body, nil
}

// maxSiblingCandidates bounds how many OA sibling versions one hop may emit.
const maxSiblingCandidates = 3

// ResolveSiblings finds open-access sibling versions (preprints, repository
// copies under a different DOI) of a work whose canonical identifier yielded
// no OA candidates. It fetches the canonical record even when it is not open
// access — the record supplies the authoritative title/year/authors used for
// strict sibling matching — then runs one title search. Max two OpenAlex
// requests.
func (r *Resolver) ResolveSiblings(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil || r.email == "" {
		return nil, nil
	}
	canonicalDOI, err := work.NormalizeDOI(requested.DOI)
	if err != nil {
		return nil, nil
	}
	canonical := work.Work{Title: requested.Title, Year: requested.Year, Authors: requested.Authors}
	if endpoint, lookup, _, err := r.lookupURL(work.Work{DOI: canonicalDOI}); err == nil && lookup != "" {
		body, err := r.fetch(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		if body != nil {
			var record workRecord
			decodeErr := decodeBoundedJSON(body, r.maxBody, &record)
			_ = body.Close()
			if decodeErr == nil {
				canonical = resolvedWork(record)
			}
		}
	}
	if strings.TrimSpace(canonical.Title) == "" {
		return nil, nil
	}

	endpoint, lookup, _, err := r.lookupURL(work.Work{Title: canonical.Title})
	if err != nil || lookup == "" {
		return nil, err
	}
	body, err := r.fetch(ctx, endpoint)
	if err != nil || body == nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	var results searchResponse
	if err := decodeBoundedJSON(body, r.maxBody, &results); err != nil {
		return nil, fmt.Errorf("openalex: invalid response: %w", err)
	}

	var candidates []resolver.Candidate
	for _, record := range results.Results {
		if len(candidates) >= maxSiblingCandidates {
			break
		}
		resolved := resolvedWork(record)
		if resolved.DOI == "" || resolved.DOI == canonicalDOI {
			continue
		}
		if normalizeSiblingTitle(record.Title) != normalizeSiblingTitle(canonical.Title) {
			continue
		}
		if canonical.Year != 0 {
			// A sibling of a dated canonical work must itself be dated and
			// close: an undated record is too weak a match to auto-accept.
			if record.PublicationYear == 0 {
				continue
			}
			if diff := record.PublicationYear - canonical.Year; diff < -3 || diff > 3 {
				continue
			}
		}
		if !sharesAuthorSurname(resolved.Authors, canonical.Authors) {
			continue
		}
		if !record.isOpenAccess() {
			continue
		}
		location, source, direct := chooseLocation(record.BestOALocation, record.Locations)
		if location == nil {
			continue
		}
		candidateURL := location.PDFURL
		if !direct {
			candidateURL = landingURL(location)
		}
		candidate := resolver.Candidate{
			Source: "openalex", URL: candidateURL, Landing: landingURL(location),
			Version: mapVersion(location.Version), AccessBasis: resolver.AccessOpen,
			ReuseLicense: reuseLicense(location.License), ExpectedMIME: expectedMIME(direct),
			Direct: direct, IdentityConfidence: 0.6, ResolvedWork: resolved,
			Evidence: []string{
				"openalex lookup=sibling",
				"openalex sibling_of=" + safeEvidenceValue(canonicalDOI),
				"openalex location=" + source,
				"openalex oa_status=" + safeEvidenceValue(record.oaStatus()),
				"openalex url=" + redact.URL(candidateURL),
			},
		}
		if err := resolver.ValidateCandidate(candidate); err != nil {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// normalizeSiblingTitle compares titles across publisher/preprint records,
// which frequently disagree on punctuation and dashes ("Trust: A Study" vs
// "Trust — A Study"). Non-alphanumeric runs collapse to single spaces. This
// is deliberately sibling-only: the primary title-search matcher keeps its
// narrower normalizeTitle semantics.
func normalizeSiblingTitle(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		} else {
			builder.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

// sharesAuthorSurname reports whether any author family name appears in both
// lists. Sibling matching deliberately keys on surnames only: preprint and
// publisher records frequently disagree on initials versus given names.
func sharesAuthorSurname(left, right []string) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	surnames := make(map[string]bool, len(left))
	for _, author := range left {
		if surname, _, ok := canonicalAuthor(author); ok {
			surnames[surname] = true
		}
	}
	for _, author := range right {
		if surname, _, ok := canonicalAuthor(author); ok && surnames[surname] {
			return true
		}
	}
	return false
}

type searchResponse struct {
	Results []workRecord `json:"results"`
}

type workRecord struct {
	ID              string       `json:"id"`
	DOI             string       `json:"doi"`
	IDs             identifiers  `json:"ids"`
	Title           string       `json:"title"`
	PublicationYear int          `json:"publication_year"`
	Authorships     []authorship `json:"authorships"`
	IsOA            bool         `json:"is_oa"`
	OpenAccess      *openAccess  `json:"open_access"`
	BestOALocation  *location    `json:"best_oa_location"`
	Locations       []location   `json:"locations"`
}

type identifiers struct {
	OpenAlex string `json:"openalex"`
	DOI      string `json:"doi"`
	PMID     string `json:"pmid"`
	ArXiv    string `json:"arxiv"`
}

type authorship struct {
	Author struct {
		DisplayName string `json:"display_name"`
	} `json:"author"`
}

type openAccess struct {
	IsOA     bool   `json:"is_oa"`
	OAStatus string `json:"oa_status"`
}

type location struct {
	IsOA           bool   `json:"is_oa"`
	PDFURL         string `json:"pdf_url"`
	LandingPageURL string `json:"landing_page_url"`
	License        string `json:"license"`
	Version        string `json:"version"`
}

func (r workRecord) isOpenAccess() bool { return r.OpenAccess != nil && r.OpenAccess.IsOA }
func (r workRecord) oaStatus() string {
	if r.OpenAccess == nil {
		return ""
	}
	return r.OpenAccess.OAStatus
}

func resolvedWork(record workRecord) work.Work {
	resolved := work.Work{
		Title: strings.TrimSpace(record.Title),
		Year:  record.PublicationYear,
	}
	if resolved.Year < 1 {
		resolved.Year = 0
	}
	for _, raw := range []string{record.DOI, record.IDs.DOI} {
		if doi, err := work.NormalizeDOI(raw); err == nil {
			resolved.DOI = doi
			break
		}
	}
	for _, raw := range []string{record.IDs.PMID} {
		if pmid, err := work.NormalizePMID(identifierTail(raw)); err == nil {
			resolved.PMID = pmid
			break
		}
	}
	for _, raw := range []string{record.IDs.ArXiv} {
		if arXiv, err := work.NormalizeArXiv(raw); err == nil {
			resolved.ArXiv = arXiv
			break
		}
	}
	for _, raw := range []string{record.ID, record.IDs.OpenAlex} {
		if openAlex, err := work.NormalizeOpenAlex(raw); err == nil {
			resolved.OpenAlex = openAlex
			break
		}
	}
	for _, authorship := range record.Authorships {
		if name := strings.TrimSpace(authorship.Author.DisplayName); name != "" {
			resolved.Authors = append(resolved.Authors, name)
		}
	}
	return resolved
}

func matchesTitleSearch(record workRecord, requested work.Work) bool {
	if normalizeTitle(record.Title) != normalizeTitle(requested.Title) {
		return false
	}
	if requested.Year != 0 && record.PublicationYear != requested.Year {
		return false
	}
	recordAuthors := make([]string, 0, len(record.Authorships))
	for _, authorship := range record.Authorships {
		if name := strings.TrimSpace(authorship.Author.DisplayName); name != "" {
			recordAuthors = append(recordAuthors, name)
		}
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
	leftSurname, leftInitial, leftOK := canonicalAuthor(left)
	rightSurname, rightInitial, rightOK := canonicalAuthor(right)
	return leftOK && rightOK && leftSurname == rightSurname && leftInitial == rightInitial
}

func canonicalAuthor(value string) (string, rune, bool) {
	value = strings.TrimSpace(value)
	if comma := strings.IndexRune(value, ','); comma >= 0 {
		surname := normalizeTitle(value[:comma])
		givenNames := strings.Fields(normalizeTitle(value[comma+1:]))
		if surname == "" || len(givenNames) == 0 {
			return "", 0, false
		}
		initial, ok := firstAuthorRune(givenNames[0])
		return surname, initial, ok
	}

	parts := strings.Fields(normalizeTitle(value))
	if len(parts) < 2 {
		return "", 0, false
	}
	if isAuthorInitial(parts[len(parts)-1]) {
		initial, ok := firstAuthorRune(parts[len(parts)-1])
		return strings.Join(parts[:len(parts)-1], " "), initial, ok
	}
	initial, ok := firstAuthorRune(parts[0])
	return parts[len(parts)-1], initial, ok
}

func isAuthorInitial(value string) bool {
	value = strings.Trim(value, ".")
	_, size := utf8.DecodeRuneInString(value)
	return size > 0 && size == len(value)
}

func firstAuthorRune(value string) (rune, bool) {
	for _, r := range value {
		return unicode.ToLower(r), true
	}
	return 0, false
}

func normalizeTitle(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func identifierTail(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return raw
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return raw
	}
	return path[strings.LastIndex(path, "/")+1:]
}

func chooseLocation(best *location, locations []location) (*location, string, bool) {
	if best != nil && best.IsOA && validHTTPURL(best.PDFURL) {
		return best, "best", true
	}
	for i := range locations {
		if locations[i].IsOA && validHTTPURL(locations[i].PDFURL) {
			return &locations[i], "fallback_pdf", true
		}
	}
	if best != nil && best.IsOA && landingURL(best) != "" {
		return best, "best_landing", false
	}
	for i := range locations {
		if locations[i].IsOA && landingURL(&locations[i]) != "" {
			return &locations[i], "fallback_landing", false
		}
	}
	return nil, "", false
}

func landingURL(location *location) string {
	if location == nil {
		return ""
	}
	landing := strings.TrimSpace(location.LandingPageURL)
	if validHTTPURL(landing) {
		return landing
	}
	return ""
}

func expectedMIME(direct bool) string {
	if !direct {
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
