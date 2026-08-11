// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

// InstitutionCutoverBlocker is the closed, privacy-safe classification of the
// fact that prevented an institutional cutover at a processing decision.
type InstitutionCutoverBlocker string

const (
	InstitutionCutoverBlockerNone                    InstitutionCutoverBlocker = "none"
	InstitutionCutoverBlockerSourceGateOnly          InstitutionCutoverBlocker = "source_gate_only"
	InstitutionCutoverBlockerLiveSourceRemaining     InstitutionCutoverBlocker = "live_source_remaining"
	InstitutionCutoverBlockerTransientRetryRemaining InstitutionCutoverBlocker = "transient_retry_remaining"
	InstitutionCutoverBlockerNoLegalRoute            InstitutionCutoverBlocker = "no_legal_route"
	InstitutionCutoverBlockerPolicyGate              InstitutionCutoverBlocker = "policy_gate"
	InstitutionCutoverBlockerIdentifierGate          InstitutionCutoverBlocker = "identifier_gate"
)

// InstitutionCutoverBlockerKey and CanaryReadyRouteExistsKey are stable keys
// in job.transition details. Their values are deliberately closed and contain
// no provider, route, URL, filesystem, or work identifiers.
const (
	InstitutionCutoverBlockerKey = "institution_cutover_blocker"
	CanaryReadyRouteExistsKey    = "canary_ready_route_exists"

	// InstitutionCutoverCanaryReadyRouteExistsKey is the fully qualified alias
	// used by callers that keep all cutover detail keys together.
	InstitutionCutoverCanaryReadyRouteExistsKey = CanaryReadyRouteExistsKey
)

// InstitutionCutoverDecision is the complete decision payload. None is an
// explicit value: a recorded decision never omits its blocker.
type InstitutionCutoverDecision struct {
	Blocker                InstitutionCutoverBlocker `json:"institution_cutover_blocker"`
	CanaryReadyRouteExists bool                      `json:"canary_ready_route_exists"`
}

// NormalizeInstitutionCutoverBlocker returns the closed vocabulary value, or
// the empty sentinel for an unknown value. Stored values are intentionally
// matched exactly so malformed details fail closed.
func NormalizeInstitutionCutoverBlocker(value string) InstitutionCutoverBlocker {
	switch InstitutionCutoverBlocker(value) {
	case InstitutionCutoverBlockerNone,
		InstitutionCutoverBlockerSourceGateOnly,
		InstitutionCutoverBlockerLiveSourceRemaining,
		InstitutionCutoverBlockerTransientRetryRemaining,
		InstitutionCutoverBlockerNoLegalRoute,
		InstitutionCutoverBlockerPolicyGate,
		InstitutionCutoverBlockerIdentifierGate:
		return InstitutionCutoverBlocker(value)
	default:
		return ""
	}
}

// Valid reports whether the decision is complete and uses the closed blocker
// vocabulary. The boolean field is intentionally always required by the map
// parser; its false value is meaningful in Phase 0.
func (d InstitutionCutoverDecision) Valid() bool {
	return NormalizeInstitutionCutoverBlocker(string(d.Blocker)) != ""
}

// NormalizeInstitutionCutoverDecision normalizes a decision value and returns
// the zero decision for an unknown blocker.
func NormalizeInstitutionCutoverDecision(decision InstitutionCutoverDecision) InstitutionCutoverDecision {
	decision.Blocker = NormalizeInstitutionCutoverBlocker(string(decision.Blocker))
	if decision.Blocker == "" {
		return InstitutionCutoverDecision{}
	}
	return decision
}

// CutoverDecisionDetail returns the stable, privacy-safe detail fields for a
// transition. Callers should merge this map into the decisive transition
// detail, never emit a separate event.
func CutoverDecisionDetail(decision InstitutionCutoverDecision) map[string]any {
	decision = NormalizeInstitutionCutoverDecision(decision)
	if !decision.Valid() {
		return nil
	}
	return map[string]any{
		InstitutionCutoverBlockerKey: string(decision.Blocker),
		CanaryReadyRouteExistsKey:    decision.CanaryReadyRouteExists,
	}
}

// WithCutoverDecision copies detail and adds one complete decision payload.
// It returns nil for an invalid decision so callers cannot accidentally record
// a partial or open-ended classification.
func WithCutoverDecision(detail map[string]any, decision InstitutionCutoverDecision) map[string]any {
	fields := CutoverDecisionDetail(decision)
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(detail)+len(fields))
	for key, value := range detail {
		out[key] = value
	}
	for key, value := range fields {
		out[key] = value
	}
	return out
}

// ParseInstitutionCutoverDecision parses a transition detail map strictly:
// both stable keys must be present, the blocker must be known, and the route
// flag must be a JSON boolean. Other transition detail fields are ignored.
func ParseInstitutionCutoverDecision(detail map[string]any) (InstitutionCutoverDecision, bool) {
	if detail == nil {
		return InstitutionCutoverDecision{}, false
	}
	blocker, ok := detail[InstitutionCutoverBlockerKey].(string)
	if !ok {
		return InstitutionCutoverDecision{}, false
	}
	canary, ok := detail[CanaryReadyRouteExistsKey].(bool)
	if !ok {
		return InstitutionCutoverDecision{}, false
	}
	decision := InstitutionCutoverDecision{
		Blocker:                NormalizeInstitutionCutoverBlocker(blocker),
		CanaryReadyRouteExists: canary,
	}
	if !decision.Valid() {
		return InstitutionCutoverDecision{}, false
	}
	return decision, true
}

// ParseCutoverDecision is the concise alias used by diagnosis projections.
func ParseCutoverDecision(detail map[string]any) (InstitutionCutoverDecision, bool) {
	return ParseInstitutionCutoverDecision(detail)
}

// LatestInstitutionCutoverDecision returns the decision on the newest
// relevant event: job.transition or job.retry_requested. A retry request
// starts a new decision epoch, so it deliberately clears an older transition
// payload; an incidental transition after a decision also clears the
// projection.
func LatestInstitutionCutoverDecision(events []map[string]any) (InstitutionCutoverDecision, bool) {
	var (
		latestKind   string
		latestDetail map[string]any
	)
	for _, event := range events {
		kind, _ := event["kind"].(string)
		if kind != "job.transition" && kind != "job.retry_requested" {
			continue
		}
		latestKind = kind
		latestDetail, _ = event["detail"].(map[string]any)
	}
	if latestKind != "job.transition" || latestDetail == nil {
		return InstitutionCutoverDecision{}, false
	}
	return ParseInstitutionCutoverDecision(latestDetail)
}
