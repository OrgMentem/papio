// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package illiad is a client for the Atlas ILLiad Web Platform API v1, the
// only route ADR-0017 Decision 3A allows to compile `auto_capable`: it
// documents transaction create, transaction lookup, and patron
// request-list, authenticated with an institution-issued server-side
// ApiKey. The base URL is institution-hosted (there is no public default)
// and is always supplied by the caller's configuration.
//
// This package never sends a provider copyright-compliance flag (e.g.
// ILLiad's CopyrightAlreadyPaid) on papio's behalf: TransactionRequest is a
// closed, explicit struct with no such field and no generic map
// passthrough, so no caller can smuggle one in. The one caller-named JSON
// key — TransactionRequest.ReferenceField, which carries papio's
// idempotency token in an institution-approved transaction field such as
// "ItemInfo4" — is rejected by MarshalJSON if it names anything resembling
// a copyright field.
package illiad

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
)

const defaultMaxBody = int64(1 << 20)

// HTTPClient is the injected HTTP dependency used to call the ILLiad Web
// Platform API.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Client. BaseURL is the institution's ILLiad Web
// Platform API root (e.g. "https://illiad.campus.example.edu/ILLiadWebPlatform")
// — there is no public default, since every deployment is institution-hosted.
type Options struct {
	Client           HTTPClient
	BaseURL          string
	APIKey           string
	MaxResponseBytes int64
}

// Client is an ILLiad Web Platform API v1 client bound to one institution's
// deployment and server-side ApiKey.
type Client struct {
	client  HTTPClient
	baseURL string
	apiKey  string
	maxBody int64
}

// New constructs a Client for the given institution-hosted base URL and
// server-side ApiKey.
func New(client HTTPClient, baseURL, apiKey string) *Client {
	return NewWithOptions(Options{Client: client, BaseURL: baseURL, APIKey: apiKey})
}

// NewWithOptions constructs a Client with injected dependencies.
func NewWithOptions(opts Options) *Client {
	maxBody := opts.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	return &Client{
		client:  opts.Client,
		baseURL: strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		apiKey:  strings.TrimSpace(opts.APIKey),
		maxBody: maxBody,
	}
}

// CredentialError marks a permanent authentication/authorization failure
// (HTTP 401 or 403): the configured ApiKey is missing, revoked, or lacks
// permission for the requested operation. It is never retried.
type CredentialError struct {
	StatusCode int
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("illiad: request rejected (HTTP %d); check the configured ApiKey", e.StatusCode)
}

// ErrNotFound is returned when the ILLiad Web Platform API reports no
// record for the requested transaction (HTTP 404).
var ErrNotFound = errors.New("illiad: transaction not found")

// TemporaryError marks a retryable ILLiad API failure and optionally
// carries the server-requested wait. This package returns its own error
// type rather than internal/resolver's TemporaryError: it is consumed by
// internal/delivery's create/poll/reconcile flow (ADR-0017 Decision 1/4),
// not by the resolver.Resolver contract, and must not import the resolver
// package to say so.
type TemporaryError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *TemporaryError) Error() string { return e.Err.Error() }
func (e *TemporaryError) Unwrap() error { return e.Err }

// Temporary reports whether err is a retryable ILLiad API failure and its
// suggested wait.
func Temporary(err error) (time.Duration, bool) {
	var te *TemporaryError
	if errors.As(err, &te) {
		return te.RetryAfter, true
	}
	return 0, false
}

