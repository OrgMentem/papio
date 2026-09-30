// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package resolvertest

import (
	"strings"
	"testing"
	"time"

	"papio/internal/resolver"
)

// CheckParseRetryAfterClampsHugeValues pins the overflow-safe Retry-After
// parsing: a header large enough to overflow the nanosecond multiply must clamp
// to the max duration rather than wrap to a garbage (possibly negative) value.
// Canonical table reproduced from arxiv/europepmc/core/crossreftdm.
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
