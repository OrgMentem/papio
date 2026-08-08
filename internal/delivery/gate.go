// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package delivery

import (
	"papio/internal/config"
)

// GateClass is the compiled Decision 3A classification for one institution
// profile × provider × patron class × request class × legal basis unit.
type GateClass string

const (
	GateClassAutoCapable GateClass = "auto_capable"
	GateClassPrefillOnly GateClass = "prefill_only"
	GateClassInvalid     GateClass = "invalid"
)

// FulfillmentChannelPatronWeb is the only fulfillment channel v1 compiles
// (2026-08-07 ADR-0017 amendment "Fulfillment retrieval"): the patron-web
// "View PDF" page (ILLiad Web Platform form 75), driven through the
// ordinary browser-handoff machinery once a request reaches fulfilled.
// GateProfile.FulfillmentChannel is "" when no channel compiles.
const FulfillmentChannelPatronWeb = "patron_web"

// Blocker vocabulary (ADR-0017 Decision 3A) — closed, exactly these 13
// strings. A blocker code may appear more than once on a profile with
// different Evidence text; the vocabulary itself never grows ad hoc.
const (
	BlockerProviderNotImplemented         = "provider_not_implemented"
	BlockerProviderNotAutoCapable         = "provider_not_auto_capable"
	BlockerAPICredentialMissing           = "api_credential_missing"
	BlockerPatronMappingUnverified        = "patron_mapping_unverified"
	BlockerRequestClassUnsupported        = "request_class_unsupported"
	BlockerPerRequestLogin                = "per_request_login"
	BlockerPerRequestTerms                = "per_request_terms"
	BlockerPerRequestCopyrightDeclaration = "per_request_copyright_declaration"
	BlockerPerRequestPurposeStatement     = "per_request_purpose_statement"
	BlockerPatronFeeNotZero               = "patron_fee_not_zero"
	BlockerPatronFeeUnknown               = "patron_fee_unknown"
	BlockerReconciliationUnavailable      = "reconciliation_unavailable"
	BlockerInstitutionPolicyUnknown       = "institution_policy_unknown"
)

// evidenceNoLiveAcceptance is the fixed evidence text CompileGateProfile
// attaches to the BlockerProviderNotAutoCapable it always appends for a
// structurally eligible profile (Decision 3A's third hard rule: "An
// auto_capable compile additionally requires recorded live acceptance").
// Service.ResolveGateProfile matches on this exact blocker to know it is
// the *only* thing standing between prefill_only and auto_capable, and
// GateProfile.WithLiveAcceptance lifts it once acceptance is on record.
const evidenceNoLiveAcceptance = "no recorded live acceptance"

// Blocker is one closed-vocabulary reason a profile did not compile
// auto_capable, with the recorded evidence Decision 3A requires.
type Blocker struct {
	Code     string
	Evidence string
}

// providerCapability is the static, in-code declaration of what one
// document-delivery adapter can do (Decision 3A: "the provider adapter's
// declared capabilities"). Only illiad ships a create-and-reconcile
// contract in v1; openurl, libkey, and custom route to a form and are
// permanently prefill-only regardless of every other declaration.
type providerCapability struct {
	canCreate      bool
	canLookup      bool
	canListPatron  bool
	canReconcile   bool
	requestClasses map[string]bool
}

var providerCapabilities = map[string]providerCapability{
	"illiad": {
		canCreate:     true,
		canLookup:     true,
		canListPatron: true,
		canReconcile:  true,
		requestClasses: map[string]bool{
			"digital_journal_article": true,
		},
	},
	// openurl, libkey, and custom are shipped adapters (config accepts
	// them) but supply no create/lookup/reconciliation contract — every
	// capability flag stays false and requestClasses stays nil.
	"openurl": {},
	"libkey":  {},
	"custom":  {},
}

