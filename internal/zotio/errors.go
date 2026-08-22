// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"papio/internal/job"
)

// Error classes are a deliberately small, stable vocabulary for failures at
// the Zotio boundary. They are safe to persist and show to a local CLI user.
const (
	ErrorClassZoteroHTTP4xx            = "zotero_http_4xx"
	ErrorClassZoteroFileStorageRefused = "zotero_file_storage_refused"
	ErrorClassZoteroStorageQuota       = "zotero_storage_quota_exceeded"
	ErrorClassZoteroFieldValidation    = "zotero_field_validation"
	ErrorClassMirrorSyncFailed         = "mirror_sync_failed"
	ErrorClassZotioExecTimeout         = "zotio_exec_timeout"
	ErrorClassZotioCanceled            = "zotio_canceled"
	ErrorClassZotioNotConfigured       = "zotio_not_configured"
	ErrorClassPlanConfirmationMismatch = "plan_confirmation_mismatch"
	ErrorClassReservationConflict      = "reservation_conflict"
	ErrorClassLocalDBLocked            = "local_db_locked"
	ErrorClassNetwork                  = "network"
	ErrorClassBundleValidation         = "bundle_validation"
	ErrorClassRoutingRequiresDOI       = "routing_requires_doi"
	ErrorClassAlreadyInLibrary         = "already_in_library"
	ErrorClassUnknown                  = "unknown"
)

const maxClassificationBytes = 8 << 10
const maxErrorHintBytes = 120

// ErrorInfo is the redacted error metadata that may cross the Zotio boundary.
// Hint is a fixed, sanitized diagnostic rather than copied upstream text.
type ErrorInfo struct {
	Class      string
	Hint       string
	HTTPStatus int
}

// ClassifiedError preserves the original error chain while exposing only safe
// metadata to callers that persist or render it.
type ClassifiedError struct {
	cause error
	info  ErrorInfo
}

func (e *ClassifiedError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *ClassifiedError) Unwrap() error { return e.cause }

// ErrorClass, ErrorHint, and ErrorHTTPStatus intentionally form a tiny
// structural interface so the application layer need not import Zotio.
func (e *ClassifiedError) ErrorClass() string {
	if e == nil {
		return ErrorClassUnknown
	}
	return e.info.Class
}

func (e *ClassifiedError) ErrorHint() string {
	if e == nil {
		return ""
	}
	return e.info.Hint
}

func (e *ClassifiedError) ErrorHTTPStatus() int {
	if e == nil {
		return 0
	}
	return e.info.HTTPStatus
}

// WithErrorInfo classifies err once, preserving an earlier classification that
// may have inspected a Zotio mutation envelope.
func WithErrorInfo(err error, envelopes ...json.RawMessage) error {
	if err == nil {
		return nil
	}
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return err
	}
	return &ClassifiedError{cause: err, info: ClassifyError(err, envelopes...)}
}

// ErrorInfoFrom returns a precomputed classification when available, otherwise
// it performs a bounded classification from the error chain alone.
func ErrorInfoFrom(err error) ErrorInfo {
	if err == nil {
		return ErrorInfo{Class: ErrorClassUnknown}
	}
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return classified.info
	}
	return ClassifyError(err)
}

