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
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Contract versions locked after live browser, acquisition, bundle-export, and
// Zotio import acceptance.
const (
	WorkRequestSchemaVersion       = "work-request/1"
	AcquisitionBundleSchemaVersion = "acquisition-bundle/1"
	// AcquisitionBundleSchemaVersionV2 adds candidate.entitlement. It is a new
	// schema rather than an added v1 field because v1 sets
	// additionalProperties:false and DecodeAcquisitionBundle rejects unknown
	// fields recursively, so a v1 consumer would reject the whole document.
	AcquisitionBundleSchemaVersionV2 = "acquisition-bundle/2"
	BrowserProtocolVersion           = "papio-browser/1"
)

// MaxBrowserMessageBytes caps one encoded native-messaging frame well below
// Chrome's documented 1 MiB host-to-extension limit.
const MaxBrowserMessageBytes = 256 << 10

// MaxBrowserInteger is the largest integer represented exactly by both Go
// int64 and JavaScript number values.
const MaxBrowserInteger int64 = 1<<53 - 1

var (
	requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	adapterIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// entitlementRefRE mirrors the consumer's closed vocabulary. The cleartext
	// source form is preferred: hashing a public constant like "crossref_tdm"
	// buys no secrecy and destroys legibility in an audit trail whose whole
	// point is to explain why a candidate was accepted.
	entitlementRefRE = regexp.MustCompile(`^entitlement:(source:[a-z0-9_]{1,64}|sha256:[0-9a-f]{64})$`)
	msgIDRE          = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	clientFeatureRE  = regexp.MustCompile(`^[a-z0-9_]+$`)
	zoteroKeyRE      = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
	doiRE            = regexp.MustCompile(`^10\.[0-9]{4,9}/\S{1,200}$`)
	pmidRE           = regexp.MustCompile(`^[0-9]{1,10}$`)
	arxivRE          = regexp.MustCompile(`^([0-9]{4}\.[0-9]{4,5})(v[0-9]+)?$|^[a-z-]+(\.[A-Z]{2})?/[0-9]{7}$`)
	isbnRE           = regexp.MustCompile(`^[0-9Xx-]{10,17}$`)
	openalexRE       = regexp.MustCompile(`^W[0-9]{4,12}$`)
	sha256RE         = regexp.MustCompile(`^[a-f0-9]{64}$`)
	provenanceRE     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hostRE           = regexp.MustCompile(`^[a-z0-9.-]{3,253}$`)
	rfc3986URITextRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]*$`)
	// originHostRE is the resolver-origin host grammar used ONLY by
	// validateResolverOriginHint: lowercase RFC 1035 labels (alnum first/last
	// character, hyphens interior only, 1-63 chars per label) joined by
	// single dots — ONE label or several. It must stay byte-identical to
	// ORIGIN_HOST_RE in extension/src/protocol.ts and the host portion of
	// session_evidence.origin_hint's pattern in
	// protocol/browser-v1.schema.json — see validateResolverOriginHint for
	// why label count is deliberately unconstrained.
	originHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	// originPortRE is the resolver-origin port grammar used ONLY by
	// validateResolverOriginHint, mirroring the schema pattern's trailing
	// `(:[0-9]{1,5})?` group: 1-5 decimal digits, never empty. net/url
	// already rejects a non-numeric port at Parse time, so the only shapes
	// left to bound here are length and emptiness — but bounding them is
	// the fix: net/url places no upper bound on a numeric port and returns
	// "" from Port() both for "no port at all" and for a bare trailing
	// colon with nothing after it, so without this check
	// "https://library:123456" and "https://library:" both decoded in Go
	// while the schema's `{1,5}` port group rejects them — Go laxer than
	// its own published contract, the direction this package exists to
	// prevent.
	originPortRE = regexp.MustCompile(`^[0-9]{1,5}$`)
	errorCodeRE  = regexp.MustCompile(`^[a-z0-9_]{2,50}$`)
	filenameRE   = regexp.MustCompile(`^[^/\\]{1,255}$`)
	base64RE     = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)
	rfc3339RE    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)
)

// strictDecode unmarshals data into v, rejecting unknown fields and trailing input.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data after JSON document")
		}
		return fmt.Errorf("trailing data after JSON document: %w", err)
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

func browserRejectNoncanonicalFields(fields map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if _, present, err := lookupJSONKey(fields, key); err != nil {
			return err
		} else if present {
			if _, canonical := fields[key]; !canonical {
				return fmt.Errorf("field %q must use canonical casing", key)
			}
		}
	}
	return nil
}

func browserTextLen(value string) int {
	return utf8.RuneCountInString(value)
}

func browserHasNUL(value string) bool {
	return strings.IndexByte(value, 0) >= 0
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
	if err := enumOK("access_mode_override", wr.AccessModeOverride, "conservative", "assisted", "delegated"); err != nil {
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

// BundleEntitlement records the route by which access was obtained. It exists
// only in acquisition-bundle/2.
//
// A route, never an identity: papio never authenticates a human and never holds
// institutional credentials, so it can report how bytes were reached but not
// who was entitled to them. Every value here is observed; nothing is inferred,
// and the whole object is omitted rather than guessed (ADR-0009, ADR-0010).
type BundleEntitlement struct {
	// Route is a sanitised bare reference: scheme://host with no path, query,
	// fragment, or userinfo. Enforced at emission and fail-closed, so papio
	// never sends a value a consumer must reject.
	Route string `json:"route"`
	// EntitlementRef names WHICH entitlement, never a secret and never a
	// credential instance. Rotating an API key does not change it; no rotation
	// semantics may be built on this field.
	EntitlementRef string `json:"entitlement_ref,omitempty"`
	// AcquisitionMode is derived from the accepted candidate's access basis.
	AcquisitionMode string `json:"acquisition_mode"`
}

// BundleCandidate records which source supplied the artifact and on what basis.
type BundleCandidate struct {
	Source         string             `json:"source"`
	Version        string             `json:"version"`
	AccessBasis    string             `json:"access_basis"`
	ReuseLicense   string             `json:"reuse_license"`
	LandingURL     string             `json:"landing_url,omitempty"`
	SourceRecordID string             `json:"source_record_id,omitempty"`
	Entitlement    *BundleEntitlement `json:"entitlement,omitempty"`
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
	if err := checkEntitlementWireShape(data); err != nil {
		return nil, fmt.Errorf("acquisition bundle: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("acquisition bundle: %w", err)
	}
	return &b, nil
}

// checkEntitlementWireShape closes the holes that declaring optional fields
// opened. Both are cases where the decoded Go value cannot distinguish "absent"
// from "explicitly empty", so Validate is structurally unable to see them and
// the check has to look at the raw document.
//
// Before candidate.entitlement existed, v1's DisallowUnknownFields rejected the
// key outright; now the key is known and JSON null decodes to a nil pointer, so
// a v1 document carrying `"entitlement": null` would have passed and silently
// widened a frozen schema. Likewise `"entitlement_ref": ""` decodes to the same
// empty string as an omitted field, while the published schema requires any
// present value to match the reference pattern.
func checkEntitlementWireShape(data []byte) error {
	var document struct {
		Candidate map[string]json.RawMessage `json:"candidate"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil // strictDecode already rejected anything malformed
	}
	rawEntitlement, present, err := lookupJSONKey(document.Candidate, "entitlement")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(rawEntitlement), []byte("null")) {
		return fmt.Errorf("candidate.entitlement must be an object, not null")
	}
	var entitlement map[string]json.RawMessage
	if err := json.Unmarshal(rawEntitlement, &entitlement); err != nil {
		return nil
	}
	rawRef, present, err := lookupJSONKey(entitlement, "entitlement_ref")
	if err != nil {
		return err
	}
	if present {
		trimmed := bytes.TrimSpace(rawRef)
		if bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
			return fmt.Errorf("candidate.entitlement.entitlement_ref must be omitted rather than empty")
		}
	}
	return nil
}

// lookupJSONKey resolves one key the way encoding/json resolves a struct field,
// and refuses to guess when it cannot.
//
// encoding/json matches field names case-insensitively, and when an object
// carries several matching members the LAST one in document order wins. A raw
// map cannot reproduce that: json.Unmarshal into map[string]json.RawMessage
// keeps each distinct spelling under its own entry, and Go's map iteration has
// no order. So a guard that picks the exact spelling can inspect a different
// member than the decoder used. That is not theoretical — it let
// `{"entitlement": {...}, "Entitlement": null}` carry an entitlement key
// through a v1 document, because the decoder saw the trailing null and left the
// field nil while the guard saw the leading object and passed.
//
// Replaying decoder order would mean re-tokenising the document. Colliding
// spellings are ambiguous, papio never emits them, and every honest producer
// has exactly one, so the guard refuses the document instead. Fail closed.
func lookupJSONKey(object map[string]json.RawMessage, key string) (json.RawMessage, bool, error) {
	var found json.RawMessage
	matches := 0
	for name, raw := range object {
		if strings.EqualFold(name, key) {
			matches++
			found = raw
		}
	}
	if matches > 1 {
		return nil, false, fmt.Errorf("ambiguous %q: %d case-colliding keys", key, matches)
	}
	return found, matches == 1, nil
}

// Validate enforces the schema plus the cross-field invariant that the
// artifact path is exactly its content address.
func (b *AcquisitionBundle) Validate() error {
	switch b.SchemaVersion {
	case AcquisitionBundleSchemaVersion:
		// v1 froze its shape with additionalProperties:false. Accepting an
		// entitlement here would silently widen a published schema, so a v1
		// document carrying one is a malformed v1 document, not a lenient v2.
		if b.Candidate.Entitlement != nil {
			return fmt.Errorf("candidate.entitlement requires schema_version %q", AcquisitionBundleSchemaVersionV2)
		}
	case AcquisitionBundleSchemaVersionV2:
		if err := b.Candidate.Entitlement.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("schema_version %q, want %q or %q",
			b.SchemaVersion, AcquisitionBundleSchemaVersion, AcquisitionBundleSchemaVersionV2)
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

// validate enforces the sanitised-reference rule at emission. A nil entitlement
// is valid: the object is optional and omitted whenever papio did not observe a
// route. papio fails closed here rather than shipping a value the consumer must
// reject — the rule is papio's obligation, not a note about the consumer.
func (e *BundleEntitlement) validate() error {
	if e == nil {
		return nil
	}
	if err := validateBareRoute(e.Route); err != nil {
		return err
	}
	if e.EntitlementRef != "" && !entitlementRefRE.MatchString(e.EntitlementRef) {
		return fmt.Errorf("invalid candidate.entitlement.entitlement_ref %q: want entitlement:source:<name> or entitlement:sha256:<64 hex>", e.EntitlementRef)
	}
	return enumRequired("candidate.entitlement.acquisition_mode", e.AcquisitionMode,
		"open_access", "daemon_held_credential", "operator_browser_session")
}

// validateBareRoute admits only a bare origin: an https URL with a host and
// nothing else — no path, query, fragment, or userinfo.
//
// It must never be laxer than the published schema's `^https://[^/?#@]+$` plus
// maxLength, because this project validates twice on purpose and a validator
// laxer than its own schema is a decoder disagreement waiting to be exported.
// It is deliberately stricter in places — net/url rejects unbracketed IPv6,
// percent-encoded and whitespace-bearing hosts that the bare character class
// would admit — and stricter is the safe direction. The path check earns its
// place: a path is where signed tokens live in several CDN schemes, and
// redact.URL preserves the whole path when the source URL had no query string,
// so an emitter that reached for redact.URL instead of redact.Host would sail
// past a scheme/host-only check.
//
// The scheme is compared against the raw prefix rather than u.Scheme because
// url.Parse lowercases the scheme, so "HTTPS://host" would satisfy a
// u.Scheme == "https" test while the schema's case-sensitive pattern rejects
// it — laxer than the published contract, in exactly the direction that
// matters.
//
// The literal '?' and '#' checks are likewise not redundant with the parsed
// fields: redact.URL returns "…?<redacted>" to mark that evidence was removed,
// which is right for an operator log and fatal for a route, because the marker
// IS query data.
func validateBareRoute(route string) error {
	if route == "" {
		return fmt.Errorf("candidate.entitlement.route required")
	}
	// Code points, matching JSON Schema's maxLength, rather than bytes.
	if utf8.RuneCountInString(route) > 2000 {
		return fmt.Errorf("candidate.entitlement.route length %d exceeds 2000", utf8.RuneCountInString(route))
	}
	if strings.ContainsAny(route, "?#") {
		return fmt.Errorf("candidate.entitlement.route %q must not retain URL query or fragment data", route)
	}
	if !strings.HasPrefix(route, "https://") {
		return fmt.Errorf("candidate.entitlement.route %q must be an https URL with a host", route)
	}
	u, err := url.Parse(route)
	if err != nil {
		return fmt.Errorf("invalid candidate.entitlement.route %q", route)
	}
	if u.Host == "" {
		return fmt.Errorf("candidate.entitlement.route %q must be an https URL with a host", route)
	}
	if u.User != nil {
		return fmt.Errorf("candidate.entitlement.route %q must not retain URL credentials", route)
	}
	if u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return fmt.Errorf("candidate.entitlement.route %q must be a bare origin with no path", route)
	}
	return nil
}

// validateBareLowercaseOrigin enforces the shared bare-origin wire rule: an
// https origin with a lowercase, structurally valid host and
// nothing else — no path, query, fragment, userinfo, or mixed-case host.
// This function exists because three independent implementations of "bare
// resolver origin" (this package, parseBrowserMessage in
// extension/src/protocol.ts, and the published JSON Schema) previously
// disagreed on whether "https://EXAMPLE.com" decodes at all:
//   - Go, via validateBareRoute, accepted it because net/url's Hostname()
//     preserves case and nothing downstream compared it against anything
//     case-sensitive.
//   - The TypeScript parser rejected it, but only as a side effect: the
//     WHATWG URL parser lowercases the host of a special scheme internally,
//     so `${parsed.protocol}//${parsed.host}` no longer equals the raw
//     uppercase input and the round-trip equality check fails. That is an
//     accident of implementation, not a stated rule — nothing prevents a
//     future refactor away from the round-trip trick from losing it.
//
// The rule settled on: reject rather than normalize case, even though
// ResolverProfileForOrigin (internal/config/config.go) already lowercases
// and does a case-insensitive Hostname comparison, so case alone cannot
// currently misroute a match. That downstream leniency is not a reason to
// leave the wire format ambiguous: the bug this closes is that the three
// implementations disagreed about whether an uppercase host decodes AT
// ALL, which is a fail-closed-contract violation independent of what any
// one consumer happens to tolerate afterward. Normalizing case here
// instead of rejecting it would trade that decode-layer bug for a silent
// one: a caller reading the decoded OriginHint back (logs, a future
// consumer added without ResolverProfileForOrigin's EqualFold) would see a
// value that never appeared on the wire. Rejecting keeps the decoded value
// always equal to the wire value, which is the invariant every other
// browser-v1 string field relies on. Uppercase is unreachable from a
// genuine producer — origin_hint is always derived via `new URL(...)`,
// which lowercases the host for https — so rejecting it costs nothing a
// real deployment needs.
//
// The host must NOT be required to have two or more labels. An earlier
// version of this function did require it, on the unverified assumption
// that "a resolver is never bare-hostname-only" — checked against
// internal/config/config.go's validateOpenURLBase (which requires only an
// https scheme and a non-empty host: no FQDN, no label count) and found
// false. browser.openurl_base_url = "https://library" is a valid config
// today for an intranet resolver reachable only by a single-label
// hostname, and the extension derives origin_hint straight from that
// configured origin. The multi-label requirement therefore rejected a
// value a legitimate deployment could and did emit — and because
// Bridge.send (extension/src/background.ts) self-validates every outbound
// frame and silently drops an invalid one, that institution's
// session_evidence frames vanished permanently (no crash, just a
// console.error nobody watches), while on the inbound side the same value
// is a fatal decode under version skew (this project ships two papio
// binaries; see AGENTS.md), repeatedly killing the native-messaging
// session. The wire validator must never be stricter than what a valid
// config can produce, so label count is deliberately NOT constrained here,
// and neither is a minimum host length beyond "non-empty". Do not re-add
// either bound without first tightening validateOpenURLBase to match, or
// this exact bug reopens.
//
// A bracketed IPv6 literal (e.g. "https://[::1]") is rejected, but not by
// a dedicated check: originHostRE's charset has no room for '[', ']', or
// ':', so it never matches. This is the one deliberate gap against "never
// reject what a valid config can produce": validateOpenURLBase does not
// exclude an IPv6-literal openurl_base_url either. It is accepted anyway,
// scoped narrowly, for a concrete reason distinct from the single-label
// case above: the two runtimes cannot even agree on what string to test.
// net/url's Hostname() strips the brackets ("::1") while the WHATWG URL
// parser's .hostname keeps them ("[::1]"), AND — unlike a DNS host, which
// both runtimes lowercase identically before this validator ever sees it —
// net/url does NOT lowercase IPv6 hex digits (`url.Parse("https://[FE80::1]/").Hostname()`
// stays "FE80::1") while the WHATWG parser does. Accepting IPv6 correctly
// would need a second, bracket-and-case-aware code path duplicated three
// ways instead of one shared regex, which is exactly the kind of
// implementation drift this function exists to close. No observed
// browser.resolvers.* origin is an IPv6 literal, so the safer near-term
// call is to reject the one host shape the three implementations cannot
// trivially agree on, rather than risk them agreeing on paper and drifting
// in practice. Revisit with an explicit bracket-aware rule in all three
// places together if a real deployment ever needs it.
// originHostRE's grammar (RFC 1035 labels joined by single dots) also
// forecloses a leading/trailing dot and a ".." run for free, because every
// dot in that grammar sits between two non-empty labels — no separate pass
// needed, unlike delivery_context.page_host next to this function's caller.
//
// The three implementations agree exactly only over the DNS-shaped subset a
// genuine producer emits: an https origin whose host is a lowercase RFC
// 1035 label chain (one label or several, per the note above) and whose
// port, if present, is 1-5 non-empty decimal digits. Three shapes are
// known, accepted divergences outside that subset, none reachable from a
// genuine producer because origin_hint is always derived via
// `new URL(...)` on the browser side before it ever reaches the wire:
//   - A purely numeric single-label host (e.g. "https://123"): Go and the
//     schema both accept it — originHostRE's grammar treats a digit like
//     any other label character — but the WHATWG URL parser's "host ends
//     in a number" rule reparses it as an IPv4 address before
//     ORIGIN_HOST_RE ever runs, so `new URL("https://123").host` is
//     "0.0.0.123" and parseBrowserMessage's round-trip equality check
//     rejects the original string. Matching that reparse would mean
//     duplicating WHATWG's IPv4 grammar here; not worth it for a shape no
//     real config produces (a bare numeric openurl_base_url host is not an
//     institutional resolver).
//   - A non-canonical port, e.g. a zero-padded ":08443": `new URL(...)`
//     drops the leading zero, so parseBrowserMessage's round-trip check
//     rejects the original digits while Go and the schema — which only
//     bound digit count, not leading zeros — accept them. Same
//     reject-rather-than-normalize tradeoff as case above, just not worth
//     a dedicated no-leading-zero rule.
//   - An explicit empty origin_hint (`""`): this function rejects it, but
//     SessionEvidencePayload.validate only calls it when the field is
//     non-empty, treating "" the same as an omitted optional field — so an
//     explicit `"origin_hint": ""` decodes in Go. The schema pattern
//     (which always requires the "https://" prefix) and
//     `new URL("")` in TypeScript both reject it outright. The gap is one
//     layer up, in the caller's presence check, not in this function.
//
// A fourth shape — a port outside 1-5 digits, including the
// empty-after-colon form "https://library:" (net/url happily parses it
// with Port() == "") — used to be a divergence in the dangerous direction:
// Go accepted it because u.Hostname() silently discards the port, while
// the schema's `(:[0-9]{1,5})?$` group rejects it. That direction — Go
// laxer than its own published contract — is exactly what this package
// exists to prevent, so it is closed below: the port is validated
// explicitly against originPortRE instead of trusting Hostname() to have
// stripped away anything that mattered.
// validateBareLowercaseOrigin is the single shared implementation of the
// "bare https origin with a lowercase host" wire rule, used by both
// session_evidence.origin_hint (via validateResolverOriginHint below) and
// page_bulk_submit_request.source.origin (PageBulkSubmitSource.validate).
// what is the field path used in every returned error message.
func validateBareLowercaseOrigin(what, value string) error {
	if value == "" {
		return fmt.Errorf("%s required", what)
	}
	if strings.ContainsAny(value, "?#") {
		return fmt.Errorf("%s %q must not retain URL query or fragment data", what, value)
	}
	if !strings.HasPrefix(value, "https://") {
		return fmt.Errorf("%s %q must be an https URL with a host", what, value)
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid %s %q", what, value)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q must be an https URL with a host", what, value)
	}
	if u.User != nil {
		return fmt.Errorf("%s %q must not retain URL credentials", what, value)
	}
	if u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return fmt.Errorf("%s %q must be a bare origin with no path", what, value)
	}
	if !originHostRE.MatchString(u.Hostname()) {
		return fmt.Errorf("%s %q must have a lowercase resolver host", what, value)
	}
	// u.Hostname() silently drops the port; validate it explicitly rather
	// than letting a malformed or oversized one pass unseen.
	if port := strings.TrimPrefix(u.Host, u.Hostname()); port != "" {
		if !originPortRE.MatchString(strings.TrimPrefix(port, ":")) {
			return fmt.Errorf("%s %q must have a 1-5 digit port", what, value)
		}
	}
	return nil
}

