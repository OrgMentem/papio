// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package openaire resolves open-access locations from the OpenAIRE Graph
// API. OpenAIRE's strength is European repository coverage: records
// aggregate publisher and institutional-repository instances of one work.
// Instances carry URLs without a "this is the file" marker, so candidates
// are emitted as landing-style observations (Direct=false) and the existing
// landing-expansion machinery derives the actual PDF when the page
// advertises one.
//
// OpenAIRE Graph metadata is CC-BY: papio acknowledges OpenAIRE as the
// source in candidate provenance (Source/Evidence) and in the privacy
// documentation, and does not rebrand Graph metadata as its own.
package openaire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"papio/internal/redact"
	"papio/internal/resolver"
	"papio/internal/work"
)

const (
	defaultBaseURL = "https://api.openaire.eu/graph/v1"
	defaultMaxBody = int64(1 << 20)

	// maxInstanceCandidates bounds how many of one record's instances become
	// candidates: a heavily-aggregated record can list dozens of mirrors,
	// and every candidate past the first few is another fetch attempt at
	// the exhaustion boundary.
	maxInstanceCandidates = 3
)

// HTTPClient is the injected HTTP dependency used to call OpenAIRE.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Resolver. Tokens is the preferred credential path (see
// auth.go: a registered service's client id and secret do not expire, while a
// personal access token lasts an hour); APIKey is a raw pre-issued bearer kept
// for short manual checks. Keyless access works at OpenAIRE's public rate
// limit. BaseURL is for tests or explicitly configured dev endpoints.
type Options struct {
	Client           HTTPClient
	Tokens           TokenSource
	APIKey           string
	BaseURL          string
	MaxResponseBytes int64
}

// Resolver implements resolver.Resolver against the Graph API's
// researchProducts lookup by persistent identifier.
type Resolver struct {
	client  HTTPClient
	tokens  TokenSource
	baseURL string
	maxBody int64
}

var _ resolver.Resolver = (*Resolver)(nil)

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
	tokens := opts.Tokens
	if tokens == nil {
		if key := strings.TrimSpace(opts.APIKey); key != "" {
			tokens = StaticToken(key)
		}
	}
	return &Resolver{
		client:  opts.Client,
		tokens:  tokens,
		baseURL: baseURL,
		maxBody: maxBody,
	}
}

// Name identifies this adapter to the resolver registry.
func (*Resolver) Name() string { return "openaire" }

