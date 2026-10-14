// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"fmt"
	"testing"

	"papio/internal/job"
)

// humanActionNextSteps lists every action kind the daemon records, with the
// next step HumanActionNextStepFor owes it. Reminder tests iterate the same
// list, so a kind added here is covered on both surfaces.
var humanActionNextSteps = []struct {
	kind        string
	forcesLogin bool
	command     string
	instruction string
}{
	{kind: "openurl_handoff", command: actionsOpenCommand},
	{kind: "manual_download", command: actionsOpenCommand, instruction: "download the PDF yourself — papio will adopt it"},
	{kind: "verify_identity"},
	{kind: "unsafe_pdf"},
	{kind: "validation_error"},
	{kind: "human_auth_required", forcesLogin: true},
	{kind: "terms_acceptance_required"},
	{kind: "openurl_available"},
	{kind: job.ActionKindDownloadsAccessRequired},
	{kind: job.ActionKindDocumentDelivery},
}

func TestHumanActionNextStepFor(t *testing.T) {
	for _, kind := range humanActionNextSteps {
		for _, requiresAuth := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/requires_auth=%t", kind.kind, requiresAuth), func(t *testing.T) {
				next := HumanActionNextStepFor(job.HumanAction{
					Kind: kind.kind, RequiresAuth: requiresAuth,
					Detail: "ignored", BlockedBy: "paywall",
				})
				if want := requiresAuth || kind.forcesLogin; next.RequiresInstitutionalLogin != want {
					t.Fatalf("requires institutional login = %t, want %t", next.RequiresInstitutionalLogin, want)
				}
				if next.Command != kind.command {
					t.Fatalf("command = %q, want %q", next.Command, kind.command)
				}
				if next.Instruction != kind.instruction {
					t.Fatalf("instruction = %q, want %q", next.Instruction, kind.instruction)
				}
			})
		}
	}
}
