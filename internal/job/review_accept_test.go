// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// papio deliberately never files a PDF carrying active or embedded content, so
// an unsafe-PDF review offers reject and dismiss but not accept. The refusal
// used to reuse ErrHumanActionKind, telling the operator the action's KIND was
// unsupported - which is false, since reject resolves that exact action, and it
// reads as papio not recognising its own ask. Measured on the live store: three
// parked unsafe-PDF reviews, the oldest eight days old, whose only diagnosis
// was `human action 965 has unsupported kind "unsafe_pdf"`.
func TestUnsafePDFAcceptRefusalNamesTheRuleNotTheKind(t *testing.T) {
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
	actionID, err := js.OpenHumanAction(ctx, id, "unsafe_pdf",
		"PDF is encrypted or contains active/embedded content", Access(false, ""),
		WithHumanActionBinding(HumanActionBinding{
			CandidateID:      candidate.ID,
			QuarantinePath:   "/tmp/quarantined.pdf",
			QuarantineSHA256: strings.Repeat("a", 64),
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = js.ResolveReview(ctx, actionID, "accept")
	if err == nil {
		t.Fatal("accept on an unsafe-PDF review must be refused")
	}
	var typed *ErrReviewAcceptUnavailable
	if !errors.As(err, &typed) {
		t.Fatalf("error = %T (%v), want *ErrReviewAcceptUnavailable", err, err)
	}
	message := err.Error()
	for _, want := range []string{"active or embedded content", "reject", "dismiss"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal = %q, want it to mention %q", message, want)
		}
	}
	if strings.Contains(message, "unsupported kind") {
		t.Fatalf("refusal still blames the action kind: %q", message)
	}

	// The same action resolves by rejection: the kind was never the problem.
	if _, _, err := js.ResolveReview(ctx, actionID, "reject"); err != nil {
		t.Fatalf("reject on the same action: %v", err)
	}
}