// TransactionRequest is the citation and routing payload for a new ILLiad
// transaction. It is an explicit, closed struct — never a map — so no
// caller can add an arbitrary JSON key to the serialized request; ILLiad's
// copyright-compliance fields (e.g. CopyrightAlreadyPaid) are simply absent
// from this type's vocabulary (ADR-0017 Decision 3A).
type TransactionRequest struct {
	// Username identifies the ILLiad patron account. Exactly one of
	// Username or ExternalUserID is required by the Web Platform API.
	Username       string `json:"Username,omitempty"`
	ExternalUserID string `json:"ExternalUserID,omitempty"`

	// ProcessType selects the ILLiad workflow queue. CreateTransaction
	// defaults it to "Borrowing" when empty.
	ProcessType string `json:"ProcessType"`
	// RequestType selects the ILLiad request type. CreateTransaction
	// defaults it to "Article": ADR-0017 Decision 3A limits v1
	// auto-submission to digital journal articles only.
	RequestType string `json:"RequestType"`

	PhotoJournalTitle          string `json:"PhotoJournalTitle,omitempty"`
	PhotoArticleTitle          string `json:"PhotoArticleTitle,omitempty"`
	PhotoArticleAuthor         string `json:"PhotoArticleAuthor,omitempty"`
	PhotoJournalVolume         string `json:"PhotoJournalVolume,omitempty"`
	PhotoJournalIssue          string `json:"PhotoJournalIssue,omitempty"`
	PhotoJournalYear           string `json:"PhotoJournalYear,omitempty"`
	PhotoJournalInclusivePages string `json:"PhotoJournalInclusivePages,omitempty"`
	DOI                        string `json:"DOI,omitempty"`
	PMID                       string `json:"PMID,omitempty"`
	ISSN                       string `json:"ISSN,omitempty"`

	// ReferenceField names the institution-approved ILLiad transaction
	// field (in practice one of ILLiad's five general-purpose fields,
	// "ItemInfo1".."ItemInfo5") that carries ReferenceValue — papio's
	// idempotency token. This mapping is configured per institution and
	// passed in by the caller on every request; it is never hardcoded in
	// this package (ADR-0017 Decision 1/3A). Left empty, no reference
	// field is sent.
	ReferenceField string `json:"-"`
	ReferenceValue string `json:"-"`
}