// GateProfile is the compiled Decision 3A gate profile for one institution
// profile. CompileGateProfile produces it from configuration alone; only
// Service.ResolveGateProfile can fold in the store-backed live-acceptance
// fact that decides whether a structurally eligible profile actually
// reaches GateClassAutoCapable.
type GateProfile struct {
	ProfileName string
	Provider    string // the configured document_delivery.kind
	Class       GateClass
	Blockers    []Blocker

	// Snapshot of the static declarations EvaluateGate's seven-point
	// per-request gate consults (Decision 3B), so the gate never has to
	// re-read config or the provider capability table.
	SubmitPolicy            string
	RequestClasses          []string
	SupportedRequestClasses map[string]bool // configured ∩ provider-supported
	PatronFeePolicy         string
	MonthlyRequestCap       int
	StatusPollMinutes       int
	RequiresOperatorStep    bool // any per_request_* blocker was compiled
	LiveAccepted            bool

	// FulfillmentChannel is the 2026-08-07 ADR-0017 amendment's compiled
	// retrieval capability: FulfillmentChannelPatronWeb when kind=illiad
	// and patron_web_base_url is configured, else "". It is independent of
	// Class/auto_if_unconditional — a profile can be auto_capable for
	// SUBMISSION (creating the request) with an empty FulfillmentChannel,
	// meaning papio still routes a fulfilled request to the Decision 4
	// manual reconciliation action rather than an automatic patron-web
	// retrieval: submission-auto is not end-to-end-auto. See
	// documentDeliveryOrUnset/doctor's document_delivery:*:result line and
	// GateEvaluated.FulfillmentChannel for the other two surfaces this
	// distinction is required to reach.
	FulfillmentChannel string
}

