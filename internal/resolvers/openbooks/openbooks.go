// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package openbooks resolves an ISBN to the open-access PDF of that edition,
// read from the repository record that lists the ISBN: DSpace 7 servers the
// operator names (the WHO IRIS by default) and the OAPEN Library.
//
// An ISBN names an edition, not a file, and almost every ISBN is a commercial
// book with no open copy anywhere. So this source is deliberately narrow and
// every gate fails closed:
//
//   - it acts only on a work whose sole strong identifier is the ISBN
//     (work.Work.OpenBooksEligible); a chapter with a DOI is fetched as the
//     chapter, never as the book it sits in;
//   - a record is a candidate only if the record's OWN ISBN field lists the
//     requested ISBN. A repository search is full text, so it also returns
//     documents that merely mention the number, and those are discarded;
//   - the record must carry an open licence statement, or the repository must
//     be open access by definition (OAPEN);
//   - the record must hold exactly one PDF in its ORIGINAL bundle. A record
//     with several (volumes, chapter files, a preview beside a book) is
//     refused rather than guessed at.
//
// The candidate carries an ISBN binding. It is evidence for PDF validation,
// which accepts the file only when its own text also agrees (see
// pdf.MatchISBNBoundIdentity); a repository's statement about its record is
// never, alone, a statement about the bytes.
package openbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	// SourceName is the [sources.*] key and the candidate source label.
	SourceName = "open_books"

	// DefaultOAPENBase is the OAPEN Library's DSpace 6 REST root.
	DefaultOAPENBase = "https://library.oapen.org/rest"

	defaultMaxBody = int64(2 << 20)
	// defaultGap separates consecutive requests inside one lookup. The source's
	// token bucket paces lookups; this paces the two or three requests each
	// lookup makes to the same small public service.
	defaultGap = time.Second
	// searchSize bounds a repository's search page. The ISBN is a full-text
	// term, so the record that lists it competes with documents that cite it.
	searchSize = 20
	// maxSearchPages bounds how many result pages one repository is read for.
	// A search that needs more is refused: with that many documents naming the
	// number, the one record that lists it cannot be told from its neighbours.
	maxSearchPages = 5
	// maxAuthors bounds the creators copied onto the resolved work.
	maxAuthors = 8
)

var (
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// oapenRetrieveRE is the DSpace 6 bitstream retrieve path, with or without
	// the REST context prefix the server may include.
	oapenRetrieveRE = regexp.MustCompile(`^(/rest)?/bitstreams/([0-9a-fA-F-]{36})/retrieve$`)
	yearRE          = regexp.MustCompile(`^\s*(\d{4})`)
	// openLicenceRE names an open licence or an open-access statement. A rights
	// string that matches nothing here ("All rights reserved", a bare
	// copyright line, an embargo notice) is not open.
	openLicenceRE = regexp.MustCompile(`(?i)\b(cc[- ]?by|cc[- ]?0|cc[- ]?zero|creative\s*commons|public\s*domain)\b|creativecommons\.org/(licenses|publicdomain)/`)
	// closedRightsRE vetoes a rights value that names a restriction even when
	// it also names a licence ("CC BY, embargoed until 2027").
	closedRightsRE = regexp.MustCompile(`(?i)\b(all\s+rights\s+reserved|embargo\w*|restricted|closed|not\s+open|no\s+reuse)\b`)
	licenceURLRE   = regexp.MustCompile(`(?i)creativecommons\.org/(licenses|publicdomain)/([a-z0-9-]+)(?:/([0-9.]+))?(?:/([a-z]+))?`)
)

// HTTPClient is the injected HTTP dependency used to call the repositories.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. The zero value searches no DSpace server and
// not OAPEN, so a Resolver built from it never makes a request.
type Options struct {
	Client HTTPClient
	// DSpaceServers are DSpace 7 REST roots (".../server"), searched in order.
	DSpaceServers []string
	// OAPEN enables the OAPEN Library lookup, after the DSpace servers.
	OAPEN bool
	// OAPENBaseURL overrides DefaultOAPENBase; a test or dev endpoint.
	OAPENBaseURL string
	// ContactEmail is named in the User-Agent so a repository operator can
	// reach whoever is sending the lookups.
	ContactEmail     string
	MaxResponseBytes int64
	// RequestGap is the pause between consecutive requests in one lookup. Zero
	// selects the default; a negative value disables it (tests).
	RequestGap time.Duration
}

