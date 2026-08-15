// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package resolvertest

import (
	"testing"
	"time"
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
