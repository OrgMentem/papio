// Copyright 2026 OrgMentem. Licensed under MIT.

package job

import "strings"

// Diagnosis reasons are stable, agent-facing classifications. They describe
// the next human or operator step; they do not authorize that step.
const (
	DiagnosisReasonProviderAdapterMissing = "provider_adapter_missing"
	DiagnosisReasonProviderAdapterDrift   = "provider_adapter_drift"
	DiagnosisReasonAdoptedPDFInvalid      = "adopted_pdf_failed_validation"
	DiagnosisReasonWrongWork              = "wrong_work"
	DiagnosisReasonLandingPageOnly        = "landing_page_only"
	DiagnosisReasonInstitutionalHandoff   = "institutional_handoff"
	DiagnosisReasonHumanAuthRequired      = "human_auth_required"
	DiagnosisReasonTermsRequired          = "terms_acceptance_required"
	DiagnosisReasonIdentityReview         = "identity_review"
	DiagnosisReasonRetryWait              = "retry_wait"
	DiagnosisReasonInProgress             = "in_progress"
	DiagnosisReasonComplete               = "complete"
	DiagnosisReasonFailed                 = "failed"
	DiagnosisReasonUnavailable            = "unavailable"
	DiagnosisReasonUnknown                = "unknown"
)

