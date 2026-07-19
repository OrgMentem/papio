// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package protocol implements papio's cross-process contracts with strict,
// fail-closed decoding: unknown fields, unknown message types, oversized
// messages, and cross-field inconsistencies are errors, never warnings. The
// browser bridge, work-request, and acquisition-bundle contracts are locked
// at v1. The JSON Schema documents in protocol/ are the human/TypeScript
// source of truth; this package must accept and reject exactly the same corpus
// (testdata/protocol/valid and testdata/protocol/invalid).
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Contract versions locked after live browser, acquisition, bundle-export, and
// Zotio import acceptance.
const (
	WorkRequestSchemaVersion       = "work-request/1"
	AcquisitionBundleSchemaVersion = "acquisition-bundle/1"
	BrowserProtocolVersion         = "papio-browser/1"
)

// MaxBrowserMessageBytes caps one encoded native-messaging frame well below
// Chrome's documented 1 MiB host-to-extension limit.
const MaxBrowserMessageBytes = 256 << 10

// MaxBrowserInteger is the largest integer represented exactly by both Go
// int64 and JavaScript number values.
const MaxBrowserInteger int64 = 1<<53 - 1

var (
	requestIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	msgIDRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	zoteroKeyRE  = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
	doiRE        = regexp.MustCompile(`^10\.[0-9]{4,9}/\S{1,200}$`)
	pmidRE       = regexp.MustCompile(`^[0-9]{1,10}$`)
	arxivRE      = regexp.MustCompile(`^([0-9]{4}\.[0-9]{4,5})(v[0-9]+)?$|^[a-z-]+(\.[A-Z]{2})?/[0-9]{7}$`)
	isbnRE       = regexp.MustCompile(`^[0-9Xx-]{10,17}$`)
	openalexRE   = regexp.MustCompile(`^W[0-9]{4,12}$`)
	sha256RE     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	provenanceRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hostRE       = regexp.MustCompile(`^[a-z0-9.-]{3,253}$`)
	errorCodeRE  = regexp.MustCompile(`^[a-z0-9_]{2,50}$`)
	filenameRE   = regexp.MustCompile(`^[^/\\]{1,255}$`)
	rfc3339RE    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)
)

// strictDecode unmarshals data into v, rejecting unknown fields and trailing input.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after JSON document")
	}
	return nil
}

func browserObjectFields(data []byte, what string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return nil, fmt.Errorf("%s must be an object: %w", what, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%s must be an object", what)
	}
	return fields, nil
}

func browserFieldIsNull(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	return ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func browserRequireFields(fields map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if _, ok := fields[key]; !ok || browserFieldIsNull(fields, key) {
			return fmt.Errorf("field %q is required and cannot be null", key)
		}
	}
	return nil
}

func browserRejectNullFields(fields map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if browserFieldIsNull(fields, key) {
			return fmt.Errorf("field %q cannot be null", key)
		}
	}
	return nil
}

func browserTextLen(value string) int {
	return utf8.RuneCountInString(value)
}

// ---------------------------------------------------------------------------
// WorkRequest (work-request/1)
// ---------------------------------------------------------------------------

// Identifiers carries the recognized scholarly identifiers.
type Identifiers struct {
	DOI      string `json:"doi,omitempty"`
	PMID     string `json:"pmid,omitempty"`
	ArXiv    string `json:"arxiv,omitempty"`
	ISBN     string `json:"isbn,omitempty"`
	OpenAlex string `json:"openalex,omitempty"`
}

func (id Identifiers) empty() bool {
	return id.DOI == "" && id.PMID == "" && id.ArXiv == "" && id.ISBN == "" && id.OpenAlex == ""
}

// WorkRequest is one explicitly requested work.
type WorkRequest struct {
	SchemaVersion      string       `json:"schema_version"`
	RequestID          string       `json:"request_id"`
	Identifiers        *Identifiers `json:"identifiers,omitempty"`
	Title              string       `json:"title,omitempty"`
	Authors            []string     `json:"authors,omitempty"`
	Year               int          `json:"year,omitempty"`
	ZotioItemKey       string       `json:"zotio_item_key,omitempty"`
	Collection         string       `json:"collection,omitempty"`
	DesiredVersion     string       `json:"desired_version,omitempty"`
	AccessModeOverride string       `json:"access_mode_override,omitempty"`
	Resolver           string       `json:"resolver,omitempty"`
	MaxCostUSD         *float64     `json:"max_cost_usd,omitempty"`
	SourcesAllow       []string     `json:"sources_allow,omitempty"`
	SourcesDeny        []string     `json:"sources_deny,omitempty"`
}