// CompileGateProfile compiles the static half of Decision 3A: the
// operator's Decision 2 declarations plus the provider capability table.
// It is a pure function of configuration — papio init must be able to
// print the compiled answer before saving, with no store open yet
// (Decision 3C) — so it never observes a live-acceptance record and always
// compiles at most GateClassPrefillOnly, carrying a trailing
// BlockerProviderNotAutoCapable / evidenceNoLiveAcceptance blocker whenever
// every other static condition holds. Service.ResolveGateProfile is the
// store-aware wrapper that folds in the real answer via WithLiveAcceptance.
func CompileGateProfile(inst config.Institution, profileName string) GateProfile {
	profile := GateProfile{ProfileName: profileName}

	dd := inst.DocumentDelivery
	if dd == nil {
		profile.Class = GateClassInvalid
		profile.Blockers = []Blocker{{
			Code:     BlockerInstitutionPolicyUnknown,
			Evidence: "no document_delivery is configured for this institution profile",
		}}
		return profile
	}

	profile.Provider = dd.Kind
	profile.SubmitPolicy = dd.SubmitPolicy
	profile.RequestClasses = append([]string(nil), dd.RequestClasses...)
	profile.PatronFeePolicy = dd.PatronFeePolicy
	profile.MonthlyRequestCap = dd.MonthlyRequestCap
	profile.StatusPollMinutes = dd.StatusPollMinutes
	if dd.Kind == "illiad" && dd.PatronWebBaseURL != "" {
		// Independent of Class: compiles alongside prefill_only just as
		// readily as auto_capable — a profile whose submission stays
		// human still gets automatic retrieval once *something* (a human
		// submission, or a future auto-submit) lands the request
		// fulfilled.
		profile.FulfillmentChannel = FulfillmentChannelPatronWeb
	}

	capa, known := providerCapabilities[dd.Kind]
	if !known {
		// Defensive: config.Load already fails closed on oclc/rapido/an
		// unrecognized kind, but CompileGateProfile must not assume every
		// caller went through that validator.
		profile.Class = GateClassInvalid
		profile.Blockers = []Blocker{{
			Code:     BlockerProviderNotImplemented,
			Evidence: quote(dd.Kind) + " has no delivery adapter",
		}}
		return profile
	}
	profile.SupportedRequestClasses = intersectClasses(dd.RequestClasses, capa.requestClasses)

	if !capa.canCreate {
		// Decision 3A: "Only source-controlled API integrations can
		// compile auto_capable — v1: illiad ... openurl, libkey, and
		// custom routes are permanently prefill-only." No other
		// declaration can outweigh this, so it is the profile's only
		// blocker.
		profile.Class = GateClassPrefillOnly
		profile.Blockers = []Blocker{{
			Code:     BlockerProviderNotAutoCapable,
			Evidence: "kind " + quote(dd.Kind) + " routes to a request form and supplies no submission-and-reconciliation contract",
		}}
		return profile
	}

	var blockers []Blocker
	if len(profile.SupportedRequestClasses) == 0 {
		blockers = append(blockers, Blocker{
			Code:     BlockerRequestClassUnsupported,
			Evidence: "no configured request_classes entry is supported by this provider (v1: digital_journal_article only)",
		})
	}
	if dd.SubmitPolicy != "auto_if_unconditional" {
		blockers = append(blockers, Blocker{
			Code:     BlockerInstitutionPolicyUnknown,
			Evidence: "submit_policy is " + orUnset(dd.SubmitPolicy) + ", not auto_if_unconditional",
		})
	}
	switch dd.PatronAttestation {
	case "not_required", "standing_completed":
	case "per_request":
		blockers = append(blockers, Blocker{
			Code:     BlockerPerRequestTerms,
			Evidence: "patron_attestation is per_request: a per-request patron declaration is required before creating this request",
		})
	default: // "" or "unknown"
		blockers = append(blockers, Blocker{
			Code:     BlockerInstitutionPolicyUnknown,
			Evidence: "patron_attestation is " + orUnset(dd.PatronAttestation),
		})
	}
	switch dd.PatronFeePolicy {
	case "zero_standard":
	case "per_request":
		blockers = append(blockers, Blocker{Code: BlockerPatronFeeNotZero, Evidence: "patron_fee_policy is per_request"})
	default: // "" or "unknown"
		blockers = append(blockers, Blocker{Code: BlockerPatronFeeUnknown, Evidence: "patron_fee_policy is " + orUnset(dd.PatronFeePolicy)})
	}
	if dd.LegalBasis == "copyright_act_s49" {
		// Decision 3A: under AU document supply the patron's declaration
		// is an affirmative, request-scoped statutory act papio must
		// never automate — this compiles prefill_only by law, not by
		// caution.
		blockers = append(blockers, Blocker{
			Code:     BlockerPerRequestCopyrightDeclaration,
			Evidence: "legal_basis is copyright_act_s49 (Australian document supply): the patron's per-request statutory declaration cannot be automated",
		})
	}
	if dd.APIKey == "" {
		blockers = append(blockers, Blocker{Code: BlockerAPICredentialMissing, Evidence: "api_key is not configured"})
	}
	if dd.PatronRef == "" {
		blockers = append(blockers, Blocker{Code: BlockerPatronMappingUnverified, Evidence: "patron_ref is not configured"})
	}
	if !capa.canReconcile {
		blockers = append(blockers, Blocker{
			Code:     BlockerReconciliationUnavailable,
			Evidence: "kind " + quote(dd.Kind) + " has no reconciliation capability",
		})
	}
	// Decision 3A's third hard rule: recorded live acceptance is necessary
	// but this pure function can never observe it (see the doc comment
	// above). Always compile as though it is outstanding; WithLiveAcceptance
	// is the only path to GateClassAutoCapable.
	blockers = append(blockers, Blocker{Code: BlockerProviderNotAutoCapable, Evidence: evidenceNoLiveAcceptance})

	profile.RequiresOperatorStep = requiresOperatorStep(blockers)
	profile.Class = GateClassPrefillOnly
	profile.Blockers = blockers
	return profile
}

// WithLiveAcceptance folds in the store-backed live-acceptance fact
// CompileGateProfile cannot observe. When accepted is true and the
// evidenceNoLiveAcceptance blocker was the profile's only blocker, this
// lifts it to GateClassAutoCapable; any other outstanding blocker leaves
// the profile exactly as compiled.
func (p GateProfile) WithLiveAcceptance(accepted bool) GateProfile {
	p.LiveAccepted = accepted
	if !accepted {
		return p
	}
	if len(p.Blockers) == 1 && p.Blockers[0].Code == BlockerProviderNotAutoCapable && p.Blockers[0].Evidence == evidenceNoLiveAcceptance {
		p.Class = GateClassAutoCapable
		p.Blockers = nil
	}
	return p
}

// Action is what Decision 3B's per-request gate tells the caller to do.
type Action string

const (
	ActionSubmit            Action = "submit"
	ActionPrefill           Action = "prefill"
	ActionEnrichThenPrefill Action = "enrich_then_prefill"
)

