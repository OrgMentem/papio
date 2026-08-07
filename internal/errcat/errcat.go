// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package errcat turns job status into actionable, user-facing categories and
// next steps. It is shared by every surface that reports why a job is parked
// or settled without a file — the CLI (`papio status`, `papio acquire --wait`)
// and the MCP `papio_status` tool — so humans and agents get the same
// diagnosis from one catalog.
package errcat

import (
	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
)

// Explanation is an actionable interpretation of a job's state: a short, stable
// category plus one concrete next step the user or agent can take.
type Explanation struct {
	Category string
	Guidance string
}

// Explain maps a job's state and latest transition reason — and, for the
// no-access case, the job's snapshotted access mode plus the current
// configuration — into an actionable category and next step.
//
// The daemon's snake_case reason strings are consumed as a read-only contract.
// Unknown reasons fall back per state, so a new daemon reason never renders a
// blank category; it degrades to the generic guidance for that state.
func Explain(state, reason, resolver, accessMode string, cfg config.Config) Explanation {
	switch reason {
	case "institutional_handoff":
		return Explanation{"login_required",
			"Sign in at your institution in the browser, then run `papio actions open` to launch the handoff tab. If the sign-in page reports a stale or expired request, run it again — every open mints a fresh link."}
	case "open_access_browser_handoff":
		return Explanation{"browser_fetch_pending",
			"An open-access copy needs a browser fetch; run `papio actions open` to complete it. No login is required."}
	case "landing_page_only":
		return Explanation{"manual_download",
			"The link resolved to a landing page, not a PDF; open the handoff and download the PDF manually."}
	case "validation_error":
		return Explanation{"validation_incomplete",
			"PDF validation could not finish within its bounds; inspect the quarantined file, then re-run or override."}
	case "encrypted_or_active_content":
		return Explanation{"unsafe_pdf",
			"The PDF is encrypted or carries active/embedded content; review it before adopting."}
	case "semantic_or_identity_review":
		return Explanation{"identity_review",
			"Confirm the downloaded PDF is the requested paper; approve it to finish, or reject to try another source."}
	case "resolver_temporarily_unavailable", "candidate_temporarily_unavailable", "acquisition_inputs_temporarily_unavailable":
		return Explanation{"retrying",
			"A source was temporarily unavailable; papio will retry automatically. No action needed."}
	case "document_delivery_pending":
		return Explanation{"document_delivery_pending",
			"A document-delivery request is lodged; papio is polling the provider. No action needed."}
	case "no_identifier":
		// The single most expensive wrong answer papio can give is "sign in" for
		// a work no login can deliver. Name what is missing and the one remedy
		// that closes the loop, and say plainly that authenticating will not help.
		return Explanation{"no_identifier",
			"No DOI, PMID, or arXiv id could be confirmed for this title — books, chapters, reports, and theses usually have none. An institutional sign-in cannot make an identifier-less request fetchable. Find a DOI and re-submit with `papio acquire --doi <doi>`; for a Zotero item, apply `zotio --yes items enrich --missing-doi` then re-run `papio acquire --from-zotio`."}
	case "doi_not_registered":
		// Same principle as no_identifier, one step further in: the identifier
		// is present and well-formed but names nothing. Say so plainly, because
		// the user's instinct on a paywall message is to go sign in again.
		return Explanation{"doi_not_registered",
			"This DOI is not registered with the DOI system, so it resolves to a \"DOI NOT FOUND\" page and no link resolver can match it — almost always a typo or a mangled copy-paste. Signing in will not help. Check the DOI against the article's own page and re-submit with `papio acquire --doi <doi>`."}
	case "candidates_exhausted", "no_legal_candidates":
		return explainNoAccess(resolver, accessMode, cfg)
	}

	// Fall back per state so nothing renders blank when the daemon emits a
	// reason this catalog does not yet name.
	switch state {
	case "awaiting_human":
		return Explanation{"action_required",
			"This job is waiting on a browser action; run `papio actions open`."}
	case "needs_review":
		return Explanation{"review_required",
			"This job needs human review; see `papio actions` and approve or reject it."}
	case "unavailable":
		return explainNoAccess(resolver, accessMode, cfg)
	case "failed":
		return Explanation{"failed",
			"This job hit an unexpected error; check its recent events with `papio jobs` and re-submit if needed."}
	case "cancelled":
		return Explanation{"cancelled", "This job was cancelled."}
	}
	return Explanation{}
}

