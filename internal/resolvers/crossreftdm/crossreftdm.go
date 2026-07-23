// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package crossreftdm resolves licensed PDF links from Crossref or a configured
// institutional/publisher text-and-data-mining endpoint.
package crossreftdm

import (
	"bytes"
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

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL = "https://api.crossref.org/works"
	defaultMaxBody = 1 << 20
)

var errUnsafeHTTPClient = errors.New("crossref_tdm: configured HTTP client cannot enforce credential-safe redirects")

// HTTPClient is the injected HTTP dependency used to call a metadata service.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a TDM resolver. APIKey is a credential which authorizes
// retrieval through the selected service; without it this adapter makes no
// metadata requests and returns no licensed candidates.
type Options struct {
	Client           HTTPClient
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver for authorized Crossref TDM links.
type Resolver struct {
	client  HTTPClient
	apiKey  string
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

// New constructs a resolver for Crossref's works endpoint. It is inert until
// supplied a configured service credential.
func New(client HTTPClient, apiKey string) *Resolver {
	return NewWithOptions(Options{Client: client, APIKey: apiKey})
}

// NewConfigured constructs a resolver from a source policy. A configured token
// alone is insufficient when the source policy is disabled.
func NewConfigured(client HTTPClient, source config.Source) *Resolver {
	if !source.Enabled {
		return NewWithOptions(Options{Client: client})
	}
	return NewWithOptions(Options{Client: client, APIKey: source.APIKey, BaseURL: source.BaseURLForDev})
}

// NewWithOptions constructs a resolver with injected dependencies.
func NewWithOptions(opts Options) *Resolver {
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.BaseURL == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxBody
	}
	return &Resolver{client: opts.Client, apiKey: strings.TrimSpace(opts.APIKey), baseURL: opts.BaseURL, maxBody: opts.MaxResponseBytes}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "crossref_tdm" }

// Resolve reads authoritative link metadata for a DOI and selects only links
// whose metadata explicitly identifies a PDF. Link metadata by itself does not
// establish entitlement: licensed_api is emitted only for a configured token.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.apiKey == "" {
		return nil, nil
	}
	doi, err := work.NormalizeDOI(requested.DOI)
	if err != nil {
		return nil, nil
	}
	response, err := r.lookup(ctx, doi)
	if err != nil {
		return nil, err
	}
	return r.candidates(response, doi), nil
}

