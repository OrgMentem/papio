// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package openalex resolves open-access work locations from the OpenAlex API.
package openalex

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
	"sync"
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
	// workSelectFields bounds every works response to the fields this adapter
	// actually reads. OpenAlex supports select on the singleton endpoint as
	// well as on searches, and a full work record is an order of magnitude
	// larger than the parts used here.
	workSelectFields = "id,doi,ids,title,publication_year,authorships,open_access,best_oa_location,locations"
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

	// records memoizes DOI singleton lookups so the sibling hop can reuse the
	// canonical record a preceding Resolve already paid for instead of GETting
	// it again. It holds metadata for title/year/author matching only — never a
	// candidate URL — so a stale entry carries no dead-link risk.
	mu      sync.Mutex
	records map[string]recordMemo
}

type recordMemo struct {
	record workRecord
	found  bool
	at     time.Time
}

const recordMemoTTL = 2 * time.Minute
const recordMemoCap = 512

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
		records: make(map[string]recordMemo),
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

	anon := resolver.AnonymousCredentials(ctx)
	endpoint, lookup, search, err := r.lookupURL(requested, anon)
	if err != nil {
		return nil, err
	}
	if lookup == "" {
		return nil, nil
	}
	// memoDOI is the normalized DOI this lookup keyed on, when it keyed on one;
	// memoOpenAlex likewise for the other exact endpoint. lookupURL does not
	// expose either, and neither can fail here — lookupURL already validated the
	// same input — but the error is checked before any memo write.
	memoDOI, memoOpenAlex := "", ""
	switch lookup {
	case "doi":
		if doi, doiErr := work.NormalizeDOI(requested.DOI); doiErr == nil {
			memoDOI = doi
		}
	case "openalex":
		if id, idErr := work.NormalizeOpenAlex(requested.OpenAlex); idErr == nil {
			memoOpenAlex = id
		}
	}
	body, err := r.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if body == nil {
		// A 404 is a durable answer about this DOI: remember the absence so a
		// sibling hop in the same pass does not re-ask.
		if memoDOI != "" {
			r.writeMemo(memoDOI, workRecord{}, false)
		}
		return nil, nil
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
	} else if memoDOI != "" && !echoesDOI(record, memoDOI) {
		// A misrouted or duplicated upstream answer must not reach the memo
		// either: ResolveSiblings reads it later for its search basis, so
		// caching an unverified record would launder a wrong-work title into a
		// sibling search that no longer has the requested DOI to check against.
		return nil, nil
	} else if memoOpenAlex != "" && !echoesOpenAlex(record, memoOpenAlex) {
		// The SAME rule for the other exact endpoint. Both publish at
		// IdentityConfidence 1.0, so both must prove the response is about the
		// identity that was requested; verifying only the DOI path left an
		// OpenAlex-ID lookup trusting whatever came back.
		return nil, nil
	} else if memoDOI != "" {
		// Written before OA/candidate filtering: a paywalled record still
		// carries the authoritative bibliography sibling matching needs.
		r.writeMemo(memoDOI, record, true)
	}

	// Every EXACT lookup must come back about the identity that was asked for:
	// without that, a misrouted or duplicated upstream answer is published with
	// IdentityConfidence 1.0, acquiring a different paper under this citation.
	// Verified above, before the memo write.
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

