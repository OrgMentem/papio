// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"fmt"
	"testing"
)

func TestTerminalReasonVocabularyCoversEveryWriter(t *testing.T) {
	// Keep this table aligned with every terminal transition writer: app
	// exhaustion/identifier paths, browser handoff outcomes, cancellation, and
	// review rejection. A new reason must have a named constant and be added
	// here before it can be treated as a known terminal outcome.
	writers := []TerminalReason{
		TerminalReasonNoLegalCandidates,
		TerminalReasonTemporarySourceFailuresDidNotClear,
		TerminalReasonTemporaryCandidateFailuresDidNotClear,
		TerminalReasonCandidatesExhausted,
		TerminalReasonNoIdentifier,
		TerminalReasonNoEntitlement,
		TerminalReasonBrowserRejected,
		TerminalReasonDocumentDeliveryAvailable,
		TerminalReasonCancelledByUser,
		TerminalReasonBrowserCancelled,
		TerminalReasonUserDismissed,
		TerminalReasonReviewRejected,
	}
	for _, reason := range writers {
		if got := NormalizeTerminalReason(string(reason)); got != reason {
			t.Errorf("NormalizeTerminalReason(%q) = %q, want %q", reason, got, reason)
		}
	}
}

func TestTerminalReasonRoundTripsAndNormalizesLegacyValues(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	reasons := []TerminalReason{
		TerminalReasonUnknown,
		TerminalReasonNoLegalCandidates,
		TerminalReasonTemporarySourceFailuresDidNotClear,
		TerminalReasonTemporaryCandidateFailuresDidNotClear,
		TerminalReasonCandidatesExhausted,
		TerminalReasonNoIdentifier,
		TerminalReasonNoEntitlement,
		TerminalReasonBrowserRejected,
		TerminalReasonDocumentDeliveryAvailable,
		TerminalReasonCancelledByUser,
		TerminalReasonBrowserCancelled,
		TerminalReasonUserDismissed,
		TerminalReasonReviewRejected,
	}
	for i, want := range reasons {
		id, err := js.CreateRequest(ctx, fmt.Sprintf("wr_terminal_reason_%02d", i), testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
		if err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, StateResolving, StateUnavailable, nil, WithTerminalReason(want)); err != nil {
			t.Fatal(err)
		}
		row, err := js.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.TerminalReason != string(want) {
			t.Errorf("round trip reason = %q, want %q", row.TerminalReason, want)
		}
	}

	id, err := js.CreateRequest(ctx, "wr_terminal_reason_legacy", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET terminal_reason = ? WHERE id = ?`, "legacy free-form reason", id); err != nil {
		t.Fatal(err)
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.TerminalReason != string(TerminalReasonUnknown) {
		t.Fatalf("legacy terminal reason = %q, want %q", row.TerminalReason, TerminalReasonUnknown)
	}
}

func TestCreateRequestPersistsExplicitPrincipal(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, err := js.CreateRequest(ctx, "wr_principal_mcp", testWork(), "", "", testPolicy(), nil, PrincipalMCP)
	if err != nil {
		t.Fatal(err)
	}
	var requester string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT requester FROM work_requests WHERE id = (SELECT work_request_id FROM jobs WHERE id = ?)`, id).Scan(&requester); err != nil {
		t.Fatal(err)
	}
	if requester != string(PrincipalMCP) {
		t.Fatalf("requester = %q, want %q", requester, PrincipalMCP)
	}

	id, err = js.CreateRequest(ctx, "wr_principal_empty", testWork(), "", "", testPolicy(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT requester FROM work_requests WHERE id = (SELECT work_request_id FROM jobs WHERE id = ?)`, id).Scan(&requester); err != nil {
		t.Fatal(err)
	}
	if requester != string(PrincipalUnknown) {
		t.Fatalf("empty principal persisted as %q, want %q", requester, PrincipalUnknown)
	}
}