func (r *Resolver) lookup(ctx context.Context, doi string) (response, error) {
	base, err := url.Parse(r.baseURL)
	if err != nil {
		return response{}, errors.New("crossref_tdm: invalid configured endpoint")
	}
	escapedPrefix := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = strings.TrimRight(base.Path, "/") + "/" + doi
	// Path stores decoded text; RawPath prevents a DOI slash or percent escape
	// from becoming an extra path separator or being double-escaped.
	base.RawPath = escapedPrefix + "/" + url.PathEscape(doi)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return response{}, fmt.Errorf("crossref_tdm: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Crossref's documented metadata credential is the Plus API token header.
	// It is metadata-only: publisher download URLs never receive this token.
	req.Header.Set("Crossref-Plus-API-Token", "Bearer "+r.apiKey)
	resp, err := r.do(req)
	if err != nil {
		if ctx.Err() != nil {
			return response{}, contextError(ctx, "crossref_tdm")
		}
		if errors.Is(err, errUnsafeHTTPClient) {
			return response{}, err
		}
		// Custom transports can include credential-bearing request details in errors.
		return response{}, &resolver.TemporaryError{Err: errors.New("crossref_tdm: request failed")}
	}
	if resp == nil {
		return response{}, &resolver.TemporaryError{Err: errors.New("crossref_tdm: empty HTTP response")}
	}
	if resp.Body == nil {
		return response{}, errors.New("crossref_tdm: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return response{}, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return response{}, errors.New("crossref_tdm: authentication failed (401); verify the configured TDM token")
	case resp.StatusCode == http.StatusForbidden:
		return response{}, errors.New("crossref_tdm: access forbidden (403); verify the configured TDM entitlement")
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return response{}, temporaryStatus(resp)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return response{}, fmt.Errorf("crossref_tdm: service returned HTTP %d", resp.StatusCode)
	}

	var payload response
	if err := decodeBoundedJSON(resp.Body, r.maxBody, &payload); err != nil {
		return response{}, fmt.Errorf("crossref_tdm: invalid response: %w", err)
	}
	return payload, nil
}

// do permits only clients that protect the Plus API token across cross-origin
// redirects. fetch.SecureHTTPClient clears all caller-supplied headers outside
// the origin, while http.Client is copied with a redirect policy that strips
// the token; opaque HTTPClient implementations are rejected.
func (r *Resolver) do(req *http.Request) (*http.Response, error) {
	switch client := r.client.(type) {
	case *fetch.SecureHTTPClient:
		return client.Do(req)
	case *http.Client:
		copyClient := *client
		previous := client.CheckRedirect
		copyClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if !sameAuthority(next.URL, req.URL) {
				next.Header.Del("Crossref-Plus-API-Token")
			}
			if previous != nil {
				return previous(next, via)
			}
			return nil
		}
		return copyClient.Do(req)
	default:
		return nil, errUnsafeHTTPClient
	}
}

func sameAuthority(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func contextError(ctx context.Context, source string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &resolver.TemporaryError{Err: fmt.Errorf("%s: request deadline exceeded", source)}
	}
	return ctx.Err()
}

type response struct {
	Message message `json:"message"`
	Links   []link  `json:"links"`
}

type message struct {
	DOI             string    `json:"DOI"`
	Title           []string  `json:"title"`
	Author          []author  `json:"author"`
	PublishedPrint  dateParts `json:"published-print"`
	PublishedOnline dateParts `json:"published-online"`
	License         []license `json:"license"`
	Link            []link    `json:"link"`
}

type license struct {
	URL             string `json:"URL"`
	ContentVersion  string `json:"content-version"`
	ContentVersion2 string `json:"contentVersion"`
}

func (l license) version() string {
	if l.ContentVersion != "" {
		return l.ContentVersion
	}
	return l.ContentVersion2
}

type author struct {
	Given  string `json:"given"`
	Family string `json:"family"`
	Name   string `json:"name"`
}

type dateParts struct {
	DateParts [][]int `json:"date-parts"`
}

func (d dateParts) year() int {
	if len(d.DateParts) == 0 || len(d.DateParts[0]) == 0 {
		return 0
	}
	return d.DateParts[0][0]
}

type link struct {
	URL                  string `json:"URL"`
	URLLower             string `json:"url"`
	ContentType          string `json:"content-type"`
	ContentTypeAlt       string `json:"contentType"`
	ContentVersion       string `json:"content-version"`
	ContentVersion2      string `json:"contentVersion"`
	IntendedApplication  string `json:"intended-application"`
	IntendedApplication2 string `json:"intendedApplication"`
}

func (l link) address() string {
	if l.URL != "" {
		return l.URL
	}
	return l.URLLower
}
func (l link) mime() string {
	if l.ContentType != "" {
		return l.ContentType
	}
	return l.ContentTypeAlt
}
func (l link) version() string {
	if l.ContentVersion != "" {
		return l.ContentVersion
	}
	return l.ContentVersion2
}
func (l link) isTDM() bool {
	application := strings.ToLower(strings.TrimSpace(l.IntendedApplication))
	if application == "" {
		application = strings.ToLower(strings.TrimSpace(l.IntendedApplication2))
	}
	return application == "text-mining"
}

func (r *Resolver) candidates(payload response, requestedDOI string) []resolver.Candidate {
	metadata := payload.Message
	doi, err := work.NormalizeDOI(metadata.DOI)
	if err != nil || doi != requestedDOI {
		return nil
	}
	links := append([]link(nil), metadata.Link...)
	links = append(links, payload.Links...)
	seen := make(map[string]struct{})
	var candidates []resolver.Candidate
	for _, link := range links {
		address := link.address()
		if !link.isTDM() || !isPDF(link.mime()) || !validHTTPURL(address) {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		candidates = append(candidates, resolver.Candidate{
			Source:             "crossref_tdm",
			URL:                address,
			Landing:            "https://doi.org/" + url.PathEscape(doi),
			Version:            mapVersion(link.version()),
			AccessBasis:        resolver.AccessLicensedAPI,
			ReuseLicense:       metadata.licenseFor(link.version()),
			ExpectedMIME:       "application/pdf",
			ResolvedWork:       metadata.resolvedWork(),
			Direct:             true,
			IdentityConfidence: 1,
			Evidence:           []string{"crossref tdm DOI match; PDF link metadata"},
		})
	}
	return candidates
}

// licenseFor selects the license explicitly applicable to the linked
// content version. An unversioned entry is only a fallback; unrelated
// version-specific licenses are never attributed to another PDF.
func (m message) licenseFor(linkVersion string) string {
	linkVersion = contentVersion(linkVersion)
	var generic string
	for _, entry := range m.License {
		address := strings.TrimSpace(entry.URL)
		if address == "" {
			continue
		}
		version := contentVersion(entry.version())
		if version == "" {
			if generic == "" {
				generic = address
			}
			continue
		}
		if linkVersion != "" && version == linkVersion {
			return address
		}
	}
	return genericOrUnknown(generic)
}

func genericOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func (m message) resolvedWork() work.Work {
	authors := make([]string, 0, len(m.Author))
	for _, person := range m.Author {
		name := strings.TrimSpace(person.Name)
		if name == "" {
			name = strings.TrimSpace(person.Given + " " + person.Family)
		}
		if name != "" {
			authors = append(authors, name)
		}
	}
	var title string
	if len(m.Title) != 0 {
		title = strings.TrimSpace(m.Title[0])
	}
	year := m.PublishedPrint.year()
	if year == 0 {
		year = m.PublishedOnline.year()
	}
	doi, _ := work.NormalizeDOI(m.DOI)
	return work.Work{DOI: doi, Title: title, Authors: authors, Year: year}
}

func isPDF(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return contentType == "application/pdf" || strings.HasPrefix(contentType, "application/pdf;")
}
func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}
func mapVersion(value string) string {
	switch contentVersion(value) {
	case "vor":
		return resolver.VersionPublished
	case "am":
		return resolver.VersionAccepted
	default:
		return resolver.VersionUnknown
	}
}

func contentVersion(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "vor", "am":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func decodeBoundedJSON(body io.Reader, maximum int64, destination any) error {
	if maximum <= 0 {
		return errors.New("invalid response limit")
	}
	data, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maximum {
		return fmt.Errorf("response exceeds %d-byte limit", maximum)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
func temporaryStatus(resp *http.Response) error {
	return &resolver.TemporaryError{Err: fmt.Errorf("crossref_tdm: service returned HTTP %d", resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
}
func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		const maxDuration = time.Duration(1<<63 - 1)
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return time.Until(when)
	}
	return 0
}