// lookupURL builds the works endpoint for whichever identifier the work
// carries. anon omits the configured API key: the keyed identity's own
// daily-quota signal has sent this call to OpenAlex's keyless tier, which is a
// separate identity with its own budget.
func (r *Resolver) lookupURL(requested work.Work, anon bool) (*url.URL, string, bool, error) {
	base, err := url.Parse(r.baseURL)
	if err != nil || !validHTTPURL(base.String()) {
		return nil, "", false, errors.New("openalex: invalid endpoint configuration")
	}
	// The OpenAlex works API is free in the polite pool: a contact email is
	// the requirement (enforced by the Resolve/ResolveSiblings entry checks),
	// an API key is optional premium capacity. The discovery client takes the
	// same stance.
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
	query.Set("select", workSelectFields)
	// Unconditional Del before the key is re-applied: a dev base URL may carry
	// its own api_key, and an "anonymous" request that silently stayed keyed
	// would be metered against the wrong identity by both the observer and the
	// budget manager.
	query.Del("api_key")
	if r.apiKey != "" && !anon {
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
		// Wrap, never replace. Which typed causes actually arrive here depends on
		// the wiring, and the earlier version of this comment asserted one that
		// is only half true: resolverEntries injects a bare sourcegate.Observer
		// (admission for that path happens upstream at the app.go AcquireAny
		// call site, which is why it is deliberately NOT wrapped in
		// sourcegate.Client), while the discovery wiring does wrap one. So the
		// causes worth preserving are the Observer's own *ErrQuotaLatched and,
		// on the wrapped paths, *budget.ErrDeferred / *budget.ErrExceeded.
		// Substituting a fresh error destroyed all of them: a refusal naming a
		// specific identity and reset instant arrived as an undifferentiated
		// transport failure and cycled through generic retry instead of parking.
		// TemporaryError unwraps, so errors.As finds the cause again while the
		// retry classification stays as it was.
		return nil, &resolver.TemporaryError{Err: fmt.Errorf("openalex: request failed: %w", err)}
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
// no OA candidates. At most one OpenAlex request (the title search); the
// canonical record, when needed for matching, is reused from a preceding
// same-DOI Resolve call (TTL-fresh) or falls back to the requested work's own
// metadata — never fetched here. With neither, returns
// resolver.ErrNoSearchBasis without any request.
func (r *Resolver) ResolveSiblings(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil || r.email == "" {
		return nil, nil
	}
	canonicalDOI, err := work.NormalizeDOI(requested.DOI)
	if err != nil {
		return nil, nil
	}
	anon := resolver.AnonymousCredentials(ctx)
	canonical := work.Work{Title: requested.Title, Year: requested.Year, Authors: requested.Authors}
	// A fresh negative memo means only "no MEMO-derived basis": it proves the
	// provider does not currently resolve DOI X, not that the caller's own
	// title and authors are unusable for finding a sibling indexed under its own
	// DOI. Treating it as a full stop suppressed perfectly good caller
	// metadata.
	// A positive memo wins ONLY if it yields a usable basis. Replacing the
	// caller's metadata wholesale let a positive-but-incomplete record — a
	// canonical work with a title and no legible authors, say — suppress caller
	// authors and cancel the hop, which is the same defect the negative-memo
	// case had, arriving by the opposite route.
	if record, ok := r.recordFor(canonicalDOI); ok {
		if memoized := resolvedWork(record); usableSiblingBasis(memoized) {
			canonical = memoized
		}
	}
	if !usableSiblingBasis(canonical) {
		// Zero requests were made, and the caller must not charge one.
		//
		// "Usable" means everything the post-search acceptance predicate
		// requires, not merely a non-empty title. Every result is later required
		// to share an author surname, and sharesAuthorSurname returns false
		// whenever either list is empty — so a title with no canonicalizable
		// author bought a ten-credit search that could not produce a candidate
		// under any response.
		//
		// This deliberately does NOT re-earn a basis with a singleton lookup.
		// That was tried and reverted: paying one credit here breaks the
		// contract this sentinel exists to state (the caller reads it as "the
		// adapter made no request at all" and skips charging the pass), and it
		// let a single admission cover two HTTP requests — so the ten-credit
		// search could still go out after the singleton's own response had
		// already installed the quota floor. The availability gap it was meant
		// to close is real but belongs to the caller, which can admit and
		// charge two calls separately; see dev/active/openalex-spend-remainders.md.
		return nil, resolver.ErrNoSearchBasis
	}

	endpoint, lookup, _, err := r.lookupURL(work.Work{Title: canonical.Title}, anon)
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

// usableSiblingBasis reports whether a work carries enough to make the
// ten-credit title search capable of yielding a candidate. It is deliberately
// tied to the acceptance side: a search whose every possible result would be
// rejected must never be bought, so this requires at least what
// sharesAuthorSurname needs — a title to search on, and one author surname the
// canonicalizer can actually read.
func usableSiblingBasis(basis work.Work) bool {
	if strings.TrimSpace(basis.Title) == "" {
		return false
	}
	for _, author := range basis.Authors {
		if _, _, ok := canonicalAuthor(author); ok {
			return true
		}
	}
	return false
}

// writeMemo records what a DOI singleton lookup learned, so a sibling hop in
// the same pass can match against it without paying for the record again.
//
// At the cap, expired entries are evicted first; if none had expired, exactly
// one oldest entry is evicted. Dropping the whole map was a real availability
// bug rather than a tidy simplification, and bounding it to "only when expiry
// frees nothing" merely moved the trigger to 512 simultaneously fresh entries:
// a DOI-only job whose caller metadata has no title depends on this memo for
// its search basis, so unrelated traffic pushing the resolver past the cap
// between Resolve and the sibling hop silently removed that job's ability to
// search at all.
func (r *Resolver) writeMemo(doi string, record workRecord, found bool) {
	key := "doi:" + doi
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records == nil {
		r.records = make(map[string]recordMemo)
	}
	// Only a write that GROWS the map can breach the cap. Refreshing a key
	// already present used to run eviction anyway, so unrelated repeat traffic
	// at capacity destroyed a live job's canonical basis while leaving the map
	// exactly the size it already was - a bounded victim chosen for no reason.
	_, replacing := r.records[key]
	if !replacing && len(r.records) >= recordMemoCap {
		now := time.Now()
		for existing, entry := range r.records {
			if now.Sub(entry.at) > recordMemoTTL {
				delete(r.records, existing)
			}
		}
		// If nothing had expired, evict the single oldest entry. Dropping the
		// whole map instead reintroduced exactly the availability failure this
		// cap-handling exists to avoid, merely moving its trigger: 512
		// simultaneously fresh entries between a job's Resolve and its sibling
		// hop deleted that job's only canonical basis, and the hop then reported
		// no search basis at all. One bounded victim keeps the cap honest.
		for len(r.records) >= recordMemoCap {
			oldestKey, oldestAt := "", time.Time{}
			for key, entry := range r.records {
				if oldestKey == "" || entry.at.Before(oldestAt) {
					oldestKey, oldestAt = key, entry.at
				}
			}
			if oldestKey == "" {
				break
			}
			delete(r.records, oldestKey)
		}
	}
	r.records[key] = recordMemo{record: record, found: found, at: time.Now()}
}

// echoesDOI reports whether a record is about the DOI that was requested.
//
// Fail-closed in three ways, because the caller publishes a match at
// IdentityConfidence 1.0. At least one parseable DOI is required, so a record
// echoing nothing legible is rejected; EVERY parseable echo must equal the
// requested DOI, so an internally inconsistent record — `doi` naming the
// requested work while `ids.doi` names another — is rejected rather than
// accepted on whichever field happens to parse first; and normalization is the
// acquisition-side, version-preserving form, so v2 is not v1 here.
func echoesDOI(record workRecord, want string) bool {
	parsed := 0
	for _, raw := range []string{record.DOI, record.IDs.DOI} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		doi, err := work.NormalizeDOI(raw)
		if err != nil {
			return false
		}
		if doi != want {
			return false
		}
		parsed++
	}
	return parsed > 0
}

// echoesOpenAlex is echoesDOI for the other exact endpoint, with the same
// fail-closed rules: at least one parseable echo is required, every parseable
// echo must equal the requested id, and normalization is the acquisition-side
// version-preserving form. Both endpoints publish at IdentityConfidence 1.0, so
// both must prove the response describes the identity that was requested.
func echoesOpenAlex(record workRecord, want string) bool {
	parsed := 0
	for _, raw := range []string{record.ID, record.IDs.OpenAlex} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		id, err := work.NormalizeOpenAlex(raw)
		if err != nil {
			return false
		}
		if id != want {
			return false
		}
		parsed++
	}
	return parsed > 0
}

// recordFor returns a record written by a preceding Resolve call for the same
// DOI, if one is fresh (< recordMemoTTL). It never fetches: on a miss,
// ResolveSiblings falls back to the caller-supplied title/year/authors, exactly
// like a malformed canonical response already did. The record is only ever used
// for title/year/author matching, never to (re)construct a fetchable candidate
// URL — ResolveSiblings' own candidates always come from a live, unmemoized
// search response — so a stale entry carries no dead-URL risk; it is a
// TTL-fresh metadata memo, resolver-wide and cross-pass by construction, not a
// pass-scoped structure.
func (r *Resolver) recordFor(doi string) (workRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.records["doi:"+doi]
	if !ok || time.Since(entry.at) > recordMemoTTL || !entry.found {
		return workRecord{}, false
	}
	return entry.record, true
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
	// A duplicated identifier field must AGREE with itself, or it is dropped.
	// Taking the first parseable value let an exact lookup launder a conflicting
	// secondary identifier: verification only checks the identifier that selected
	// the endpoint, so a DOI lookup that echoes its DOI correctly could still
	// publish `ids.openalex = W2` beside `id = W1` — one silently discarded, the
	// winner attached to a candidate at IdentityConfidence 1.0 and adopted as this
	// work's identity. A response that contradicts itself has not identified
	// anything, so the disagreeing field carries no identity at all.
	resolved.DOI = agreeingIdentifier([]string{record.DOI, record.IDs.DOI}, work.NormalizeDOI)
	resolved.PMID = agreeingIdentifier([]string{record.IDs.PMID}, func(raw string) (string, error) {
		return work.NormalizePMID(identifierTail(raw))
	})
	resolved.ArXiv = agreeingIdentifier([]string{record.IDs.ArXiv}, work.NormalizeArXiv)
	resolved.OpenAlex = agreeingIdentifier([]string{record.ID, record.IDs.OpenAlex}, work.NormalizeOpenAlex)
	for _, authorship := range record.Authorships {
		if name := strings.TrimSpace(authorship.Author.DisplayName); name != "" {
			resolved.Authors = append(resolved.Authors, name)
		}
	}
	return resolved
}

// agreeingIdentifier normalizes every non-empty candidate spelling of one
// identifier and returns it only when they all agree. An unparseable or
// disagreeing spelling yields "": the provider contradicted itself about this
// identifier, and no reading of that is authoritative.
func agreeingIdentifier(raws []string, normalize func(string) (string, error)) string {
	agreed := ""
	for _, raw := range raws {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		value, err := normalize(raw)
		if err != nil {
			return ""
		}
		if agreed != "" && value != agreed {
			return ""
		}
		agreed = value
	}
	return agreed
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
	switch len(parts) {
	case 0:
		return "", 0, false
	case 1:
		// Mononymous authors ("Madonna") carry no separate initial; match on
		// the bare name so a valid single-name author cannot sink the list.
		return parts[0], 0, true
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
