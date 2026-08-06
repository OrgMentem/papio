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
	"strings"
	"time"
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
	// value is knowing which entitlement obtained the work. The digest form is
	// accepted so an opaque reference stays expressible.
	entitlementRefRE = regexp.MustCompile(`^entitlement:(source:[a-z0-9_]{1,64}|sha256:[0-9a-f]{64})$`)
	msgIDRE          = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	zoteroKeyRE      = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
	doiRE            = regexp.MustCompile(`^10\.[0-9]{4,9}/\S{1,200}$`)
	pmidRE           = regexp.MustCompile(`^[0-9]{1,10}$`)
	arxivRE          = regexp.MustCompile(`^([0-9]{4}\.[0-9]{4,5})(v[0-9]+)?$|^[a-z-]+(\.[A-Z]{2})?/[0-9]{7}$`)
	isbnRE           = regexp.MustCompile(`^[0-9Xx-]{10,17}$`)
	openalexRE       = regexp.MustCompile(`^W[0-9]{4,12}$`)
	sha256RE         = regexp.MustCompile(`^[a-f0-9]{64}$`)
	provenanceRE     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hostRE           = regexp.MustCompile(`^[a-z0-9.-]{3,253}$`)
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

// validateResolverOriginHint enforces session_evidence.origin_hint's exact
// wire rule: an https origin with a lowercase, structurally valid host and
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
func validateResolverOriginHint(hint string) error {
	if hint == "" {
		return fmt.Errorf("session_evidence.origin_hint required")
	}
	if strings.ContainsAny(hint, "?#") {
		return fmt.Errorf("session_evidence.origin_hint %q must not retain URL query or fragment data", hint)
	}
	if !strings.HasPrefix(hint, "https://") {
		return fmt.Errorf("session_evidence.origin_hint %q must be an https URL with a host", hint)
	}
	u, err := url.Parse(hint)
	if err != nil {
		return fmt.Errorf("invalid session_evidence.origin_hint %q", hint)
	}
	if u.Host == "" {
		return fmt.Errorf("session_evidence.origin_hint %q must be an https URL with a host", hint)
	}
	if u.User != nil {
		return fmt.Errorf("session_evidence.origin_hint %q must not retain URL credentials", hint)
	}
	if u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return fmt.Errorf("session_evidence.origin_hint %q must be a bare origin with no path", hint)
	}
	if !originHostRE.MatchString(u.Hostname()) {
		return fmt.Errorf("session_evidence.origin_hint %q must have a lowercase resolver host", hint)
	}
	// u.Hostname() silently drops the port; validate it explicitly rather
	// than letting a malformed or oversized one pass unseen.
	if port := strings.TrimPrefix(u.Host, u.Hostname()); port != "" {
		if !originPortRE.MatchString(strings.TrimPrefix(port, ":")) {
			return fmt.Errorf("session_evidence.origin_hint %q must have a 1-5 digit port", hint)
		}
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
	MsgHello                    = "hello"
	MsgHelloAck                 = "hello_ack"
	MsgPageAcquire              = "page_acquire"
	MsgPageAcquireAck           = "page_acquire_ack"
	MsgPageCapture              = "page_capture"
	MsgPageCaptureRequest       = "page_capture_request"
	MsgPageCaptureRequestResult = "page_capture_request_result"
	MsgJobOffer                 = "job_offer"
	MsgHandoffOutcome           = "handoff_outcome"
	MsgJobAccept                = "job_accept"
	MsgJobReject                = "job_reject"
	MsgAuthPending              = "auth_pending"
	MsgAuthReturned             = "auth_returned"
	MsgSessionEvidence          = "session_evidence"
	MsgDownloadStarted          = "download_started"
	MsgDownloadComplete         = "download_complete"
	MsgDeliveryContext          = "delivery_context"
	MsgProviderOutcome          = "provider_outcome"
	MsgCancel                   = "cancel"
	MsgHandoffFocus             = "handoff_focus"
	MsgAck                      = "ack"
	MsgError                    = "error"
	MsgTriageSnapshotRequest    = "triage_snapshot_request"
	MsgTriageSnapshotResponse   = "triage_snapshot_response"
	MsgTriageCountsRequest      = "triage_counts_request"
	MsgTriageCountsResponse     = "triage_counts_response"
	MsgTriageDecide             = "triage_decide"
	MsgTriageDecideResult       = "triage_decide_result"
	MsgHumanActionResolve       = "human_action_resolve"
	MsgHumanActionResolveResult = "human_action_resolve_result"
	MsgReviewPreviewRequest     = "review_preview_request"
	MsgReviewPreviewResult      = "review_preview_result"
	MsgStatsRequest             = "stats_request"
	MsgStatsResponse            = "stats_response"
	MsgActivityRequest          = "activity_request"
	MsgActivityResponse         = "activity_response"
)