// validateResolverOriginHint enforces session_evidence.origin_hint's exact
// wire rule by delegating to validateBareLowercaseOrigin — see that
// function's doc comment above for the shared grammar, and this function's
// historical doc comment (still above, unmoved) for why the rule is what
// it is.
func validateResolverOriginHint(hint string) error {
	return validateBareLowercaseOrigin("session_evidence.origin_hint", hint)
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
	MsgHello                           = "hello"
	MsgHelloAck                        = "hello_ack"
	MsgPageAcquire                     = "page_acquire"
	MsgPageAcquireAck                  = "page_acquire_ack"
	MsgPageCapture                     = "page_capture"
	MsgPageCaptureRequest              = "page_capture_request"
	MsgPageCaptureRequestResult        = "page_capture_request_result"
	MsgJobOffer                        = "job_offer"
	MsgHandoffOutcome                  = "handoff_outcome"
	MsgJobAccept                       = "job_accept"
	MsgJobReject                       = "job_reject"
	MsgAuthPending                     = "auth_pending"
	MsgAuthReturned                    = "auth_returned"
	MsgSessionEvidence                 = "session_evidence"
	MsgDownloadStarted                 = "download_started"
	MsgDownloadComplete                = "download_complete"
	MsgDeliveryContext                 = "delivery_context"
	MsgProviderOutcome                 = "provider_outcome"
	MsgCancel                          = "cancel"
	MsgHandoffFocus                    = "handoff_focus"
	MsgAck                             = "ack"
	MsgError                           = "error"
	MsgTriageSnapshotRequest           = "triage_snapshot_request"
	MsgTriageSnapshotResponse          = "triage_snapshot_response"
	MsgTriageCountsRequest             = "triage_counts_request"
	MsgTriageCountsResponse            = "triage_counts_response"
	MsgTriageDecide                    = "triage_decide"
	MsgTriageDecideResult              = "triage_decide_result"
	MsgHumanActionResolve              = "human_action_resolve"
	MsgHumanActionResolveResult        = "human_action_resolve_result"
	MsgReviewPreviewRequest            = "review_preview_request"
	MsgReviewPreviewResult             = "review_preview_result"
	MsgStatsRequest                    = "stats_request"
	MsgStatsResponse                   = "stats_response"
	MsgActivityRequest                 = "activity_request"
	MsgActivityResponse                = "activity_response"
	MsgPageBulkStatusRequest           = "page_bulk_status_request"
	MsgPageBulkStatusResult            = "page_bulk_status_result"
	MsgPageBulkSubmitRequest           = "page_bulk_submit_request"
	MsgPageBulkSubmitResult            = "page_bulk_submit_result"
	MsgDeliveryReconcileRequest        = "delivery_reconcile_request"
	MsgDeliveryReconcileResult         = "delivery_reconcile_result"
	MsgHandoffLinkRequest              = "handoff_link_request"
	MsgHandoffLinkResult               = "handoff_link_result"
	MsgProviderDirectGetRequest        = "provider_direct_get_request"
	MsgProviderDirectGetResult         = "provider_direct_get_result"
	MsgProviderDriveEpochStartRequest  = "provider_drive_epoch_start_request"
	MsgProviderDriveEpochStartResult   = "provider_drive_epoch_start_result"
	MsgProviderDriveEpochResultRequest = "provider_drive_epoch_result_request"
	MsgProviderDriveEpochResult        = "provider_drive_epoch_result"
	// institutional_materialization_v1 is the dark, strict Phase 1
	// materialization protocol. Its handlers are feature-disabled until a
	// later phase enables durable claims and browser effects.
	MsgInstitutionalCandidateOffer    = "institutional_candidate_offer"
	MsgInstitutionalClaimRequest      = "institutional_claim_request"
	MsgInstitutionalClaimResponse     = "institutional_claim_response"
	MsgInstitutionalBindRequest       = "institutional_bind_request"
	MsgInstitutionalBindResponse      = "institutional_bind_response"
	MsgInstitutionalRouteRequest      = "institutional_route_request"
	MsgInstitutionalRouteResponse     = "institutional_route_response"
	MsgInstitutionalNavigatedRequest  = "institutional_navigated_request"
	MsgInstitutionalNavigatedResponse = "institutional_navigated_response"
	MsgInstitutionalReconcileRequest  = "institutional_reconcile_request"
	MsgInstitutionalReconcileResponse = "institutional_reconcile_response"
	// pdf_grab_v1). MsgPdfGrabResult is sent twice per grab: synchronously in
	// reply to MsgPdfGrabRequest (request_id set, outcome "steering" with
	// grab_id+steering_path, or a refusal outcome), and again later,
	// unsolicited, once the grab sweeper finishes identifying the captured
	// file (request_id empty, outcome one of the terminal identification
	// outcomes). See PdfGrabResultPayload.
	MsgPdfGrabRequest        = "pdf_grab_request"
	MsgPdfGrabResult         = "pdf_grab_result"
	MsgPdfGrabStatusRequest  = "pdf_grab_status_request"
	MsgPdfGrabStatusResult   = "pdf_grab_status_result"
	MsgPdfGrabAbandonRequest = "pdf_grab_abandon_request"
	MsgPdfGrabAbandonResult  = "pdf_grab_abandon_result"
)

const InstitutionalMaterializationFeature = "institutional_materialization_v1"

// Institutional materialization payloads intentionally carry only opaque
// identifiers and bounded ordinals. Job-scoped requests use the envelope's
// job_id; reconcile is session-scoped and has no job_id.
// InstitutionalCandidateOfferPayload is the daemon's URL-free, job-scoped
// offer of one explicit browser-tab materialization candidate.
type InstitutionalCandidateOfferPayload struct {
	CandidateID         string `json:"candidate_id"`
	MaterializationKind string `json:"materialization_kind"`
	ExpiresAt           string `json:"expires_at"`
}

type InstitutionalClaimRequestPayload struct {
	RequestID           string `json:"request_id"`
	CandidateID         string `json:"candidate_id"`
	MaterializationKind string `json:"materialization_kind"`
}
type InstitutionalClaimResponsePayload struct {
	RequestID               string `json:"request_id"`
	Outcome                 string `json:"outcome"`
	CandidateID             string `json:"candidate_id,omitempty"`
	ClaimID                 string `json:"claim_id,omitempty"`
	BindingID               string `json:"binding_id,omitempty"`
	BrowserHolderGeneration *int64 `json:"browser_holder_generation,omitempty"`
	LeaseUntil              string `json:"lease_until,omitempty"`
	Detail                  string `json:"detail,omitempty"`
}
type InstitutionalBindRequestPayload struct {
	RequestID string `json:"request_id"`
	ClaimID   string `json:"claim_id"`
	BindingID string `json:"binding_id"`
	TabID     int64  `json:"tab_id"`
}
type InstitutionalBindResponsePayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	ClaimID   string `json:"claim_id,omitempty"`
	BindingID string `json:"binding_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
type InstitutionalRouteRequestPayload struct {
	RequestID string `json:"request_id"`
	ClaimID   string `json:"claim_id"`
	BindingID string `json:"binding_id"`
}
type InstitutionalRouteResponsePayload struct {
	RequestID            string `json:"request_id"`
	Outcome              string `json:"outcome"`
	ClaimID              string `json:"claim_id,omitempty"`
	BindingID            string `json:"binding_id,omitempty"`
	RouteIssuanceOrdinal int64  `json:"route_issuance_ordinal,omitempty"`
	URL                  string `json:"url,omitempty"`
	Detail               string `json:"detail,omitempty"`
}
type InstitutionalNavigatedRequestPayload struct {
	RequestID            string `json:"request_id"`
	ClaimID              string `json:"claim_id"`
	BindingID            string `json:"binding_id"`
	RouteIssuanceOrdinal int64  `json:"route_issuance_ordinal"`
	TabID                int64  `json:"tab_id"`
}
type InstitutionalNavigatedResponsePayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	ClaimID   string `json:"claim_id,omitempty"`
	BindingID string `json:"binding_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
type InstitutionalReconcileBinding struct {
	BindingID string `json:"binding_id"`
	TabID     int64  `json:"tab_id"`
}
type InstitutionalReconcileRequestPayload struct {
	RequestID string                          `json:"request_id"`
	Bindings  []InstitutionalReconcileBinding `json:"bindings"`
}
type InstitutionalReconcileClaim struct {
	ClaimID     string `json:"claim_id"`
	BindingID   string `json:"binding_id"`
	CandidateID string `json:"candidate_id"`
	Phase       string `json:"phase"`
	TabID       *int64 `json:"tab_id,omitempty"`
}
type InstitutionalReconcileResponsePayload struct {
	RequestID string                        `json:"request_id"`
	Outcome   string                        `json:"outcome"`
	Claims    []InstitutionalReconcileClaim `json:"claims,omitempty"`
	Detail    string                        `json:"detail,omitempty"`
}

// jobScoped lists the types that must carry a job_id.
var jobScoped = map[string]bool{
	MsgDownloadStarted: true, MsgDownloadComplete: true, MsgDeliveryContext: true,
	MsgProviderOutcome: true, MsgProviderDirectGetRequest: true, MsgProviderDirectGetResult: true,
	MsgProviderDriveEpochStartRequest: true, MsgProviderDriveEpochStartResult: true,
	MsgProviderDriveEpochResultRequest: true, MsgProviderDriveEpochResult: true,
	MsgCancel: true, MsgHandoffFocus: true,
	MsgInstitutionalCandidateOffer: true,
	MsgInstitutionalClaimRequest:   true, MsgInstitutionalClaimResponse: true,
	MsgInstitutionalBindRequest: true, MsgInstitutionalBindResponse: true,
	MsgInstitutionalRouteRequest: true, MsgInstitutionalRouteResponse: true,
	MsgInstitutionalNavigatedRequest: true, MsgInstitutionalNavigatedResponse: true,
}

