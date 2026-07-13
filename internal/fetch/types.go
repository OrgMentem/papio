// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package fetch downloads one candidate URL under strict bounds: HTTPS-only
// (loopback HTTP only when explicitly allowed for tests/dev), per-hop
// SSRF/redirect policy, size caps enforced independently of Content-Length,
// and streaming into a quarantine temp file. This file pins the contract; the
// implementation lives in fetch.go.
package fetch

import (
	"fmt"
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
}

func (e *Error) Error() string {
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("fetch %s (HTTP %d): %s", e.Class, e.HTTPStatus, e.Msg)
	}
	return fmt.Sprintf("fetch %s: %s", e.Class, e.Msg)
}