// ExplainWithOpenAction gives a current open human action precedence over the
// transition that first parked the job. Handoff maintenance may replace that
// action later, so the transition reason can no longer safely describe what
// the user can do now.
func ExplainWithOpenAction(state, reason, resolver, accessMode string, actions []job.HumanAction, cfg config.Config) Explanation {
	if action, ok := latestOpenAction(actions); ok {
		return explainOpenAction(action)
	}
	return Explain(state, reason, resolver, accessMode, cfg)
}

func latestOpenAction(actions []job.HumanAction) (job.HumanAction, bool) {
	var current job.HumanAction
	found := false
	for _, action := range actions {
		if action.Status != "open" || (found && action.ID <= current.ID) {
			continue
		}
		current = action
		found = true
	}
	return current, found
}

func explainOpenAction(action job.HumanAction) Explanation {
	next := app.HumanActionNextStepFor(action)
	switch action.Kind {
	case "openurl_handoff":
		if next.Command == "" {
			break
		}
		if next.RequiresInstitutionalLogin {
			return Explanation{"login_required",
				"Sign in at your institution in the browser, then run `" + next.Command + "` to launch the handoff tab. If the sign-in page reports a stale or expired request, run it again — every open mints a fresh link."}
		}
		return Explanation{"browser_fetch_pending",
			"An open-access copy needs a browser fetch; run `" + next.Command + "` to complete it. No login is required."}
	case "manual_download":
		if next.Instruction == "" {
			break
		}
		if next.RequiresInstitutionalLogin {
			return Explanation{"manual_download",
				"Sign in at your institution in the browser, then " + next.Instruction + "."}
		}
		return Explanation{"manual_download", "You need to " + next.Instruction + ". No login is required."}
	case "validation_error":
		return Explanation{"validation_incomplete",
			"PDF validation could not finish within its bounds; inspect the quarantined file, then re-run or override."}
	case "unsafe_pdf":
		return Explanation{"unsafe_pdf",
			"The PDF is encrypted or carries active/embedded content; review it before adopting."}
	case "verify_identity":
		return Explanation{"identity_review",
			"Confirm the downloaded PDF is the requested paper; approve it to finish, or reject to try another source."}
	case "terms_acceptance_required":
		return Explanation{"terms_acceptance_required",
			"Review and accept the provider's terms in the browser, then retry the acquisition."}
	case "openurl_available":
		return Explanation{"openurl_available",
			"An institutional OpenURL route is available; set access_mode to \"assisted\" or \"delegated\" and retry the acquisition."}
	}
	if next.RequiresInstitutionalLogin {
		return Explanation{"login_required",
			"Sign in at your institution in the browser, then complete the requested human action."}
	}
	return Explanation{"action_required", "This job is waiting on a human action; see `papio actions list` for details."}
}

// explainNoAccess distinguishes the reasons a job found no accessible copy. The
// highest-value case for a new user is that no institution is configured, so
// institutional access was never attempted — a fixable setup gap, not a dead
// end. The job's snapshotted access mode says what the job actually did; the
// current config says whether an institution is now configurable to fix it.
func explainNoAccess(resolver, accessMode string, cfg config.Config) Explanation {
	switch accessMode {
	case config.ModeAssisted, config.ModeDelegated:
		if _, ok := cfg.InstitutionFor(resolver); !ok {
			return Explanation{"institution_not_configured",
				"No institution is configured, so institutional access was never attempted. Run `papio init` and set your library's OpenURL resolver base (Institution step)."}
		}
		return Explanation{"no_access",
			"No open-access copy exists and your institution's OpenURL resolver returned no entitled full text."}
	case config.ModeConservative:
		return Explanation{"no_access_conservative",
			"Conservative mode only checks open sources. Set access_mode to \"assisted\" or \"delegated\" to route this work through your institution."}
	}
	return Explanation{"no_access", "No legally accessible copy was found for this work."}
}

// WaitGuidanceWithOpenAction is the acquire-side form for a job detail, where
// a live action supersedes the reason recorded when the job first parked.
func WaitGuidanceWithOpenAction(state, reason, resolver, accessMode string, actions []job.HumanAction, cfg config.Config) string {
	return renderWaitGuidance(state, ExplainWithOpenAction(state, reason, resolver, accessMode, actions, cfg))
}

func renderWaitGuidance(state string, exp Explanation) string {
	switch state {
	case "awaiting_human", "needs_review", "unavailable", "failed", "cancelled":
	default:
		return ""
	}
	if exp.Category == "" {
		return ""
	}
	return "  [" + exp.Category + "]\n    → " + exp.Guidance
}
