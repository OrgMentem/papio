// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"testing"

	"papio/internal/job"
)

func TestHumanActionNextStepFor(t *testing.T) {
	kinds := []struct {
		kind        string
		forcesLogin bool
		command     string
		instruction string
	}{
		{kind: "openurl_handoff", command: actionsOpenCommand},
		{kind: "manual_download", instruction: "download the PDF yourself — papio will adopt it"},
		{kind: "verify_identity"},
		{kind: "human_auth_required", forcesLogin: true},
		{kind: "terms_acceptance_required"},
		{kind: "openurl_available"},
		{kind: "downloads_access_required"},
	}
	for _, kind := range kinds {
		for _, requiresAuth := range []bool{false, true} {
			authName := "false"
			if requiresAuth {
				authName = "true"
			}
			t.Run(kind.kind+"/requires_auth="+authName, func(t *testing.T) {
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
