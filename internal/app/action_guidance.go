// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import "papio/internal/job"

const actionsOpenCommand = "papio actions open"

// HumanActionNextStep is the actionable part of a human action. Command is
// empty when papio has no command that can perform the required human step.
type HumanActionNextStep struct {
	RequiresInstitutionalLogin bool
	Command                    string
	Instruction                string
}

// actionGuidanceDefaultOK records ActionKind values whose next step is
// intentionally the ordinary fallback below: no kind-specific command or
// instruction is available. Adding an ActionKind requires an explicit
// disposition here or a case in HumanActionNextStepFor.
var actionGuidanceDefaultOK = map[string]struct{}{
	job.ActionKindDocumentDelivery:        {},
	job.ActionKindDownloadsAccessRequired: {},
}

// HumanActionNextStepFor is the one authority for the next step implied by a
// human action's Kind and RequiresAuth fields. Every surface with the current
// action must use it, so a replacement action cannot inherit a command from
// the transition that created the action it replaced.
func HumanActionNextStepFor(action job.HumanAction) HumanActionNextStep {
	next := HumanActionNextStep{RequiresInstitutionalLogin: action.RequiresAuth}
	switch action.Kind {
	case "human_auth_required":
		next.RequiresInstitutionalLogin = true
	case "openurl_handoff":
		next.Command = actionsOpenCommand
	case "manual_download":
		// papio asked the human to fetch this file, so the human needs the page
		// it lives on. The route is the institution's, not the bare DOI: 32 of
		// the 34 open manual downloads measured on 2026-08-21 require auth, and
		// a canonical publisher link paywalls every one of them. `actions open`
		// resolves the same fresh route a handoff gets; the instruction says
		// what to do once there.
		next.Command = actionsOpenCommand
		next.Instruction = "download the PDF yourself — papio will adopt it"
	default:
		if _, ok := actionGuidanceDefaultOK[action.Kind]; !ok {
			// Unknown kinds retain the conservative fallback until the
			// disposition coverage test forces an explicit entry.
			return next
		}
	}
	return next
}
