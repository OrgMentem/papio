// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"strings"
	"testing"
)

// Accepting an unsafe-PDF review means "re-validate these exact bytes", never
// "file them as they are". It used to be refused outright, which left the three
// parked reviews measured on the live store 2026-08-27 with no resolution at
// all: reject only asked for a file papio already held.
//
// Nothing here waives the active-content rule. The verdict re-queues the same
// candidate for validation, and validateCandidate's encrypted/active branch has
// no review_override escape (internal/app/app.go), so a file papio cannot
// sanitize parks again rather than reaching the library.
func TestUnsafePDFAcceptRequeuesTheCandidateForValidation(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	id, err := js.CreateRequest(ctx, "wr_unsafe_accept", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.InsertCandidates(ctx, id, []Candidate{{
		JobID: id, Source: "unpaywall", URLRedacted: "https://x/1", URLKey: "k1",
		Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := js.NextPendingCandidate(ctx, id)
	if err != nil || candidate == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateNeedsReview, nil); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	actionID, err := js.OpenHumanAction(ctx, id, "unsafe_pdf",
		"PDF is encrypted or contains active/embedded content", Access(false, ""),
		WithHumanActionBinding(HumanActionBinding{
			CandidateID:      candidate.ID,
			QuarantinePath:   "/tmp/quarantined.pdf",
			QuarantineSHA256: sha,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	jobID, state, err := js.ResolveReview(ctx, actionID, "accept")
	if err != nil {
		t.Fatalf("accept on an unsafe-PDF review: %v", err)
	}
	if jobID != id {
		t.Fatalf("job = %q, want %q", jobID, id)
	}
	if state != StateFetching {
		t.Fatalf("state = %q, want %q so validation runs again", state, StateFetching)
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateFetching {
		t.Fatalf("state = %q, want %q so validation runs again", row.State, StateFetching)
	}
	again, err := js.GetCandidate(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ReviewOverride || again.Status != "pending" {
		t.Fatalf("candidate = %+v, want a pending review override", again)
	}
}