// HelloPayload announces the extension, its adapter versions, and the
// capabilities it explicitly negotiates with the daemon. Features are
// optional so old extensions retain the legacy URL-bearing offer path.
type HelloPayload struct {
	ExtensionVersion string            `json:"extension_version"`
	AdapterVersions  map[string]string `json:"adapter_versions,omitempty"`
	Features         []string          `json:"features,omitempty"`
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

// PageAcquirePayload asks the daemon to queue the paper identified on the
// user's current page. Source is advisory provenance only. DOI stays optional
// on the wire for forward evolution, although the current daemon requires it
// before it will submit an acquisition.
type PageAcquirePayload struct {
	URL    string `json:"url"`
	DOI    string `json:"doi,omitempty"`
	Title  string `json:"title,omitempty"`
	Source string `json:"source,omitempty"`
}

// PageAcquireAckPayload reports the durable queue result without exposing
// internal state to the browser.
type PageAcquireAckPayload struct {
	JobID     string `json:"job_id,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Error     string `json:"error,omitempty"`
}

// PageCapturePayload confines diagnostic HTML to a single bounded frame so a
// capture cannot turn native messaging into an unbounded transfer channel.
//
// RequestID echoes the page_capture_request that asked for this content, and
// is the ONLY thing that ties the two together: an unsolicited capture (the
// developer panel's captureFixture) answers no request and omits it. Before
// it existed the daemon correlated on provider+scenario alone, so an
// unsolicited capture could satisfy a concurrent CLI `papio adapter capture`
// waiting on the same session for the same pair and hand its caller the wrong
// file path (papio-85a7420f4cd2564f).
type PageCapturePayload struct {
	Host           string `json:"host"`
	Scenario       string `json:"scenario"`
	AdapterID      string `json:"adapter_id,omitempty"`
	AdapterVersion string `json:"adapter_version,omitempty"`
	Encoding       string `json:"encoding"`
	Bytes          int64  `json:"bytes"`
	Body           string `json:"body"`
	RequestID      string `json:"request_id,omitempty"`
}

// PageCaptureRequestPayload directs the extension to capture one https page
// through its ordinary browser session. SettleMS is optional on the wire; zero
// means the extension's bounded default.
type PageCaptureRequestPayload struct {
	RequestID string `json:"request_id"`
	URL       string `json:"url"`
	Provider  string `json:"provider"`
	Scenario  string `json:"scenario"`
	SettleMS  *int64 `json:"settle_ms,omitempty"`
}

// PageCaptureRequestResultPayload reports the routine outcome of a requested
// capture. The sanitized content itself remains the existing page_capture frame.
type PageCaptureRequestResultPayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
}

// HandoffLinkRequestPayload asks the daemon to mint the current handoff URL
// for one parked job. RequestID optionally correlates the response.
type HandoffLinkRequestPayload struct {
	RequestID string `json:"request_id,omitempty"`
	JobID     string `json:"job_id"`
}

// HandoffLinkResultPayload reports a fresh handoff URL or a closed routine
// refusal. Failures never carry a raw daemon error.
type HandoffLinkResultPayload struct {
	RequestID string `json:"request_id,omitempty"`
	Outcome   string `json:"outcome"`
	URL       string `json:"url,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// PdfGrabRequestPayload asks the daemon to allocate a capture slot for a
// browser PDF tab (ADR-0020). The extension keeps the full tab URL local;
// the daemon receives only its bare hostname and title.
type PdfGrabRequestPayload struct {
	RequestID string `json:"request_id"`
	Host      string `json:"host"`
	Title     string `json:"title,omitempty"`
}

// PdfGrabStatusRequestPayload asks for the durable state of one grab.
type PdfGrabStatusRequestPayload struct {
	RequestID string `json:"request_id"`
	GrabID    string `json:"grab_id"`
}

// PdfGrabStatusResultPayload reports durable grab state. Unknown grabs are a
// routine not_found outcome, not a session-fatal RPC error.
type PdfGrabStatusResultPayload struct {
	RequestID string `json:"request_id"`
	GrabID    string `json:"grab_id"`
	State     string `json:"state"`
	Outcome   string `json:"outcome,omitempty"`
	Detail    string `json:"detail,omitempty"`
	JobID     string `json:"job_id,omitempty"`
}

// PdfGrabAbandonRequestPayload asks the daemon to settle an unfulfilled grab
// after the browser reports its download interrupted.
type PdfGrabAbandonRequestPayload struct {
	RequestID string `json:"request_id"`
	GrabID    string `json:"grab_id"`
}

// PdfGrabAbandonResultPayload reports the durable abandoned state.
type PdfGrabAbandonResultPayload struct {
	RequestID string `json:"request_id"`
	GrabID    string `json:"grab_id"`
	State     string `json:"state"`
	Outcome   string `json:"outcome,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// PdfGrabResultPayload reports one PDF grab's outcome. GrabID is the durable
// correlator across both frames this message type carries per grab, when
// one was allocated:
//   - the synchronous reply to pdf_grab_request (RequestID set; Outcome
//     "steering" with GrabID+SteeringPath, or a refusal outcome —
//     "not_supported" or "unavailable" — with Detail). A refusal decided
//     before allocation (e.g. an unhealthy adoption latch) carries no
//     GrabID at all — there is no grab to name yet — while one decided
//     after allocation (e.g. the capture directory could not be created)
//     may carry the GrabID it never got to use.
//   - a later, unsolicited push once the grab sweeper (internal/browser
//     SweepGrabs) finishes identifying the captured file (RequestID empty;
//     GrabID always set; Outcome one of "job_created", "already_owned",
//     "needs_identifier", "failed_validation").
//
// A workspace tab that closed before the second frame arrives simply never
// sees it — the grab's eventual disposition survives durably in the grabs
// table and its job (if any) regardless of whether the push was delivered.
//
// Never a raw error: every failure mode is a closed enum value plus a
// bounded human-readable Detail. An unhandled outcome or a raw Go error
// string here would still decode on the extension side, but stray text in a
// field this codebase treats as a closed vocabulary is exactly the
// session-fatal footgun page_capture_request_result and its siblings already
// avoid — the outcome enum is what a UI can safely switch on.
type PdfGrabResultPayload struct {
	RequestID    string `json:"request_id,omitempty"`
	GrabID       string `json:"grab_id,omitempty"`
	Outcome      string `json:"outcome"`
	SteeringPath string `json:"steering_path,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// JobOfferPayload asks the extension to open one OpenURL-resolved job.
type JobOfferPayload struct {
	OpenURL           string            `json:"openurl"`
	ProviderHosts     []string          `json:"provider_hosts"`
	Expected          *JobOfferExpected `json:"expected,omitempty"`
	AccessMode        string            `json:"access_mode,omitempty"`
	LoginEntityID     string            `json:"login_entity_id,omitempty"`
	ProquestAccountID string            `json:"proquest_account_id,omitempty"`
	RequiresAuth      bool              `json:"requires_auth,omitempty"`
	DriveAttemptID    string            `json:"drive_attempt_id,omitempty"`
	DriveOrdinal      *int64            `json:"drive_ordinal,omitempty"`
	DriveStrategy     string            `json:"drive_strategy,omitempty"`
	DriveRevision     string            `json:"drive_revision,omitempty"`
	ExpiresAt         string            `json:"expires_at"`
}

// JobOfferExpected carries wrong-work guard hints.
type JobOfferExpected struct {
	DOI   string `json:"doi,omitempty"`
	Title string `json:"title,omitempty"`
}

// HandoffOutcomePayload reports that a handoff tab terminated on an
// identity-provider failure page. FinalHost is a bare hostname; no path,
// query, or page content ever crosses the bridge.
type HandoffOutcomePayload struct {
	Outcome   string `json:"outcome"`
	FinalHost string `json:"final_host"`
}

// AuthPayload deliberately carries only timing. No URL, host, title, query, or
// fragment fields exist so identity-provider addresses cannot cross the bridge.
type AuthPayload struct {
	ElapsedMS *int64 `json:"elapsed_ms,omitempty"`
}

// SessionEvidencePayload reports timing-only evidence that the institutional
// resolver session is available. origin_hint is a bare resolver origin and
// never carries an IdP path or query.
type SessionEvidencePayload struct {
	Evidence   string `json:"evidence"`
	OriginHint string `json:"origin_hint,omitempty"`
	At         string `json:"at"`
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

// DeliveryContextPayload records the observed route and browser-session
// evidence for one completed download. PageHost is optional and hostname-only.
type DeliveryContextPayload struct {
	DownloadID      int64  `json:"download_id"`
	Route           string `json:"route"`
	PageHost        string `json:"page_host,omitempty"`
	SessionEvidence string `json:"session_evidence"`
}

// ProviderOutcomePayload is the adapter's terminal observation for a job.
type ProviderOutcomePayload struct {
	Outcome        string `json:"outcome"`
	AdapterID      string `json:"adapter_id,omitempty"`
	AdapterVersion string `json:"adapter_version,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// ProviderDirectGetRequestPayload asks a feature-capable extension to fetch one
// daemon-selected public provider route. The URL is constrained to the declared
// origin/path envelope; it carries no credentials or opaque query parameters.
type ProviderDirectGetRequestPayload struct {
	DriveAttemptID     string `json:"drive_attempt_id"`
	Ordinal            int64  `json:"ordinal"`
	RouteRevision      string `json:"route_revision"`
	ExpectedIdentifier string `json:"expected_identifier"`
	URL                string `json:"url"`
	AllowedOrigin      string `json:"allowed_origin"`
	PathFamily         string `json:"path_family"`
	TermsPolicy        string `json:"terms_policy"`
}

// ProviderDirectGetResultPayload is the extension's classified terminal
// observation. FinalHost/FinalPath are sanitized landing metadata, never a URL
// with query, fragment, userinfo, or secret-bearing components.
type ProviderDirectGetResultPayload struct {
	DriveAttemptID string `json:"drive_attempt_id"`
	Ordinal        int64  `json:"ordinal"`
	RouteRevision  string `json:"route_revision"`
	Outcome        string `json:"outcome"`
	FinalHost      string `json:"final_host,omitempty"`
	FinalPath      string `json:"final_path,omitempty"`
	LandingClass   string `json:"landing_class"`
	Detail         string `json:"detail,omitempty"`
}
type ProviderDriveEpochStartRequestPayload struct {
	RequestID      string `json:"request_id,omitempty"`
	DriveAttemptID string `json:"drive_attempt_id"`
	Ordinal        int64  `json:"ordinal"`
	Strategy       string `json:"strategy"`
	Revision       string `json:"revision"`
}

type ProviderDriveEpochStartResultPayload struct {
	RequestID      string `json:"request_id,omitempty"`
	DriveAttemptID string `json:"drive_attempt_id"`
	Ordinal        int64  `json:"ordinal"`
	Strategy       string `json:"strategy"`
	Revision       string `json:"revision"`
	Outcome        string `json:"outcome"`
	Detail         string `json:"detail,omitempty"`
}

type ProviderDriveEpochResultRequestPayload struct {
	RequestID      string `json:"request_id,omitempty"`
	DriveAttemptID string `json:"drive_attempt_id"`
	Ordinal        int64  `json:"ordinal"`
	Strategy       string `json:"strategy"`
	Revision       string `json:"revision"`
	Outcome        string `json:"outcome"`
	Detail         string `json:"detail,omitempty"`
}

type ProviderDriveEpochResultPayload struct {
	RequestID      string `json:"request_id,omitempty"`
	DriveAttemptID string `json:"drive_attempt_id"`
	Ordinal        int64  `json:"ordinal"`
	Strategy       string `json:"strategy"`
	Revision       string `json:"revision"`
	Outcome        string `json:"outcome"`
	Detail         string `json:"detail,omitempty"`
}

// ErrorPayload is a normalized bridge error. RequestID is optional so an
// application failure can settle the request that produced it without
// changing the behavior of unsolicited protocol errors.
type ErrorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// EmptyPayload is used by types that carry no data (ack, job_accept,
// job_reject, cancel, handoff_focus).
type EmptyPayload struct{}

// TriageSnapshotRequestPayload requests one immutable inbox page. Schema
// versions are negotiated explicitly because a future snapshot schema cannot
// safely add fields to this locked browser message family.
type TriageSnapshotRequestPayload struct {
	RequestID      string  `json:"request_id"`
	SchemaVersions []int64 `json:"schema_versions"`
	Limit          int64   `json:"limit,omitempty"`
	Cursor         string  `json:"cursor,omitempty"`
}

// TriageCounts contains complete, unpaginated inbox counts. The optional
// ActionsRequiresAuth field is populated only for the negotiated counts
// response schema v2; snapshots and legacy counts responses omit it.
type TriageCounts struct {
	PendingTotal        int64  `json:"pending_total"`
	WatchHits           int64  `json:"watch_hits"`
	Actions             int64  `json:"actions"`
	ActionsRequiresAuth *int64 `json:"actions_requires_auth,omitempty"`
	Retractions         int64  `json:"retractions"`
	JobsWorking         int64  `json:"jobs_working"`
	JobsNeedsReview     int64  `json:"jobs_needs_review"`
	FailureGroups7d     int64  `json:"failure_groups_7d"`
}

// TriageFact is bounded display text attached to an inbox item.
type TriageFact struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// TriageLink is a daemon-derived destination for an inbox item.
type TriageLink struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

// TriageWork is the immutable work identity attached to a watch hit.
type TriageWork struct {
	DOI     string `json:"doi"`
	Title   string `json:"title"`
	Authors string `json:"authors"`
	Year    int64  `json:"year"`
	IsOA    bool   `json:"is_oa"`
}

// TriageWatch identifies a watch contributing a grouped watch hit. Work keys
// are deliberately absent: they are daemon-only mutation inputs.
type TriageWatch struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// TriageSnapshotItem is one inbox item. Its kind-specific fields are flat on
// the wire to keep each snapshot schema stable.
type TriageSnapshotItem struct {
	Kind  string       `json:"kind"`
	ID    string       `json:"id"`
	Rank  int64        `json:"rank"`
	Title string       `json:"title"`
	Facts []TriageFact `json:"facts"`
	Links []TriageLink `json:"links"`
	Ops   []string     `json:"ops"`

	// Attention is triage-snapshot/3's closed presentation-priority signal:
	// required on every schema-3 item, forbidden below schema 3. "working"
	// means papio is proceeding on its own (nothing for the operator to do
	// yet); "required" means the item needs a human decision; "advisory" is
	// informational only (e.g. a retraction notice). It replaces any
	// presentation inference from action_kind or requires_auth (r5).
	Attention string `json:"attention,omitempty"`

	Work        *TriageWork   `json:"work,omitempty"`
	Abstract    string        `json:"abstract,omitempty"`
	Watches     []TriageWatch `json:"watches,omitempty"`
	FirstSeenAt string        `json:"first_seen_at,omitempty"`

	ActionID     int64  `json:"action_id,omitempty"`
	JobID        string `json:"job_id,omitempty"`
	ActionKind   string `json:"action_kind,omitempty"`
	JobState     string `json:"job_state,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	RequiresAuth *bool  `json:"requires_auth,omitempty"`
	BlockedBy    string `json:"blocked_by,omitempty"`
	// RouteClass is triage-snapshot/3's closed routing classifier for a
	// human_action item: required on schema 3, forbidden below. It
	// formalizes the existing action-kind vocabulary (plus document_delivery)
	// into a fixed enum decoupled from action_kind's open one, so a v3
	// extension can safely branch on it even after a future daemon ships an
	// action_kind it has never seen (ADR-0016 Decision 4).
	RouteClass string `json:"route_class,omitempty"`
	// AuthRequirement is ADR-0016 Decision 4's tri-state auth carrier, wired
	// as a string enum ("true"/"false"/"unknown") rather than a bare bool so
	// "unknown" is representable: required on schema 3 human_action items,
	// forbidden below. The existing RequiresAuth boolean is UNCHANGED and
	// stays exactly the narrow execution gate ADR-0016 pins it to; only
	// AuthRequirement may drive presentation copy.
	AuthRequirement string `json:"auth_requirement,omitempty"`
	// Delivery is present only on a document_delivery human_action item
	// (forbidden on every other action kind and below schema 3). "fulfilled"
	// means the provider supplied the document — never that papio holds
	// trusted bytes yet (ADR-0017).
	Delivery *TriageDelivery `json:"delivery,omitempty"`

	DOI       string `json:"doi,omitempty"`
	Nature    string `json:"nature,omitempty"`
	NoticedAt string `json:"noticed_at,omitempty"`
	NoticeDOI string `json:"notice_doi,omitempty"`

	// Label and Grab are the exact triage-snapshot/4 pdf_grab item shape.
	// They are intentionally separate from the generic title/id/job fields:
	// grabs remain jobless until an identifier is supplied.
	Label string      `json:"label,omitempty"`
	Grab  *TriageGrab `json:"grab,omitempty"`
}

type TriageGrab struct {
	GrabID string `json:"grab_id"`
	State  string `json:"state"`
}

// TriageDelivery is triage-snapshot/3's document_delivery sub-object: the
// provider, the provider's own reference for the request (empty until one
// is assigned), and its delivery state (internal/delivery's existing
// vocabulary; ADR-0017). It never carries delivered bytes or a URL to them.
type TriageDelivery struct {
	Provider          string `json:"provider"`
	ProviderReference string `json:"provider_reference,omitempty"`
	State             string `json:"state"`
}

func (d TriageDelivery) validate() error {
	if err := validateTriageText("human_action.delivery.provider", d.Provider, 100); err != nil || d.Provider == "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("human_action.delivery.provider is required")
	}
	if err := validateTriageText("human_action.delivery.provider_reference", d.ProviderReference, 300); err != nil {
		return err
	}
	return enumRequired("human_action.delivery.state", d.State,
		"offered", "submitted", "pending", "fulfilled", "declined", "cancelled", "unknown_outcome")
}

// triageRouteClasses is triage-snapshot/3's closed route_class vocabulary —
// see TriageSnapshotItem.RouteClass's doc comment for why it is closed
// independently of action_kind.
var triageRouteClasses = []string{
	"openurl_handoff", "manual_download", "verify_identity", "openurl_available",
	"human_auth_required", "terms_acceptance_required", "document_delivery",
	"downloads_access_required",
}

// TriageRouteClasses returns schema 3's closed route_class vocabulary. The
// bridge consults it to keep snapshots legal: an action kind outside this
// list cannot be represented on a v3 frame and must be omitted, never
// emitted invalid (the vocabulary grows only with a schema revision).
// TriageRouteClassesV4 returns schema 4's vocabulary. The v3 function above
// remains frozen so its omit guard continues to exclude pdf_identifier_needed.
func TriageRouteClassesV4() []string {
	return append(slices.Clone(triageRouteClasses), "pdf_identifier_needed")
}
func TriageRouteClasses() []string {
	return slices.Clone(triageRouteClasses)
}

// triageBlockedByV2 is schema 2's exact closed set, shipped and locked
// (ADR-0017's correction of the stale "not yet shipped" claim): a schema-2
// frame must never carry a value outside it. triageBlockedByV3 is schema
// 3's strict superset — the new values only ever appear on schema 3, never
// overloading or reinterpreting a v2 value's meaning.
var triageBlockedByV2 = []string{"anti_bot", "paywall", "landing_page"}
var triageBlockedByV3 = append(append([]string(nil), triageBlockedByV2...),
	"login", "terms", "delivery_outcome", "identity_review", "unknown")
var triageBlockedByV4 = append(append([]string(nil), triageBlockedByV3...), "identifier_missing")

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// TriageSnapshotResponsePayload is a correlated immutable snapshot.
type TriageSnapshotResponsePayload struct {
	RequestID             string               `json:"request_id"`
	Schema                int64                `json:"schema"`
	GeneratedAt           string               `json:"generated_at"`
	Counts                TriageCounts         `json:"counts"`
	Items                 []TriageSnapshotItem `json:"items"`
	Cursor                string               `json:"cursor,omitempty"`
	HasMore               bool                 `json:"has_more"`
	UnsupportedItemsCount int64                `json:"unsupported_items_count"`
}

// TriageCountsRequestPayload asks for complete counts without a snapshot page.
// Legacy extensions omit SchemaVersions and receive the locked v1 shape.
type TriageCountsRequestPayload struct {
	RequestID      string  `json:"request_id"`
	SchemaVersions []int64 `json:"schema_versions,omitempty"`
}

// TriageCountsResponsePayload is the correlated complete-count response.
type TriageCountsResponsePayload struct {
	RequestID string       `json:"request_id"`
	Counts    TriageCounts `json:"counts"`
}

// TriageDecidePayload consumes one current watch-hit item.
type TriageDecidePayload struct {
	RequestID  string          `json:"request_id"`
	ItemID     string          `json:"item_id"`
	Op         string          `json:"op"`
	WatchScope json.RawMessage `json:"watch_scope,omitempty"`
}

// TriageDecideResultPayload reports a non-replayable mutation result.
type TriageDecideResultPayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
}

// HumanActionResolvePayload binds a verdict to a rendered action revision and,
// for accept, the quarantined bytes' immutable digest.
type HumanActionResolvePayload struct {
	RequestID        string `json:"request_id"`
	ActionID         int64  `json:"action_id"`
	Verdict          string `json:"verdict"`
	ExpectedRevision int64  `json:"expected_revision"`
	ExpectedSHA256   string `json:"expected_sha256,omitempty"`
}

// HumanActionResolveResultPayload has the same contract as a triage decision.
type HumanActionResolveResultPayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
}

// DeliveryReconcilePayload asks the daemon to perform one of
// triage-snapshot/3's document_delivery reconciliation mutations
// (ADR-0017 Decision 4) against a job's open document_delivery human
// action. It is deliberately a new message rather than a widened
// human_action_resolve: that payload's verdict vocabulary is closed to
// accept/reject/dismiss against a CAS candidate binding, and overloading
// it with delivery semantics was rejected for the same reason on the IPC
// side (internal/api/delivery.go's deliveryAction). open_request_history
// is deliberately absent here — it never mutates anything, so the
// extension renders it from the item's own delivery sub-object instead of
// a round trip.
type DeliveryReconcilePayload struct {
	RequestID         string `json:"request_id"`
	JobID             string `json:"job_id"`
	Operation         string `json:"operation"`
	ProviderReference string `json:"provider_reference,omitempty"`
}

// DeliveryReconcileResultPayload has the same contract as a triage decision.
type DeliveryReconcileResultPayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
}

// ReviewPreviewRequestPayload asks for a short-lived loopback capability for
// the bound review action.
type ReviewPreviewRequestPayload struct {
	RequestID string `json:"request_id"`
	ActionID  int64  `json:"action_id"`
}

// ReviewPreviewResultPayload deliberately exposes only a capability URL and
// immutable file metadata; a quarantine path must never cross the bridge.
type ReviewPreviewResultPayload struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
	URL       string `json:"url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// StatsRequestPayload asks for the daemon's lifetime acquisition statistics.
type StatsRequestPayload struct {
	RequestID string `json:"request_id"`
}

// StatsAccess breaks acquired works down by the access basis of the accepted
// candidate. Other captures manual and unclassified sources so the buckets and
// AcquiredTotal need not agree on stale rows predating candidate recording.
type StatsAccess struct {
	OpenAccess    int64 `json:"open_access"`
	Institutional int64 `json:"institutional"`
	LicensedAPI   int64 `json:"licensed_api"`
	Other         int64 `json:"other"`
}

// StatsBucket is one time-series bucket: works acquired in the week beginning
// PeriodStart (RFC3339, midnight UTC).
type StatsBucket struct {
	PeriodStart string `json:"period_start"`
	Acquired    int64  `json:"acquired"`
}

// StatsResponsePayload reports lifetime acquisition value metrics plus a bounded
// weekly time series. All counts are non-negative; the extension derives
// success rate, handoff rate, and estimated time saved from these facts.
type StatsResponsePayload struct {
	RequestID        string        `json:"request_id"`
	GeneratedAt      string        `json:"generated_at"`
	AcquiredTotal    int64         `json:"acquired_total"`
	FailedTotal      int64         `json:"failed_total"`
	HandoffsRequired int64         `json:"handoffs_required"`
	Access           StatsAccess   `json:"access"`
	Series           []StatsBucket `json:"series"`
}

// ActivityRequestPayload asks for a bounded page of recent daemon events.
// Limit is optional on the wire; decoders apply the default of 20.
type ActivityRequestPayload struct {
	RequestID string `json:"request_id"`
	Limit     int64  `json:"limit,omitempty"`
}

// ActivityEntryPayload is the display-only event shape sent to the browser.
// Detail remains daemon-side so the bridge never exposes arbitrary event data.
type ActivityEntryPayload struct {
	Seq   int64  `json:"seq"`
	At    string `json:"at"`
	JobID string `json:"job_id,omitempty"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Title string `json:"title,omitempty"`
}

// UnmarshalJSON keeps optional job_id fail-closed when it is present but
// empty; a missing job_id is the only valid omission.
func (entry *ActivityEntryPayload) UnmarshalJSON(data []byte) error {
	fields, err := browserObjectFields(data, "activity_response.entry")
	if err != nil {
		return err
	}
	if err := browserRejectNullFields(fields, "job_id", "title"); err != nil {
		return err
	}
	type plain ActivityEntryPayload
	var decoded plain
	if err := strictDecode(data, &decoded); err != nil {
		return err
	}
	if _, present := fields["job_id"]; present && decoded.JobID == "" {
		return fmt.Errorf("activity_response.entry.job_id must be non-empty")
	}
	*entry = ActivityEntryPayload(decoded)
	return nil
}