// Resolver implements resolver.Resolver for ISBN-only works.
type Resolver struct {
	client    HTTPClient
	dspace    []string
	oapen     string
	userAgent string
	maxBody   int64
	gap       time.Duration
}

var _ resolver.Resolver = (*Resolver)(nil)

// NewWithOptions constructs a resolver with injected dependencies.
func NewWithOptions(opts Options) *Resolver {
	r := &Resolver{client: opts.Client, maxBody: opts.MaxResponseBytes, gap: opts.RequestGap}
	if r.maxBody <= 0 {
		r.maxBody = defaultMaxBody
	}
	switch {
	case r.gap == 0:
		r.gap = defaultGap
	case r.gap < 0:
		r.gap = 0
	}
	for _, server := range opts.DSpaceServers {
		if server = strings.TrimRight(strings.TrimSpace(server), "/"); server != "" {
			r.dspace = append(r.dspace, server)
		}
	}
	if opts.OAPEN {
		r.oapen = strings.TrimRight(strings.TrimSpace(opts.OAPENBaseURL), "/")
		if r.oapen == "" {
			r.oapen = DefaultOAPENBase
		}
	}
	contact := strings.TrimSpace(opts.ContactEmail)
	if contact == "" {
		contact = "unset"
	}
	r.userAgent = "papio/0.1 (legitimate research acquisition; mailto:" + contact + ")"
	return r
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return SourceName }

// Resolve looks the ISBN up in each configured repository in turn and returns
// the candidate from the first one that has a qualifying record. It returns
// (nil, nil) for a work that is not ISBN-only and for an ISBN no repository
// lists. When no repository answered with a candidate and some request failed,
// the first failure is returned so a transient outage is retried instead of
// being read as "no open copy exists".
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if !requested.OpenBooksEligible() {
		return nil, nil
	}
	isbn, err := work.NormalizeISBN(requested.ISBN)
	if err != nil {
		return nil, nil
	}
	if r.client == nil {
		return nil, errors.New("open_books: HTTP client is not configured")
	}
	lookup := &lookup{r: r, isbn: isbn}
	var firstErr error
	note := func(err error) {
		if err == nil {
			return
		}
		// A retryable failure outranks a permanent one: the permanent one
		// (a refused network) will recur, the retryable one may not.
		if _, temporary := resolver.Temporary(err); firstErr == nil || (temporary && !isTemporary(firstErr)) {
			firstErr = err
		}
	}
	for _, server := range r.dspace {
		candidates, err := lookup.dspace(ctx, server)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if len(candidates) != 0 {
			return candidates, nil
		}
		note(err)
	}
	if r.oapen != "" {
		candidates, err := lookup.oapen(ctx, r.oapen)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if len(candidates) != 0 {
			return candidates, nil
		}
		note(err)
	}
	return nil, firstErr
}

func isTemporary(err error) bool {
	_, temporary := resolver.Temporary(err)
	return temporary
}

// lookup is the state of one Resolve call: the ISBN, and whether a request has
// already been made (the first request is never delayed).
type lookup struct {
	r     *Resolver
	isbn  string
	asked bool
}

// getJSON GETs rawURL and decodes a bounded JSON body into dst. found is false
// for a 404, which every repository uses for "nothing here".
func (l *lookup) getJSON(ctx context.Context, rawURL string, dst any) (found bool, err error) {
	if l.asked && l.r.gap > 0 {
		timer := time.NewTimer(l.r.gap)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	l.asked = true
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, errors.New("open_books: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", l.r.userAgent)
	resp, err := l.r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Transport errors can carry the request URL; say nothing about it.
		return false, &resolver.TemporaryError{Err: errors.New("open_books: request failed")}
	}
	if resp == nil {
		return false, &resolver.TemporaryError{Err: errors.New("open_books: empty HTTP response")}
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	host := hostOf(rawURL)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Errorf("open_books: %s refused the request (HTTP %d); it may block this network", host, resp.StatusCode)
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode >= 500 && resp.StatusCode <= 599):
		return false, &resolver.TemporaryError{
			Err:        fmt.Errorf("open_books: %s upstream HTTP status %d", host, resp.StatusCode),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return false, fmt.Errorf("open_books: %s unexpected HTTP status %d", host, resp.StatusCode)
	}
	if resp.Body == nil {
		return false, errors.New("open_books: response body is missing")
	}
	if err := decodeBoundedJSON(resp.Body, l.r.maxBody, dst); err != nil {
		return false, fmt.Errorf("open_books: invalid response from %s: %w", host, err)
	}
	return true, nil
}

