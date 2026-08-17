// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import "testing"

// minimalConfig is the loadable prefix each tier case appends its own
// [sources.openaire] table to.
const minimalConfig = "access_mode = \"conservative\"\nemail = \"reader@example.org\"\n\n"

func TestOpenAIREPacingFollowsCredentialTier(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		wantRate  float64
		wantBurst int
	}{{
		// Keyless is the shipped default: 60 requests/hour documented,
		// 57.6 spent.
		name:      "keyless keeps the public pacing",
		body:      "",
		wantRate:  OpenAIREKeylessRatePerSec,
		wantBurst: 1,
	}, {
		// A registered service is documented at 7,200 requests/hour and
		// its credentials do not expire, so pacing follows. Without this
		// the credential authenticates and changes nothing observable.
		name:      "registered service raises pacing to its documented tier",
		body:      "[sources.openaire]\nclient_id = \"svc\"\nclient_secret = \"shh\"\n",
		wantRate:  OpenAIREAuthenticatedRatePerSec,
		wantBurst: OpenAIREAuthenticatedBurst,
	}, {
		// A personal access token also raises OpenAIRE's ceiling, but only
		// for the hour before it expires. Pacing to the authenticated tier
		// on a credential that can vanish mid-hour would leave papio
		// running at 120x an unauthenticated ceiling, so api_key stays on
		// keyless pacing deliberately.
		name:      "expiring api_key does not raise pacing",
		body:      "[sources.openaire]\napi_key = \"personal-token\"\n",
		wantRate:  OpenAIREKeylessRatePerSec,
		wantBurst: 1,
	}, {
		// Half a credential pair is not a credential.
		name:      "client_id without a secret does not raise pacing",
		body:      "[sources.openaire]\nclient_id = \"svc\"\n",
		wantRate:  OpenAIREKeylessRatePerSec,
		wantBurst: 1,
	}, {
		// The derivation applies to the untouched default only: an
		// explicit rate is the operator's decision and outranks the tier.
		name:      "an explicit rate survives the derivation",
		body:      "[sources.openaire]\nclient_id = \"svc\"\nclient_secret = \"shh\"\nrate_per_sec = 0.5\nburst = 3\n",
		wantRate:  0.5,
		wantBurst: 3,
	}} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, minimalConfig+test.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.SourcePolicy(SourceOpenAIRE)
			if got.RatePerSec != test.wantRate {
				t.Errorf("rate_per_sec = %v, want %v", got.RatePerSec, test.wantRate)
			}
			if got.Burst != test.wantBurst {
				t.Errorf("burst = %d, want %d", got.Burst, test.wantBurst)
			}
		})
	}
}

func TestAuthenticatedPacingStaysUnderTheDocumentedCeiling(t *testing.T) {
	// OpenAIRE meters a fixed hourly window, so the hour's spend is the
	// bucket's refill plus one burst. Both tiers must stay under their
	// documented ceiling with headroom; this is the arithmetic that made
	// 0.016 the keyless default, pinned so a future tune cannot breach it.
	const keylessCeiling, authenticatedCeiling = 60.0, 7200.0
	keyless := OpenAIREKeylessRatePerSec*3600 + 1
	if keyless > keylessCeiling {
		t.Errorf("keyless hourly spend %v exceeds documented %v", keyless, keylessCeiling)
	}
	authenticated := OpenAIREAuthenticatedRatePerSec*3600 + OpenAIREAuthenticatedBurst
	if authenticated > authenticatedCeiling {
		t.Errorf("authenticated hourly spend %v exceeds documented %v", authenticated, authenticatedCeiling)
	}
}

func TestHasClientCredentialsRequiresBothHalves(t *testing.T) {
	for _, test := range []struct {
		source Source
		want   bool
	}{
		{Source{ClientID: "a", ClientSecret: "b"}, true},
		{Source{ClientID: " a ", ClientSecret: " b "}, true},
		{Source{ClientID: "a"}, false},
		{Source{ClientSecret: "b"}, false},
		{Source{ClientID: "  ", ClientSecret: "b"}, false},
		{Source{APIKey: "token"}, false},
	} {
		if got := test.source.HasClientCredentials(); got != test.want {
			t.Errorf("HasClientCredentials(%+v) = %v, want %v", test.source, got, test.want)
		}
	}
}
