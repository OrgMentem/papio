// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Gate-compile matrix (Decision 3A) and per-request gate matrix (Decision
// 3B) — every ADR-cited rule traced to one assertion here.
package delivery

import (
	"testing"

	"papio/internal/config"
)

// fullHouseDocumentDelivery satisfies every Decision 3A static condition
// for illiad except recorded live acceptance.
func fullHouseDocumentDelivery() *config.DocumentDelivery {
	return &config.DocumentDelivery{
		Kind:              "illiad",
		BaseURL:           "https://illiad.example.edu/ILLiadWebPlatform",
		SubmitPolicy:      "auto_if_unconditional",
		RequestClasses:    []string{"digital_journal_article"},
		LegalBasis:        "institution_policy",
		PatronAttestation: "standing_completed",
		PatronFeePolicy:   "zero_standard",
		MonthlyRequestCap: 25,
		StatusPollMinutes: 60,
		APIKey:            "secret-key",
		PatronRef:         "configured-non-secret-reference",
	}
}

func blockerCodeSet(blockers []Blocker) map[string]bool {
	out := make(map[string]bool, len(blockers))
	for _, b := range blockers {
		out[b.Code] = true
	}
	return out
}

func TestCompileGateProfileNilDocumentDeliveryIsInvalid(t *testing.T) {
	profile := CompileGateProfile(config.Institution{}, "campus")
	if profile.Class != GateClassInvalid {
		t.Fatalf("Class = %q, want invalid", profile.Class)
	}
	if len(profile.Blockers) != 1 || profile.Blockers[0].Code != BlockerInstitutionPolicyUnknown {
		t.Fatalf("Blockers = %+v, want exactly [institution_policy_unknown]", profile.Blockers)
	}
}

func TestCompileGateProfileUnknownKindIsInvalid(t *testing.T) {
	inst := config.Institution{DocumentDelivery: &config.DocumentDelivery{Kind: "tipasa"}}
	profile := CompileGateProfile(inst, "campus")
	if profile.Class != GateClassInvalid {
		t.Fatalf("Class = %q, want invalid", profile.Class)
	}
	if len(profile.Blockers) != 1 || profile.Blockers[0].Code != BlockerProviderNotImplemented {
		t.Fatalf("Blockers = %+v, want exactly [provider_not_implemented]", profile.Blockers)
	}
}

func TestCompileGateProfileFormOnlyKindsArePermanentlyPrefillOnly(t *testing.T) {
	// Decision 3A: "openurl, libkey, and custom routes are permanently
	// prefill-only ... they route to a form and supply no deterministic
	// submission-and-reconciliation contract."
	for _, kind := range []string{"openurl", "libkey", "custom"} {
		t.Run(kind, func(t *testing.T) {
			dd := fullHouseDocumentDelivery()
			dd.Kind = kind
			profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
			if profile.Class != GateClassPrefillOnly {
				t.Fatalf("Class = %q, want prefill_only", profile.Class)
			}
			if len(profile.Blockers) != 1 || profile.Blockers[0].Code != BlockerProviderNotAutoCapable {
				t.Fatalf("Blockers = %+v, want exactly [provider_not_auto_capable]", profile.Blockers)
			}
			// WithLiveAcceptance can never lift a form-kind profile: its
			// one blocker isn't the live-acceptance sentinel.
			if got := profile.WithLiveAcceptance(true); got.Class != GateClassPrefillOnly {
				t.Fatalf("WithLiveAcceptance(true) on a form-kind profile = %q, want prefill_only (permanent)", got.Class)
			}
		})
	}
}

func TestCompileGateProfileS49LegalBasisRequiresCopyrightDeclaration(t *testing.T) {
	dd := fullHouseDocumentDelivery()
	dd.LegalBasis = "copyright_act_s49"
	profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
	if profile.Class != GateClassPrefillOnly {
		t.Fatalf("Class = %q, want prefill_only", profile.Class)
	}
	codes := blockerCodeSet(profile.Blockers)
	if !codes[BlockerPerRequestCopyrightDeclaration] {
		t.Fatalf("Blockers = %+v, want per_request_copyright_declaration present", profile.Blockers)
	}
	if !profile.RequiresOperatorStep {
		t.Fatal("RequiresOperatorStep = false, want true under s49 (a per-request human declaration is required)")
	}
	// Even with live acceptance recorded, the statutory declaration blocker
	// remains — s49 compiles prefill_only by law, not by caution.
	if got := profile.WithLiveAcceptance(true); got.Class != GateClassPrefillOnly {
		t.Fatalf("WithLiveAcceptance(true) under s49 = %q, want prefill_only (never liftable)", got.Class)
	}
}