// jobScoped lists the types that must carry a job_id.
var jobScoped = map[string]bool{
	MsgJobOffer: true, MsgJobAccept: true, MsgJobReject: true, MsgHandoffOutcome: true,
	MsgAuthPending: true, MsgAuthReturned: true,
	MsgDownloadStarted: true, MsgDownloadComplete: true, MsgDeliveryContext: true,
	MsgProviderOutcome: true, MsgCancel: true, MsgHandoffFocus: true,
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

// JobOfferPayload asks the extension to open one OpenURL-resolved job.
type JobOfferPayload struct {
	OpenURL           string            `json:"openurl"`
	ProviderHosts     []string          `json:"provider_hosts"`
	Expected          *JobOfferExpected `json:"expected,omitempty"`
	AccessMode        string            `json:"access_mode"`
	LoginEntityID     string            `json:"login_entity_id,omitempty"`
	ProquestAccountID string            `json:"proquest_account_id,omitempty"`
	RequiresAuth      bool              `json:"requires_auth,omitempty"`
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
	AdapterVersion string `json:"adapter_version,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// ErrorPayload is a normalized bridge error.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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

	DOI       string `json:"doi,omitempty"`
	Nature    string `json:"nature,omitempty"`
	NoticedAt string `json:"noticed_at,omitempty"`
	NoticeDOI string `json:"notice_doi,omitempty"`
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
			if _, ok := payloadFields["adapter_id"]; ok && p.AdapterID == "" {
				err = fmt.Errorf("page_capture.adapter_id must use the id charset (max 64)")
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
		if err = browserRequireFields(payloadFields, "openurl", "provider_hosts", "access_mode", "expires_at"); err == nil {
			err = browserRejectNullFields(payloadFields, "expected", "login_entity_id", "proquest_account_id", "requires_auth")
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
	if err := enumRequired("access_mode", p.AccessMode, "assisted", "delegated"); err != nil {
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
	if len(p.SchemaVersions) != 1 || (p.SchemaVersions[0] != 1 && p.SchemaVersions[0] != 2) {
		return fmt.Errorf("triage_snapshot_request.schema_versions must be [1] or [2]")
	}
	if p.Limit != 0 && (p.Limit < 1 || p.Limit > 100) {
		return fmt.Errorf("triage_snapshot_request.limit must be between 1 and 100")
	}
	return validateTriageText("triage_snapshot_request.cursor", p.Cursor, 256)
}

func (counts TriageCounts) validate() error {
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
	if counts.PendingTotal != counts.WatchHits+counts.Actions+counts.Retractions {
		return fmt.Errorf("triage pending_total must equal watch_hits + actions + retractions")
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

		Work        *TriageWork   `json:"work"`
		Abstract    string        `json:"abstract"`
		Watches     []TriageWatch `json:"watches"`
		FirstSeenAt string        `json:"first_seen_at"`

		ActionID     int64  `json:"action_id"`
		JobID        string `json:"job_id"`
		ActionKind   string `json:"action_kind"`
		JobState     string `json:"job_state"`
		Revision     int64  `json:"revision"`
		SHA256       string `json:"sha256"`
		SizeBytes    int64  `json:"size_bytes"`
		RequiresAuth *bool  `json:"requires_auth"`
		BlockedBy    string `json:"blocked_by"`

		DOI       string `json:"doi"`
		Nature    string `json:"nature"`
		NoticedAt string `json:"noticed_at"`
		NoticeDOI string `json:"notice_doi"`
	}
	if err := strictDecode(data, &wire); err != nil {
		return err
	}
	core := []string{"kind", "id", "rank", "title", "facts", "links", "ops"}
	allowed := append([]string(nil), core...)
	switch wire.Kind {
	case "watch_hit":
		allowed = append(allowed, "work", "abstract", "watches", "first_seen_at")
		if err := browserRequireFields(fields, append(core, "work", "abstract", "watches", "first_seen_at")...); err != nil {
			return err
		}
	case "human_action":
		allowed = append(allowed, "action_id", "job_id", "action_kind", "job_state", "revision", "sha256", "size_bytes",
			"requires_auth", "blocked_by")
		if err := browserRequireFields(fields, append(core, "action_id", "job_id", "action_kind", "job_state", "revision", "sha256", "size_bytes")...); err != nil {
			return err
		}
		if err := browserRejectNullFields(fields, "requires_auth", "blocked_by"); err != nil {
			return err
		}
		if _, ok := fields["blocked_by"]; ok {
			if err := enumRequired("human_action.blocked_by", wire.BlockedBy, "anti_bot", "paywall", "landing_page"); err != nil {
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
		Work: wire.Work, Abstract: wire.Abstract, Watches: wire.Watches, FirstSeenAt: wire.FirstSeenAt,
		ActionID: wire.ActionID, JobID: wire.JobID, ActionKind: wire.ActionKind, JobState: wire.JobState,
		Revision: wire.Revision, SHA256: wire.SHA256, SizeBytes: wire.SizeBytes,
		RequiresAuth: wire.RequiresAuth, BlockedBy: wire.BlockedBy,
		DOI: wire.DOI, Nature: wire.Nature, NoticedAt: wire.NoticedAt, NoticeDOI: wire.NoticeDOI,
	}
	return item.validate()
}

func (item TriageSnapshotItem) MarshalJSON() ([]byte, error) {
	core := map[string]any{
		"kind": item.Kind, "id": item.ID, "rank": item.Rank, "title": item.Title,
		"facts": item.Facts, "links": item.Links, "ops": item.Ops,
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
	case "retraction":
		core["doi"], core["nature"], core["noticed_at"] = item.DOI, item.Nature, item.NoticedAt
		if item.NoticeDOI != "" {
			core["notice_doi"] = item.NoticeDOI
		}
	}
	return json.Marshal(core)
}

func (item TriageSnapshotItem) validate() error {
	if err := enumRequired("triage item kind", item.Kind, "watch_hit", "human_action", "retraction"); err != nil {
		return err
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
		if err := enumRequired("triage item op", op, "acquire", "dismiss", "accept", "reject", "open", "retry"); err != nil {
			return err
		}
		if seenOps[op] {
			return fmt.Errorf("triage item ops cannot repeat %q", op)
		}
		seenOps[op] = true
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
			if err := enumRequired("human_action.blocked_by", item.BlockedBy, "anti_bot", "paywall", "landing_page"); err != nil {
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
	if p.Schema != 1 && p.Schema != 2 {
		return fmt.Errorf("triage_snapshot_response.schema must be 1 or 2")
	}
	if err := validateTriageTime("triage_snapshot_response.generated_at", p.GeneratedAt); err != nil {
		return err
	}
	if err := p.Counts.validate(); err != nil {
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
		if p.Schema == 1 && (item.RequiresAuth != nil || item.BlockedBy != "") {
			return fmt.Errorf("triage_snapshot_response.schema 1 cannot include access classification")
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
