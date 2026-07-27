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
// human action's Kind and RequiresAuth fields. It deliberately does not serve
// errcat: errcat is keyed on job state and transition reason rather than a
// HumanAction, so folding those axes together would make state guidance invent
// an action kind and repeat the wrong-command failure this prevents.
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
