// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package resolvertest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/resolver"
	"papio/internal/work"
)

// CheckHTTPFailureClassification pins how a resolver maps HTTP failure
// statuses: 404 is absence (nil, nil), 429 is temporary with the Retry-After
// wait, 500/503 are temporary, and 401/403 are hard failures.
func CheckHTTPFailureClassification(t *testing.T, newResolver func(baseURL string) resolver.Resolver, w work.Work) {
	t.Helper()
	for _, test := range []struct {
		name          string
		status        int
		retryAfter    string
		wantNilNil    bool
		wantTemporary bool
		wantWait      time.Duration
	}{
		{name: "not found is absence", status: http.StatusNotFound, wantNilNil: true},
		{name: "429 is temporary with wait", status: http.StatusTooManyRequests, retryAfter: "60", wantTemporary: true, wantWait: time.Minute},
		{name: "429 is temporary with short wait", status: http.StatusTooManyRequests, retryAfter: "7", wantTemporary: true, wantWait: 7 * time.Second},
		{name: "500 is temporary", status: http.StatusInternalServerError, wantTemporary: true},
		{name: "503 is temporary", status: http.StatusServiceUnavailable, wantTemporary: true},
		{name: "401 is a hard failure", status: http.StatusUnauthorized},
		{name: "403 is a hard failure", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					rw.Header().Set("Retry-After", test.retryAfter)
				}
				rw.WriteHeader(test.status)
			}))
			defer server.Close()

			cands, err := newResolver(server.URL).Resolve(context.Background(), w)
			if test.wantNilNil {
				if err != nil || cands != nil {
					t.Fatalf("Resolve = (%v, %v), want (nil, nil)", cands, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			wait, temporary := resolver.Temporary(err)
			if temporary != test.wantTemporary {
				t.Fatalf("Temporary(%v) = %v, want %v", err, temporary, test.wantTemporary)
			}
			if test.wantWait != 0 && wait != test.wantWait {
				t.Fatalf("retry wait = %v, want %v", wait, test.wantWait)
			}
		})
	}
}

// CheckParseRetryAfterClampsHugeValues pins the overflow-safe Retry-After
// parsing: a header large enough to overflow the nanosecond multiply must clamp
// to the max duration rather than wrap to a garbage (possibly negative) value.
// It also checks HTTP-date delays and expired deadlines.
func CheckParseRetryAfterClampsHugeValues(t *testing.T, parse func(string, time.Time) time.Duration) {
	t.Helper()
	const maxDuration = time.Duration(1<<63 - 1)
	now := time.Now()
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"garbage", "not-a-number", 0},
		{"normal seconds", "5", 5 * time.Second},
		{"overflow multiply clamps to max", "99999999999", maxDuration},
		{"beyond int64 range falls through", "9999999999999999999", 0},
		{"past HTTP-date", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parse(c.value, now)
			if got != c.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", c.value, got, c.want)
			}
			if got < 0 {
				t.Errorf("parseRetryAfter(%q) = %v, must never be negative", c.value, got)
			}
		})
	}
	t.Run("future HTTP-date", func(t *testing.T) {
		now := time.Now()
		deadline := now.Add(2 * time.Hour).UTC().Truncate(time.Second)
		value := deadline.Format(http.TimeFormat)
		got := parse(value, now)
		// Some parsers use the supplied now; others read the clock again.
		// Bound both without a scheduler-dependent timing tolerance.
		minimum := time.Until(deadline)
		maximum := deadline.Sub(now)
		if got <= 0 || got < minimum || got > maximum {
			t.Fatalf("parseRetryAfter(%q) = %v, want between %v and %v", value, got, minimum, maximum)
		}
	})
}

// oversizedMarker is the wording every resolver's bounded read uses when a
// body passes its MaxResponseBytes ("response exceeds size limit", "response
// exceeds 256-byte limit", ...).
const oversizedMarker = "response exceeds"

// CheckOversizedBodyFailsClosed pins the bounded-read contract every resolver
// shares. resolve builds the package's resolver with MaxResponseBytes set to
// maxBytes against a server that answers every request with body, and
// resolves one work. A body one byte over the limit must be refused as a
// size-limit failure that is not retryable, since the same provider answer
// will not shrink; the same body at exactly the limit must not be.
func CheckOversizedBodyFailsClosed(t *testing.T, body string, resolve func(t *testing.T, maxBytes int64, body string) error) {
	t.Helper()
	size := int64(len(body))
	t.Run("one byte over the limit", func(t *testing.T) {
		err := resolve(t, size-1, body)
		if err == nil || !strings.Contains(err.Error(), oversizedMarker) {
			t.Fatalf("Resolve = %v, want a size-limit rejection", err)
		}
		if _, temporary := resolver.Temporary(err); temporary {
			t.Fatalf("an oversized body is malformed, not retryable: %v", err)
		}
	})
	t.Run("exactly at the limit", func(t *testing.T) {
		if err := resolve(t, size, body); err != nil && strings.Contains(err.Error(), oversizedMarker) {
			t.Fatalf("Resolve = %v, want a body at the limit accepted by the size gate", err)
		}
	})
}
