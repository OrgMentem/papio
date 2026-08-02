package config

import "testing"

// A negative value does not throttle harder, it removes the throttle: the
// budget manager treats rate <= 0 as unlimited and a cost limit <= 0 as
// unmetered. So a typed minus sign silently deletes the exact protection it
// appears to configure. Zero keeps its documented meaning, because that is a
// choice someone can state; a negative number never is.
func TestNegativeSourceValuesAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Source)
	}{
		{"rate_per_sec", func(s *Source) { s.RatePerSec = -2 }},
		{"burst", func(s *Source) { s.Burst = -1 }},
		{"max_cost_usd", func(s *Source) { s.MaxCostUSD = -5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			source := cfg.Sources[SourceOpenAlex]
			tc.apply(&source)
			cfg.Sources[SourceOpenAlex] = source
			if err := cfg.validate(); err == nil {
				t.Fatalf("a negative %s was accepted; it disables the protection rather than tightening it", tc.name)
			}
		})
	}
}

// Zero is a deliberate, documented choice and must keep working.
func TestZeroSourceValuesRemainValid(t *testing.T) {
	cfg := Default()
	source := cfg.Sources[SourceOpenAlex]
	source.RatePerSec, source.Burst, source.MaxCostUSD = 0, 0, 0
	cfg.Sources[SourceOpenAlex] = source
	if err := cfg.validate(); err != nil {
		t.Fatalf("zero values rejected: %v; 0 means no pacing and no ceiling by design", err)
	}
}