// record is the repository-neutral view of one catalogue record, reduced to
// the fields every gate reads.
type record struct {
	title   string
	authors []string
	year    int
	landing string
	handle  string
	licence string // normalised open licence label, "" when none stated
	isbns   []string
	pdfs    []pdfFile
	// incomplete is set when the repository did not list every bitstream, so a
	// count of one PDF cannot be trusted.
	incomplete bool
}

type pdfFile struct {
	name string
	url  string
}

// listsISBN reports whether the record's own ISBN field lists isbn.
func (rec *record) listsISBN(isbn string) bool {
	for _, listed := range rec.isbns {
		if work.SameISBN(listed, isbn) {
			return true
		}
	}
	return false
}

// candidate turns a record that passed every gate into a candidate. Callers
// have already established the ISBN match, the licence, and a single PDF.
func (rec *record) candidate(isbn, host string) (resolver.Candidate, bool) {
	title := strings.TrimSpace(rec.title)
	if title == "" || len(rec.pdfs) != 1 || rec.pdfs[0].url == "" {
		return resolver.Candidate{}, false
	}
	file := rec.pdfs[0]
	licence := rec.licence
	if licence == "" {
		licence = "unknown"
	}
	candidate := resolver.Candidate{
		Source:             SourceName,
		URL:                file.url,
		Landing:            rec.landing,
		Version:            resolver.VersionUnknown,
		AccessBasis:        resolver.AccessOpen,
		ReuseLicense:       licence,
		ExpectedMIME:       "application/pdf",
		Direct:             true,
		IdentityConfidence: 0.95,
		Authority:          resolver.AuthorityExactEcho,
		ISBNBinding:        &resolver.ISBNBinding{ISBN: isbn, Title: title, URL: file.url},
		ResolvedWork:       work.Work{ISBN: isbn, Title: title, Authors: rec.authors, Year: rec.year},
		Evidence: []string{
			"open_books repository=" + safeEvidenceValue(host),
			"open_books record_isbn=" + isbn,
			"open_books handle=" + safeEvidenceValue(rec.handle),
			"open_books license=" + safeEvidenceValue(licence),
			"open_books file=" + safeEvidenceValue(file.name),
			"open_books url=" + redact.URL(file.url),
		},
	}
	if resolver.ValidateCandidate(candidate) != nil {
		return resolver.Candidate{}, false
	}
	return candidate, true
}

// ---- DSpace 7 -------------------------------------------------------------

type dspaceSearch struct {
	Embedded struct {
		SearchResult struct {
			Embedded struct {
				Objects []struct {
					Embedded struct {
						Item dspaceItem `json:"indexableObject"`
					} `json:"_embedded"`
				} `json:"objects"`
			} `json:"_embedded"`
			Page struct {
				TotalPages int `json:"totalPages"`
			} `json:"page"`
		} `json:"searchResult"`
	} `json:"_embedded"`
}

type dspaceItem struct {
	UUID      string                         `json:"uuid"`
	Handle    string                         `json:"handle"`
	Type      string                         `json:"type"`
	Withdrawn bool                           `json:"withdrawn"`
	InArchive *bool                          `json:"inArchive"`
	Metadata  map[string][]dspaceMetadataVal `json:"metadata"`
}

type dspaceMetadataVal struct {
	Value string `json:"value"`
}