// ClassifyError maps Zotio CLI and apply failures to stable, non-sensitive
// categories. It examines at most 8 KiB of error/envelope text and never
// returns raw text as a hint.
func ClassifyError(err error, envelopes ...json.RawMessage) ErrorInfo {
	text := classificationText(err, envelopes...)
	lower := strings.ToLower(text)

	if status := zoteroHTTP4xxStatus(text, envelopes...); status != 0 {
		if status == 413 {
			// HTTP 413 reads as "Payload Too Large", but Zotero returns it for a
			// full storage plan too, and the size reading is the wrong one: a
			// 3,040,464-byte upload succeeded while 428,128-byte ones failed
			// after the plan filled. Zotero states which it is in the response
			// body ("File would exceed quota (300.4 > 300)"), so classify from
			// that text rather than re-deriving a cause papio cannot observe.
			// Papio discarded this sentence for five days and reported an
			// anonymous 4xx while every upload failed.
			if quota := zoteroQuotaHint(text); quota != "" {
				return safeErrorInfo(ErrorClassZoteroStorageQuota, quota, status)
			}
			return safeErrorInfo(ErrorClassZoteroFileStorageRefused, "Zotero file storage refused upload (HTTP 413)", status)
		}
		return safeErrorInfo(ErrorClassZoteroHTTP4xx, "Zotero HTTP "+strconv.Itoa(status), status)
	}
	// zotio refuses a stored upload with a structured precondition rather than
	// an HTTP status when the library keeps its files on WebDAV: the Web API
	// would bill the bytes to Zotero's own plan, and Zotero's connector cannot
	// attach to an item that already exists, so the upload has no local route.
	// That refusal carries no HTTP status, so the 413 branch above never sees
	// it and it used to fall through to `unknown` with a hint cut mid-word,
	// leaving the operator no explanation and doctor no check for six papers.
	if strings.Contains(lower, "precondition_unmet") && strings.Contains(lower, "zotero_file_storage") {
		return safeErrorInfo(ErrorClassZoteroFileStorageRefused, "Zotero refused a stored upload: library files live on your own file store", 0)
	}
	if strings.Contains(lower, "unknown item field") {
		return safeErrorInfo(ErrorClassZoteroFieldValidation, "unknown item field", 0)
	}
	if errors.Is(err, context.Canceled) || strings.Contains(lower, "zotio command canceled") {
		return safeErrorInfo(ErrorClassZotioCanceled, "Zotio command canceled", 0)
	}
	if strings.Contains(lower, "zotio command timed out") || errors.Is(err, context.DeadlineExceeded) {
		return safeErrorInfo(ErrorClassZotioExecTimeout, "Zotio command timed out", 0)
	}
	if strings.Contains(lower, "zotio executable is not configured") ||
		strings.Contains(lower, "zotio integration is not configured") ||
		strings.Contains(lower, "zotio plan/apply integration is not configured") {
		return safeErrorInfo(ErrorClassZotioNotConfigured, "Zotio is not configured", 0)
	}
	if strings.Contains(lower, "confirmation sha-256 does not match") || strings.Contains(lower, "plan confirmation digest mismatch") {
		return safeErrorInfo(ErrorClassPlanConfirmationMismatch, "plan confirmation does not match", 0)
	}
	if strings.Contains(lower, "apply reservation was not finalized") || errors.Is(err, job.ErrConflict) {
		return safeErrorInfo(ErrorClassReservationConflict, "Zotio apply reservation conflict", 0)
	}
	if strings.Contains(lower, "database is locked") {
		return safeErrorInfo(ErrorClassLocalDBLocked, "local database is locked", 0)
	}
	if strings.Contains(lower, "zotio sync") || strings.Contains(lower, "syncing zotio library") ||
		strings.Contains(lower, "mirror sync") {
		return safeErrorInfo(ErrorClassMirrorSyncFailed, "Zotio mirror sync failed", 0)
	}
	if isNetworkError(err, lower) {
		return safeErrorInfo(ErrorClassNetwork, "network connection failed", 0)
	}
	if strings.Contains(lower, "bundle validation:") {
		return safeErrorInfo(ErrorClassBundleValidation, bundleValidationHint(lower), 0)
	}
	if strings.Contains(lower, "routing requires") {
		return safeErrorInfo(ErrorClassRoutingRequiresDOI, newItemRoutingRefusal, 0)
	}
	if strings.Contains(lower, "classification=") && strings.Contains(lower, "duplicate") {
		return safeErrorInfo(ErrorClassAlreadyInLibrary, "paper already in Zotero library", 0)
	}
	return safeErrorInfo(ErrorClassUnknown, SanitizeErrorHint(text), 0)
}

func bundleValidationHint(lower string) string {
	switch {
	case strings.Contains(lower, "identity has no citation title"):
		return "no citation record for this paper: verify the DOI or supply title and authors"
	case strings.Contains(lower, "identity has no citation authors"):
		return "no citation authors for this paper: verify the DOI or supply title and authors"
	case strings.Contains(lower, "identity.title length out of range"):
		return "citation title out of range"
	case strings.Contains(lower, "identity.authors must have"):
		return "citation authors missing or out of range"
	default:
		return "bundle validation failed"
	}
}

// zoteroQuotaHintRE captures the figures Zotero reports when a library's
// storage plan is full, e.g. "File would exceed quota (300.4 > 300)". The
// units are megabytes and Zotero omits them.
//
// The separator is matched in four encodings because the classifier reads raw
// envelope JSON, and the bare ">" is the form least likely to appear. zotio
// HTML-escapes it to "&gt;" in its reason text, and Go's encoding/json then
// escapes the "&" again, so the operator's ledger actually holds
// "\u0026gt;" — the doubly-escaped form, verified against a real row. Go
// alone produces "\u003e". Matching only ">" dropped the figures silently and
// left a hint with no numbers in it, and matching only the single escape still
// missed every real row.
var zoteroQuotaHintRE = regexp.MustCompile(`(?i)quota\s*\(\s*([0-9]+(?:\.[0-9]+)?)\s*(?:>|&gt;|\\u003e|\\u0026gt;)\s*([0-9]+(?:\.[0-9]+)?)\s*\)`)

// zoteroQuotaHint reports a hint naming the storage plan as full, with
// Zotero's own figures when it stated them. It returns "" when the text is a
// 413 for some other reason, so the caller keeps the weaker class rather than
// asserting a quota it did not observe.
func zoteroQuotaHint(text string) string {
	if !strings.Contains(strings.ToLower(text), "quota") {
		return ""
	}
	if m := zoteroQuotaHintRE.FindStringSubmatch(text); m != nil {
		return "Zotero storage plan is full (" + m[1] + " of " + m[2] + " MB used)"
	}
	return "Zotero storage plan is full"
}

func safeErrorInfo(class, hint string, status int) ErrorInfo {
	return ErrorInfo{Class: class, Hint: SanitizeErrorHint(hint), HTTPStatus: status}
}