// Resolve looks the work up by DOI (else PMID) and emits up to three
// open-access landing candidates from the record's instances. A record
// whose echoed identifiers name a different work, a record that is not
// OPEN, and instances without a license or explicit OPEN access right are
// all skipped — downloadable never implies an open license, and a closed
// instance's URL is not an acquisition route.
func (r *Resolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	if r.client == nil {
		return nil, errors.New("openaire: HTTP client is not configured")
	}
	pid, ok := lookupPID(requested)
	if !ok {
		return nil, nil
	}
	endpoint, err := r.endpointURL(pid)
	if err != nil {
		return nil, errors.New("openaire: invalid endpoint configuration")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("openaire: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	if r.tokens != nil {
		token, err := r.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &resolver.TemporaryError{Err: errors.New("openaire: request failed")}
	}
	if resp == nil {
		return nil, &resolver.TemporaryError{Err: errors.New("openaire: empty HTTP response")}
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// A static api_key that worked until now has most likely just
		// expired: OpenAIRE personal access tokens last one hour.
		return nil, errors.New("openaire: request was rejected (a personal access token in api_key expires one hour after issue; set client_id and client_secret for unattended access)")
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return nil, temporaryStatus(resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		return nil, temporaryStatus(resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("openaire: unexpected HTTP status %d", resp.StatusCode)
	}
	if resp.Body == nil {
		return nil, errors.New("openaire: response body is missing")
	}

	var payload searchResponse
	if err := decodeBoundedJSON(resp.Body, r.maxBody, &payload); err != nil {
		return nil, fmt.Errorf("openaire: invalid response: %w", err)
	}
	if len(payload.Results) == 0 {
		return nil, nil
	}
	record := payload.Results[0]
	if conflictsWithRequest(record, requested) {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(record.BestAccessRight.Label), "OPEN") {
		// The aggregate record is not open; individual closed instances are
		// not acquisition routes regardless of how reachable they look.
		return nil, nil
	}

	resolved := resolvedWork(record)
	candidates := make([]resolver.Candidate, 0, maxInstanceCandidates)
	seen := map[string]bool{}
	for _, instance := range record.Instances {
		if len(candidates) == maxInstanceCandidates {
			break
		}
		license := strings.ToLower(strings.TrimSpace(instance.License))
		// An instance is an acquisition route only when the provider marked
		// it OPEN or its license is genuinely open. A bare license string is
		// NOT enough: OpenAIRE dedup records carry restricted contractual
		// licenses ("Springer TDM") on paywalled publisher instances, and
		// admitting those as open_access would rank a paywall above honest
		// unknown-license OA candidates.
		openInstance := strings.EqualFold(strings.TrimSpace(instance.AccessRight.Label), "OPEN") ||
			isOpenLicense(license)
		if !openInstance {
			continue
		}
		instanceURL := firstValidURL(instance.URLs)
		if instanceURL == "" || seen[instanceURL] {
			continue
		}
		// Checked before the slot is consumed, not after: the bound is on
		// candidates emitted, so a DOI echo admitted here is a repository copy
		// dropped.
		if isDOIResolverURL(instanceURL) {
			continue
		}
		seen[instanceURL] = true
		if license == "" {
			license = "unknown"
		}
		// OpenAIRE marks no URL as the file itself, so candidates default to
		// landing observations — except an unambiguous file path, which the
		// landing-expansion step could never read (it parses HTML only).
		direct := isObviousPDFURL(instanceURL)
		mime := ""
		if direct {
			mime = "application/pdf"
		}
		candidate := resolver.Candidate{
			Source:             "openaire",
			URL:                instanceURL,
			Landing:            instanceURL,
			Version:            resolver.VersionUnknown,
			AccessBasis:        resolver.AccessOpen,
			ReuseLicense:       license,
			ExpectedMIME:       mime,
			Direct:             direct,
			IdentityConfidence: 1,
			Authority:          resolver.AuthorityExactEcho,
			ResolvedWork:       resolved,
			Evidence: []string{
				"openaire lookup=" + pid.evidence,
				"openaire refereed=" + safeEvidenceValue(instance.Refereed),
				"openaire url=" + redact.URL(instanceURL),
			},
		}
		if resolver.ValidateCandidate(candidate) != nil {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates, nil
}

type pidRef struct {
	value    string
	evidence string
}

// lookupPID picks the strongest identifier the Graph API can match with its
// pid filter. Title search is out: a fuzzy match must never become an
// automatic candidate.
func lookupPID(requested work.Work) (pidRef, bool) {
	if doi, err := work.NormalizeDOI(requested.DOI); err == nil {
		return pidRef{value: doi, evidence: "doi"}, true
	}
	if pmid, err := work.NormalizePMID(requested.PMID); err == nil {
		return pidRef{value: pmid, evidence: "pmid"}, true
	}
	return pidRef{}, false
}

func (r *Resolver) endpointURL(pid pidRef) (*url.URL, error) {
	base, err := url.Parse(r.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("invalid base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/researchProducts"
	values := base.Query()
	values.Set("pid", pid.value)
	values.Set("pageSize", "1")
	base.RawQuery = values.Encode()
	return base, nil
}

type searchResponse struct {
	Results []record `json:"results"`
}

type record struct {
	MainTitle       string `json:"mainTitle"`
	PublicationDate string `json:"publicationDate"`
	Publisher       string `json:"publisher"`
	BestAccessRight struct {
		Label string `json:"label"`
	} `json:"bestAccessRight"`
	PIDs []struct {
		Scheme string `json:"scheme"`
		Value  string `json:"value"`
	} `json:"pids"`
	Authors []struct {
		FullName string `json:"fullName"`
	} `json:"authors"`
	Container *struct {
		Name string `json:"name"`
	} `json:"container"`
	Instances []instance `json:"instances"`
}

type instance struct {
	License     string   `json:"license"`
	URLs        []string `json:"urls"`
	Refereed    string   `json:"refereed"`
	AccessRight struct {
		Label string `json:"label"`
	} `json:"accessRight"`
}

// conflictsWithRequest reports whether the record's identifiers name a
// different work. OpenAIRE deduplicates aggressively, so one record carries
// every DOI the work is registered under (publisher, repository, preprint)
// in an order unrelated to which one was queried — a conflict therefore
// exists only when the record lists identifiers of the requested scheme and
// NONE of them match. Absent values on either side are never conflicts.
func conflictsWithRequest(rec record, requested work.Work) bool {
	var recDOIs, recPMIDs []string
	for _, pid := range rec.PIDs {
		switch strings.ToLower(strings.TrimSpace(pid.Scheme)) {
		case "doi":
			if v, err := work.NormalizeDOI(pid.Value); err == nil {
				recDOIs = append(recDOIs, v)
			}
		case "pmid":
			if v, err := work.NormalizePMID(pid.Value); err == nil {
				recPMIDs = append(recPMIDs, v)
			}
		}
	}
	if reqDOI, err := work.NormalizeDOI(requested.DOI); err == nil &&
		len(recDOIs) > 0 && !slices.Contains(recDOIs, reqDOI) {
		return true
	}
	if reqPMID, err := work.NormalizePMID(requested.PMID); err == nil &&
		len(recPMIDs) > 0 && !slices.Contains(recPMIDs, reqPMID) {
		return true
	}
	return false
}

// isOpenLicense reports whether a lowercased license string names a
// genuinely open reuse license: the Creative Commons family and public
// domain marks. Everything else — including contractual strings like
// "springer tdm" — is not evidence of open access.
func isOpenLicense(license string) bool {
	if license == "" {
		return false
	}
	return strings.HasPrefix(license, "cc") || strings.Contains(license, "public domain")
}

// isObviousPDFURL reports whether the URL path itself names a PDF file —
// the only case papio may skip the landing-expansion step for an OpenAIRE
// instance, because that step parses HTML and can never read a file.
func isObviousPDFURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".pdf")
}

// isDOIResolverURL reports whether the URL is nothing but a DOI resolver.
//
// OpenAIRE lists these among a record's instances, and they are not acquisition
// routes: following one lands on the publisher page for the DOI papio submitted
// in the first place, so the candidate carries no information papio did not
// already have. Emitting them is worse than useless, because
// maxInstanceCandidates bounds a record to three candidates and a DOI echo
// consumes one: measured on the operator's own store, 29 of 42 jobs that
// reached OpenAIRE had EVERY slot filled this way, which is why the source's
// genuine contribution — a green-OA copy in an institutional repository that no
// other configured source indexes — never appeared.
//
// Handle resolvers (hdl.handle.net) are deliberately NOT included: a handle
// names a repository copy papio has no other way to learn about.
func isDOIResolverURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www.")) {
	case "doi.org", "dx.doi.org":
		return true
	}
	return false
}

func resolvedWork(rec record) work.Work {
	authors := make([]string, 0, len(rec.Authors))
	for _, author := range rec.Authors {
		if name := strings.TrimSpace(author.FullName); name != "" {
			authors = append(authors, name)
		}
	}
	resolved := work.Work{
		Title:   strings.TrimSpace(rec.MainTitle),
		Authors: authors,
		Year:    yearOf(rec.PublicationDate),
	}
	if rec.Container != nil {
		resolved.Container = strings.TrimSpace(rec.Container.Name)
	}
	for _, pid := range rec.PIDs {
		switch strings.ToLower(strings.TrimSpace(pid.Scheme)) {
		case "doi":
			if v, err := work.NormalizeDOI(pid.Value); err == nil && resolved.DOI == "" {
				resolved.DOI = v
			}
		case "pmid":
			if v, err := work.NormalizePMID(pid.Value); err == nil && resolved.PMID == "" {
				resolved.PMID = v
			}
		}
	}
	return resolved
}

func yearOf(date string) int {
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil || year < 1000 || year > 3000 {
		return 0
	}
	return year
}

func firstValidURL(values []string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if validHTTPURL(value) {
			return value
		}
	}
	return ""
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
		return errors.New("response exceeds the configured size limit")
	}
	return json.Unmarshal(payload, destination)
}

func temporaryStatus(resp *http.Response) error {
	wait := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return &resolver.TemporaryError{
		Err:        fmt.Errorf("openaire: returned HTTP %d", resp.StatusCode),
		RetryAfter: wait,
	}
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
