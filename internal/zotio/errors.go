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
	ErrorClassZoteroFieldValidation    = "zotero_field_validation"
	ErrorClassMirrorSyncFailed         = "mirror_sync_failed"
	ErrorClassZotioExecTimeout         = "zotio_exec_timeout"
	ErrorClassZotioNotConfigured       = "zotio_not_configured"
	ErrorClassPlanConfirmationMismatch = "plan_confirmation_mismatch"
	ErrorClassReservationConflict      = "reservation_conflict"
	ErrorClassLocalDBLocked            = "local_db_locked"
	ErrorClassNetwork                  = "network"
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
		return safeErrorInfo(ErrorClassZoteroHTTP4xx, "Zotero HTTP "+strconv.Itoa(status), status)
	}
	if strings.Contains(lower, "unknown item field") {
		return safeErrorInfo(ErrorClassZoteroFieldValidation, "unknown item field", 0)
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
	return ErrorInfo{Class: ErrorClassUnknown}
}

func safeErrorInfo(class, hint string, status int) ErrorInfo {
	return ErrorInfo{Class: class, Hint: SanitizeErrorHint(hint), HTTPStatus: status}
}

// IsErrorClass reports whether class is part of the stable Zotio boundary
// vocabulary.
func IsErrorClass(class string) bool {
	switch class {
	case ErrorClassZoteroHTTP4xx,
		ErrorClassZoteroFieldValidation,
		ErrorClassMirrorSyncFailed,
		ErrorClassZotioExecTimeout,
		ErrorClassZotioNotConfigured,
		ErrorClassPlanConfirmationMismatch,
		ErrorClassReservationConflict,
		ErrorClassLocalDBLocked,
		ErrorClassNetwork,
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
	for len(value) > maxErrorHintBytes {
		value = value[:len(value)-1]
	}
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}
