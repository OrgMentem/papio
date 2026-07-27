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
		next.Instruction = "download the PDF yourself — papio will adopt it"
	}
	return next
}