// ActivityResponsePayload is a correlated bounded page of recent events.
type ActivityResponsePayload struct {
	RequestID   string                 `json:"request_id"`
	GeneratedAt string                 `json:"generated_at"`
	Entries     []ActivityEntryPayload `json:"entries"`
}

// ---------------------------------------------------------------------------
// Page-bulk acquisition (page_bulk_acquire_v1, ADR-0019 Decision 7)
// ---------------------------------------------------------------------------

// PageBulkIdentifier is one page-detected scholarly identifier keyed by a
// browser-local correlation id. LocalID never leaves the extension's own
// scan snapshot; the daemon echoes it back on PageBulkStatusItem so the
// selection workspace can re-associate a resolved status with the row it
// scanned (ADR-0019 Decision 4).
type PageBulkIdentifier struct {
	LocalID string `json:"local_id"`
	Kind    string `json:"kind"`
	Value   string `json:"value"`
}

// PageBulkStatusRequestPayload asks for the ownership/job status of up to
// 200 identifiers detected on one scanned page, correlated by ScanID with
// the extension's local scan snapshot. RenderedRecordCountHint is an
// optional, honest structural denominator: a content-script detector that
// recognizes the page's result-list shape (definition-list rows, repeated
// card containers, reference-list items) counts the visible records
// without reading their contents; absent (nil) means no recognized shape,
// never a guess (dev/post-build-followups.md item 3).
type PageBulkStatusRequestPayload struct {
	RequestID               string               `json:"request_id"`
	ScanID                  string               `json:"scan_id"`
	Identifiers             []PageBulkIdentifier `json:"identifiers"`
	RenderedRecordCountHint *int64               `json:"rendered_record_count_hint,omitempty"`
}

// PageBulkStatusItem reports one identifier's resolved holdings/job status
// from the daemon's existing ownership/job lookup (ADR-0019 Decision 5).
// CanonicalKey is omitted when Status is "invalid" — an identifier that
// never resolved has no canonical work identity to report. JobID is
// populated only when Status is "queued". ZotioItemKey is populated only
// when Status is "owned_missing_pdf" and the match came from a zotio
// library lookup, naming the existing Zotero parent item so the extension
// can offer a direct handoff. Status "ownership_unknown" means zotio is
// configured but its answer could not be trusted this round (unavailable,
// a stale mirror, or a sync failure) — deliberately distinct from
// "ownership_incomplete" (no ownership source configured/queried) and
// never collapsed into a plain not-owned/"eligible" claim (ADR-0008).
type PageBulkStatusItem struct {
	LocalID           string `json:"local_id"`
	CanonicalKey      string `json:"canonical_key,omitempty"`
	Status            string `json:"status"`
	OwnershipComplete bool   `json:"ownership_complete"`
	JobID             string `json:"job_id,omitempty"`
	ZotioItemKey      string `json:"zotio_item_key,omitempty"`
}

// PageBulkStatusResultPayload is the correlated response to a status
// request. Truncated is required and explicit, never silent (ADR-0019
// Decision 3's raw-candidate cap applies one layer up, in the content
// script; this field is the daemon's own truncation report for Items).
type PageBulkStatusResultPayload struct {
	RequestID string               `json:"request_id"`
	ScanID    string               `json:"scan_id"`
	Items     []PageBulkStatusItem `json:"items"`
	Truncated bool                 `json:"truncated"`
}

// PageBulkSubmitSource records per-source provenance on the created batch
// manifest, distinct from the daemon-assigned consumer (ADR-0019 Decision
// 6). Origin is the bare scheme+host only — never path, query, fragment, or
// page title.
type PageBulkSubmitSource struct {
	Kind     string `json:"kind"`
	Origin   string `json:"origin"`
	Detector string `json:"detector"`
}

// PageBulkSubmitRequestPayload asks the daemon to create one ordinary batch
// from up to 50 canonical keys already resolved by a prior status request.
// The daemon assigns consumer ("browser-page"); the extension never
// supplies it (ADR-0019 Decision 6).
type PageBulkSubmitRequestPayload struct {
	RequestID     string               `json:"request_id"`
	ScanID        string               `json:"scan_id"`
	CanonicalKeys []string             `json:"canonical_keys"`
	Source        PageBulkSubmitSource `json:"source"`
}

