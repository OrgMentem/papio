// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package errcat turns the daemon's internal job transition reasons into
// actionable, user-facing categories and next steps. It is shared by every
// surface that reports why a job is parked or settled without a file — the CLI
// (`papio status`, `papio acquire --wait`) and the MCP `papio_status` tool — so
// humans and agents get the same diagnosis from one catalog.
package errcat

import "papio/internal/config"

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
			"Sign in at your institution in the browser, then run `papio actions --open` to launch the handoff tab."}
	case "open_access_browser_handoff":
		return Explanation{"browser_fetch_pending",
			"An open-access copy needs a browser fetch; run `papio actions --open` to complete it."}
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
	case "resolver_temporarily_unavailable", "candidate_temporarily_unavailable":
		return Explanation{"retrying",
			"A source was temporarily unavailable; papio will retry automatically. No action needed."}
	case "candidates_exhausted", "no_legal_candidates":
		return explainNoAccess(resolver, accessMode, cfg)
	}

	// Fall back per state so nothing renders blank when the daemon emits a
	// reason this catalog does not yet name.
	switch state {
	case "awaiting_human":
		return Explanation{"action_required",
			"This job is waiting on a browser action; run `papio actions --open`."}
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

// explainNoAccess distinguishes the reasons a job found no accessible copy. The
// highest-value case for a new user is that no institution is configured, so
// institutional access was never attempted — a fixable setup gap, not a dead
// end. The job's snapshotted access mode says what the job actually did; the
// current config says whether an institution is now configurable to fix it.
func explainNoAccess(resolver, accessMode string, cfg config.Config) Explanation {
	switch accessMode {
	case config.ModeAssisted, config.ModeMaximal:
		if _, ok := cfg.InstitutionFor(resolver); !ok {
			return Explanation{"institution_not_configured",
				"No institution is configured, so institutional access was never attempted. Run `papio init` and set your library's OpenURL resolver base (Institution step)."}
		}
		return Explanation{"no_access",
			"No open-access copy exists and your institution's OpenURL resolver returned no entitled full text."}
	case config.ModeConservative:
		return Explanation{"no_access_conservative",
			"Conservative mode only checks open sources. Set access_mode to \"assisted\" or \"maximal\" to route this work through your institution."}
	}
	return Explanation{"no_access", "No legally accessible copy was found for this work."}
}

// WaitGuidance renders a category and next-step block for a job that
// `papio acquire --wait` settled into a parked or no-file terminal state, or ""
// for success and states that need no user action. It is the acquire-side twin
// of the status dashboard's per-job guidance.
func WaitGuidance(state, reason, resolver, accessMode string, cfg config.Config) string {
	switch state {
	case "awaiting_human", "needs_review", "unavailable", "failed", "cancelled":
	default:
		return ""
	}
	exp := Explain(state, reason, resolver, accessMode, cfg)
	if exp.Category == "" {
		return ""
	}
	return "  [" + exp.Category + "]\n    \u2192 " + exp.Guidance
}