// MarshalJSON serializes the fixed citation and routing fields, then adds
// exactly one additional JSON key: the institution-configured
// ReferenceField naming ReferenceValue. ReferenceField is the only
// caller-controlled JSON key name this type can ever produce, and this
// method refuses to serialize one that names a copyright field, so a
// misconfigured mapping cannot smuggle a provider compliance flag into the
// request (ADR-0017 Decision 3A).
func (r TransactionRequest) MarshalJSON() ([]byte, error) {
	type alias TransactionRequest
	base, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	if r.ReferenceField == "" {
		return base, nil
	}
	if strings.Contains(strings.ToLower(r.ReferenceField), "copyright") {
		return nil, fmt.Errorf("illiad: reference field %q must not be a copyright field", r.ReferenceField)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	valueJSON, err := json.Marshal(r.ReferenceValue)
	if err != nil {
		return nil, err
	}
	fields[r.ReferenceField] = valueJSON
	return json.Marshal(fields)
}

// Transaction is the ILLiad transaction record as echoed by the Web
// Platform API: the fields papio reads back for status polling,
// reconciliation, and idempotency matching (ADR-0017 Decision 1/4).
type Transaction struct {
	TransactionNumber int    `json:"TransactionNumber"`
	TransactionStatus string `json:"TransactionStatus"`
	CreationDate      string `json:"CreationDate"`

	PhotoJournalTitle          string `json:"PhotoJournalTitle,omitempty"`
	PhotoArticleTitle          string `json:"PhotoArticleTitle,omitempty"`
	PhotoArticleAuthor         string `json:"PhotoArticleAuthor,omitempty"`
	PhotoJournalVolume         string `json:"PhotoJournalVolume,omitempty"`
	PhotoJournalIssue          string `json:"PhotoJournalIssue,omitempty"`
	PhotoJournalYear           string `json:"PhotoJournalYear,omitempty"`
	PhotoJournalInclusivePages string `json:"PhotoJournalInclusivePages,omitempty"`
	DOI                        string `json:"DOI,omitempty"`
	PMID                       string `json:"PMID,omitempty"`
	ISSN                       string `json:"ISSN,omitempty"`

	// ItemInfo1..ItemInfo5 are ILLiad's five general-purpose transaction
	// fields. An institution's approved reference-field mapping (see
	// TransactionRequest.ReferenceField) always names one of these; papio
	// echoes all five so a caller configured with any one of them can find
	// its idempotency token via ReferenceValue without this package
	// guessing which field the institution chose.
	ItemInfo1 string `json:"ItemInfo1,omitempty"`
	ItemInfo2 string `json:"ItemInfo2,omitempty"`
	ItemInfo3 string `json:"ItemInfo3,omitempty"`
	ItemInfo4 string `json:"ItemInfo4,omitempty"`
	ItemInfo5 string `json:"ItemInfo5,omitempty"`
}

// ReferenceValue looks up one of ILLiad's five general-purpose transaction
// fields by name (e.g. "ItemInfo4"), matching the institution-approved
// TransactionRequest.ReferenceField mapping the caller configured. The
// second return is false for any other field name.
func (t Transaction) ReferenceValue(field string) (string, bool) {
	switch field {
	case "ItemInfo1":
		return t.ItemInfo1, true
	case "ItemInfo2":
		return t.ItemInfo2, true
	case "ItemInfo3":
		return t.ItemInfo3, true
	case "ItemInfo4":
		return t.ItemInfo4, true
	case "ItemInfo5":
		return t.ItemInfo5, true
	default:
		return "", false
	}
}

// CreateTransaction creates a new ILLiad transaction (POST {base}/Transaction/).
// Exactly one of req.Username or req.ExternalUserID is required. ProcessType
// defaults to "Borrowing" and RequestType to "Article" when left empty.
func (c *Client) CreateTransaction(ctx context.Context, req TransactionRequest) (Transaction, error) {
	if c.client == nil {
		return Transaction{}, errors.New("illiad: HTTP client is not configured")
	}
	if req.Username == "" && req.ExternalUserID == "" {
		return Transaction{}, errors.New("illiad: CreateTransaction requires Username or ExternalUserID")
	}
	if req.ProcessType == "" {
		req.ProcessType = "Borrowing"
	}
	if req.RequestType == "" {
		req.RequestType = "Article"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Transaction{}, fmt.Errorf("illiad: could not encode transaction request: %w", err)
	}
	var tx Transaction
	if err := c.do(ctx, http.MethodPost, "Transaction/", body, &tx); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// GetTransaction looks up one transaction by number (GET
// {base}/Transaction/{number}). A missing transaction returns ErrNotFound.
func (c *Client) GetTransaction(ctx context.Context, number int) (Transaction, error) {
	if c.client == nil {
		return Transaction{}, errors.New("illiad: HTTP client is not configured")
	}
	var tx Transaction
	if err := c.do(ctx, http.MethodGet, "Transaction/"+strconv.Itoa(number), nil, &tx); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// UserRequests lists a patron's transactions (GET
// {base}/Transaction/User/{userRef}), bounded by the configured response
// size limit. userRef is the ILLiad username or external user id used to
// create the transactions.
func (c *Client) UserRequests(ctx context.Context, userRef string) ([]Transaction, error) {
	if c.client == nil {
		return nil, errors.New("illiad: HTTP client is not configured")
	}
	if strings.TrimSpace(userRef) == "" {
		return nil, errors.New("illiad: UserRequests requires a non-empty user reference")
	}
	var txs []Transaction
	if err := c.do(ctx, http.MethodGet, "Transaction/User/"+url.PathEscape(userRef), nil, &txs); err != nil {
		return nil, err
	}
	return txs, nil
}

// do issues one ILLiad Web Platform API request, classifies the response,
// and (for a successful response, when out is non-nil) bounded-decodes the
// body into out.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	endpoint, err := c.endpointURL(path)
	if err != nil {
		return errors.New("illiad: invalid endpoint configuration")
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return errors.New("illiad: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("ApiKey", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return &TemporaryError{Err: fmt.Errorf("illiad: request failed: %w", err)}
	}
	if resp == nil {
		return &TemporaryError{Err: errors.New("illiad: empty HTTP response")}
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &CredentialError{StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return temporaryStatus(resp)
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		return temporaryStatus(resp)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("illiad: unexpected HTTP status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if resp.Body == nil {
		return errors.New("illiad: response body is missing")
	}
	if err := decodeBoundedJSON(resp.Body, c.maxBody, out); err != nil {
		return fmt.Errorf("illiad: invalid response: %w", err)
	}
	return nil
}

// endpointURL joins c.baseURL with path, which the caller has already
// percent-encoded wherever it carries dynamic content (see
// UserRequests' use of url.PathEscape). It re-parses the fully assembled
// URL string rather than assigning into base.Path, because the latter
// treats its input as unescaped and would double-encode any '%' the
// caller already produced.
func (c *Client) endpointURL(path string) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return "", errors.New("invalid base URL")
	}
	full := strings.TrimRight(base.String(), "/") + "/" + strings.TrimLeft(path, "/")
	parsed, err := url.Parse(full)
	if err != nil {
		return "", errors.New("invalid base URL")
	}
	return parsed.String(), nil
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
	return &TemporaryError{
		Err:        fmt.Errorf("illiad: returned HTTP %d", resp.StatusCode),
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