// ActionDiagnosis is the daemon's explanation of one current human action.
// Capabilities describe explicit CLI operations only; they are not an
// instruction to perform them automatically.
type ActionDiagnosis struct {
	ActionID      int64  `json:"action_id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	Why           string `json:"why"`
	Next          string `json:"next"`
	Source        string `json:"source"`
	CanOpenAction bool   `json:"can_open_action"`
	NeedsBrowser  bool   `json:"needs_browser"`
}

// Diagnosis is a bounded, read-only explanation of a job's current state.
// It intentionally contains no raw event history, URLs, or filesystem paths;
// use jobs.get or adapter diagnose when that evidence is needed.
type Diagnosis struct {
	JobID           string           `json:"job_id"`
	State           string           `json:"state"`
	Work            string           `json:"work"`
	Title           string           `json:"title,omitempty"`
	Reason          string           `json:"reason"`
	Why             string           `json:"why"`
	Next            string           `json:"next"`
	Source          string           `json:"source"`
	ProviderOutcome string           `json:"provider_outcome,omitempty"`
	AdapterID       string           `json:"adapter_id,omitempty"`
	AdapterVersion  string           `json:"adapter_version,omitempty"`
	CanRetry        bool             `json:"can_retry"`
	NeedsBrowser    bool             `json:"needs_browser"`
	Action          *ActionDiagnosis `json:"action,omitempty"`
}

// Diagnose folds the current job row, its actions, and its append-only events
// into one operator explanation. Events are oldest first, as returned by
// Store.Events. The classifier prefers the latest provider outcome because it
// records what the browser actually observed, then uses the action detail for
// resolver and adoption paths that have no provider outcome.
func Diagnose(row *Row, actions []HumanAction, events []map[string]any) Diagnosis {
	if row == nil {
		return Diagnosis{Reason: DiagnosisReasonUnknown, Why: "the job could not be loaded", Next: "run papio jobs get with a valid job id", Source: "input"}
	}

	d := Diagnosis{
		JobID:    row.ID,
		State:    row.State,
		Work:     row.Work.Describe(),
		Title:    row.Work.Title,
		Source:   "state",
		CanRetry: row.State == StateRetryWait || row.State == StateFailed || row.State == StateUnavailable,
	}

	outcome, adapterID, adapterVersion, providerDetail, hasProvider := latestProviderOutcome(events)
	if hasProvider {
		d.ProviderOutcome = outcome
		d.AdapterID = adapterID
		d.AdapterVersion = adapterVersion
	}

	var current *HumanAction
	for i := range actions {
		if actions[i].JobID != row.ID || actions[i].Status != "open" {
			continue
		}
		if current == nil || actions[i].ID > current.ID {
			current = &actions[i]
		}
	}
	if current != nil {
		action := classifyAction(*current, outcome, providerDetail)
		d.Action = &action
		d.Reason, d.Why, d.Next, d.Source = action.Reason, action.Why, action.Next, action.Source
		d.NeedsBrowser = action.NeedsBrowser
		return d
	}

	switch row.State {
	case StateRetryWait:
		d.Reason = DiagnosisReasonRetryWait
		d.Why = "the job is waiting for its next scheduled acquisition attempt"
		d.Next = "wait for the scheduled retry, or run papio jobs retry when an explicit retry is appropriate"
	case StateFailed:
		d.Reason = DiagnosisReasonFailed
		d.Why = "the acquisition failed without an open human action"
		d.Next = "inspect papio jobs get and retry only after addressing the recorded failure"
	case StateUnavailable:
		d.Reason = DiagnosisReasonUnavailable
		d.Why = "papio has no remaining automatic route for this job"
		d.Next = "inspect papio jobs get and decide whether an explicit retry is warranted"
	case StateReady, StateImported:
		d.Reason = DiagnosisReasonComplete
		d.Why = "the acquisition reached a terminal successful state"
		d.Next = "export or consume the acquired artifact"
	case StateAwaitingHuman, StateNeedsReview:
		d.Reason = DiagnosisReasonUnknown
		d.Why = "the job is waiting for a human action that is not currently open"
		d.Next = "run papio jobs get and papio actions list to inspect the durable action history"
	default:
		d.Reason = DiagnosisReasonInProgress
		d.Why = "papio is still processing the acquisition"
		d.Next = "wait for the next durable job transition"
	}
	return d
}

func classifyAction(action HumanAction, outcome, providerDetail string) ActionDiagnosis {
	result := ActionDiagnosis{
		ActionID:      action.ID,
		Kind:          action.Kind,
		Status:        action.Status,
		Source:        "action",
		CanOpenAction: action.Status == "open" && (action.Kind == "manual_download" || action.Kind == "openurl_handoff"),
		NeedsBrowser:  action.Kind == "manual_download" || action.Kind == "openurl_handoff" || action.Kind == "human_auth_required" || action.Kind == "terms_acceptance_required",
	}
	text := strings.ToLower(action.Detail + " " + providerDetail)
	if strings.TrimSpace(outcome) != "" {
		result.Source = "provider_outcome"
	}
	reason := ""
	switch {
	case strings.Contains(text, "no adapter") || strings.Contains(text, "no source-controlled adapter"):
		reason = DiagnosisReasonProviderAdapterMissing
		result.Why = "no compiled provider adapter matched the page papio reached"
		result.Next = "open the handoff and download the requested PDF; inspect papio adapter captures when contributing an adapter"
	case strings.Contains(text, "adopted download failed validation"):
		reason = DiagnosisReasonAdoptedPDFInvalid
		result.Why = "the browser supplied a file, but papio's PDF validation rejected it"
		result.Next = "replace the file with a PDF that matches the requested work"
	case outcome == "wrong_work" || strings.Contains(text, "different work"):
		reason = DiagnosisReasonWrongWork
		result.Why = "the browser reached a different work than the requested work"
		result.Next = "find and download the requested PDF"
	case strings.Contains(text, "could not drive") || outcome == "ui_changed":
		reason = DiagnosisReasonProviderAdapterDrift
		result.Why = "a provider page did not reach a safe state for the installed adapter"
		result.Next = "open the handoff and download the requested PDF; inspect papio adapter diagnose for adapter repair"
	case outcome == "landing_only" || strings.Contains(text, "landing page") || strings.Contains(text, "no verified direct pdf"):
		reason = DiagnosisReasonLandingPageOnly
		result.Why = "a resolver returned a landing page without a verified direct PDF"
		result.Next = "open the page and download the requested PDF"
	case action.Kind == "human_auth_required":
		reason = DiagnosisReasonHumanAuthRequired
		result.Why = "the provider requires the operator's browser session to authenticate"
		result.Next = "complete sign-in or MFA in the browser; papio will resume afterward"
	case action.Kind == "terms_acceptance_required":
		reason = DiagnosisReasonTermsRequired
		result.Why = "the provider requires an operator-mediated terms step"
		result.Next = "review and accept the provider terms in the browser if you agree"
	case action.Kind == "verify_identity":
		reason = DiagnosisReasonIdentityReview
		result.Why = "papio needs a human identity decision for the quarantined PDF"
		result.Next = "review the PDF and resolve the identity action"
	case action.Kind == "openurl_handoff":
		reason = DiagnosisReasonInstitutionalHandoff
		result.Why = "the requested work needs the operator's institutional browser route"
		result.Next = "open the handoff and complete the provider step in the browser"
	case action.Kind == "manual_download":
		reason = DiagnosisReasonInstitutionalHandoff
		result.Why = "papio needs a human-supplied PDF for this acquisition"
		result.Next = "open the handoff and download the requested PDF"
	default:
		reason = DiagnosisReasonUnknown
		result.Why = "papio has recorded a human action whose next step is not classified"
		result.Next = "inspect the action with papio jobs get"
	}
	result.Reason = reason
	return result
}

func latestProviderOutcome(events []map[string]any) (outcome, adapterID, adapterVersion, detail string, ok bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "browser.provider_outcome" {
			continue
		}
		value, _ := events[i]["detail"].(map[string]any)
		return stringValue(value, "outcome"), stringValue(value, "adapter_id"), stringValue(value, "adapter_version"), stringValue(value, "detail"), true
	}
	return "", "", "", "", false
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