func TestCompileGateProfileIlliadFullHouseMinusAcceptance(t *testing.T) {
	dd := fullHouseDocumentDelivery()
	profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
	if profile.Class != GateClassPrefillOnly {
		t.Fatalf("Class = %q, want prefill_only (no recorded live acceptance yet)", profile.Class)
	}
	if len(profile.Blockers) != 1 {
		t.Fatalf("Blockers = %+v, want exactly one (live acceptance only)", profile.Blockers)
	}
	if profile.Blockers[0].Code != BlockerProviderNotAutoCapable || profile.Blockers[0].Evidence != "no recorded live acceptance" {
		t.Fatalf("Blockers[0] = %+v, want {provider_not_auto_capable, \"no recorded live acceptance\"}", profile.Blockers[0])
	}
}

func TestCompileGateProfileWithLiveAcceptanceReachesAutoCapable(t *testing.T) {
	dd := fullHouseDocumentDelivery()
	profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
	accepted := profile.WithLiveAcceptance(true)
	if accepted.Class != GateClassAutoCapable {
		t.Fatalf("Class = %q, want auto_capable once live acceptance is recorded", accepted.Class)
	}
	if len(accepted.Blockers) != 0 {
		t.Fatalf("Blockers = %+v, want none", accepted.Blockers)
	}
	if !accepted.LiveAccepted {
		t.Fatal("LiveAccepted = false, want true")
	}
}

func TestCompileGateProfileEachMissingFieldBlocks(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*config.DocumentDelivery)
		wantCode string
	}{
		{"missing api_key", func(dd *config.DocumentDelivery) { dd.APIKey = "" }, BlockerAPICredentialMissing},
		{"missing patron_ref", func(dd *config.DocumentDelivery) { dd.PatronRef = "" }, BlockerPatronMappingUnverified},
		{"per_request patron_attestation", func(dd *config.DocumentDelivery) { dd.PatronAttestation = "per_request" }, BlockerPerRequestTerms},
		{"unknown patron_attestation", func(dd *config.DocumentDelivery) { dd.PatronAttestation = "unknown" }, BlockerInstitutionPolicyUnknown},
		{"per_request patron_fee_policy", func(dd *config.DocumentDelivery) { dd.PatronFeePolicy = "per_request" }, BlockerPatronFeeNotZero},
		{"unknown patron_fee_policy", func(dd *config.DocumentDelivery) { dd.PatronFeePolicy = "unknown" }, BlockerPatronFeeUnknown},
		{"submit_policy not auto", func(dd *config.DocumentDelivery) { dd.SubmitPolicy = "prefill_only" }, BlockerInstitutionPolicyUnknown},
		{"unsupported request class", func(dd *config.DocumentDelivery) { dd.RequestClasses = []string{} }, BlockerRequestClassUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			dd := fullHouseDocumentDelivery()
			test.mutate(dd)
			profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
			codes := blockerCodeSet(profile.Blockers)
			if !codes[test.wantCode] {
				t.Fatalf("Blockers = %+v, want %s present", profile.Blockers, test.wantCode)
			}
			if profile.Class != GateClassPrefillOnly {
				t.Fatalf("Class = %q, want prefill_only", profile.Class)
			}
		})
	}
}

// passingProfile and passingRequest are a fully compliant auto_capable
// profile plus a per-request GateRequest that satisfies all seven Decision
// 3B conditions, so EvaluateGate returns ActionSubmit.
func passingProfile() GateProfile {
	dd := fullHouseDocumentDelivery()
	profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
	return profile.WithLiveAcceptance(true)
}

func passingRequest() GateRequest {
	return GateRequest{
		EffectiveAccessMode: config.ModeDelegated,
		RequestClass:        "digital_journal_article",
		HasRequiredFields:   true,
		SubmittedThisMonth:  0,
	}
}

