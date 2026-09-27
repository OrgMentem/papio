// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package fetch downloads one candidate URL under strict bounds: HTTPS-only
// (loopback HTTP only when explicitly allowed for tests/dev), per-hop
// SSRF/redirect policy, size caps enforced independently of Content-Length,
// and streaming into a quarantine temp file. This file pins the contract; the
// implementation lives in fetch.go.
package fetch

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Policy bounds one download.
type Policy struct {
	MaxBytes          int64
	Timeout           time.Duration
	ConnectTimeout    time.Duration
	HeaderTimeout     time.Duration
	BodyTimeout       time.Duration
	MaxRedirects      int
	AllowHTTPLoopback bool // tests/dev only; production is HTTPS-only
	UserAgent         string
}

// DefaultPolicy returns production bounds (overridden by config).
func DefaultPolicy() Policy {
	return Policy{
		MaxBytes:       100 << 20,
		Timeout:        120 * time.Second,
		ConnectTimeout: 15 * time.Second,
		HeaderTimeout:  30 * time.Second,
		BodyTimeout:    90 * time.Second,
		MaxRedirects:   5,
		UserAgent:      "papio/0.1 (legitimate research acquisition; mailto:unset)",
	}
}

// Result describes a completed download sitting in quarantine.
type Result struct {
	TempPath    string // file inside the job's quarantine dir
	SHA256      string
	SizeBytes   int64
	SniffedMIME string // from magic bytes, not headers
	ContentType string // server-declared, informational only
	HTTPStatus  int
	FinalHost   string // last host after redirects (redacted-safe)
}

// Error classes drive the job state machine.
const (
	ClassRetryable = "retryable" // timeouts, 429, 5xx, transient network
	ClassInvalid   = "invalid"   // wrong payload/type/size; try next candidate
	ClassBlocked   = "blocked"   // SSRF/policy refusal; never retried
)

// Error is a classified fetch failure.
type Error struct {
	Class      string
	HTTPStatus int
	RetryAfter time.Duration
	Msg        string // redacted; never contains query values
	// FinalHost is the lowercased hostname that answered the last request
	// hop (after redirects), or "" when nothing answered (network, timeout,
	// policy refusal, host-gate refusal). It is a hostname only, never a URL.
	FinalHost string
	// SuppressedHost names the hop host a HostGate refused; "" otherwise.
	SuppressedHost string
	// BackedOffHost names the hop host a HostGate deferred because it is
	// inside its 429 backoff; "" otherwise. RetryAfter carries the wait.
	BackedOffHost string
}

func (e *Error) Error() string {
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("fetch %s (HTTP %d): %s", e.Class, e.HTTPStatus, e.Msg)
	}
	return fmt.Sprintf("fetch %s: %s", e.Class, e.Msg)
}

// Persisted fetch failure kinds. attempts.detail stores one of these (plus
// the destination host) so operators can tell a challenge from a plain 403
// without a schema change. The vocabulary is closed: Msg is redacted but
// still free text, so only these constants may reach the store. Rows written
// before this existed hold the bare "*fetch.Error" type name and keep
// parsing as kind "" (unknown).
const (
	FetchKindChallenge = "challenge" // anti-bot/captcha response, any status
	FetchKindNotAPDF   = "not-a-pdf" // 200 HTML (or audio/ID3) where a PDF was asked for
	FetchKindSizeLimit = "size-limit"
	FetchKindRedirect  = "redirect"
	FetchKindNetwork   = "network"
	FetchKindTimeout   = "timeout"
	FetchKindCancelled = "cancelled"
	FetchKindBlocked   = "blocked"
	FetchKindLocal     = "local"   // quarantine/request-shape failure, never the provider
	FetchKindBackoff   = "backoff" // a hop deferred inside its host's 429 backoff; nothing sent to that host
)

// FailureKind maps a classified error to its persisted kind. HTTP statuses
// map to http-<code> with short names for the frequent ones (http-403,
// http-404, http-429) and http-5xx for the whole server-error range, so
// per-host 403/429 counts stay a LIKE query. Anything unrecognized falls
// back to the error class, which is always one of the three Class constants.
func (e *Error) FailureKind() string {
	if e == nil {
		return ""
	}
	// HTTP 429 outranks every other signal: it is pacing, never a wall.
	if e.HTTPStatus == 429 {
		return "http-429"
	}
	if e.BackedOffHost != "" {
		return FetchKindBackoff
	}
	msg := strings.ToLower(e.Msg)
	if strings.Contains(msg, "challenge") || strings.Contains(msg, "anti-bot") ||
		strings.Contains(msg, "captcha") {
		return FetchKindChallenge
	}
	if e.Class == ClassBlocked {
		return FetchKindBlocked
	}
	if e.HTTPStatus > 0 {
		switch e.HTTPStatus {
		case 401:
			return "http-401"
		case 403:
			return "http-403"
		case 404:
			return "http-404"
		case 410:
			return "http-410"
		case 429:
			return "http-429"
		}
		if e.HTTPStatus >= 500 {
			return "http-5xx"
		}
		return "http-" + strconv.Itoa(e.HTTPStatus)
	}
	switch {
	case strings.Contains(msg, "not an acceptable pdf"):
		return FetchKindNotAPDF
	case strings.Contains(msg, "exceeds configured size limit") ||
		strings.Contains(msg, "invalid content-length"):
		return FetchKindSizeLimit
	case strings.Contains(msg, "redirect"):
		return FetchKindRedirect
	case strings.Contains(msg, "timed out"):
		return FetchKindTimeout
	case strings.Contains(msg, "cancelled"):
		return FetchKindCancelled
	case strings.Contains(msg, "network request failed") ||
		strings.Contains(msg, "lookup failed") ||
		strings.Contains(msg, "body read failed"):
		return FetchKindNetwork
	case strings.Contains(msg, "quarantine") || strings.Contains(msg, "invalid request") ||
		strings.Contains(msg, "only get requests") || strings.Contains(msg, "quarantine path"):
		return FetchKindLocal
	}
	return e.Class
}

