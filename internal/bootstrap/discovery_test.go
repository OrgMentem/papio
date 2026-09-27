package bootstrap

import (
	"testing"

	"papio/internal/config"
)

// Discovery selection and acquisition-source enablement are independent:
// discovery.sources may fall back to OpenAlex while the OpenAlex ACQUISITION
// source is disabled. Passing that flag into the budget gate vetoed every
// discovery request on such a config — a backend constructed and then
// silently refused, breaking search, MCP, watch digests and DOI-only
// enrichment for anyone who had not separately enabled the acquisition
// source. It escaped a live smoke test because the operator's own config
// happened to enable it.
func TestDiscoveryIsNotVetoedByAcquisitionEnablement(t *testing.T) {
	cfg := config.Default()
	// Pin the regression shape explicitly instead of relying on the shipped
	// default: acquisition off, discovery fallback on. (The shipped default
	// now enables the acquisition source; the gate must stay independent of
	// the flag either way.)
	off := cfg.Sources[config.SourceOpenAlex]
	off.Enabled = false
	cfg.Sources[config.SourceOpenAlex] = off
	if len(cfg.Discovery.Sources) != 0 {
		t.Fatalf("default discovery.sources = %v, want empty so the OpenAlex fallback is exercised", cfg.Discovery.Sources)
	}
	if got := discoveryPolicy(cfg, config.SourceOpenAlex); !got.Enabled {
		t.Fatal("discovery policy is disabled on a default config; every discovery request would be refused before it is made")
	}
}

// The gate still has to pace and bound spend, or moving enablement out would
// have quietly removed the throttling with it.
func TestDiscoveryPolicyKeepsPacingAndSpendCeiling(t *testing.T) {
	cfg := config.Default()
	source := cfg.SourcePolicy(config.SourceOpenAlex)
	got := discoveryPolicy(cfg, config.SourceOpenAlex)
	if got.RatePerSec != source.RatePerSec || got.Burst != source.Burst {
		t.Fatalf("pacing = %v/%v, want the source's %v/%v: discovery and acquisition share a provider",
			got.RatePerSec, got.Burst, source.RatePerSec, source.Burst)
	}
	if got.MaxCostUSD != source.MaxCostUSD {
		t.Fatalf("spend ceiling = %v, want the source's %v", got.MaxCostUSD, source.MaxCostUSD)
	}
}