// Decision is EvaluateGate's verdict: what to do, and — for a prefill
// verdict driven by a closed-vocabulary reason — which blockers explain it.
type Decision struct {
	Action   Action
	Blockers []string
}

// GateRequest carries the per-request facts Decision 3B's gate needs beyond
// the compiled GateProfile: the job's effective access mode (already
// narrowed by config.NarrowAccessMode/EffectiveAccessMode), the request
// class, whether every provider-required field survived enrichment, and
// how many requests this profile+provider has already submitted this
// month.
type GateRequest struct {
	EffectiveAccessMode string
	RequestClass        string
	HasRequiredFields   bool
	SubmittedThisMonth  int
}

// EvaluateGate implements Decision 3B's seven-point per-request gate. Only
// an auto_capable profile can reach submit or enrich_then_prefill; any
// other class, or any condition false or unknown, routes to prefill — never
// permission, per the ADR ("Unknown is missing information, never
// permission").
func EvaluateGate(profile GateProfile, req GateRequest) Decision {
	if profile.Class != GateClassAutoCapable {
		return Decision{Action: ActionPrefill, Blockers: blockerCodes(profile.Blockers)}
	}

	var blockers []string

	// Condition 1: effective access mode is delegated. No blocker in the
	// closed vocabulary names an access-mode shortfall; conservative/
	// assisted callers already know why from the access mode itself.
	ok := req.EffectiveAccessMode == config.ModeDelegated
	// Condition 2: submit_policy is auto_if_unconditional. NOTE: conditions
	// 2, 5, and 6 are also compile-time facts — CompileGateProfile never
	// yields an auto_capable class when any of them fails, so through the
	// real ResolveGateProfile → EvaluateGate pipeline these branches are
	// defense-in-depth against a hand-built or future profile source, not
	// live per-request checks. The genuinely per-request conditions are 1
	// (access mode), 3 (request class), 4 (required fields), and 7 (cap).
	if profile.SubmitPolicy != "auto_if_unconditional" {
		ok = false
	}
	// Condition 3: the request class is supported and configured.
	if !profile.SupportedRequestClasses[req.RequestClass] {
		ok = false
		blockers = append(blockers, BlockerRequestClassUnsupported)
	}
	// Condition 5: no operator step is required before creating the
	// request (condition 4 — required fields — is handled separately
	// below, since it routes to enrich_then_prefill rather than prefill).
	if profile.RequiresOperatorStep {
		ok = false
	}
	// Condition 6: the zero-patron-fee policy applies to this request.
	switch profile.PatronFeePolicy {
	case "zero_standard":
	case "per_request":
		ok = false
		blockers = append(blockers, BlockerPatronFeeNotZero)
	default:
		ok = false
		blockers = append(blockers, BlockerPatronFeeUnknown)
	}
	// Condition 7: the monthly auto-submit cap has headroom. 0 means no
	// cap declared.
	if profile.MonthlyRequestCap > 0 && req.SubmittedThisMonth >= profile.MonthlyRequestCap {
		ok = false
	}

	if !ok {
		return Decision{Action: ActionPrefill, Blockers: blockers}
	}
	// Condition 4: every provider-required field is present after papio's
	// normal metadata enrichment. Metadata incomplete routes to
	// enrich_then_prefill, not prefill — a distinct action, not a blocker.
	if !req.HasRequiredFields {
		return Decision{Action: ActionEnrichThenPrefill}
	}
	return Decision{Action: ActionSubmit}
}

func blockerCodes(blockers []Blocker) []string {
	if len(blockers) == 0 {
		return nil
	}
	out := make([]string, len(blockers))
	for i, b := range blockers {
		out[i] = b.Code
	}
	return out
}

func requiresOperatorStep(blockers []Blocker) bool {
	for _, b := range blockers {
		switch b.Code {
		case BlockerPerRequestLogin, BlockerPerRequestTerms, BlockerPerRequestCopyrightDeclaration, BlockerPerRequestPurposeStatement:
			return true
		}
	}
	return false
}

func intersectClasses(configured []string, supported map[string]bool) map[string]bool {
	out := make(map[string]bool, len(configured))
	for _, c := range configured {
		if supported[c] {
			out[c] = true
		}
	}
	return out
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return quote(s)
}

func quote(s string) string { return `"` + s + `"` }