// PersistedDetail renders the attempts.detail value for a fetch failure:
//
//	*fetch.Error(<kind>) host=<candidate host> [final=<responding host>]
//
// The "*fetch.Error" prefix is the historical type name, so legacy rows stay
// in the same family. host is the candidate URL's hostname. final is present
// exactly when some host answered the last request hop (e.FinalHost, set by
// the downloader after redirects) and absent when nothing answered (network,
// timeout, policy or host-gate refusal): an absent final never fabricates a
// responder. Suppression keys on final when present, since a redirector's
// 403 belongs to the publisher that answered. Both halves are hostnames only
// (never userinfo, path, or query); an unparseable URL records no host.
func (e *Error) PersistedDetail(rawURL string) string {
	kind := e.FailureKind()
	detail := "*fetch.Error(" + kind + ")"
	if host := detailHost(rawURL); host != "" {
		detail += " host=" + host
	}
	if final := strings.ToLower(strings.TrimSpace(e.FinalHost)); final != "" {
		detail += " final=" + final
	}
	return detail
}

// ParsePersistedDetail splits an attempts.detail value back into its failure
// kind, candidate host, and responding host. Legacy rows holding the bare
// "*fetch.Error" name parse as kind "" (unknown) with no hosts; rows written
// before the final-host half existed parse with final "" (unknown responder);
// anything that never was a fetch detail parses the same way.
func ParsePersistedDetail(detail string) (kind, host, final string) {
	rest, ok := strings.CutPrefix(detail, "*fetch.Error")
	if !ok {
		return "", "", ""
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") {
		return "", "", ""
	}
	end := strings.Index(rest, ")")
	if end < 0 {
		return "", "", ""
	}
	kind = rest[1:end]
	for _, field := range strings.Fields(rest[end+1:]) {
		value, ok := strings.CutPrefix(field, "host=")
		if ok {
			host = value
			continue
		}
		if value, ok := strings.CutPrefix(field, "final="); ok {
			final = value
		}
	}
	return kind, host, final
}

// SuppressionHost is the host a failure counts against: the responding host
// when one answered, the candidate host otherwise, "" when neither is known.
// kind stays a parameter so callers name the (pooled) kind at the call site.
func SuppressionHost(kind, host, final string) string {
	_ = kind
	if final != "" {
		return final
	}
	return host
}

// Suppressed is a per-hop host-gate refusal: the named host is under
// suppression and the hop was not attempted. ClassInvalid keeps a walled
// host out of the retry path; HTTPStatus and FinalHost stay zero so the
// refusal never reads as a wire answer the provider never sent. The app
// records it as the `suppressed` attempt outcome.
func Suppressed(host string) *Error {
	host = strings.ToLower(strings.TrimSpace(host))
	return &Error{Class: ClassInvalid, Msg: "host suppressed", SuppressedHost: host}
}

// IsSuppressed reports whether err is a host-gate refusal and names the
// refused hop host.
func IsSuppressed(err error) (string, bool) {
	var fe *Error
	if !errors.As(err, &fe) || fe.SuppressedHost == "" {
		return "", false
	}
	return fe.SuppressedHost, true
}

// BackedOff is a per-hop host-gate deferral: the named host is inside its
// 429 backoff for another wait, so the hop was not attempted. It is
// retryable pacing, never a wall, and carries no HTTPStatus or FinalHost
// because the host never answered.
func BackedOff(host string, wait time.Duration) *Error {
	host = strings.ToLower(strings.TrimSpace(host))
	return &Error{Class: ClassRetryable, RetryAfter: wait, Msg: "host inside rate-limit backoff", BackedOffHost: host}
}

// IsBackedOff reports whether err is a host-gate backoff deferral, naming
// the deferred hop host and the remaining wait.
func IsBackedOff(err error) (string, time.Duration, bool) {
	var fe *Error
	if !errors.As(err, &fe) || fe.BackedOffHost == "" {
		return "", 0, false
	}
	return fe.BackedOffHost, fe.RetryAfter, true
}

func detailHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