// PageBulkSubmitResultPayload reports the created batch's disposition
// counts: Submitted (a new job was created), Joined (the key matched an
// existing job), AlreadyOwned, and Invalid (the key no longer resolved by
// submit time). Every submitted canonical key falls into exactly one bucket.
type PageBulkSubmitResultPayload struct {
	RequestID    string `json:"request_id"`
	ScanID       string `json:"scan_id"`
	Submitted    int64  `json:"submitted"`
	Joined       int64  `json:"joined"`
	AlreadyOwned int64  `json:"already_owned"`
	Invalid      int64  `json:"invalid"`
	BatchID      string `json:"batch_id"`
}

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
	if (env.Type == MsgInstitutionalReconcileRequest || env.Type == MsgInstitutionalReconcileResponse) && env.JobID != "" {
		return nil, fmt.Errorf("browser message: type %q must not carry job_id", env.Type)
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
			err = browserRejectNullFields(payloadFields, "adapter_versions", "features")
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
			} else if len(p.Features) > 32 {
				err = fmt.Errorf("hello.features capped at 32")
			}
		}
		if err == nil {
			seen := make(map[string]struct{}, len(p.Features))
			for _, feature := range p.Features {
				if browserTextLen(feature) < 1 || browserTextLen(feature) > 64 || !clientFeatureRE.MatchString(feature) {
					err = fmt.Errorf("hello.features entries must match [a-z0-9_]{1,64}")
					break
				}
				if _, exists := seen[feature]; exists {
					err = fmt.Errorf("hello.features contains duplicate %q", feature)
					break
				}
				seen[feature] = struct{}{}
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
	case MsgPageAcquire:
		p := &PageAcquirePayload{}
		if err = browserRequireFields(payloadFields, "url"); err == nil {
			err = browserRejectNullFields(payloadFields, "doi", "title", "source")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageAcquireAck:
		p := &PageAcquireAckPayload{}
		err = browserRejectNullFields(payloadFields, "job_id", "duplicate", "error")
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if _, ok := payloadFields["job_id"]; ok && p.JobID == "" {
				err = fmt.Errorf("page_acquire_ack.job_id must be non-empty")
			} else if _, ok := payloadFields["error"]; ok && p.Error == "" {
				err = fmt.Errorf("page_acquire_ack.error must be non-empty")
			}
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageCapture:
		p := &PageCapturePayload{}
		if err = browserRequireFields(payloadFields, "host", "scenario", "encoding", "bytes", "body"); err == nil {
			err = browserRejectNullFields(payloadFields, "adapter_id", "adapter_version", "request_id")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			// A present-but-empty string is not the same wire shape as an
			// absent field, and only the absent form means "unsolicited". Both
			// the TS parser and the schema reject "" (their charset bound has
			// a minimum length), so accepting it here would let the two
			// implementations disagree on the same frame.
			for _, field := range [...]struct {
				name  string
				value string
			}{{"adapter_id", p.AdapterID}, {"request_id", p.RequestID}} {
				if _, ok := payloadFields[field.name]; ok && field.value == "" {
					err = fmt.Errorf("page_capture.%s must not be empty when present", field.name)
					break
				}
			}
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageCaptureRequest:
		p := &PageCaptureRequestPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "url", "provider", "scenario"); err == nil {
			err = browserRejectNullFields(payloadFields, "settle_ms")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageCaptureRequestResult:
		p := &PageCaptureRequestResultPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "outcome"); err == nil {
			err = browserRejectNullFields(payloadFields, "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgJobOffer:
		p := &JobOfferPayload{}
		if err = browserRequireFields(payloadFields, "openurl", "provider_hosts", "expires_at"); err == nil {
			err = browserRejectNullFields(payloadFields, "expected", "access_mode", "login_entity_id", "proquest_account_id", "requires_auth", "drive_attempt_id", "drive_ordinal", "drive_strategy", "drive_revision")
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
	case MsgHandoffOutcome:
		p := &HandoffOutcomePayload{}
		if err = browserRequireFields(payloadFields, "outcome", "final_host"); err == nil {
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
	case MsgSessionEvidence:
		p := &SessionEvidencePayload{}
		if err = browserRequireFields(payloadFields, "evidence", "at"); err == nil {
			err = browserRejectNullFields(payloadFields, "origin_hint")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
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
	case MsgDeliveryContext:
		p := &DeliveryContextPayload{}
		if err = browserRequireFields(payloadFields, "download_id", "route", "session_evidence"); err == nil {
			err = browserRejectNullFields(payloadFields, "page_host")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
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
			err = browserRejectNullFields(payloadFields, "adapter_id", "adapter_version", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if _, present := payloadFields["adapter_id"]; present && p.AdapterID == "" {
				err = fmt.Errorf("provider_outcome.adapter_id must not be empty when present")
			}
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgProviderDirectGetRequest:
		p := &ProviderDirectGetRequestPayload{}
		if err = browserRequireFields(payloadFields, "drive_attempt_id", "ordinal", "route_revision", "expected_identifier", "url", "allowed_origin", "path_family", "terms_policy"); err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgProviderDirectGetResult:
		p := &ProviderDirectGetResultPayload{}
		if err = browserRequireFields(payloadFields, "drive_attempt_id", "ordinal", "route_revision", "outcome", "landing_class"); err == nil {
			err = browserRejectNullFields(payloadFields, "final_host", "final_path", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgProviderDriveEpochStartRequest:
		p := &ProviderDriveEpochStartRequestPayload{}
		if err = browserRequireFields(payloadFields, "drive_attempt_id", "ordinal", "strategy", "revision"); err == nil {
			err = browserRejectNullFields(payloadFields, "request_id")
			if err == nil {
				err = strictDecode(env.Payload, p)
			}
		}
		if err == nil {
			err = validateDriveEpochTuple(p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision, "provider_drive_epoch_start_request")
		}
		msg.Payload = p
	case MsgProviderDriveEpochStartResult:
		p := &ProviderDriveEpochStartResultPayload{}
		if err = browserRequireFields(payloadFields, "drive_attempt_id", "ordinal", "strategy", "revision", "outcome"); err == nil {
			err = browserRejectNullFields(payloadFields, "request_id", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = validateDriveEpochTuple(p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision, "provider_drive_epoch_start_result")
		}
		if err == nil {
			err = enumRequired("provider_drive_epoch_start_result.outcome", p.Outcome, "started", "stale", "unsupported", "error")
		}
		msg.Payload = p
	case MsgProviderDriveEpochResultRequest:
		p := &ProviderDriveEpochResultRequestPayload{}
		if err = browserRequireFields(payloadFields, "drive_attempt_id", "ordinal", "strategy", "revision", "outcome"); err == nil {
			err = browserRejectNullFields(payloadFields, "request_id", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = validateDriveEpochTuple(p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision, "provider_drive_epoch_result_request")
		}
		msg.Payload = p
	case MsgProviderDriveEpochResult:
		p := &ProviderDriveEpochResultPayload{}
		if err = browserRequireFields(payloadFields, "drive_attempt_id", "ordinal", "strategy", "revision", "outcome"); err == nil {
			err = browserRejectNullFields(payloadFields, "request_id", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = validateDriveEpochTuple(p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision, "provider_drive_epoch_result")
		}
		if err == nil {
			err = enumRequired("provider_drive_epoch_result.outcome", p.Outcome, "applied", "stale", "duplicate", "unsupported", "error")
		}
		msg.Payload = p
	case MsgInstitutionalCandidateOffer:
		p := &InstitutionalCandidateOfferPayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_candidate_offer", []string{"candidate_id", "materialization_kind", "expires_at"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgInstitutionalClaimRequest:
		p := &InstitutionalClaimRequestPayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_claim_request", []string{"request_id", "candidate_id", "materialization_kind"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgInstitutionalClaimResponse:
		p := &InstitutionalClaimResponsePayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_claim_response", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		if err == nil {
			fields := []string{"candidate_id", "claim_id", "binding_id", "browser_holder_generation", "lease_until"}
			if p.Outcome == "claimed" {
				err = institutionalRequirePresence(payloadFields, "institutional_claim_response", fields...)
			} else {
				err = institutionalRejectPresence(payloadFields, "institutional_claim_response", fields...)
			}
		}
		msg.Payload = p
	case MsgInstitutionalBindRequest:
		p := &InstitutionalBindRequestPayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_bind_request", []string{"request_id", "claim_id", "binding_id", "tab_id"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgInstitutionalBindResponse:
		p := &InstitutionalBindResponsePayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_bind_response", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		if err == nil {
			fields := []string{"claim_id", "binding_id"}
			if p.Outcome == "bound" {
				err = institutionalRequirePresence(payloadFields, "institutional_bind_response", fields...)
			} else {
				err = institutionalRejectPresence(payloadFields, "institutional_bind_response", fields...)
			}
		}
		msg.Payload = p
	case MsgInstitutionalRouteRequest:
		p := &InstitutionalRouteRequestPayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_route_request", []string{"request_id", "claim_id", "binding_id"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgInstitutionalRouteResponse:
		p := &InstitutionalRouteResponsePayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_route_response", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		if err == nil {
			fields := []string{"claim_id", "binding_id", "route_issuance_ordinal", "url"}
			if p.Outcome == "issued" {
				err = institutionalRequirePresence(payloadFields, "institutional_route_response", fields...)
			} else {
				err = institutionalRejectPresence(payloadFields, "institutional_route_response", fields...)
			}
		}
		msg.Payload = p
	case MsgInstitutionalNavigatedRequest:
		p := &InstitutionalNavigatedRequestPayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_navigated_request", []string{"request_id", "claim_id", "binding_id", "route_issuance_ordinal", "tab_id"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgInstitutionalNavigatedResponse:
		p := &InstitutionalNavigatedResponsePayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_navigated_response", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		if err == nil {
			fields := []string{"claim_id", "binding_id"}
			if p.Outcome == "acknowledged" {
				err = institutionalRequirePresence(payloadFields, "institutional_navigated_response", fields...)
			} else {
				err = institutionalRejectPresence(payloadFields, "institutional_navigated_response", fields...)
			}
		}
		msg.Payload = p
	case MsgInstitutionalReconcileRequest:
		p := &InstitutionalReconcileRequestPayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_reconcile_request", []string{"request_id", "bindings"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgInstitutionalReconcileResponse:
		p := &InstitutionalReconcileResponsePayload{}
		err = decodeInstitutionalPayload(env.Payload, payloadFields, "institutional_reconcile_response", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		if err == nil && p.Outcome != "reconciled" {
			err = institutionalRejectPresence(payloadFields, "institutional_reconcile_response", "claims")
		}
		msg.Payload = p
	case MsgError:
		p := &ErrorPayload{}
		if err = browserRequireFields(payloadFields, "code", "message"); err == nil {
			err = browserRejectNullFields(payloadFields, "request_id")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if !errorCodeRE.MatchString(p.Code) {
				err = fmt.Errorf("invalid error code %q", p.Code)
			} else if p.Message == "" || browserTextLen(p.Message) > 1000 {
				err = fmt.Errorf("error message required (max 1000)")
			} else if p.RequestID != "" {
				err = validateCorrelationID("error.request_id", p.RequestID)
			}
		}
		msg.Payload = p
	case MsgTriageSnapshotRequest:
		p := &TriageSnapshotRequestPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "triage_snapshot_request",
			[]string{"request_id", "schema_versions"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgTriageSnapshotResponse:
		p := &TriageSnapshotResponsePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "triage_snapshot_response",
			[]string{"request_id", "schema", "generated_at", "counts", "items", "has_more", "unsupported_items_count"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgTriageCountsRequest:
		p := &TriageCountsRequestPayload{}
		if err = browserRequireFields(payloadFields, "request_id"); err == nil {
			err = browserRejectNullFields(payloadFields, "schema_versions")
		}
		if err == nil {
			err = decodeTriagePayload(env.Payload, payloadFields, "triage_counts_request", []string{"request_id"}, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgTriageCountsResponse:
		p := &TriageCountsResponsePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "triage_counts_response", []string{"request_id", "counts"}, p)
		if err == nil {
			err = validateCorrelationID("triage_counts_response.request_id", p.RequestID)
		}
		if err == nil {
			err = p.Counts.validate()
		}
		msg.Payload = p
	case MsgTriageDecide:
		p := &TriageDecidePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "triage_decide", []string{"request_id", "item_id", "op"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgTriageDecideResult:
		p := &TriageDecideResultPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "triage_decide_result", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate("triage_decide_result")
		}
		msg.Payload = p
	case MsgHumanActionResolve:
		p := &HumanActionResolvePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "human_action_resolve",
			[]string{"request_id", "action_id", "verdict", "expected_revision"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgHumanActionResolveResult:
		p := &HumanActionResolveResultPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "human_action_resolve_result", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgDeliveryReconcileRequest:
		p := &DeliveryReconcilePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "delivery_reconcile_request",
			[]string{"request_id", "job_id", "operation"}, p)
		if err == nil {
			// A raw-field presence check, not a value check: an explicit
			// "provider_reference": "" on confirm_request_absent must be
			// rejected the same as a non-empty one (parity with
			// extension/src/protocol.ts's "provider_reference" in p check
			// and the schema's "not": {"required": ["provider_reference"]}).
			// p.ProviderReference == "" alone cannot distinguish "absent"
			// from "present and empty".
			if _, present := payloadFields["provider_reference"]; present && p.Operation != "confirm_request_exists" {
				err = fmt.Errorf("delivery_reconcile_request.provider_reference is only valid for confirm_request_exists")
			}
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgDeliveryReconcileResult:
		p := &DeliveryReconcileResultPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "delivery_reconcile_result", []string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgHandoffLinkRequest:
		p := &HandoffLinkRequestPayload{}
		if err = browserRequireFields(payloadFields, "job_id"); err == nil {
			err = browserRejectNoncanonicalFields(payloadFields, "job_id", "request_id")
		}
		if err == nil {
			err = browserRejectNullFields(payloadFields, "request_id")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if _, present := payloadFields["request_id"]; present && p.RequestID == "" {
				err = fmt.Errorf("handoff_link_request.request_id must not be empty when present")
			}
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgHandoffLinkResult:
		p := &HandoffLinkResultPayload{}
		if err = browserRequireFields(payloadFields, "outcome"); err == nil {
			err = browserRejectNoncanonicalFields(payloadFields, "outcome", "request_id", "url", "detail")
		}
		if err == nil {
			err = browserRejectNullFields(payloadFields, "request_id", "url", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			if _, present := payloadFields["request_id"]; present && p.RequestID == "" {
				err = fmt.Errorf("handoff_link_result.request_id must not be empty when present")
			}
		}
		if err == nil {
			err = p.validate()
		}
		if err == nil {
			_, hasURL := payloadFields["url"]
			_, hasDetail := payloadFields["detail"]
			if p.Outcome == "opened" && hasDetail {
				err = fmt.Errorf("handoff_link_result.opened forbids detail")
			} else if p.Outcome != "opened" && hasURL {
				err = fmt.Errorf("handoff_link_result.%s forbids url", p.Outcome)
			}
		}
		msg.Payload = p
	case MsgReviewPreviewRequest:
		p := &ReviewPreviewRequestPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "review_preview_request", []string{"request_id", "action_id"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgReviewPreviewResult:
		p := &ReviewPreviewResultPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "review_preview_result",
			[]string{"request_id", "outcome"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgStatsRequest:
		p := &StatsRequestPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "stats_request", []string{"request_id"}, p)
		if err == nil {
			err = validateCorrelationID("stats_request.request_id", p.RequestID)
		}
		msg.Payload = p
	case MsgStatsResponse:
		p := &StatsResponsePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "stats_response",
			[]string{"request_id", "generated_at", "acquired_total", "failed_total", "handoffs_required", "access", "series"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgActivityRequest:
		p := &ActivityRequestPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "activity_request", []string{"request_id"}, p)
		if err == nil {
			if _, ok := payloadFields["limit"]; !ok {
				p.Limit = 20
			}
			err = p.validate()
		}
		msg.Payload = p
	case MsgActivityResponse:
		p := &ActivityResponsePayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "activity_response",
			[]string{"request_id", "generated_at", "entries"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageBulkStatusRequest:
		p := &PageBulkStatusRequestPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "page_bulk_status_request",
			[]string{"request_id", "scan_id", "identifiers"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageBulkStatusResult:
		p := &PageBulkStatusResultPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "page_bulk_status_result",
			[]string{"request_id", "scan_id", "items", "truncated"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageBulkSubmitRequest:
		p := &PageBulkSubmitRequestPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "page_bulk_submit_request",
			[]string{"request_id", "scan_id", "canonical_keys", "source"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPageBulkSubmitResult:
		p := &PageBulkSubmitResultPayload{}
		err = decodeTriagePayload(env.Payload, payloadFields, "page_bulk_submit_result",
			[]string{"request_id", "scan_id", "submitted", "joined", "already_owned", "invalid", "batch_id"}, p)
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPdfGrabRequest:
		p := &PdfGrabRequestPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "host"); err == nil {
			err = browserRejectNullFields(payloadFields, "title")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPdfGrabStatusRequest:
		p := &PdfGrabStatusRequestPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "grab_id"); err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPdfGrabResult:
		p := &PdfGrabResultPayload{}
		if err = browserRequireFields(payloadFields, "outcome"); err == nil {
			err = browserRejectNullFields(payloadFields, "request_id", "grab_id", "steering_path", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPdfGrabStatusResult:
		p := &PdfGrabStatusResultPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "grab_id", "state"); err == nil {
			err = browserRejectNullFields(payloadFields, "outcome", "detail", "job_id")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPdfGrabAbandonRequest:
		p := &PdfGrabAbandonRequestPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "grab_id"); err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgPdfGrabAbandonResult:
		p := &PdfGrabAbandonResultPayload{}
		if err = browserRequireFields(payloadFields, "request_id", "grab_id", "state"); err == nil {
			err = browserRejectNullFields(payloadFields, "outcome", "detail")
		}
		if err == nil {
			err = strictDecode(env.Payload, p)
		}
		if err == nil {
			err = p.validate()
		}
		msg.Payload = p
	case MsgAck, MsgJobAccept, MsgJobReject, MsgCancel, MsgHandoffFocus:
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

func (p *PageAcquirePayload) validate() error {
	if browserTextLen(p.URL) == 0 || browserTextLen(p.URL) > 4000 {
		return fmt.Errorf("page_acquire.url required (max 4000)")
	}
	if browserHasNUL(p.URL) {
		return fmt.Errorf("page_acquire.url cannot contain NUL")
	}
	u, err := url.ParseRequestURI(p.URL)
	if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Host == "" {
		return fmt.Errorf("page_acquire.url must be a parseable http(s) URL")
	}
	if browserTextLen(p.DOI) > 512 {
		return fmt.Errorf("page_acquire.doi exceeds 512 chars")
	}
	if browserHasNUL(p.DOI) {
		return fmt.Errorf("page_acquire.doi cannot contain NUL")
	}
	if browserTextLen(p.Title) > 1024 {
		return fmt.Errorf("page_acquire.title exceeds 1024 chars")
	}
	if browserHasNUL(p.Title) {
		return fmt.Errorf("page_acquire.title cannot contain NUL")
	}
	if browserTextLen(p.Source) > 1024 {
		return fmt.Errorf("page_acquire.source exceeds 1024 chars")
	}
	if browserHasNUL(p.Source) {
		return fmt.Errorf("page_acquire.source cannot contain NUL")
	}
	return nil
}

func (p *PageAcquireAckPayload) validate() error {
	hasJobID := p.JobID != ""
	hasError := p.Error != ""
	if hasJobID == hasError {
		return fmt.Errorf("page_acquire_ack requires exactly one of job_id or error")
	}
	if hasJobID && !requestIDRE.MatchString(p.JobID) {
		return fmt.Errorf("page_acquire_ack.job_id is invalid")
	}
	if browserTextLen(p.Error) > 1000 {
		return fmt.Errorf("page_acquire_ack.error exceeds 1000 chars")
	}
	if browserHasNUL(p.Error) {
		return fmt.Errorf("page_acquire_ack.error cannot contain NUL")
	}
	if p.Duplicate && !hasJobID {
		return fmt.Errorf("page_acquire_ack.duplicate requires job_id")
	}
	return nil
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
	if p.AccessMode != "" {
		if err := enumRequired("access_mode", p.AccessMode, "assisted", "delegated"); err != nil {
			return err
		}
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

func (p *HandoffOutcomePayload) validate() error {
	if err := enumRequired("handoff_outcome.outcome", p.Outcome, "stale_sso", "auth_error"); err != nil {
		return err
	}
	if p.FinalHost == "" || len(p.FinalHost) > 253 || !hostRE.MatchString(p.FinalHost) {
		return fmt.Errorf("handoff_outcome.final_host must be a bounded hostname")
	}
	return nil
}

func (p *PageCapturePayload) validate() error {
	if p.Host == "" || len(p.Host) > 253 || !hostRE.MatchString(p.Host) {
		return fmt.Errorf("page_capture.host must be a bounded hostname")
	}
	if err := enumRequired("page_capture.scenario", p.Scenario,
		"observed", "success", "login-return", "no-entitlement", "drift", "terms"); err != nil {
		return err
	}
	if p.AdapterID != "" && !adapterIDRE.MatchString(p.AdapterID) {
		return fmt.Errorf("page_capture.adapter_id must use the id charset (max 64)")
	}
	if browserTextLen(p.AdapterVersion) > 50 {
		return fmt.Errorf("page_capture.adapter_version exceeds 50 chars")
	}
	if p.Encoding != "gzip+base64" {
		return fmt.Errorf("page_capture.encoding must be gzip+base64")
	}
	if p.Bytes < 1 || p.Bytes > 2<<20 {
		return fmt.Errorf("page_capture.bytes must be in range 1..%d", 2<<20)
	}
	if !base64RE.MatchString(p.Body) {
		return fmt.Errorf("page_capture.body must be canonical base64")
	}
	// Optional: only a requested capture echoes one back. When present it must
	// be a real correlation id, so a malformed echo can never bind to a
	// pending request by accident.
	if p.RequestID != "" {
		if err := validateCorrelationID("page_capture.request_id", p.RequestID); err != nil {
			return err
		}
	}
	return nil
}

func (p *PageCaptureRequestPayload) validate() error {
	if err := validateCorrelationID("page_capture_request.request_id", p.RequestID); err != nil {
		return err
	}
	parsed, err := url.ParseRequestURI(p.URL)
	if browserTextLen(p.URL) == 0 || browserTextLen(p.URL) > 4000 ||
		err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("page_capture_request.url must be a bounded parseable https URL")
	}
	if !adapterIDRE.MatchString(p.Provider) {
		return fmt.Errorf("page_capture_request.provider must use the id charset (max 64)")
	}
	if err := enumRequired("page_capture_request.scenario", p.Scenario,
		"success", "login-return", "no-entitlement", "drift", "terms"); err != nil {
		return err
	}
	if p.SettleMS != nil && (*p.SettleMS < 0 || *p.SettleMS > 10_000) {
		return fmt.Errorf("page_capture_request.settle_ms must be in range 0..10000")
	}
	return nil
}

func (p *PageCaptureRequestResultPayload) validate() error {
	if err := validateCorrelationID("page_capture_request_result.request_id", p.RequestID); err != nil {
		return err
	}
	if err := enumRequired("page_capture_request_result.outcome", p.Outcome,
		"captured", "nav_failed", "timeout", "not_permitted", "busy"); err != nil {
		return err
	}
	return validateTriageText("page_capture_request_result.detail", p.Detail, 1000)
}
func (p *PdfGrabRequestPayload) validate() error {
	if err := validateCorrelationID("pdf_grab_request.request_id", p.RequestID); err != nil {
		return err
	}
	if browserTextLen(p.Host) == 0 || browserTextLen(p.Host) > 253 ||
		!pdfGrabHostRE.MatchString(p.Host) {
		return fmt.Errorf("pdf_grab_request.host must be a bare hostname")
	}
	return validateTriageText("pdf_grab_request.title", p.Title, 500)
}

var pdfGrabHostRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
var pdfGrabItemIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// pdfGrabSteeringPathRE matches the "papio/grabs/<id>/" relative path the
// daemon returns as SteeringPath — the same "papio/" download-relocation
// prefix background.ts already hardcodes for ordinary job adoption
// (`papio/${job_id}/${base}`), with "grabs/<grab-id>/" naming the reserved
// subtree SweepTerminalAdoptions's unknown-dir hygiene must never sweep.
var pdfGrabSteeringPathRE = regexp.MustCompile(`^papio/grabs/[A-Za-z0-9_-]{8,64}/$`)

func (p *PdfGrabResultPayload) validate() error {
	if p.RequestID != "" {
		if err := validateCorrelationID("pdf_grab_result.request_id", p.RequestID); err != nil {
			return err
		}
	}
	if p.GrabID != "" {
		if err := validateCorrelationID("pdf_grab_result.grab_id", p.GrabID); err != nil {
			return err
		}
	}
	if err := enumRequired("pdf_grab_result.outcome", p.Outcome,
		"steering", "existing", "not_supported", "unavailable",
		"job_created", "already_owned", "needs_identifier", "failed_validation", "abandoned"); err != nil {
		return err
	}
	switch p.Outcome {
	case "steering":
		if p.RequestID == "" {
			return fmt.Errorf("pdf_grab_result: steering outcome requires request_id (the synchronous allocation reply)")
		}
		if p.GrabID == "" {
			return fmt.Errorf("pdf_grab_result: steering outcome requires grab_id")
		}
		if !pdfGrabSteeringPathRE.MatchString(p.SteeringPath) {
			return fmt.Errorf("pdf_grab_result.steering_path must match papio/grabs/<grab-id>/")
		}
	case "existing":
		if p.RequestID == "" || p.GrabID == "" {
			return fmt.Errorf("pdf_grab_result: existing outcome requires request_id and grab_id")
		}
		if p.SteeringPath != "" {
			return fmt.Errorf("pdf_grab_result: existing outcome must not carry steering_path")
		}
	case "not_supported", "unavailable":
		// grab_id is intentionally optional here: a refusal decided before
		// allocation (an unhealthy adoption latch, an unusable tab URL) has
		// no grab to name; one decided after allocation (the capture
		// directory could not be created) may still carry it.
		if p.RequestID == "" {
			return fmt.Errorf("pdf_grab_result: refusal outcome %q requires request_id", p.Outcome)
		}
		if p.SteeringPath != "" {
			return fmt.Errorf("pdf_grab_result: refusal outcome %q must not carry steering_path", p.Outcome)
		}
	default: // job_created, already_owned, needs_identifier, failed_validation
		if p.RequestID != "" {
			return fmt.Errorf("pdf_grab_result: %s is an unsolicited outcome and must not carry request_id", p.Outcome)
		}
		if p.GrabID == "" {
			return fmt.Errorf("pdf_grab_result: %s requires grab_id", p.Outcome)
		}
		if p.SteeringPath != "" {
			return fmt.Errorf("pdf_grab_result: %s must not carry steering_path", p.Outcome)
		}
	}
	return validateTriageText("pdf_grab_result.detail", p.Detail, 1000)
}

func (p *PdfGrabStatusRequestPayload) validate() error {
	if err := validateCorrelationID("pdf_grab_status_request.request_id", p.RequestID); err != nil {
		return err
	}
	return validateCorrelationID("pdf_grab_status_request.grab_id", p.GrabID)
}

func (p *PdfGrabStatusResultPayload) validate() error {
	if err := validateCorrelationID("pdf_grab_status_result.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("pdf_grab_status_result.grab_id", p.GrabID); err != nil {
		return err
	}
	if p.State == "" {
		if p.Outcome != "not_found" && p.Outcome != "unavailable" {
			return fmt.Errorf("pdf_grab_status_result.state required")
		}
	} else if err := enumRequired("pdf_grab_status_result.state", p.State,
		"awaiting_file", "quarantined", "identified", "job_created", "parked_no_identifier", "failed_validation", "abandoned"); err != nil {
		return err
	}
	if p.Outcome != "" && p.Outcome != "not_found" && p.Outcome != "unavailable" &&
		p.Outcome != "job_created" && p.Outcome != "already_owned" &&
		p.Outcome != "needs_identifier" && p.Outcome != "failed_validation" && p.Outcome != "abandoned" {
		return fmt.Errorf("pdf_grab_status_result.outcome invalid")
	}
	if p.JobID != "" {
		if err := validateCorrelationID("pdf_grab_status_result.job_id", p.JobID); err != nil {
			return err
		}
	}
	return validateTriageText("pdf_grab_status_result.detail", p.Detail, 1000)
}

func (p *PdfGrabAbandonRequestPayload) validate() error {
	if err := validateCorrelationID("pdf_grab_abandon_request.request_id", p.RequestID); err != nil {
		return err
	}
	return validateCorrelationID("pdf_grab_abandon_request.grab_id", p.GrabID)
}

func (p *PdfGrabAbandonResultPayload) validate() error {
	if err := validateCorrelationID("pdf_grab_abandon_result.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("pdf_grab_abandon_result.grab_id", p.GrabID); err != nil {
		return err
	}
	if p.Outcome != "" && p.Outcome != "abandoned" && p.Outcome != "not_found" && p.Outcome != "unavailable" && p.Outcome != "conflict" {
		return fmt.Errorf("pdf_grab_abandon_result.outcome invalid")
	}
	if p.State == "" {
		if p.Outcome != "not_found" && p.Outcome != "unavailable" {
			return fmt.Errorf("pdf_grab_abandon_result.state required")
		}
	} else if err := enumRequired("pdf_grab_abandon_result.state", p.State,
		"awaiting_file", "quarantined", "identified", "job_created", "parked_no_identifier", "failed_validation", "abandoned"); err != nil {
		return err
	}
	if p.Outcome == "abandoned" && p.State != "abandoned" {
		return fmt.Errorf("pdf_grab_abandon_result.abandoned state required")
	}
	return validateTriageText("pdf_grab_abandon_result.detail", p.Detail, 1000)
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
	if p.AdapterID != "" && !adapterIDRE.MatchString(p.AdapterID) {
		return fmt.Errorf("provider_outcome.adapter_id must use the id charset (max 64)")
	}
	if browserTextLen(p.AdapterVersion) > 50 {
		return fmt.Errorf("adapter_version exceeds 50 chars")
	}
	if browserTextLen(p.Detail) > 500 {
		return fmt.Errorf("detail exceeds 500 chars")
	}
	return nil
}
func (p *ProviderDirectGetRequestPayload) validate() error {
	if err := validateCorrelationID("provider_direct_get_request.drive_attempt_id", p.DriveAttemptID); err != nil {
		return err
	}
	if p.Ordinal < 0 || p.Ordinal > MaxBrowserInteger {
		return fmt.Errorf("provider_direct_get_request.ordinal must be in range 0..%d", MaxBrowserInteger)
	}
	if p.RouteRevision == "" || browserTextLen(p.RouteRevision) > 128 || !strings.Contains(p.RouteRevision, "/") {
		return fmt.Errorf("provider_direct_get_request.route_revision is invalid")
	}
	if p.ExpectedIdentifier == "" || browserTextLen(p.ExpectedIdentifier) > 256 || strings.ContainsAny(p.ExpectedIdentifier, "?#{}\\\x00\r\n@") || directGetIdentifierUnsafe(p.ExpectedIdentifier) {
		return fmt.Errorf("provider_direct_get_request.expected_identifier is invalid")
	}
	if browserTextLen(p.PathFamily) == 0 || browserTextLen(p.PathFamily) > 512 || strings.ContainsAny(p.PathFamily, "?#\\\x00\r\n") {
		return fmt.Errorf("provider_direct_get_request.path_family is invalid")
	}
	if browserTextLen(p.URL) == 0 || browserTextLen(p.URL) > 2048 || browserTextLen(p.AllowedOrigin) == 0 || browserTextLen(p.AllowedOrigin) > 300 {
		return fmt.Errorf("provider_direct_get_request URL envelope exceeds bounds")
	}
	if err := validateDirectGetEnvelope(p.URL, p.AllowedOrigin, p.PathFamily, p.ExpectedIdentifier); err != nil {
		return fmt.Errorf("provider_direct_get_request: %w", err)
	}
	if err := enumRequired("provider_direct_get_request.terms_policy", p.TermsPolicy, "none", "durable_consent"); err != nil {
		return err
	}
	return nil
}

func (p *ProviderDirectGetResultPayload) validate() error {
	if err := validateCorrelationID("provider_direct_get_result.drive_attempt_id", p.DriveAttemptID); err != nil {
		return err
	}
	if p.Ordinal < 0 || p.Ordinal > MaxBrowserInteger {
		return fmt.Errorf("provider_direct_get_result.ordinal must be in range 0..%d", MaxBrowserInteger)
	}
	if p.RouteRevision == "" || browserTextLen(p.RouteRevision) > 128 || !strings.Contains(p.RouteRevision, "/") {
		return fmt.Errorf("provider_direct_get_result.route_revision is invalid")
	}
	if err := enumRequired("provider_direct_get_result.outcome", p.Outcome,
		"success", "not_pdf", "foreign", "login", "terms", "challenge", "cancelled",
		"timeout", "network", "rate_limited", "server_error", "unknown"); err != nil {
		return err
	}
	if err := enumRequired("provider_direct_get_result.landing_class", p.LandingClass,
		"pdf", "html", "login", "terms", "challenge", "foreign", "unknown"); err != nil {
		return err
	}
	if p.FinalHost != "" && (!hostRE.MatchString(p.FinalHost) || strings.ToLower(p.FinalHost) != p.FinalHost) {
		return fmt.Errorf("provider_direct_get_result.final_host must be a lowercase hostname")
	}
	if p.FinalPath != "" && (browserTextLen(p.FinalPath) > 1000 || !strings.HasPrefix(p.FinalPath, "/") || strings.ContainsAny(p.FinalPath, "?#\x00\r\n")) {
		return fmt.Errorf("provider_direct_get_result.final_path must be a sanitized path")
	}
	if p.Outcome == "success" && (p.LandingClass != "pdf" || p.FinalHost == "" || p.FinalPath == "") {
		return fmt.Errorf("provider_direct_get_result success requires pdf landing and final envelope")
	}
	if browserTextLen(p.Detail) > 500 {
		return fmt.Errorf("provider_direct_get_result.detail exceeds 500 chars")
	}
	return nil
}
func validateDriveEpochTuple(attempt string, ordinal int64, strategy, revision, what string) error {
	if err := validateCorrelationID(what+".drive_attempt_id", attempt); err != nil {
		return err
	}
	if ordinal < 0 || ordinal > MaxBrowserInteger {
		return fmt.Errorf("%s.ordinal out of range", what)
	}
	if strategy == "" || browserTextLen(strategy) > 128 || strings.ContainsAny(strategy, "\x00\r\n") {
		return fmt.Errorf("%s.strategy invalid", what)
	}
	if revision == "" || browserTextLen(revision) > 128 || strings.ContainsAny(revision, "\x00\r\n") {
		return fmt.Errorf("%s.revision invalid", what)
	}
	return nil
}

func validateDirectGetEnvelope(rawURL, rawOrigin, pathFamily, expectedIdentifier string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Fragment != "" {
		return fmt.Errorf("url must be an https URL without userinfo or fragment")
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Scheme != "https" || origin.User != nil || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" || origin.Host == "" || u.Host != origin.Host {
		return fmt.Errorf("url is outside allowed origin")
	}
	if u.RawQuery != "" && u.RawQuery != "download=true" {
		return fmt.Errorf("url contains an unsupported query")
	}
	kind, identifier, ok := strings.Cut(expectedIdentifier, ":")
	if !ok || kind == "" || identifier == "" || strings.ContainsAny(kind, "{}") {
		return fmt.Errorf("expected_identifier is invalid")
	}
	placeholder := "{" + kind + "}"
	if strings.Count(pathFamily, "{") != 1 || strings.Count(pathFamily, "}") != 1 || !strings.Contains(pathFamily, placeholder) {
		return fmt.Errorf("path_family must contain exactly one placeholder matching expected_identifier")
	}
	prefix, suffix, found := strings.Cut(pathFamily, placeholder)
	if !found || prefix == "" {
		return fmt.Errorf("path_family does not match expected_identifier")
	}
	expectedPath := prefix + escapeDirectGetIdentifier(identifier) + suffix
	if u.EscapedPath() != expectedPath {
		return fmt.Errorf("url path does not match path_family")
	}
	return nil
}
func escapeDirectGetIdentifier(identifier string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for segmentIndex, segment := range strings.Split(identifier, "/") {
		if segmentIndex > 0 {
			b.WriteByte('/')
		}
		dotSegment := segment == "." || segment == ".."
		for i := range len(segment) {
			c := segment[i]
			unreserved := !dotSegment &&
				(c >= 'A' && c <= 'Z' ||
					c >= 'a' && c <= 'z' ||
					c >= '0' && c <= '9' ||
					c == '-' || c == '.' || c == '_' || c == '~')
			if unreserved {
				b.WriteByte(c)
			} else {
				b.WriteByte('%')
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0x0f])
			}
		}
	}
	return b.String()
}

func directGetIdentifierUnsafe(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// validateDownload rejects download_id 0. Delivery provenance correlates on
// browserDownloadKey{JobID, DownloadID} (internal/browser/bridge.go): two
// downloads reported with download_id 0 for the same job collide on that key,
// so a download_complete for the second overwrites the pending entry's
// CandidateID from the first, and a delivery_context meant for the first
// download applies its access_basis to the second, unrelated candidate — the
// exact mis-binding class delivery provenance exists to prevent, reached
// through the correlation key instead of the candidate id. This is fail-closed
// hardening, not a live-traffic fix: chrome.downloads allocates ids starting
// at 1 and increasing, so no genuine extension ever sends 0, and a floor of 1
// cannot reject any frame a real client produces.
func validateDownload(id int64, filename string) error {
	if id < 1 || id > MaxBrowserInteger {
		return fmt.Errorf("download_id must be in range 1..%d", MaxBrowserInteger)
	}
	if !filenameRE.MatchString(filename) {
		return fmt.Errorf("filename must be a bare name without path separators")
	}
	return nil
}

func (p *DeliveryContextPayload) validate() error {
	if p.DownloadID < 1 || p.DownloadID > MaxBrowserInteger {
		return fmt.Errorf("delivery_context.download_id must be in range 1..%d", MaxBrowserInteger)
	}
	if err := enumRequired("delivery_context.route", p.Route, "resolver", "direct", "oa"); err != nil {
		return err
	}
	if err := enumRequired("delivery_context.session_evidence", p.SessionEvidence, "fresh_auth", "warm", "none"); err != nil {
		return err
	}
	if p.Route == "oa" && p.SessionEvidence != "none" {
		return fmt.Errorf("delivery_context.route oa requires session_evidence none")
	}
	if p.PageHost != "" {
		if browserTextLen(p.PageHost) > 128 || !hostRE.MatchString(p.PageHost) ||
			strings.Contains(p.PageHost, "..") || strings.HasPrefix(p.PageHost, ".") || strings.HasSuffix(p.PageHost, ".") {
			return fmt.Errorf("delivery_context.page_host must be a bounded lowercase registrable hostname")
		}
	}
	return nil
}

func decodeTriagePayload(data []byte, fields map[string]json.RawMessage, what string, required []string, target any) error {
	if err := browserRequireFields(fields, required...); err != nil {
		return err
	}
	if err := browserRejectNullValues(data, what); err != nil {
		return err
	}
	if err := strictDecode(data, target); err != nil {
		return err
	}
	return nil
}
func decodeInstitutionalPayload(data []byte, fields map[string]json.RawMessage, what string, required []string, target any) error {
	if err := browserRequireFields(fields, required...); err != nil {
		return err
	}
	if err := browserRejectNullValues(data, what); err != nil {
		return err
	}
	return strictDecode(data, target)
}

func institutionalID(what, value string) error {
	if !requestIDRE.MatchString(value) {
		return fmt.Errorf("%s must be a bounded opaque ID (8..128 chars)", what)
	}
	return nil
}
func institutionalRequestID(what, value string) error {
	if !requestIDRE.MatchString(value) {
		return fmt.Errorf("%s must be a bounded request_id (8..128 chars)", what)
	}
	return nil
}

func institutionalOutcome(what, value string, allowed ...string) error {
	return enumRequired(what, value, allowed...)
}

func institutionalOrdinal(what string, value int64) error {
	if value < 0 || value > MaxBrowserInteger {
		return fmt.Errorf("%s must be in range 0..%d", what, MaxBrowserInteger)
	}
	return nil
}

func institutionalTabID(what string, value int64) error {
	if value < 0 || value > MaxBrowserInteger {
		return fmt.Errorf("%s must be in range 0..%d", what, MaxBrowserInteger)
	}
	return nil
}
func (p *InstitutionalCandidateOfferPayload) validate() error {
	if err := institutionalID("institutional_candidate_offer.candidate_id", p.CandidateID); err != nil {
		return err
	}
	if err := enumRequired("institutional_candidate_offer.materialization_kind", p.MaterializationKind, "browser_tab"); err != nil {
		return err
	}
	return validateTriageTime("institutional_candidate_offer.expires_at", p.ExpiresAt)
}

func (p *InstitutionalClaimRequestPayload) validate() error {
	if err := institutionalRequestID("institutional_claim_request.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalID("institutional_claim_request.candidate_id", p.CandidateID); err != nil {
		return err
	}
	return enumRequired("institutional_claim_request.materialization_kind", p.MaterializationKind, "browser_tab", "direct_download")
}
func (p *InstitutionalClaimResponsePayload) validate() error {
	if err := institutionalRequestID("institutional_claim_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalOutcome("institutional_claim_response.outcome", p.Outcome,
		"feature_disabled", "claimed", "stale", "not_eligible", "busy", "error"); err != nil {
		return err
	}
	if err := validateTriageText("institutional_claim_response.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Outcome == "claimed" && p.Detail != "" {
		return fmt.Errorf("institutional_claim_response.claimed must not carry detail")
	}
	if p.Outcome == "claimed" {
		for name, value := range map[string]string{"candidate_id": p.CandidateID, "claim_id": p.ClaimID, "binding_id": p.BindingID} {
			if err := institutionalID("institutional_claim_response."+name, value); err != nil {
				return err
			}
		}
		if p.BrowserHolderGeneration == nil {
			return fmt.Errorf("institutional_claim_response.browser_holder_generation is required")
		}
		if err := institutionalOrdinal("institutional_claim_response.browser_holder_generation", *p.BrowserHolderGeneration); err != nil {
			return err
		}
		if err := validateTriageTime("institutional_claim_response.lease_until", p.LeaseUntil); err != nil {
			return err
		}
	} else if p.CandidateID != "" || p.ClaimID != "" || p.BindingID != "" || p.BrowserHolderGeneration != nil || p.LeaseUntil != "" {
		return fmt.Errorf("institutional_claim_response.%s forbids claim fields", p.Outcome)
	}
	return nil
}
func (p *InstitutionalBindRequestPayload) validate() error {
	if err := institutionalRequestID("institutional_bind_request.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalID("institutional_bind_request.claim_id", p.ClaimID); err != nil {
		return err
	}
	if err := institutionalID("institutional_bind_request.binding_id", p.BindingID); err != nil {
		return err
	}
	return institutionalTabID("institutional_bind_request.tab_id", p.TabID)
}
func (p *InstitutionalBindResponsePayload) validate() error {
	if err := institutionalRequestID("institutional_bind_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalOutcome("institutional_bind_response.outcome", p.Outcome,
		"feature_disabled", "bound", "stale", "not_eligible", "error"); err != nil {
		return err
	}
	if err := validateTriageText("institutional_bind_response.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Outcome == "bound" {
		if p.Detail != "" {
			return fmt.Errorf("institutional_bind_response.bound must not carry detail")
		}
		if err := institutionalID("institutional_bind_response.claim_id", p.ClaimID); err != nil {
			return err
		}
		return institutionalID("institutional_bind_response.binding_id", p.BindingID)
	}
	if p.ClaimID != "" || p.BindingID != "" {
		return fmt.Errorf("institutional_bind_response.%s forbids claim fields", p.Outcome)
	}
	return nil
}
func (p *InstitutionalRouteRequestPayload) validate() error {
	if err := institutionalRequestID("institutional_route_request.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalID("institutional_route_request.claim_id", p.ClaimID); err != nil {
		return err
	}
	return institutionalID("institutional_route_request.binding_id", p.BindingID)
}
func (p *InstitutionalRouteResponsePayload) validate() error {
	if err := institutionalRequestID("institutional_route_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalOutcome("institutional_route_response.outcome", p.Outcome,
		"feature_disabled", "issued", "stale", "not_eligible", "busy", "error"); err != nil {
		return err
	}
	if err := validateTriageText("institutional_route_response.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Outcome == "issued" {
		if p.Detail != "" {
			return fmt.Errorf("institutional_route_response.issued must not carry detail")
		}
		if err := institutionalID("institutional_route_response.claim_id", p.ClaimID); err != nil {
			return err
		}
		if err := institutionalID("institutional_route_response.binding_id", p.BindingID); err != nil {
			return err
		}
		if err := institutionalOrdinal("institutional_route_response.route_issuance_ordinal", p.RouteIssuanceOrdinal); err != nil {
			return err
		}
		if err := validateTriageURL("institutional_route_response.url", p.URL, "https"); err != nil {
			return err
		}
		return nil
	}
	if p.ClaimID != "" || p.BindingID != "" || p.RouteIssuanceOrdinal != 0 || p.URL != "" {
		return fmt.Errorf("institutional_route_response.%s forbids route fields", p.Outcome)
	}
	return nil
}
func (p *InstitutionalNavigatedRequestPayload) validate() error {
	if err := institutionalRequestID("institutional_navigated_request.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalID("institutional_navigated_request.claim_id", p.ClaimID); err != nil {
		return err
	}
	if err := institutionalID("institutional_navigated_request.binding_id", p.BindingID); err != nil {
		return err
	}
	if err := institutionalOrdinal("institutional_navigated_request.route_issuance_ordinal", p.RouteIssuanceOrdinal); err != nil {
		return err
	}
	return institutionalTabID("institutional_navigated_request.tab_id", p.TabID)
}
func (p *InstitutionalNavigatedResponsePayload) validate() error {
	if err := institutionalRequestID("institutional_navigated_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalOutcome("institutional_navigated_response.outcome", p.Outcome,
		"feature_disabled", "acknowledged", "stale", "not_eligible", "error"); err != nil {
		return err
	}
	if err := validateTriageText("institutional_navigated_response.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Outcome == "acknowledged" {
		if p.Detail != "" {
			return fmt.Errorf("institutional_navigated_response.acknowledged must not carry detail")
		}
		if err := institutionalID("institutional_navigated_response.claim_id", p.ClaimID); err != nil {
			return err
		}
		return institutionalID("institutional_navigated_response.binding_id", p.BindingID)
	}
	if p.ClaimID != "" || p.BindingID != "" {
		return fmt.Errorf("institutional_navigated_response.%s forbids claim fields", p.Outcome)
	}
	return nil
}
func (p *InstitutionalReconcileRequestPayload) validate() error {
	if err := institutionalRequestID("institutional_reconcile_request.request_id", p.RequestID); err != nil {
		return err
	}
	if len(p.Bindings) > 32 {
		return fmt.Errorf("institutional_reconcile_request.bindings capped at 32")
	}
	seen := map[string]bool{}
	for _, b := range p.Bindings {
		if err := institutionalID("institutional_reconcile_request.binding_id", b.BindingID); err != nil {
			return err
		}
		if seen[b.BindingID] {
			return fmt.Errorf("institutional_reconcile_request.binding_id values must be unique")
		}
		seen[b.BindingID] = true
		if err := institutionalTabID("institutional_reconcile_request.tab_id", b.TabID); err != nil {
			return err
		}
	}
	return nil
}
func (p *InstitutionalReconcileResponsePayload) validate() error {
	if err := institutionalRequestID("institutional_reconcile_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := institutionalOutcome("institutional_reconcile_response.outcome", p.Outcome,
		"feature_disabled", "reconciled", "error"); err != nil {
		return err
	}
	if err := validateTriageText("institutional_reconcile_response.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Outcome == "reconciled" && p.Detail != "" {
		return fmt.Errorf("institutional_reconcile_response.reconciled must not carry detail")
	}
	if p.Outcome != "reconciled" {
		if len(p.Claims) != 0 {
			return fmt.Errorf("institutional_reconcile_response.%s forbids claims", p.Outcome)
		}
		return nil
	}
	if len(p.Claims) > 32 {
		return fmt.Errorf("institutional_reconcile_response.claims capped at 32")
	}
	for _, c := range p.Claims {
		if err := institutionalID("institutional_reconcile_response.claim_id", c.ClaimID); err != nil {
			return err
		}
		if err := institutionalID("institutional_reconcile_response.binding_id", c.BindingID); err != nil {
			return err
		}
		if err := institutionalID("institutional_reconcile_response.candidate_id", c.CandidateID); err != nil {
			return err
		}
		if err := enumRequired("institutional_reconcile_response.phase", c.Phase, "claimed", "bound", "route_issued", "navigated", "settled", "abandoned"); err != nil {
			return err
		}
		if c.TabID != nil {
			if err := institutionalTabID("institutional_reconcile_response.tab_id", *c.TabID); err != nil {
				return err
			}
		}
	}
	return nil
}

func browserRejectNullValues(data []byte, what string) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	var visit func(any) bool
	visit = func(v any) bool {
		switch typed := v.(type) {
		case nil:
			return true
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		case map[string]any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	if visit(value) {
		return fmt.Errorf("%s cannot contain null", what)
	}
	return nil
}
func institutionalRequirePresence(fields map[string]json.RawMessage, what string, names ...string) error {
	for _, name := range names {
		if _, ok := fields[name]; !ok || browserFieldIsNull(fields, name) {
			return fmt.Errorf("%s.%s is required for this outcome", what, name)
		}
	}
	return nil
}

func institutionalRejectPresence(fields map[string]json.RawMessage, what string, names ...string) error {
	for _, name := range names {
		if _, ok := fields[name]; ok {
			return fmt.Errorf("%s.%s is forbidden for this outcome", what, name)
		}
	}
	return nil
}

func validateCorrelationID(what, value string) error {
	if !msgIDRE.MatchString(value) {
		return fmt.Errorf("%s must match the msg_id charset (8..64 chars)", what)
	}
	return nil
}

func validateTriageText(what, value string, max int) error {
	if browserTextLen(value) > max {
		return fmt.Errorf("%s exceeds %d chars", what, max)
	}
	if browserHasNUL(value) {
		return fmt.Errorf("%s cannot contain NUL", what)
	}
	return nil
}

func validateTriageTime(what, value string) error {
	if !rfc3339RE.MatchString(value) {
		return fmt.Errorf("%s must be RFC3339", what)
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (p *SessionEvidencePayload) validate() error {
	if err := enumRequired("session_evidence.evidence", p.Evidence, "warm_verified", "auth_returned"); err != nil {
		return err
	}
	if err := validateTriageTime("session_evidence.at", p.At); err != nil {
		return err
	}
	if p.OriginHint != "" {
		if utf8.RuneCountInString(p.OriginHint) > 300 {
			return fmt.Errorf("session_evidence.origin_hint exceeds 300 chars")
		}
		if err := validateResolverOriginHint(p.OriginHint); err != nil {
			return fmt.Errorf("session_evidence.origin_hint: %w", err)
		}
	}
	return nil
}

func (p *TriageCountsRequestPayload) validate() error {
	if err := validateCorrelationID("triage_counts_request.request_id", p.RequestID); err != nil {
		return err
	}
	if len(p.SchemaVersions) == 0 {
		return nil
	}
	if len(p.SchemaVersions) != 1 || (p.SchemaVersions[0] != 1 && p.SchemaVersions[0] != 2) {
		return fmt.Errorf("triage_counts_request.schema_versions must be [1] or [2]")
	}
	return nil
}

func (p *TriageSnapshotRequestPayload) validate() error {
	if err := validateCorrelationID("triage_snapshot_request.request_id", p.RequestID); err != nil {
		return err
	}
	validSingle := len(p.SchemaVersions) == 1 &&
		(p.SchemaVersions[0] == 1 || p.SchemaVersions[0] == 2 || p.SchemaVersions[0] == 3 || p.SchemaVersions[0] == 4)
	validFallback := len(p.SchemaVersions) == 2 && p.SchemaVersions[0] == 4 && p.SchemaVersions[1] == 3
	if !validSingle && !validFallback {
		return fmt.Errorf("triage_snapshot_request.schema_versions must be [1], [2], [3], [4], or [4,3]")
	}
	if p.Limit != 0 && (p.Limit < 1 || p.Limit > 100) {
		return fmt.Errorf("triage_snapshot_request.limit must be between 1 and 100")
	}
	return validateTriageText("triage_snapshot_request.cursor", p.Cursor, 256)
}

func (counts TriageCounts) validate(additionalPending ...int) error {
	values := []int64{
		counts.PendingTotal, counts.WatchHits, counts.Actions, counts.Retractions,
		counts.JobsWorking, counts.JobsNeedsReview, counts.FailureGroups7d,
	}
	if counts.ActionsRequiresAuth != nil {
		values = append(values, *counts.ActionsRequiresAuth)
	}
	for _, value := range values {
		if value < 0 || value > MaxBrowserInteger {
			return fmt.Errorf("triage counts must be in range 0..%d", MaxBrowserInteger)
		}
	}
	extra := int64(0)
	floor := false
	if len(additionalPending) > 0 {
		extra = int64(additionalPending[0])
	}
	if len(additionalPending) > 1 {
		floor = additionalPending[1] != 0
	}
	expected := counts.WatchHits + counts.Actions + counts.Retractions + extra
	if floor {
		if counts.PendingTotal < expected {
			return fmt.Errorf("triage pending_total must be at least visible items plus pdf grabs")
		}
	} else if counts.PendingTotal != expected {
		return fmt.Errorf("triage pending_total must equal visible items plus pdf grabs")
	}
	return nil
}

func (item *TriageSnapshotItem) UnmarshalJSON(data []byte) error {
	fields, err := browserObjectFields(data, "triage item")
	if err != nil {
		return err
	}
	var wire struct {
		Kind  string       `json:"kind"`
		ID    string       `json:"id"`
		Rank  int64        `json:"rank"`
		Title string       `json:"title"`
		Facts []TriageFact `json:"facts"`
		Links []TriageLink `json:"links"`
		Ops   []string     `json:"ops"`

		Attention string `json:"attention"`

		Work        *TriageWork   `json:"work"`
		Abstract    string        `json:"abstract"`
		Watches     []TriageWatch `json:"watches"`
		FirstSeenAt string        `json:"first_seen_at"`

		ActionID        int64           `json:"action_id"`
		JobID           string          `json:"job_id"`
		ActionKind      string          `json:"action_kind"`
		JobState        string          `json:"job_state"`
		Revision        int64           `json:"revision"`
		SHA256          string          `json:"sha256"`
		SizeBytes       int64           `json:"size_bytes"`
		RequiresAuth    *bool           `json:"requires_auth"`
		BlockedBy       string          `json:"blocked_by"`
		RouteClass      string          `json:"route_class"`
		AuthRequirement string          `json:"auth_requirement"`
		Delivery        *TriageDelivery `json:"delivery"`

		DOI       string `json:"doi"`
		Nature    string `json:"nature"`
		NoticedAt string `json:"noticed_at"`
		NoticeDOI string `json:"notice_doi"`

		Label string      `json:"label"`
		Grab  *TriageGrab `json:"grab"`
	}
	if err := strictDecode(data, &wire); err != nil {
		return err
	}
	core := []string{"kind", "id", "rank", "title", "facts", "links", "ops"}
	allowed := append([]string(nil), core...)
	allowed = append(allowed, "attention")
	switch wire.Kind {
	case "pdf_grab":
		allowed = []string{"kind", "label", "grab", "route_class", "blocked_by", "attention", "ops"}
		if err := browserRequireFields(fields, "kind", "label", "grab", "route_class", "blocked_by", "attention", "ops"); err != nil {
			return err
		}
		if wire.Grab == nil || wire.Grab.GrabID == "" || wire.Grab.State == "" ||
			wire.Label == "" || wire.RouteClass != "pdf_identifier_needed" ||
			wire.BlockedBy != "identifier_missing" || wire.Attention != "required" ||
			len(wire.Ops) != 2 || wire.Ops[0] != "provide_identifier" || wire.Ops[1] != "dismiss" {
			return fmt.Errorf("invalid pdf_grab item")
		}
	case "watch_hit":
		allowed = append(allowed, "work", "abstract", "watches", "first_seen_at")
		if err := browserRequireFields(fields, append(core, "work", "abstract", "watches", "first_seen_at")...); err != nil {
			return err
		}
	case "human_action":
		allowed = append(allowed, "action_id", "job_id", "action_kind", "job_state", "revision", "sha256", "size_bytes",
			"requires_auth", "blocked_by", "route_class", "auth_requirement", "delivery")
		if err := browserRequireFields(fields, append(core, "action_id", "job_id", "action_kind", "job_state", "revision", "sha256", "size_bytes")...); err != nil {
			return err
		}
		if err := browserRejectNullFields(fields, "requires_auth", "blocked_by", "route_class", "auth_requirement", "delivery"); err != nil {
			return err
		}
		if _, ok := fields["blocked_by"]; ok {
			if err := enumRequired("human_action.blocked_by", wire.BlockedBy, triageBlockedByV3...); err != nil {
				return err
			}
		}
		if _, ok := fields["route_class"]; ok {
			if err := enumRequired("human_action.route_class", wire.RouteClass, triageRouteClasses...); err != nil {
				return err
			}
		}
		if _, ok := fields["auth_requirement"]; ok {
			if err := enumRequired("human_action.auth_requirement", wire.AuthRequirement, "true", "false", "unknown"); err != nil {
				return err
			}
		}
	case "retraction":
		allowed = append(allowed, "doi", "nature", "noticed_at", "notice_doi")
		if err := browserRequireFields(fields, append(core, "doi", "nature", "noticed_at")...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported triage item kind %q", wire.Kind)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key := range fields {
		if !allowedSet[key] {
			return fmt.Errorf("triage item %s: unknown field %q", wire.Kind, key)
		}
	}
	*item = TriageSnapshotItem{
		Kind: wire.Kind, ID: wire.ID, Rank: wire.Rank, Title: wire.Title, Facts: wire.Facts, Links: wire.Links, Ops: wire.Ops,
		Attention: wire.Attention,
		Work:      wire.Work, Abstract: wire.Abstract, Watches: wire.Watches, FirstSeenAt: wire.FirstSeenAt,
		ActionID: wire.ActionID, JobID: wire.JobID, ActionKind: wire.ActionKind, JobState: wire.JobState,
		Revision: wire.Revision, SHA256: wire.SHA256, SizeBytes: wire.SizeBytes,
		RequiresAuth: wire.RequiresAuth, BlockedBy: wire.BlockedBy,
		RouteClass: wire.RouteClass, AuthRequirement: wire.AuthRequirement, Delivery: wire.Delivery,
		DOI: wire.DOI, Nature: wire.Nature, NoticedAt: wire.NoticedAt, NoticeDOI: wire.NoticeDOI,
		Label: wire.Label, Grab: wire.Grab,
	}
	return item.validate()
}
func (item TriageSnapshotItem) MarshalJSON() ([]byte, error) {
	if item.Kind == "pdf_grab" {
		return json.Marshal(map[string]any{
			"kind": item.Kind, "label": item.Label, "grab": item.Grab,
			"route_class": item.RouteClass, "blocked_by": item.BlockedBy,
			"attention": item.Attention, "ops": item.Ops,
		})
	}
	core := map[string]any{
		"kind": item.Kind, "id": item.ID, "rank": item.Rank, "title": item.Title,
		"facts": item.Facts, "links": item.Links, "ops": item.Ops,
	}
	if item.Attention != "" {
		core["attention"] = item.Attention
	}
	switch item.Kind {
	case "watch_hit":
		core["work"], core["abstract"], core["watches"], core["first_seen_at"] =
			item.Work, item.Abstract, item.Watches, item.FirstSeenAt
	case "human_action":
		core["action_id"], core["job_id"], core["action_kind"], core["job_state"] =
			item.ActionID, item.JobID, item.ActionKind, item.JobState
		core["revision"], core["sha256"], core["size_bytes"] = item.Revision, item.SHA256, item.SizeBytes
		if item.RequiresAuth != nil {
			core["requires_auth"] = *item.RequiresAuth
		}
		if item.BlockedBy != "" {
			core["blocked_by"] = item.BlockedBy
		}
		if item.RouteClass != "" {
			core["route_class"] = item.RouteClass
		}
		if item.AuthRequirement != "" {
			core["auth_requirement"] = item.AuthRequirement
		}
		if item.Delivery != nil {
			core["delivery"] = item.Delivery
		}
	case "retraction":
		core["doi"], core["nature"], core["noticed_at"] = item.DOI, item.Nature, item.NoticedAt
		if item.NoticeDOI != "" {
			core["notice_doi"] = item.NoticeDOI
		}
	}
	return json.Marshal(core)
}

func (item TriageSnapshotItem) validate() error {
	if err := enumRequired("triage item kind", item.Kind, "watch_hit", "human_action", "retraction", "pdf_grab"); err != nil {
		return err
	}
	if item.Kind == "pdf_grab" {
		if item.Label == "" || item.Grab == nil || item.Grab.GrabID == "" || item.Grab.State == "" {
			return fmt.Errorf("pdf_grab requires label and grab")
		}
		if err := validateTriageText("pdf_grab.label", item.Label, 500); err != nil {
			return err
		}
		if !pdfGrabItemIDRE.MatchString(item.Grab.GrabID) {
			return fmt.Errorf("pdf_grab.grab.grab_id must match %s", pdfGrabItemIDRE)
		}
		if err := validateTriageText("pdf_grab.grab.grab_id", item.Grab.GrabID, 128); err != nil {
			return err
		}
		if err := enumRequired("pdf_grab.grab.state", item.Grab.State,
			"awaiting_file", "quarantined", "identified", "job_created", "parked_no_identifier", "failed_validation"); err != nil {
			return err
		}
		if item.RouteClass != "pdf_identifier_needed" || !containsString(triageBlockedByV4, item.BlockedBy) ||
			item.Attention != "required" || len(item.Ops) != 2 ||
			item.Ops[0] != "provide_identifier" || item.Ops[1] != "dismiss" {
			return fmt.Errorf("invalid pdf_grab presentation fields")
		}
		return nil
	}
	if item.ID == "" {
		return fmt.Errorf("triage item id is required")
	}
	if err := validateTriageText("triage item.id", item.ID, 1024); err != nil {
		return err
	}
	if item.Rank < 0 || item.Rank > MaxBrowserInteger {
		return fmt.Errorf("triage item.rank must be in range 0..%d", MaxBrowserInteger)
	}
	if err := validateTriageText("triage item.title", item.Title, 500); err != nil {
		return err
	}
	if len(item.Facts) > 8 {
		return fmt.Errorf("triage item.facts capped at 8")
	}
	for _, fact := range item.Facts {
		if err := validateTriageText("triage fact.label", fact.Label, 40); err != nil {
			return err
		}
		if err := validateTriageText("triage fact.text", fact.Text, 400); err != nil {
			return err
		}
	}
	if len(item.Links) > 16 {
		return fmt.Errorf("triage item.links capped at 16")
	}
	for _, link := range item.Links {
		if err := enumRequired("triage link.rel", link.Rel, "doi", "arxiv", "openalex", "landing", "preview"); err != nil {
			return err
		}
		if err := validateTriageURL("triage link.url", link.URL, "https"); err != nil {
			return err
		}
	}
	seenOps := make(map[string]bool, len(item.Ops))
	for _, op := range item.Ops {
		if err := enumRequired("triage item op", op, "acquire", "dismiss", "accept", "reject", "open", "retry",
			"open_request_history", "confirm_request_exists", "confirm_request_absent"); err != nil {
			return err
		}
		if seenOps[op] {
			return fmt.Errorf("triage item ops cannot repeat %q", op)
		}
		seenOps[op] = true
	}
	if item.Attention != "" {
		if err := enumRequired("triage item.attention", item.Attention, "working", "required", "advisory"); err != nil {
			return err
		}
	}
	if item.Kind != "human_action" && (item.RouteClass != "" || item.AuthRequirement != "" || item.Delivery != nil) {
		return fmt.Errorf("triage item.route_class/auth_requirement/delivery are human_action only")
	}
	switch item.Kind {
	case "watch_hit":
		if item.Work == nil || len(item.Watches) == 0 || len(item.Watches) > 100 {
			return fmt.Errorf("watch_hit requires 1..100 watches and work")
		}
		if err := validateTriageText("watch_hit.work.doi", item.Work.DOI, 300); err != nil {
			return err
		}
		if err := validateTriageText("watch_hit.work.title", item.Work.Title, 500); err != nil {
			return err
		}
		if err := validateTriageText("watch_hit.work.authors", item.Work.Authors, 200); err != nil {
			return err
		}
		if item.Work.Year < 0 || item.Work.Year > MaxBrowserInteger {
			return fmt.Errorf("watch_hit.work.year must be in range 0..%d", MaxBrowserInteger)
		}
		if err := validateTriageText("watch_hit.abstract", item.Abstract, 2000); err != nil {
			return err
		}
		if err := validateTriageTime("watch_hit.first_seen_at", item.FirstSeenAt); err != nil {
			return err
		}
		seen := make(map[int64]bool, len(item.Watches))
		for _, watch := range item.Watches {
			if watch.ID <= 0 || watch.ID > MaxBrowserInteger || seen[watch.ID] {
				return fmt.Errorf("watch_hit.watches must have unique positive IDs")
			}
			seen[watch.ID] = true
			if err := validateTriageText("watch_hit.watches.label", watch.Label, 500); err != nil {
				return err
			}
		}
	case "human_action":
		if item.ActionID <= 0 || item.ActionID > MaxBrowserInteger || item.Revision <= 0 || item.Revision > MaxBrowserInteger {
			return fmt.Errorf("human_action action_id and revision must be positive browser integers")
		}
		if !requestIDRE.MatchString(item.JobID) {
			return fmt.Errorf("human_action.job_id is invalid")
		}
		if err := validateTriageText("human_action.action_kind", item.ActionKind, 100); err != nil || item.ActionKind == "" {
			if err != nil {
				return err
			}
			return fmt.Errorf("human_action.action_kind is required")
		}
		if err := validateTriageText("human_action.job_state", item.JobState, 50); err != nil || item.JobState == "" {
			if err != nil {
				return err
			}
			return fmt.Errorf("human_action.job_state is required")
		}
		if item.SHA256 != "" && !sha256RE.MatchString(item.SHA256) {
			return fmt.Errorf("human_action.sha256 must be a lowercase SHA-256")
		}
		if item.SizeBytes < 0 || item.SizeBytes > MaxBrowserInteger {
			return fmt.Errorf("human_action.size_bytes must be in range 0..%d", MaxBrowserInteger)
		}
		if (item.RequiresAuth != nil) != (item.BlockedBy != "") {
			return fmt.Errorf("human_action.requires_auth and blocked_by must be present together")
		}
		if item.BlockedBy != "" {
			if err := enumRequired("human_action.blocked_by", item.BlockedBy, triageBlockedByV3...); err != nil {
				return err
			}
		}
		if item.RouteClass != "" {
			if err := enumRequired("human_action.route_class", item.RouteClass, triageRouteClasses...); err != nil {
				return err
			}
		}
		if item.AuthRequirement != "" {
			if err := enumRequired("human_action.auth_requirement", item.AuthRequirement, "true", "false", "unknown"); err != nil {
				return err
			}
		}
		if item.Delivery != nil {
			if item.ActionKind != "document_delivery" {
				return fmt.Errorf("human_action.delivery is only valid for document_delivery items")
			}
			if err := item.Delivery.validate(); err != nil {
				return err
			}
		}
	case "retraction":
		if err := validateTriageText("retraction.doi", item.DOI, 300); err != nil || item.DOI == "" {
			if err != nil {
				return err
			}
			return fmt.Errorf("retraction.doi is required")
		}
		if err := enumRequired("retraction.nature", item.Nature, "retraction", "correction", "concern"); err != nil {
			return err
		}
		if err := validateTriageTime("retraction.noticed_at", item.NoticedAt); err != nil {
			return err
		}
		if err := validateTriageText("retraction.notice_doi", item.NoticeDOI, 300); err != nil {
			return err
		}
	}
	return nil
}

func (p *TriageSnapshotResponsePayload) validate() error {
	if err := validateCorrelationID("triage_snapshot_response.request_id", p.RequestID); err != nil {
		return err
	}
	if p.Schema != 1 && p.Schema != 2 && p.Schema != 3 && p.Schema != 4 {
		return fmt.Errorf("triage_snapshot_response.schema must be 1, 2, 3, or 4")
	}
	grabCount := 0
	for _, item := range p.Items {
		if item.Kind == "pdf_grab" {
			grabCount++
		}
	}
	floorFlag := 0
	if p.Schema == 4 {
		floorFlag = 1
	}
	if err := p.Counts.validate(grabCount, floorFlag); err != nil {
		return err
	}
	if p.Counts.ActionsRequiresAuth != nil {
		return fmt.Errorf("triage_snapshot_response.counts cannot include actions_requires_auth")
	}
	if len(p.Items) > 100 {
		return fmt.Errorf("triage_snapshot_response.items capped at 100")
	}
	for _, item := range p.Items {
		if err := item.validate(); err != nil {
			return err
		}
		v3Fields := item.RouteClass != "" || item.AuthRequirement != "" || item.Delivery != nil
		switch p.Schema {
		case 1:
			if item.RequiresAuth != nil || item.BlockedBy != "" {
				return fmt.Errorf("triage_snapshot_response.schema 1 cannot include access classification")
			}
			if item.Attention != "" || v3Fields {
				return fmt.Errorf("triage_snapshot_response.schema 1 cannot include triage-snapshot/3 fields")
			}
		case 2:
			if item.Attention != "" || v3Fields {
				return fmt.Errorf("triage_snapshot_response.schema 2 cannot include triage-snapshot/3 fields")
			}
			if item.BlockedBy != "" && !containsString(triageBlockedByV2, item.BlockedBy) {
				return fmt.Errorf("triage_snapshot_response.schema 2 blocked_by must be a schema-2 value")
			}
		case 3:
			if item.Kind == "pdf_grab" {
				return fmt.Errorf("triage_snapshot_response.schema 3 cannot include pdf_grab")
			}
			if item.Attention == "" {
				return fmt.Errorf("triage_snapshot_response.schema 3 requires attention on every item")
			}
			if item.Kind == "human_action" {
				if item.RouteClass == "" || item.AuthRequirement == "" {
					return fmt.Errorf("triage_snapshot_response.schema 3 human_action items require route_class and auth_requirement")
				}
			} else if v3Fields {
				return fmt.Errorf("triage_snapshot_response.schema 3 route_class/auth_requirement/delivery are human_action only")
			}
		case 4:
			if item.Attention == "" {
				return fmt.Errorf("triage_snapshot_response.schema 4 requires attention on every item")
			}
			if item.Kind == "pdf_grab" {
				if item.RouteClass != "pdf_identifier_needed" || item.BlockedBy != "identifier_missing" {
					return fmt.Errorf("triage_snapshot_response.schema 4 pdf_grab fields are invalid")
				}
			} else if item.Kind == "human_action" {
				if item.RouteClass == "" || item.AuthRequirement == "" {
					return fmt.Errorf("triage_snapshot_response.schema 4 human_action items require route_class and auth_requirement")
				}
			} else if v3Fields {
				return fmt.Errorf("triage_snapshot_response.schema 4 route_class/auth_requirement/delivery are human_action only")
			}
		}
	}
	if p.UnsupportedItemsCount < 0 || p.UnsupportedItemsCount > MaxBrowserInteger {
		return fmt.Errorf("triage_snapshot_response.unsupported_items_count must be non-negative")
	}
	if p.HasMore && p.Cursor == "" {
		return fmt.Errorf("triage_snapshot_response.cursor required when has_more")
	}
	if !p.HasMore && p.Cursor != "" {
		return fmt.Errorf("triage_snapshot_response.cursor must be omitted when not has_more")
	}
	return validateTriageText("triage_snapshot_response.cursor", p.Cursor, 256)
}

func (p *TriageDecidePayload) validate() error {
	if err := validateCorrelationID("triage_decide.request_id", p.RequestID); err != nil {
		return err
	}
	if p.ItemID == "" {
		return fmt.Errorf("triage_decide.item_id is required")
	}
	if err := validateTriageText("triage_decide.item_id", p.ItemID, 1024); err != nil {
		return err
	}
	if err := enumRequired("triage_decide.op", p.Op, "acquire", "dismiss"); err != nil {
		return err
	}
	if p.Op == "acquire" && len(p.WatchScope) != 0 {
		return fmt.Errorf("triage_decide.watch_scope is only valid for dismiss")
	}
	if p.Op == "dismiss" {
		if len(p.WatchScope) == 0 {
			return fmt.Errorf("triage_decide.watch_scope is required for dismiss")
		}
		return validateTriageWatchScope(p.WatchScope)
	}
	return nil
}

func validateTriageWatchScope(raw json.RawMessage) error {
	var all string
	if err := strictDecode(raw, &all); err == nil {
		if all != "all" {
			return fmt.Errorf("triage_decide.watch_scope must be all or watch IDs")
		}
		return nil
	}
	var ids []int64
	if err := strictDecode(raw, &ids); err != nil || len(ids) == 0 || len(ids) > 100 {
		return fmt.Errorf("triage_decide.watch_scope must be all or 1 to 100 watch IDs")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || id > MaxBrowserInteger || seen[id] {
			return fmt.Errorf("triage_decide.watch_scope contains an invalid watch ID")
		}
		seen[id] = true
	}
	return nil
}

func (p *TriageDecideResultPayload) validate(what string) error {
	if err := validateCorrelationID(what+".request_id", p.RequestID); err != nil {
		return err
	}
	if err := enumRequired(what+".outcome", p.Outcome, "applied", "already_applied", "conflict", "error"); err != nil {
		return err
	}
	return validateTriageText(what+".detail", p.Detail, 1000)
}

func (p *HumanActionResolvePayload) validate() error {
	if err := validateCorrelationID("human_action_resolve.request_id", p.RequestID); err != nil {
		return err
	}
	if p.ActionID <= 0 || p.ActionID > MaxBrowserInteger || p.ExpectedRevision <= 0 || p.ExpectedRevision > MaxBrowserInteger {
		return fmt.Errorf("human_action_resolve.action_id and expected_revision must be positive browser integers")
	}
	if err := enumRequired("human_action_resolve.verdict", p.Verdict, "accept", "reject", "dismiss"); err != nil {
		return err
	}
	if p.Verdict == "accept" && !sha256RE.MatchString(p.ExpectedSHA256) {
		return fmt.Errorf("human_action_resolve.expected_sha256 is required for accept")
	}
	if p.ExpectedSHA256 != "" && !sha256RE.MatchString(p.ExpectedSHA256) {
		return fmt.Errorf("human_action_resolve.expected_sha256 must be a lowercase SHA-256")
	}
	return nil
}

func (p *HumanActionResolveResultPayload) validate() error {
	return (&TriageDecideResultPayload{RequestID: p.RequestID, Outcome: p.Outcome, Detail: p.Detail}).validate("human_action_resolve_result")
}

func (p *DeliveryReconcilePayload) validate() error {
	if err := validateCorrelationID("delivery_reconcile_request.request_id", p.RequestID); err != nil {
		return err
	}
	if !requestIDRE.MatchString(p.JobID) {
		return fmt.Errorf("delivery_reconcile_request.job_id is invalid")
	}
	if err := enumRequired("delivery_reconcile_request.operation", p.Operation,
		"confirm_request_exists", "confirm_request_absent"); err != nil {
		return err
	}
	if p.Operation == "confirm_request_exists" {
		if err := validateTriageText("delivery_reconcile_request.provider_reference", p.ProviderReference, 300); err != nil {
			return err
		}
		if p.ProviderReference == "" {
			return fmt.Errorf("delivery_reconcile_request.provider_reference is required for confirm_request_exists")
		}
	} else if p.ProviderReference != "" {
		return fmt.Errorf("delivery_reconcile_request.provider_reference is only valid for confirm_request_exists")
	}
	return nil
}

func (p *DeliveryReconcileResultPayload) validate() error {
	return (&TriageDecideResultPayload{RequestID: p.RequestID, Outcome: p.Outcome, Detail: p.Detail}).validate("delivery_reconcile_result")
}
func (p *HandoffLinkRequestPayload) validate() error {
	if !requestIDRE.MatchString(p.JobID) {
		return fmt.Errorf("handoff_link_request.job_id is invalid")
	}
	if p.RequestID != "" {
		return validateCorrelationID("handoff_link_request.request_id", p.RequestID)
	}
	return nil
}

func (p *HandoffLinkResultPayload) validate() error {
	if p.RequestID != "" {
		if err := validateCorrelationID("handoff_link_result.request_id", p.RequestID); err != nil {
			return err
		}
	}
	if err := enumRequired("handoff_link_result.outcome", p.Outcome,
		"opened", "job_gone", "not_open_action", "not_openurl", "unavailable"); err != nil {
		return err
	}
	if p.Outcome == "opened" {
		if p.Detail != "" {
			return fmt.Errorf("handoff_link_result.opened must not carry detail")
		}
		return validateTriageURL("handoff_link_result.url", p.URL, "https")
	}
	if p.URL != "" {
		return fmt.Errorf("handoff_link_result.%s must not carry url", p.Outcome)
	}
	if err := validateTriageText("handoff_link_result.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Detail == "" {
		return fmt.Errorf("handoff_link_result.%s requires detail", p.Outcome)
	}
	return nil
}

func (p *ReviewPreviewRequestPayload) validate() error {
	if err := validateCorrelationID("review_preview_request.request_id", p.RequestID); err != nil {
		return err
	}
	if p.ActionID <= 0 || p.ActionID > MaxBrowserInteger {
		return fmt.Errorf("review_preview_request.action_id must be a positive browser integer")
	}
	return nil
}

func validateTriageURL(what, value, scheme string) error {
	if err := validateTriageText(what, value, 4000); err != nil {
		return err
	}
	if !rfc3986URITextRE.MatchString(value) {
		return fmt.Errorf("%s must be an RFC 3986 URI", what)
	}
	u, err := url.ParseRequestURI(value)
	if err != nil || u.Scheme != scheme || u.Host == "" {
		return fmt.Errorf("%s must be a %s URL", what, scheme)
	}
	return nil
}

func (p *ReviewPreviewResultPayload) validate() error {
	if err := validateCorrelationID("review_preview_result.request_id", p.RequestID); err != nil {
		return err
	}
	if err := enumRequired("review_preview_result.outcome", p.Outcome, "ok", "error"); err != nil {
		return err
	}
	if err := validateTriageText("review_preview_result.detail", p.Detail, 1000); err != nil {
		return err
	}
	if p.Outcome == "error" {
		if p.URL != "" || p.SHA256 != "" || p.SizeBytes != 0 || p.ExpiresAt != "" {
			return fmt.Errorf("review_preview_result: error outcome must not carry capability fields")
		}
		return nil
	}
	if p.Detail != "" {
		return fmt.Errorf("review_preview_result: ok outcome must not carry a detail")
	}
	if err := validateTriageURL("review_preview_result.url", p.URL, "http"); err != nil {
		return err
	}
	u, _ := url.ParseRequestURI(p.URL)
	if u.Hostname() != "127.0.0.1" || u.Port() == "" || !strings.HasPrefix(u.Path, "/p/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("review_preview_result.url must be a loopback capability URL")
	}
	if !sha256RE.MatchString(p.SHA256) {
		return fmt.Errorf("review_preview_result.sha256 must be a lowercase SHA-256")
	}
	if p.SizeBytes < 0 || p.SizeBytes > MaxBrowserInteger {
		return fmt.Errorf("review_preview_result.size_bytes must be in range 0..%d", MaxBrowserInteger)
	}
	return validateTriageTime("review_preview_result.expires_at", p.ExpiresAt)
}

func (p *StatsResponsePayload) validate() error {
	if err := validateCorrelationID("stats_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateTriageTime("stats_response.generated_at", p.GeneratedAt); err != nil {
		return err
	}
	counts := []int64{
		p.AcquiredTotal, p.FailedTotal, p.HandoffsRequired,
		p.Access.OpenAccess, p.Access.Institutional, p.Access.LicensedAPI, p.Access.Other,
	}
	for _, value := range counts {
		if value < 0 || value > MaxBrowserInteger {
			return fmt.Errorf("stats_response counts must be in range 0..%d", MaxBrowserInteger)
		}
	}
	if len(p.Series) > 60 {
		return fmt.Errorf("stats_response.series capped at 60 buckets")
	}
	for _, bucket := range p.Series {
		if err := validateTriageTime("stats_response.series.period_start", bucket.PeriodStart); err != nil {
			return err
		}
		if bucket.Acquired < 0 || bucket.Acquired > MaxBrowserInteger {
			return fmt.Errorf("stats_response.series.acquired must be in range 0..%d", MaxBrowserInteger)
		}
	}
	return nil
}

func (p *ActivityRequestPayload) validate() error {
	if err := validateCorrelationID("activity_request.request_id", p.RequestID); err != nil {
		return err
	}
	if p.Limit < 1 || p.Limit > 50 {
		return fmt.Errorf("activity_request.limit must be in range 1..50")
	}
	return nil
}

func (p *ActivityResponsePayload) validate() error {
	if err := validateCorrelationID("activity_response.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateTriageTime("activity_response.generated_at", p.GeneratedAt); err != nil {
		return err
	}
	if len(p.Entries) > 50 {
		return fmt.Errorf("activity_response.entries capped at 50")
	}
	for _, entry := range p.Entries {
		if entry.Seq < 0 || entry.Seq > MaxBrowserInteger {
			return fmt.Errorf("activity_response.entry.seq must be in range 0..%d", MaxBrowserInteger)
		}
		if err := validateTriageTime("activity_response.entry.at", entry.At); err != nil {
			return err
		}
		if entry.JobID != "" && !requestIDRE.MatchString(entry.JobID) {
			return fmt.Errorf("activity_response.entry.job_id is invalid")
		}
		if err := validateTriageText("activity_response.entry.kind", entry.Kind, 100); err != nil {
			return err
		}
		if entry.Kind == "" {
			return fmt.Errorf("activity_response.entry.kind is required")
		}
		if err := validateTriageText("activity_response.entry.text", entry.Text, 160); err != nil {
			return err
		}
		if err := validateTriageText("activity_response.entry.title", entry.Title, 500); err != nil {
			return err
		}
	}
	return nil
}

func (p *PageBulkStatusRequestPayload) validate() error {
	if err := validateCorrelationID("page_bulk_status_request.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("page_bulk_status_request.scan_id", p.ScanID); err != nil {
		return err
	}
	if len(p.Identifiers) == 0 || len(p.Identifiers) > 200 {
		return fmt.Errorf("page_bulk_status_request.identifiers must contain 1 to 200 entries")
	}
	seen := make(map[string]bool, len(p.Identifiers))
	for _, id := range p.Identifiers {
		if err := validatePageBulkLocalID("page_bulk_status_request.identifiers", id.LocalID, seen); err != nil {
			return err
		}
		if err := enumRequired("page_bulk_status_request.identifiers.kind", id.Kind, "doi", "pmid", "arxiv", "openalex"); err != nil {
			return err
		}
		if id.Value == "" {
			return fmt.Errorf("page_bulk_status_request.identifiers.value is required")
		}
		if err := validateTriageText("page_bulk_status_request.identifiers.value", id.Value, 512); err != nil {
			return err
		}
	}
	if p.RenderedRecordCountHint != nil && (*p.RenderedRecordCountHint < 0 || *p.RenderedRecordCountHint > MaxBrowserInteger) {
		return fmt.Errorf("page_bulk_status_request.rendered_record_count_hint must be in range 0..%d", MaxBrowserInteger)
	}
	return nil
}

// validatePageBulkLocalID enforces the shared local_id rule used by both
// page_bulk_status_request.identifiers and page_bulk_status_result.items:
// non-empty, bounded, NUL-free, and unique within its own array. seen is
// mutated in place so the caller shares one dedup set across the whole
// array without allocating per entry.
func validatePageBulkLocalID(what, localID string, seen map[string]bool) error {
	if localID == "" {
		return fmt.Errorf("%s.local_id is required", what)
	}
	if err := validateTriageText(what+".local_id", localID, 128); err != nil {
		return err
	}
	if seen[localID] {
		return fmt.Errorf("%s.local_id %q is duplicated", what, localID)
	}
	seen[localID] = true
	return nil
}

func (p *PageBulkStatusResultPayload) validate() error {
	if err := validateCorrelationID("page_bulk_status_result.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("page_bulk_status_result.scan_id", p.ScanID); err != nil {
		return err
	}
	if len(p.Items) > 200 {
		return fmt.Errorf("page_bulk_status_result.items capped at 200")
	}
	seen := make(map[string]bool, len(p.Items))
	for _, item := range p.Items {
		if err := validatePageBulkLocalID("page_bulk_status_result.items", item.LocalID, seen); err != nil {
			return err
		}
		if err := enumRequired("page_bulk_status_result.items.status", item.Status,
			"eligible", "owned_with_pdf", "owned_missing_pdf", "queued",
			"previously_unavailable", "ownership_incomplete", "ownership_unknown",
			"invalid", "frame_too_large"); err != nil {
			return err
		}
		// An identifier that never resolved, or a result refused because it
		// could not fit the response, has no canonical work identity to report.
		// Every other status carries one (Decision 7).
		if item.Status == "invalid" || item.Status == "frame_too_large" {
			if item.CanonicalKey != "" {
				return fmt.Errorf("page_bulk_status_result.items.canonical_key must be omitted for %s", item.Status)
			}
		} else {
			if item.CanonicalKey == "" {
				return fmt.Errorf("page_bulk_status_result.items.canonical_key is required")
			}
			if err := validateTriageText("page_bulk_status_result.items.canonical_key", item.CanonicalKey, 300); err != nil {
				return err
			}
		}
		if item.JobID != "" {
			if item.Status != "queued" {
				return fmt.Errorf("page_bulk_status_result.items.job_id is only valid for queued")
			}
			if !requestIDRE.MatchString(item.JobID) {
				return fmt.Errorf("page_bulk_status_result.items.job_id is invalid")
			}
		}
		if item.ZotioItemKey != "" {
			if item.Status != "owned_missing_pdf" {
				return fmt.Errorf("page_bulk_status_result.items.zotio_item_key is only valid for owned_missing_pdf")
			}
			if !zoteroKeyRE.MatchString(item.ZotioItemKey) {
				return fmt.Errorf("page_bulk_status_result.items.zotio_item_key is invalid")
			}
		}
	}
	return nil
}

func (p *PageBulkSubmitRequestPayload) validate() error {
	if err := validateCorrelationID("page_bulk_submit_request.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("page_bulk_submit_request.scan_id", p.ScanID); err != nil {
		return err
	}
	if len(p.CanonicalKeys) == 0 || len(p.CanonicalKeys) > 50 {
		return fmt.Errorf("page_bulk_submit_request.canonical_keys must contain 1 to 50 entries")
	}
	seen := make(map[string]bool, len(p.CanonicalKeys))
	for _, key := range p.CanonicalKeys {
		if key == "" {
			return fmt.Errorf("page_bulk_submit_request.canonical_keys entries must be non-empty")
		}
		if err := validateTriageText("page_bulk_submit_request.canonical_keys", key, 300); err != nil {
			return err
		}
		if seen[key] {
			return fmt.Errorf("page_bulk_submit_request.canonical_keys contains a duplicate %q", key)
		}
		seen[key] = true
	}
	return p.Source.validate()
}

// validate enforces Decision 6's manifest shape: kind is pinned to the one
// value this protocol emits, origin is a bare https scheme+host with a
// lowercase host, and detector is a short, non-empty label.
//
// Origin reuses validateBareLowercaseOrigin — the same validator
// session_evidence.origin_hint enforces (papio-26fa531528e29798) — rather
// than a third copy of the rule. This field used to call the laxer
// validResolverOrigin (still used by hello_ack.resolver_origins, which has
// no case-sensitive counterpart to disagree with), which never compared
// host case: "https://Scholar.Example.EDU" decoded here while
// extension/src/protocol.ts's round-trip check and
// protocol/browser-v1.schema.json's lowercase-only pattern both rejected
// it — the exact class of bug validateBareLowercaseOrigin was written to
// close for origin_hint.
func (s *PageBulkSubmitSource) validate() error {
	if err := enumRequired("page_bulk_submit_request.source.kind", s.Kind, "browser_page"); err != nil {
		return err
	}
	if browserTextLen(s.Origin) > 300 {
		return fmt.Errorf("page_bulk_submit_request.source.origin exceeds 300 chars")
	}
	if err := validateBareLowercaseOrigin("page_bulk_submit_request.source.origin", s.Origin); err != nil {
		return err
	}
	if s.Detector == "" {
		return fmt.Errorf("page_bulk_submit_request.source.detector is required")
	}
	return validateTriageText("page_bulk_submit_request.source.detector", s.Detector, 128)
}

func (p *PageBulkSubmitResultPayload) validate() error {
	if err := validateCorrelationID("page_bulk_submit_result.request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("page_bulk_submit_result.scan_id", p.ScanID); err != nil {
		return err
	}
	for _, count := range []int64{p.Submitted, p.Joined, p.AlreadyOwned, p.Invalid} {
		if count < 0 || count > MaxBrowserInteger {
			return fmt.Errorf("page_bulk_submit_result counts must be in range 0..%d", MaxBrowserInteger)
		}
	}
	if !requestIDRE.MatchString(p.BatchID) {
		return fmt.Errorf("page_bulk_submit_result.batch_id is invalid")
	}
	return nil
}