// DecodeWorkRequest strictly parses and validates one WorkRequest document.
func DecodeWorkRequest(data []byte) (*WorkRequest, error) {
	var wr WorkRequest
	if err := strictDecode(data, &wr); err != nil {
		return nil, fmt.Errorf("work request: %w", err)
	}
	if err := wr.Validate(); err != nil {
		return nil, fmt.Errorf("work request: %w", err)
	}
	return &wr, nil
}

// Validate enforces the schema's invariants, including the identity rule:
// at least one identifier, or a full title/authors/year tuple.
func (wr *WorkRequest) Validate() error {
	if wr.SchemaVersion != WorkRequestSchemaVersion {
		return fmt.Errorf("schema_version %q, want %q", wr.SchemaVersion, WorkRequestSchemaVersion)
	}
	if !requestIDRE.MatchString(wr.RequestID) {
		return fmt.Errorf("invalid request_id %q", wr.RequestID)
	}
	hasIdentifiers := wr.Identifiers != nil && !wr.Identifiers.empty()
	if wr.Identifiers != nil {
		if wr.Identifiers.empty() {
			return fmt.Errorf("identifiers present but empty")
		}
		for _, check := range []struct {
			name, value string
			re          *regexp.Regexp
		}{
			{"doi", wr.Identifiers.DOI, doiRE},
			{"pmid", wr.Identifiers.PMID, pmidRE},
			{"arxiv", wr.Identifiers.ArXiv, arxivRE},
			{"isbn", wr.Identifiers.ISBN, isbnRE},
			{"openalex", wr.Identifiers.OpenAlex, openalexRE},
		} {
			if check.value != "" && !check.re.MatchString(check.value) {
				return fmt.Errorf("invalid %s %q", check.name, check.value)
			}
		}
	}
	hasTuple := wr.Title != "" && len(wr.Authors) > 0 && wr.Year != 0
	if !hasIdentifiers && !hasTuple {
		return fmt.Errorf("need identifiers or a title/authors/year tuple")
	}
	if wr.Title != "" && (len(wr.Title) < 3 || len(wr.Title) > 500) {
		return fmt.Errorf("title length %d out of range 3..500", len(wr.Title))
	}
	if len(wr.Authors) > 100 {
		return fmt.Errorf("too many authors (%d)", len(wr.Authors))
	}
	for _, a := range wr.Authors {
		if a == "" || len(a) > 200 {
			return fmt.Errorf("invalid author entry %q", a)
		}
	}
	if wr.Year != 0 && (wr.Year < 1000 || wr.Year > 2100) {
		return fmt.Errorf("year %d out of range", wr.Year)
	}
	if wr.ZotioItemKey != "" && !zoteroKeyRE.MatchString(wr.ZotioItemKey) {
		return fmt.Errorf("invalid zotio_item_key %q", wr.ZotioItemKey)
	}
	if err := enumOK("desired_version", wr.DesiredVersion, "published", "accepted", "preprint", "any"); err != nil {
		return err
	}
	if err := enumOK("access_mode_override", wr.AccessModeOverride, "conservative", "assisted", "maximal"); err != nil {
		return err
	}
	if wr.MaxCostUSD != nil && *wr.MaxCostUSD < 0 {
		return fmt.Errorf("max_cost_usd must be >= 0")
	}
	if len(wr.SourcesAllow) > 50 || len(wr.SourcesDeny) > 50 {
		return fmt.Errorf("source lists capped at 50 entries")
	}
	return nil
}

func enumOK(field, value string, allowed ...string) error {
	if value == "" {
		return nil
	}
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("invalid %s %q (allowed: %s)", field, value, strings.Join(allowed, ", "))
}

// ---------------------------------------------------------------------------
// AcquisitionBundle (acquisition-bundle/1)
// ---------------------------------------------------------------------------

