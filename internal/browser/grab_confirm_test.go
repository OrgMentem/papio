// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"strings"
	"testing"

	"papio/internal/grab"
	"papio/internal/pdf"
)

func TestConfirmBindsThePickedJob(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_conf_vor", vor)
	preprintJob := parkManualDownload(t, jobs, "wr_conf_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Confirm Pick")

	// The human picks the candidate the predicate would only have offered for
	// review. That is the whole point of asking: they know which of two
	// versions they wanted, and the rule does not.
	res := b.ConfirmGrabCandidate(ctx, grabID, preprintJob)
	if res.Outcome != "job_created" {
		t.Fatalf("outcome = %q detail = %q, want job_created", res.Outcome, res.Detail)
	}
	if res.JobID != preprintJob {
		t.Fatalf("job = %q, want the picked job %q", res.JobID, preprintJob)
	}
	got, err := b.grabs.Get(ctx, grabID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", got, err)
	}
	if got.State != grab.StateJobCreated || got.JobID != preprintJob {
		t.Fatalf("grab = %+v, want bound to the picked job", got)
	}

	// The audit trail must record that a human decided this, and must NOT
	// claim it as an autonomous filing: grabs.binds exists to answer "what did
	// papio do while nobody was looking", and a confirmed pick is the opposite.
	if !strings.Contains(got.BindProvenance, "operator_confirm") {
		t.Fatalf("provenance = %q, want method operator_confirm", got.BindProvenance)
	}
	binds, err := b.grabs.ListAutonomousBinds(ctx, 10)
	if err != nil {
		t.Fatalf("ListAutonomousBinds: %v", err)
	}
	for _, bind := range binds {
		if bind.GrabID == grabID {
			t.Fatalf("confirmed pick %s appears in the autonomous bind audit: %+v", grabID, bind)
		}
	}
	// Confirming twice must not double-file: the grab has left the parked state.
	if again := b.ConfirmGrabCandidate(ctx, grabID, preprintJob); again.Outcome != "wrong_state" {
		t.Fatalf("second confirm outcome = %q, want wrong_state", again.Outcome)
	}
}

// TestConfirmRefusesAPickTheDocumentContradicts covers the one place a human's
// pick loses. A parked capture can carry a conclusive front-matter DOI: the
// mismatch park path (processSettledGrab's errGrabIdentityMismatch loop) parks a
// grab whose printed DOI named a ready bundle that MatchIdentity rejected. Those
// bytes state their own identity, so a pick naming a different work is refused —
// the human is authority on which pending paper they meant, never on what the
// bytes are.
func TestConfirmRefusesAPickTheDocumentContradicts(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_veto_vor", vor)
	preprintJob := parkManualDownload(t, jobs, "wr_veto_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Confirm Veto")

	// Confirm re-reads the bytes, so the report it sees is what decides. These
	// bytes conclusively name a third work inside the blind front-matter window.
	contradicting := "Some Other Paper Entirely\nBartholomew Quincy\nDOI: 10.7777/contradiction.1\n\nAbstract\nBody.\n"
	if got := pdf.FrontMatterDOIs(contradicting); len(got) == 0 {
		t.Fatalf("fixture has no conclusive front-matter DOI; the veto could not fire")
	}
	b.svc.Validate = validateForExcerpt(contradicting)

	res := b.ConfirmGrabCandidate(ctx, grabID, preprintJob)
	if res.Outcome != "refused_identity" {
		t.Fatalf("outcome = %q detail = %q, want refused_identity", res.Outcome, res.Detail)
	}
	if res.Detail == "" {
		t.Fatal("refusal carries no detail; the operator cannot tell what the document claimed")
	}
	got, _ := b.grabs.Get(ctx, grabID)
	if got.State != grab.StateParkedNoIdentifier || got.JobID != "" {
		t.Fatalf("grab = %+v, want untouched after a refused pick", got)
	}
	if got.BindProvenance != "" {
		t.Fatalf("provenance = %q, want none written for a refusal", got.BindProvenance)
	}
}

func TestConfirmGuards(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	vor, preprint := versionPair()
	parkManualDownload(t, jobs, "wr_guard_vor", vor)
	parkManualDownload(t, jobs, "wr_guard_pre", preprint)
	grabID := parkedGrabWithPool(t, b, cfg.EffectiveAdoptionRoot(), exoticExcerpt(vor), "Confirm Guards")

	if got := b.ConfirmGrabCandidate(ctx, "grab_does_not_exist", "job_whatever"); got.Outcome != "unknown_grab" {
		t.Fatalf("unknown grab outcome = %q, want unknown_grab", got.Outcome)
	}
	// A job outside the candidate-eligible pool must be unreachable. The pool
	// is what the suggestion list was drawn from, so a stale page cannot bind
	// captured bytes to an arbitrary job id it happens to know.
	if got := b.ConfirmGrabCandidate(ctx, grabID, "job_00000000000000000000000999"); got.Outcome != "unknown_job" {
		t.Fatalf("absent job outcome = %q, want unknown_job", got.Outcome)
	}
}