// IsErrorClass reports whether class is part of the stable Zotio boundary
// vocabulary.
func IsErrorClass(class string) bool {
	switch class {
	case ErrorClassZoteroHTTP4xx,
		ErrorClassZoteroFileStorageRefused,
		ErrorClassZoteroStorageQuota,
		ErrorClassZoteroFieldValidation,
		ErrorClassMirrorSyncFailed,
		ErrorClassZotioExecTimeout,
		ErrorClassZotioCanceled,
		ErrorClassZotioNotConfigured,
		ErrorClassPlanConfirmationMismatch,
		ErrorClassReservationConflict,
		ErrorClassLocalDBLocked,
		ErrorClassNetwork,
		ErrorClassBundleValidation,
		ErrorClassRoutingRequiresDOI,
		ErrorClassAlreadyInLibrary,
		ErrorClassUnknown:
		return true
	default:
		return false
	}
}

func classificationText(err error, envelopes ...json.RawMessage) string {
	var builder strings.Builder
	appendBounded := func(value string) {
		remaining := maxClassificationBytes - builder.Len()
		if remaining <= 0 || value == "" {
			return
		}
		if len(value) > remaining {
			value = value[:remaining]
		}
		builder.WriteString(value)
	}
	if err != nil {
		appendBounded(err.Error())
	}
	for _, envelope := range envelopes {
		if builder.Len() >= maxClassificationBytes {
			break
		}
		appendBounded(string(envelope))
	}
	return builder.String()
}

var (
	httpStatusRE      = regexp.MustCompile(`(?i)\b(?:http(?:[_ -]?status)?|status(?:[_ -]?code)?)\b\s*(?:is|was|=|:)?\s*(4[0-9]{2})\b`)
	jsonHTTPStatusRE  = regexp.MustCompile(`(?i)["'](?:http_status|status_code|status)["']\s*:\s*"?(4[0-9]{2})\b`)
	urlHintRE         = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s<>"']+|\bwww\.[^\s<>"']+`)
	posixPathHintRE   = regexp.MustCompile(`(?:^|\s)(?:~/|/(?:[^\s/]+/)+[^\s/]+)`)
	windowsPathHintRE = regexp.MustCompile(`(?i)\b[a-z]:\\(?:[^\s\\]+\\)*[^\s\\]+`)
)

func zoteroHTTP4xxStatus(text string, envelopes ...json.RawMessage) int {
	for _, matcher := range []*regexp.Regexp{httpStatusRE, jsonHTTPStatusRE} {
		match := matcher.FindStringSubmatch(text)
		if len(match) == 2 {
			status, _ := strconv.Atoi(match[1])
			if status >= 400 && status <= 499 {
				return status
			}
		}
	}
	for _, envelope := range envelopes {
		if status := jsonHTTPStatus(envelope); status >= 400 && status <= 499 {
			return status
		}
	}
	return 0
}

func jsonHTTPStatus(raw json.RawMessage) int {
	if len(raw) == 0 || len(raw) > maxClassificationBytes {
		return 0
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	var visit func(any) int
	visit = func(current any) int {
		switch v := current.(type) {
		case map[string]any:
			for _, key := range []string{"http_status", "status_code", "status"} {
				if status := numericHTTPStatus(v[key]); status != 0 {
					return status
				}
			}
			for _, child := range v {
				if status := visit(child); status != 0 {
					return status
				}
			}
		case []any:
			for _, child := range v {
				if status := visit(child); status != 0 {
					return status
				}
			}
		}
		return 0
	}
	return visit(value)
}

func numericHTTPStatus(value any) int {
	switch status := value.(type) {
	case float64:
		if status == float64(int(status)) {
			return int(status)
		}
	case string:
		parsed, _ := strconv.Atoi(status)
		return parsed
	case json.Number:
		parsed, _ := status.Int64()
		return int(parsed)
	}
	return 0
}

func isNetworkError(err error, lower string) bool {
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	for _, marker := range []string{
		"network", "connection refused", "connection reset", "connection closed", "connection aborted",
		"no such host", "dns", "tls handshake", "unexpected eof",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// SanitizeErrorHint removes URL and filesystem-path shaped text, bounds the
// result, and is safe to apply again when reading durable event details.
func SanitizeErrorHint(value string) string {
	value = strings.TrimSpace(value)
	value = urlHintRE.ReplaceAllString(value, "")
	value = posixPathHintRE.ReplaceAllString(value, "")
	value = windowsPathHintRE.ReplaceAllString(value, "")
	value = strings.Join(strings.Fields(value), " ")
	if strings.ContainsAny(value, "/\\") {
		return ""
	}
	// Cut on a word boundary and say so. A byte-exact cut produced hints like
	// "attachments add requir", which reads as corruption rather than elision
	// and cost a session's diagnosis time on a refusal zotio had explained in
	// full. The bound itself is the privacy contract and does not move.
	if len(value) > maxErrorHintBytes {
		value = value[:maxErrorHintBytes-3]
		if cut := strings.LastIndexByte(value, ' '); cut > maxErrorHintBytes/2 {
			value = value[:cut]
		}
		value = strings.TrimRight(value, " ,;:.") + "..."
	}
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}
