// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"papio/internal/store"
)

// SQLite orders stored timestamps as bytes, so FormatTime's text order must be
// time order, including inside one second, where time.RFC3339Nano's trimmed
// fractions reverse it.
func TestFormatTimeSortsInTimeOrder(t *testing.T) {
	second := time.Date(2026, 9, 24, 6, 19, 5, 0, time.UTC)
	perth := time.FixedZone("AWST", 8*60*60)
	instants := []time.Time{
		second.Add(time.Second),
		second.Add(595612 * time.Microsecond),
		second,
		second.Add(595610 * time.Microsecond), // trims to .59561
		second.Add(100 * time.Millisecond),    // trims to .1
		second.Add(time.Nanosecond),
		second.Add(595612*time.Microsecond + time.Nanosecond),
		second.Add(-time.Nanosecond),
		second.Add(500 * time.Millisecond).In(perth), // a local-zone value
	}
	byText := slices.Clone(instants)
	slices.SortFunc(byText, func(a, b time.Time) int {
		return strings.Compare(store.FormatTime(a), store.FormatTime(b))
	})
	byTime := slices.Clone(instants)
	slices.SortFunc(byTime, func(a, b time.Time) int { return a.Compare(b) })
	for i := range byTime {
		if !byText[i].Equal(byTime[i]) {
			t.Fatalf("text order[%d] = %s (%q), time order = %s (%q)", i,
				byText[i], store.FormatTime(byText[i]), byTime[i], store.FormatTime(byTime[i]))
		}
	}
	for _, instant := range instants {
		text := store.FormatTime(instant)
		if len(text) != 30 || !strings.HasSuffix(text, "Z") {
			t.Errorf("FormatTime(%s) = %q, want 30 bytes of UTC", instant, text)
		}
		// Readers parse with time.RFC3339Nano, which must keep the instant.
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || !parsed.Equal(instant) {
			t.Errorf("parse(%q) = %s, %v; want %s", text, parsed, err, instant)
		}
	}
}