// BundleIdentity is the resolved bibliographic identity of the acquired work.
type BundleIdentity struct {
	DOI      string   `json:"doi,omitempty"`
	Title    string   `json:"title"`
	Authors  []string `json:"authors"`
	Year     int      `json:"year,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// BundleCandidate records which source supplied the artifact and on what basis.
type BundleCandidate struct {
	Source         string `json:"source"`
	Version        string `json:"version"`
	AccessBasis    string `json:"access_basis"`
	ReuseLicense   string `json:"reuse_license"`
	LandingURL     string `json:"landing_url,omitempty"`
	SourceRecordID string `json:"source_record_id,omitempty"`
}

// BundleArtifact describes the immutable content-addressed file.
type BundleArtifact struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MIME      string `json:"mime"`
	PageCount int    `json:"page_count"`
	TextChars int64  `json:"text_chars,omitempty"`
	OCRUsed   bool   `json:"ocr_used"`
	Path      string `json:"path"`
}

// BundleValidation records the validation decision that admitted the artifact.
type BundleValidation struct {
	Structural string   `json:"structural"`
	Identity   string   `json:"identity"`
	Notes      []string `json:"notes,omitempty"`
}

// AcquisitionBundle is bundle.json for one ready job.
type AcquisitionBundle struct {
	SchemaVersion    string           `json:"schema_version"`
	JobID            string           `json:"job_id"`
	RequestID        string           `json:"request_id"`
	Identity         BundleIdentity   `json:"identity"`
	Candidate        BundleCandidate  `json:"candidate"`
	RetrievedAt      string           `json:"retrieved_at"`
	AdapterVersion   string           `json:"adapter_version,omitempty"`
	Artifact         BundleArtifact   `json:"artifact"`
	Validation       BundleValidation `json:"validation"`
	ProvenanceDigest string           `json:"provenance_digest"`
	ZotioItemKey     string           `json:"zotio_item_key,omitempty"`
}

// DecodeAcquisitionBundle strictly parses and validates one bundle.json document.
func DecodeAcquisitionBundle(data []byte) (*AcquisitionBundle, error) {
	var b AcquisitionBundle
	if err := strictDecode(data, &b); err != nil {
		return nil, fmt.Errorf("acquisition bundle: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("acquisition bundle: %w", err)
	}
	return &b, nil
}

// Validate enforces the schema plus the cross-field invariant that the
// artifact path is exactly its content address.
func (b *AcquisitionBundle) Validate() error {
	if b.SchemaVersion != AcquisitionBundleSchemaVersion {
		return fmt.Errorf("schema_version %q, want %q", b.SchemaVersion, AcquisitionBundleSchemaVersion)
	}
	if !requestIDRE.MatchString(b.JobID) {
		return fmt.Errorf("invalid job_id %q", b.JobID)
	}
	if !requestIDRE.MatchString(b.RequestID) {
		return fmt.Errorf("invalid request_id %q", b.RequestID)
	}
	if b.Identity.DOI != "" && !doiRE.MatchString(b.Identity.DOI) {
		return fmt.Errorf("invalid identity.doi %q", b.Identity.DOI)
	}
	// Full bibliographic identity is authoritative only for NEW-item bundles,
	// where papio creates the Zotero item from the bundle. For an
	// attach-to-existing bundle (ZotioItemKey set) the item already exists in
	// Zotero with its own metadata and the attach carries only the item key and
	// file, so title/authors are descriptive, not required — but their upper
	// bounds still apply to any values that are present.
	if b.ZotioItemKey == "" {
		if len(b.Identity.Title) < 3 {
			return fmt.Errorf("identity.title length out of range")
		}
		if len(b.Identity.Authors) == 0 {
			return fmt.Errorf("identity.authors must have 1..100 entries")
		}
	}
	if len(b.Identity.Title) > 500 {
		return fmt.Errorf("identity.title length out of range")
	}
	if len(b.Identity.Authors) > 100 {
		return fmt.Errorf("identity.authors must have 1..100 entries")
	}
	if b.Identity.Year != 0 && (b.Identity.Year < 1000 || b.Identity.Year > 2100) {
		return fmt.Errorf("identity.year %d out of range", b.Identity.Year)
	}
	if b.Candidate.Source == "" {
		return fmt.Errorf("candidate.source required")
	}
	if err := enumRequired("candidate.version", b.Candidate.Version, "published", "accepted", "preprint", "unknown"); err != nil {
		return err
	}
	if err := enumRequired("candidate.access_basis", b.Candidate.AccessBasis, "open_access", "licensed_api", "institutional", "manual"); err != nil {
		return err
	}
	if b.Candidate.ReuseLicense == "" {
		return fmt.Errorf("candidate.reuse_license required (use \"unknown\" when unknown)")
	}
	if _, err := time.Parse(time.RFC3339, b.RetrievedAt); err != nil {
		return fmt.Errorf("retrieved_at: %w", err)
	}
	if !sha256RE.MatchString(b.Artifact.SHA256) {
		return fmt.Errorf("invalid artifact.sha256")
	}
	if b.Artifact.SizeBytes < 1 {
		return fmt.Errorf("artifact.size_bytes must be >= 1")
	}
	if b.Artifact.MIME == "" {
		return fmt.Errorf("artifact.mime required")
	}
	if b.Artifact.PageCount < 1 {
		return fmt.Errorf("artifact.page_count must be >= 1")
	}
	if want := "artifacts/" + b.Artifact.SHA256 + ".pdf"; b.Artifact.Path != want {
		return fmt.Errorf("artifact.path %q must equal %q", b.Artifact.Path, want)
	}
	if b.Validation.Structural != "pass" {
		return fmt.Errorf("validation.structural must be \"pass\" in an exported bundle")
	}
	if err := enumRequired("validation.identity", b.Validation.Identity, "pass", "user_confirmed"); err != nil {
		return err
	}
	if !provenanceRE.MatchString(b.ProvenanceDigest) {
		return fmt.Errorf("invalid provenance_digest")
	}
	if b.ZotioItemKey != "" && !zoteroKeyRE.MatchString(b.ZotioItemKey) {
		return fmt.Errorf("invalid zotio_item_key %q", b.ZotioItemKey)
	}
	return nil
}

func enumRequired(field, value string, allowed ...string) error {
	if value == "" {
		return fmt.Errorf("%s required", field)
	}
	return enumOK(field, value, allowed...)
}

// ---------------------------------------------------------------------------
// Browser bridge messages (locked papio-browser/1)
// ---------------------------------------------------------------------------

// Browser message types.
const (
	MsgHello            = "hello"
	MsgHelloAck         = "hello_ack"
	MsgJobOffer         = "job_offer"
	MsgJobAccept        = "job_accept"
	MsgJobReject        = "job_reject"
	MsgAuthPending      = "auth_pending"
	MsgAuthReturned     = "auth_returned"
	MsgDownloadStarted  = "download_started"
	MsgDownloadComplete = "download_complete"
	MsgProviderOutcome  = "provider_outcome"
	MsgCancel           = "cancel"
	MsgAck              = "ack"
	MsgError            = "error"
)

// jobScoped lists the types that must carry a job_id.
var jobScoped = map[string]bool{
	MsgJobOffer: true, MsgJobAccept: true, MsgJobReject: true,
	MsgAuthPending: true, MsgAuthReturned: true,
	MsgDownloadStarted: true, MsgDownloadComplete: true,
	MsgProviderOutcome: true, MsgCancel: true,
}

// HelloPayload announces the extension and its adapter versions.
type HelloPayload struct {
	ExtensionVersion string            `json:"extension_version"`
	AdapterVersions  map[string]string `json:"adapter_versions,omitempty"`
}

// HelloAckPayload announces the daemon version and supported bridge features.
// Both fields are optional so extensions remain compatible with older daemons
// that acknowledge hello with an empty object.
type HelloAckPayload struct {
	DaemonVersion string   `json:"daemon_version,omitempty"`
	Features      []string `json:"features,omitempty"`
	// ResolverOrigins are the https origins of the daemon's configured OpenURL
	// resolvers. The extension requests a host permission for each so it can
	// steer that resolver's menu; institution identity stays in config, not code.
	ResolverOrigins []string `json:"resolver_origins,omitempty"`
}

// JobOfferPayload asks the extension to open one OpenURL-resolved job.
type JobOfferPayload struct {
	OpenURL           string            `json:"openurl"`
	ProviderHosts     []string          `json:"provider_hosts"`
	Expected          *JobOfferExpected `json:"expected,omitempty"`
	AccessMode        string            `json:"access_mode"`
	LoginEntityID     string            `json:"login_entity_id,omitempty"`
	ProquestAccountID string            `json:"proquest_account_id,omitempty"`
	ExpiresAt         string            `json:"expires_at"`
}

// JobOfferExpected carries wrong-work guard hints.
type JobOfferExpected struct {
	DOI   string `json:"doi,omitempty"`
	Title string `json:"title,omitempty"`
}

// AuthPayload deliberately carries only timing. No URL, host, title, query, or
// fragment fields exist so identity-provider addresses cannot cross the bridge.
type AuthPayload struct {
	ElapsedMS *int64 `json:"elapsed_ms,omitempty"`
}

// DownloadStartedPayload reports a Chrome download the adapter initiated or
// the user selected for this job.
type DownloadStartedPayload struct {
	DownloadID int64  `json:"download_id"`
	Filename   string `json:"filename"`
}

// DownloadCompletePayload reports the finished download's metadata (never bytes).
type DownloadCompletePayload struct {
	DownloadID int64  `json:"download_id"`
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"size_bytes"`
}

