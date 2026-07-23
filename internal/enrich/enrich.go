// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package enrich adds corroborated metadata to title-only work requests.
package enrich

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
	"unicode/utf8"

	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL = "https://api.crossref.org/works"
	defaultMaxBody = 1 << 20
	requestTimeout = 10 * time.Second
)

// HTTPClient is the injected HTTP dependency used to call Crossref.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures an Enricher.
type Options struct {
	Client           HTTPClient
	BaseURL          string
	MaxResponseBytes int64
}

// Enricher searches Crossref metadata to add a DOI only when a result is
// independently corroborated by the submitted work metadata.
type Enricher struct {
	client  HTTPClient
	baseURL string
	maxBody int64
}

// New constructs an enricher with default options.
func New(client HTTPClient) *Enricher {
	return NewWithOptions(Options{Client: client})
}

// NewWithOptions constructs an enricher with injectable dependencies.
func NewWithOptions(opts Options) *Enricher {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: requestTimeout}
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = defaultMaxBody
	}
	return &Enricher{client: opts.Client, baseURL: strings.TrimRight(opts.BaseURL, "/"), maxBody: opts.MaxResponseBytes}
}

// Enrich searches Crossref for a title-only work. It adopts only an exact
// normalized title match, with compatible year and at least one corroborating
// author family name when authors were supplied.
func (e *Enricher) Enrich(ctx context.Context, requested work.Work) (work.Work, bool, error) {
	if strings.TrimSpace(requested.DOI) != "" || strings.TrimSpace(requested.Title) == "" {
		return requested, false, nil
	}
	if e == nil || e.client == nil {
		return requested, false, errors.New("enrich: HTTP client is not configured")
	}

	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint, err := e.searchURL(requested.Title)
	if err != nil {
		return requested, false, err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return requested, false, errors.New("enrich: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: request deadline exceeded")}
			}
			return requested, false, requestCtx.Err()
		}
		return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: request failed")}
	}
	if resp == nil {
		return requested, false, &resolver.TemporaryError{Err: errors.New("enrich: empty HTTP response")}
	}
	if resp.Body == nil {
		return requested, false, errors.New("enrich: response body is missing")
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return requested, false, temporaryStatus(resp)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return requested, false, fmt.Errorf("enrich: Crossref returned HTTP %d", resp.StatusCode)
	}

	var payload response
	if err := decodeBoundedJSON(resp.Body, e.maxBody, &payload); err != nil {
		return requested, false, fmt.Errorf("enrich: invalid Crossref response: %w", err)
	}
	for _, candidate := range payload.Message.Items {
		if !matches(candidate, requested) {
			continue
		}
		doi, err := work.NormalizeDOI(candidate.DOI)
		if err != nil {
			continue
		}
		enriched := requested
		enriched.DOI = doi
		if enriched.Year == 0 {
			enriched.Year = candidate.year()
		}
		if enriched.Container == "" && len(candidate.ContainerTitle) > 0 {
			enriched.Container = strings.TrimSpace(candidate.ContainerTitle[0])
		}
		return enriched, true, nil
	}
	return requested, false, nil
}

func (e *Enricher) searchURL(title string) (*url.URL, error) {
	endpoint, err := url.Parse(e.baseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("enrich: invalid configured endpoint")
	}
	query := endpoint.Query()
	query.Set("query.title", strings.TrimSpace(title))
	query.Set("rows", "5")
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

type response struct {
	Message struct {
		Items []record `json:"items"`
	} `json:"message"`
}

type record struct {
	DOI             string    `json:"DOI"`
	Title           []string  `json:"title"`
	Author          []author  `json:"author"`
	ContainerTitle  []string  `json:"container-title"`
	PublishedPrint  dateParts `json:"published-print"`
	PublishedOnline dateParts `json:"published-online"`
	Issued          dateParts `json:"issued"`
}

type author struct {
	Family string `json:"family"`
}

type dateParts struct {
	DateParts [][]int `json:"date-parts"`
}

func (d dateParts) year() int {
	if len(d.DateParts) == 0 || len(d.DateParts[0]) == 0 || d.DateParts[0][0] < 1 {
		return 0
	}
	return d.DateParts[0][0]
}

func (r record) year() int {
	for _, date := range []dateParts{r.PublishedPrint, r.PublishedOnline, r.Issued} {
		if year := date.year(); year != 0 {
			return year
		}
	}
	return 0
}

func matches(candidate record, requested work.Work) bool {
	if len(candidate.Title) == 0 || normalizeTitle(candidate.Title[0]) != normalizeTitle(requested.Title) {
		return false
	}
	if candidateYear := candidate.year(); requested.Year != 0 && candidateYear != 0 && candidateYear != requested.Year {
		return false
	}
	if len(requested.Authors) == 0 {
		return true
	}
	for _, requestedAuthor := range requested.Authors {
		family := authorFamily(requestedAuthor)
		if family == "" {
			continue
		}
		for _, candidateAuthor := range candidate.Author {
			if family == normalizeTitle(candidateAuthor.Family) {
				return true
			}
		}
	}
	return false
}

func normalizeTitle(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func authorFamily(value string) string {
	value = strings.TrimSpace(value)
	if comma := strings.IndexRune(value, ','); comma >= 0 {
		return normalizeTitle(value[:comma])
	}
	parts := strings.Fields(normalizeTitle(value))
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		// A single-word author is a bare family name or mononym; corroborate
		// on it directly instead of silently skipping the author check.
		return parts[0]
	}
	if isAuthorInitial(parts[len(parts)-1]) {
		return strings.Join(parts[:len(parts)-1], " ")
	}
	return parts[len(parts)-1]
}

func isAuthorInitial(value string) bool {
	value = strings.Trim(value, ".")
	_, size := utf8.DecodeRuneInString(value)
	return size > 0 && size == len(value)
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
	return &resolver.TemporaryError{Err: fmt.Errorf("enrich: Crossref returned HTTP %d", resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
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