func TestEvaluateGateAllConditionsPassSubmits(t *testing.T) {
	decision := EvaluateGate(passingProfile(), passingRequest())
	if decision.Action != ActionSubmit {
		t.Fatalf("Decision = %+v, want submit", decision)
	}
}

func TestEvaluateGateEachConditionFlipsToPrefill(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(*GateProfile, *GateRequest)
		wantAction Action
	}{
		{"access mode conservative", func(p *GateProfile, r *GateRequest) { r.EffectiveAccessMode = config.ModeConservative }, ActionPrefill},
		{"access mode assisted", func(p *GateProfile, r *GateRequest) { r.EffectiveAccessMode = config.ModeAssisted }, ActionPrefill},
		{"submit_policy not auto", func(p *GateProfile, r *GateRequest) { p.SubmitPolicy = "prefill_only" }, ActionPrefill},
		{"request class not supported", func(p *GateProfile, r *GateRequest) { r.RequestClass = "ill_book" }, ActionPrefill},
		{"operator step required", func(p *GateProfile, r *GateRequest) { p.RequiresOperatorStep = true }, ActionPrefill},
		{"fee not zero", func(p *GateProfile, r *GateRequest) { p.PatronFeePolicy = "per_request" }, ActionPrefill},
		{"fee unknown", func(p *GateProfile, r *GateRequest) { p.PatronFeePolicy = "unknown" }, ActionPrefill},
		{"cap exhausted", func(p *GateProfile, r *GateRequest) { p.MonthlyRequestCap = 3; r.SubmittedThisMonth = 3 }, ActionPrefill},
		{"required fields missing", func(p *GateProfile, r *GateRequest) { r.HasRequiredFields = false }, ActionEnrichThenPrefill},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := passingProfile()
			req := passingRequest()
			test.mutate(&profile, &req)
			decision := EvaluateGate(profile, req)
			if decision.Action != test.wantAction {
				t.Fatalf("Decision.Action = %q, want %q (profile=%+v req=%+v)", decision.Action, test.wantAction, profile, req)
			}
		})
	}
}

func TestEvaluateGateCapWithHeadroomStillSubmits(t *testing.T) {
	profile := passingProfile()
	profile.MonthlyRequestCap = 5
	req := passingRequest()
	req.SubmittedThisMonth = 4
	if decision := EvaluateGate(profile, req); decision.Action != ActionSubmit {
		t.Fatalf("Decision = %+v, want submit (4 < cap 5)", decision)
	}
}

func TestEvaluateGateZeroCapMeansUnlimited(t *testing.T) {
	profile := passingProfile()
	profile.MonthlyRequestCap = 0
	req := passingRequest()
	req.SubmittedThisMonth = 1000
	if decision := EvaluateGate(profile, req); decision.Action != ActionSubmit {
		t.Fatalf("Decision = %+v, want submit (0 = no cap declared)", decision)
	}
}

func TestEvaluateGateNonAutoCapableProfileNeverSubmits(t *testing.T) {
	dd := fullHouseDocumentDelivery()
	dd.Kind = "openurl"
	profile := CompileGateProfile(config.Institution{DocumentDelivery: dd}, "campus")
	decision := EvaluateGate(profile, passingRequest())
	if decision.Action != ActionPrefill {
		t.Fatalf("Decision.Action = %q, want prefill (profile never reached auto_capable)", decision.Action)
	}
	if len(decision.Blockers) != 1 || decision.Blockers[0] != BlockerProviderNotAutoCapable {
		t.Fatalf("Decision.Blockers = %v, want [provider_not_auto_capable] surfaced from the compiled profile", decision.Blockers)
	}
}

func TestGateProfileDigestStableAndSensitiveToClass(t *testing.T) {
	p1 := passingProfile()
	p2 := passingProfile()
	if p1.Digest() != p2.Digest() {
		t.Fatal("Digest() differs across two identically-compiled profiles")
	}
	p3 := p1
	p3.Class = GateClassPrefillOnly
	if p1.Digest() == p3.Digest() {
		t.Fatal("Digest() unchanged after Class changed — recompiles would be silently misattributed")
	}
}