// ProviderOutcomePayload is the adapter's terminal observation for a job.
type ProviderOutcomePayload struct {
	Outcome        string `json:"outcome"`
	AdapterVersion string `json:"adapter_version,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// ErrorPayload is a normalized bridge error.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EmptyPayload is used by types that carry no data (ack, job_accept,
// job_reject, cancel).
type EmptyPayload struct{}

// BrowserMessage is one decoded native-messaging envelope. Payload holds the
// type-specific struct (e.g. *HelloPayload).
type BrowserMessage struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	MsgID    string `json:"msg_id"`
	JobID    string `json:"job_id,omitempty"`
	Seq      int64  `json:"seq"`
	Payload  any    `json:"payload"`
}

// browserEnvelope is the wire form before payload dispatch.
type browserEnvelope struct {
	Protocol string          `json:"protocol"`
	Type     string          `json:"type"`
	MsgID    string          `json:"msg_id"`
	JobID    string          `json:"job_id,omitempty"`
	Seq      *int64          `json:"seq"`
	Payload  json.RawMessage `json:"payload"`
}

// DecodeBrowserMessage strictly parses one bridge frame: size cap, envelope,
// then a fail-closed type-specific payload decode.
func DecodeBrowserMessage(data []byte) (*BrowserMessage, error) {
	if len(data) > MaxBrowserMessageBytes {
		return nil, fmt.Errorf("browser message: %d bytes exceeds cap %d", len(data), MaxBrowserMessageBytes)
	}
	var env browserEnvelope
	if err := strictDecode(data, &env); err != nil {
		return nil, fmt.Errorf("browser message: %w", err)
	}
	envelopeFields, err := browserObjectFields(data, "browser message")
	if err != nil {
		return nil, err
	}
	if _, ok := envelopeFields["job_id"]; ok {
		if browserFieldIsNull(envelopeFields, "job_id") || !requestIDRE.MatchString(env.JobID) {
			return nil, fmt.Errorf("browser message: invalid job_id %q", env.JobID)
		}
	}
	if env.Protocol != BrowserProtocolVersion {
		return nil, fmt.Errorf("browser message: protocol %q, want %q", env.Protocol, BrowserProtocolVersion)
	}
	if !msgIDRE.MatchString(env.MsgID) {
		return nil, fmt.Errorf("browser message: invalid msg_id %q", env.MsgID)
	}
	if env.Seq == nil || *env.Seq < 0 || *env.Seq > MaxBrowserInteger {
		return nil, fmt.Errorf("browser message: seq required in range 0..%d", MaxBrowserInteger)
	}
	if jobScoped[env.Type] && env.JobID == "" {
		return nil, fmt.Errorf("browser message: type %q requires a valid job_id", env.Type)
	}
	if env.Payload == nil {
		return nil, fmt.Errorf("browser message: payload required")
	}
	payloadFields, err := browserObjectFields(env.Payload, "browser message payload")
	if err != nil {
		return nil, err
	}

	msg := &BrowserMessage{Protocol: env.Protocol, Type: env.Type, MsgID: env.MsgID, JobID: env.JobID, Seq: *env.Seq}
	switch env.Type {
	case MsgHello:
		p := &HelloPayload{}
		if err = browserRequireFields(payloadFields, "extension_version"); err == nil {
			err = browserRejectNullFields(payloadFields, "adapter_versions")
		}
		var adapterFields map[string]json.RawMessage
		if raw, ok := payloadFields["adapter_versions"]; ok && err == nil {
			adapterFields, err = browserObjectFields(raw, "hello.adapter_versions")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if p.ExtensionVersion == "" || browserTextLen(p.ExtensionVersion) > 50 {
				err = fmt.Errorf("hello.extension_version required (max 50)")
			} else if len(p.AdapterVersions) > 50 {
				err = fmt.Errorf("hello.adapter_versions capped at 50")
			}
		}
		for key, raw := range adapterFields {
			if err != nil {
				break
			}
			var value string
			if browserFieldIsNull(adapterFields, key) {
				err = fmt.Errorf("hello.adapter_versions.%s cannot be null", key)
			} else if err = strictDecode(raw, &value); err == nil && browserTextLen(value) > 50 {
				err = fmt.Errorf("hello.adapter_versions.%s exceeds 50 chars", key)
			}
		}
		msg.Payload = p
	case MsgHelloAck:
		p := &HelloAckPayload{}
		err = browserRejectNullFields(payloadFields, "daemon_version", "features", "resolver_origins")
		var features []json.RawMessage
		if raw, ok := payloadFields["features"]; ok && err == nil {
			err = strictDecode(raw, &features)
		}
		var origins []json.RawMessage
		if raw, ok := payloadFields["resolver_origins"]; ok && err == nil {
			err = strictDecode(raw, &origins)
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		for _, feature := range features {
			if err != nil {
				break
			}
			if string(feature) == "null" {
				err = fmt.Errorf("hello_ack.features entries cannot be null")
			}
		}
		for _, origin := range origins {
			if err != nil {
				break
			}
			if string(origin) == "null" {
				err = fmt.Errorf("hello_ack.resolver_origins entries cannot be null")
			}
		}
		msg.Payload = p
	case MsgJobOffer:
		p := &JobOfferPayload{}
		if err = browserRequireFields(payloadFields, "openurl", "provider_hosts", "access_mode", "expires_at"); err == nil {
			err = browserRejectNullFields(payloadFields, "expected", "login_entity_id", "proquest_account_id")
		}
		if raw, ok := payloadFields["expected"]; ok && err == nil {
			var expectedFields map[string]json.RawMessage
			if expectedFields, err = browserObjectFields(raw, "job_offer.expected"); err == nil {
				err = browserRejectNullFields(expectedFields, "doi", "title")
			}
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgAuthPending, MsgAuthReturned:
		p := &AuthPayload{}
		err = browserRejectNullFields(payloadFields, "elapsed_ms")
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil && p.ElapsedMS != nil && (*p.ElapsedMS < 0 || *p.ElapsedMS > MaxBrowserInteger) {
			err = fmt.Errorf("elapsed_ms must be in range 0..%d", MaxBrowserInteger)
		}
		msg.Payload = p
	case MsgDownloadStarted:
		p := &DownloadStartedPayload{}
		if err = browserRequireFields(payloadFields, "download_id", "filename"); err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = validateDownload(p.DownloadID, p.Filename)
		}
		msg.Payload = p
	case MsgDownloadComplete:
		p := &DownloadCompletePayload{}
		if err = browserRequireFields(payloadFields, "download_id", "filename", "size_bytes"); err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = validateDownload(p.DownloadID, p.Filename)
		}
		if err == nil && (p.SizeBytes < 1 || p.SizeBytes > MaxBrowserInteger) {
			err = fmt.Errorf("size_bytes must be in range 1..%d", MaxBrowserInteger)
		}
		msg.Payload = p
	case MsgProviderOutcome:
		p := &ProviderOutcomePayload{}
		if err = browserRequireFields(payloadFields, "outcome"); err == nil {
			err = browserRejectNullFields(payloadFields, "adapter_version", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgError:
		p := &ErrorPayload{}
		if err = browserRequireFields(payloadFields, "code", "message"); err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if !errorCodeRE.MatchString(p.Code) {
				err = fmt.Errorf("invalid error code %q", p.Code)
			} else if p.Message == "" || browserTextLen(p.Message) > 1000 {
				err = fmt.Errorf("error message required (max 1000)")
			}
		}
		msg.Payload = p
	case MsgAck, MsgJobAccept, MsgJobReject, MsgCancel:
		p := &EmptyPayload{}
		err = strictDecode(env.Payload, p)
		msg.Payload = p
	default:
		return nil, fmt.Errorf("browser message: unknown type %q (fail closed)", env.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("browser message %s: %w", env.Type, err)
	}
	return msg, nil
}

func (p *JobOfferPayload) validate() error {
	if p.OpenURL == "" || browserTextLen(p.OpenURL) > 4000 || !strings.HasPrefix(p.OpenURL, "https://") {
		return fmt.Errorf("openurl must be a bounded https URL")
	}
	if p.LoginEntityID != "" && (browserTextLen(p.LoginEntityID) > 4000 || !strings.HasPrefix(p.LoginEntityID, "https://")) {
		return fmt.Errorf("login_entity_id must be a bounded https URL")
	}
	if p.ProquestAccountID != "" {
		if len(p.ProquestAccountID) > 64 {
			return fmt.Errorf("proquest_account_id must be digits")
		}
		for _, r := range p.ProquestAccountID {
			if r < '0' || r > '9' {
				return fmt.Errorf("proquest_account_id must be digits")
			}
		}
	}
	if len(p.ProviderHosts) < 1 || len(p.ProviderHosts) > 20 {
		return fmt.Errorf("provider_hosts must have 1..20 entries")
	}
	for _, h := range p.ProviderHosts {
		if !hostRE.MatchString(h) {
			return fmt.Errorf("invalid provider host %q", h)
		}
	}
	if err := enumRequired("access_mode", p.AccessMode, "assisted", "maximal"); err != nil {
		return err
	}
	if !rfc3339RE.MatchString(p.ExpiresAt) {
		return fmt.Errorf("expires_at must be RFC3339")
	}
	if _, err := time.Parse(time.RFC3339, p.ExpiresAt); err != nil {
		return fmt.Errorf("expires_at: %w", err)
	}
	if p.Expected != nil {
		if browserTextLen(p.Expected.DOI) > 300 || browserTextLen(p.Expected.Title) > 500 {
			return fmt.Errorf("expected hints exceed bounds")
		}
	}
	return nil
}

func (p *HelloAckPayload) validate() error {
	if browserTextLen(p.DaemonVersion) > 50 {
		return fmt.Errorf("hello_ack.daemon_version exceeds 50 chars")
	}
	if len(p.Features) > 32 {
		return fmt.Errorf("hello_ack.features capped at 32")
	}
	for _, feature := range p.Features {
		if feature == "" || browserTextLen(feature) > 64 {
			return fmt.Errorf("hello_ack.features entries must be non-empty (max 64)")
		}
	}
	if len(p.ResolverOrigins) > 32 {
		return fmt.Errorf("hello_ack.resolver_origins capped at 32")
	}
	for _, origin := range p.ResolverOrigins {
		if !validResolverOrigin(origin) {
			return fmt.Errorf("hello_ack.resolver_origins entries must be bounded https origins")
		}
	}
	return nil
}

// validResolverOrigin reports whether s is a bounded https origin
// (scheme://host[:port]) with no path, query, or fragment. The extension
// re-validates with URL() before requesting a host permission for it.
func validResolverOrigin(s string) bool {
	if browserTextLen(s) > 300 || !strings.HasPrefix(s, "https://") {
		return false
	}
	host := s[len("https://"):]
	return host != "" && !strings.ContainsAny(host, "/?#")
}

func (p *ProviderOutcomePayload) validate() error {
	if err := enumRequired("outcome", p.Outcome,
		"no_entitlement", "document_delivery_available", "wrong_work", "ui_changed",
		"rate_limited", "terms_acceptance_required", "human_auth_required", "cancelled"); err != nil {
		return err
	}
	if browserTextLen(p.AdapterVersion) > 50 {
		return fmt.Errorf("adapter_version exceeds 50 chars")
	}
	if browserTextLen(p.Detail) > 500 {
		return fmt.Errorf("detail exceeds 500 chars")
	}
	return nil
}

func validateDownload(id int64, filename string) error {
	if id < 0 || id > MaxBrowserInteger {
		return fmt.Errorf("download_id must be in range 0..%d", MaxBrowserInteger)
	}
	if !filenameRE.MatchString(filename) {
		return fmt.Errorf("filename must be a bare name without path separators")
	}
	return nil
}