func (i dspaceItem) values(keys ...string) []string {
	var out []string
	for _, key := range keys {
		for _, v := range i.Metadata[key] {
			if v := strings.TrimSpace(v.Value); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

type dspaceBundles struct {
	Embedded struct {
		Bundles []struct {
			Name     string `json:"name"`
			Embedded struct {
				Bitstreams struct {
					Embedded struct {
						Bitstreams []struct {
							UUID     string `json:"uuid"`
							Name     string `json:"name"`
							Embedded struct {
								Format struct {
									MIMEType string `json:"mimetype"`
								} `json:"format"`
							} `json:"_embedded"`
						} `json:"bitstreams"`
					} `json:"_embedded"`
					Page struct {
						TotalElements int `json:"totalElements"`
					} `json:"page"`
				} `json:"bitstreams"`
			} `json:"_embedded"`
		} `json:"bundles"`
	} `json:"_embedded"`
	Page struct {
		TotalElements int `json:"totalElements"`
	} `json:"page"`
}

// dspace searches one DSpace 7 server. A server's search is full text, so the
// result is filtered to items whose own ISBN field lists the requested ISBN.
func (l *lookup) dspace(ctx context.Context, server string) ([]resolver.Candidate, error) {
	base, err := url.Parse(server)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("open_books: invalid DSpace server configuration")
	}
	api := strings.TrimRight(base.String(), "/") + "/api"

	query := url.Values{}
	query.Set("query", l.isbn)
	query.Set("dsoType", "ITEM")
	query.Set("size", strconv.Itoa(searchSize))

	// Every page is read: the ambiguity rule below is only as good as the list
	// it sees, and a search that spills past one page must not be treated as
	// complete. A result larger than maxSearchPages is refused outright.
	var matches []dspaceItem
	for page := 0; ; page++ {
		if page >= maxSearchPages {
			return nil, nil
		}
		query.Set("page", strconv.Itoa(page))
		var search dspaceSearch
		found, err := l.getJSON(ctx, api+"/discover/search/objects?"+query.Encode(), &search)
		if err != nil || !found {
			return nil, err
		}
		result := search.Embedded.SearchResult
		for _, object := range result.Embedded.Objects {
			item := object.Embedded.Item
			if item.Type != "item" || item.Withdrawn || (item.InArchive != nil && !*item.InArchive) || !uuidRE.MatchString(item.UUID) {
				continue
			}
			var listed []string
			for _, value := range item.values("dc.identifier.isbn") {
				listed = append(listed, work.ISBNsIn(value)...)
			}
			rec := record{isbns: listed}
			if !rec.listsISBN(l.isbn) || dspaceIsChild(item) {
				continue
			}
			matches = append(matches, item)
		}
		if len(matches) > 1 {
			return nil, nil
		}
		// A server that reports no page count and returns a full page has not
		// said whether more follow; that is incomplete, not finished.
		if result.Page.TotalPages == 0 && len(result.Embedded.Objects) >= searchSize {
			return nil, nil
		}
		if page+1 >= result.Page.TotalPages {
			break
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
	default:
		// Two items listing one ISBN: a translation, a re-deposit, or an
		// error. Which is the requested book is not for this source to guess.
		return nil, nil
	}
	item := matches[0]

	licence, open := openLicence(item.values("dc.rights.uri", "dc.rights", "dc.rights.license", "dcterms.license"))
	if !open {
		return nil, nil
	}

	var bundles dspaceBundles
	found, err := l.getJSON(ctx, api+"/core/items/"+item.UUID+"/bundles?embed=bitstreams/format", &bundles)
	if err != nil || !found {
		return nil, err
	}
	rec := record{
		title:   first(item.values("dc.title")),
		authors: capped(item.values("dc.contributor.author", "dc.contributor.editor", "dc.creator"), maxAuthors),
		year:    yearOf(first(item.values("dc.date.issued"))),
		handle:  item.Handle,
		licence: licence,
	}
	if landing := first(item.values("dc.identifier.uri")); validHTTPURL(landing) {
		rec.landing = landing
	}
	if bundles.Page.TotalElements > len(bundles.Embedded.Bundles) {
		rec.incomplete = true
	}
	for _, bundle := range bundles.Embedded.Bundles {
		if bundle.Name != "ORIGINAL" {
			continue
		}
		streams := bundle.Embedded.Bitstreams
		if streams.Page.TotalElements > len(streams.Embedded.Bitstreams) {
			rec.incomplete = true
		}
		for _, stream := range streams.Embedded.Bitstreams {
			if !isPDF(stream.Embedded.Format.MIMEType, stream.Name) {
				continue
			}
			// Every ORIGINAL PDF is counted, whether or not its id is usable:
			// a record with two PDFs is ambiguous even when one is malformed.
			file := pdfFile{name: stream.Name}
			if uuidRE.MatchString(stream.UUID) {
				file.url = api + "/core/bitstreams/" + stream.UUID + "/content"
			}
			rec.pdfs = append(rec.pdfs, file)
		}
	}
	if rec.incomplete {
		return nil, nil
	}
	candidate, ok := rec.candidate(l.isbn, base.Host)
	if !ok {
		return nil, nil
	}
	return []resolver.Candidate{candidate}, nil
}

// dspaceIsChild reports an item that is a chapter or part of a larger book:
// its ISBN, when it lists one, is the container's, and the book is not the
// file the item holds.
func dspaceIsChild(item dspaceItem) bool {
	for _, kind := range item.values("dc.type") {
		if isChapterType(kind) {
			return true
		}
	}
	return false
}

// ---- OAPEN (DSpace 6 REST) ------------------------------------------------

type oapenItem struct {
	UUID      string          `json:"uuid"`
	Handle    string          `json:"handle"`
	Type      string          `json:"type"`
	Withdrawn json.RawMessage `json:"withdrawn"`
	Metadata  []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"metadata"`
	Bitstreams []struct {
		UUID         string `json:"uuid"`
		Name         string `json:"name"`
		BundleName   string `json:"bundleName"`
		MIMEType     string `json:"mimeType"`
		RetrieveLink string `json:"retrieveLink"`
	} `json:"bitstreams"`
}

func (i oapenItem) values(keys ...string) []string {
	var out []string
	for _, key := range keys {
		for _, m := range i.Metadata {
			if m.Key == key {
				if v := strings.TrimSpace(m.Value); v != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// oapen searches the OAPEN Library, whose every book is open access by
// definition, so no licence statement is required (one is still reported).
// The REST /search endpoint takes a Solr query and returns a JSON array.
func (l *lookup) oapen(ctx context.Context, restBase string) ([]resolver.Candidate, error) {
	base, err := url.Parse(restBase)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("open_books: invalid OAPEN endpoint configuration")
	}
	query := url.Values{}
	query.Set("query", `dc.identifier.isbn:"`+l.isbn+`"`)
	query.Set("expand", "metadata,bitstreams")
	query.Set("limit", strconv.Itoa(searchSize))

	// Every page is read (the endpoint returns a bare array, so a full page is
	// the only sign that more may follow). A result larger than maxSearchPages
	// is refused rather than treated as complete.
	var matches []oapenItem
	for page := 0; ; page++ {
		query.Set("offset", strconv.Itoa(page*searchSize))
		var items []oapenItem
		found, err := l.getJSON(ctx, restBase+"/search?"+query.Encode(), &items)
		if err != nil || !found {
			return nil, err
		}
		for _, item := range items {
			if (item.Type != "" && item.Type != "item") || withdrawn(item.Withdrawn) {
				continue
			}
			var listed []string
			for _, value := range item.values("dc.identifier.isbn") {
				listed = append(listed, work.ISBNsIn(value)...)
			}
			rec := record{isbns: listed}
			if !rec.listsISBN(l.isbn) || oapenIsChild(item) {
				continue
			}
			matches = append(matches, item)
		}
		if len(matches) > 1 {
			return nil, nil
		}
		if len(items) < searchSize {
			break
		}
		if page+1 >= maxSearchPages {
			return nil, nil
		}
	}
	if len(matches) != 1 {
		return nil, nil
	}
	item := matches[0]

	licence, _ := openLicence(item.values("dc.rights.uri", "dc.rights"))
	rec := record{
		title:   first(item.values("dc.title")),
		authors: capped(item.values("dc.contributor.author", "dc.contributor.editor"), maxAuthors),
		year:    yearOf(first(item.values("dc.date.issued"))),
		handle:  item.Handle,
		licence: licence,
	}
	if handle := strings.TrimSpace(item.Handle); handle != "" && !strings.ContainsAny(handle, " \t\r\n?#") {
		landing := base.Scheme + "://" + base.Host + "/handle/" + handle
		if validHTTPURL(landing) {
			rec.landing = landing
		}
	}
	for _, stream := range item.Bitstreams {
		if stream.BundleName != "ORIGINAL" || !isPDF(stream.MIMEType, stream.Name) {
			continue
		}
		// Every ORIGINAL PDF is counted, whether or not its link is usable: a
		// record with two PDFs is ambiguous even when one has a broken link.
		file := pdfFile{name: stream.Name}
		if match := oapenRetrieveRE.FindStringSubmatch(stream.RetrieveLink); match != nil && uuidRE.MatchString(match[2]) {
			path := match[0]
			if match[1] == "" {
				path = strings.TrimRight(base.Path, "/") + path
			}
			file.url = base.Scheme + "://" + base.Host + path
		}
		rec.pdfs = append(rec.pdfs, file)
	}
	candidate, ok := rec.candidate(l.isbn, base.Host)
	if !ok {
		return nil, nil
	}
	return []resolver.Candidate{candidate}, nil
}

func oapenIsChild(item oapenItem) bool {
	for _, kind := range item.values("dc.type") {
		if isChapterType(kind) {
			return true
		}
	}
	return len(item.values("oapen.relation.isPartOfBook")) != 0
}

func withdrawn(raw json.RawMessage) bool {
	var flag bool
	if len(raw) != 0 && json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var text string
	if len(raw) != 0 && json.Unmarshal(raw, &text) == nil {
		return strings.EqualFold(text, "true")
	}
	return false
}

// ---- shared helpers -------------------------------------------------------

func isChapterType(kind string) bool {
	return strings.Contains(strings.ToLower(kind), "chapter")
}

// isPDF reports a bitstream that is, or may be, a PDF: its format says so, or
// its name does. Either signal counts, because the question this answers is "how
// many PDFs does the record hold", and a miscounted PDF makes a record look
// unambiguous when it is not. The fetch layer sniffs the bytes either way.
func isPDF(mimeType, name string) bool {
	return strings.EqualFold(strings.TrimSpace(mimeType), "application/pdf") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".pdf")
}

// openLicence reads the rights values of one record. It reports the normalised
// licence label and whether the record is open: at least one value names an
// open licence and none names a restriction.
func openLicence(values []string) (label string, open bool) {
	for _, value := range values {
		if closedRightsRE.MatchString(value) {
			return "", false
		}
	}
	for _, value := range values {
		if !openLicenceRE.MatchString(value) {
			continue
		}
		if label == "" {
			label = licenceLabel(value)
		}
		open = true
	}
	return label, open
}

// licenceLabel renders a Creative Commons URL or "CC BY-NC-SA 3.0 IGO" text as
// "cc-by-nc-sa-3.0-igo", the lowercase form the other sources report.
func licenceLabel(value string) string {
	if m := licenceURLRE.FindStringSubmatch(value); m != nil {
		parts := []string{"cc", m[2]}
		for _, p := range m[3:] {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "-")
	}
	label := strings.ToLower(strings.TrimSpace(value))
	label = strings.Join(strings.Fields(label), "-")
	if len(label) > 64 {
		label = label[:64]
	}
	return label
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func capped(values []string, limit int) []string {
	if len(values) > limit {
		values = values[:limit]
	}
	return append([]string(nil), values...)
}

func yearOf(value string) int {
	if m := yearRE.FindStringSubmatch(value); m != nil {
		year, _ := strconv.Atoi(m[1])
		return year
	}
	return 0
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func hostOf(rawURL string) string {
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "repository"
}

func safeEvidenceValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	value = strings.NewReplacer("\n", " ", "\r", " ").Replace(value)
	if len(value) > 120 {
		value = value[:120]
	}
	return value
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
