package api

import (
	"testing"
	"time"
)

// TestParseSinceAcceptsEveryLookbackForm pins the one grammar every --since
// shares: a Go duration, a whole number of days, or an RFC3339 instant. The CLI's
// export ledger and stats producers call this same parser, so "7d" cannot work on
// one command and be refused by another.
func TestParseSinceAcceptsEveryLookbackForm(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Time
	}{
		{"", time.Time{}},
		{"  ", time.Time{}},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"0d", now},
		{"2026-07-01T00:00:00Z", time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-07-01T02:00:00+02:00", time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)},
	} {
		got, err := ParseSince(test.value, now)
		if err != nil {
			t.Fatalf("ParseSince(%q): %v", test.value, err)
		}
		if !got.Equal(test.want) {
			t.Fatalf("ParseSince(%q) = %s, want %s", test.value, got, test.want)
		}
	}
	for _, value := range []string{"soon", "d", "1.5d", "7days"} {
		if _, err := ParseSince(value, now); err == nil {
			t.Fatalf("ParseSince(%q) succeeded, want an error", value)
		}
	}
}
