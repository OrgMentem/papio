// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// terminalReasonWriters lists every terminal transition writer: app
// exhaustion/identifier paths, browser handoff outcomes, cancellation, and
// review rejection. A new reason must have a named constant and be added here
// before it can be treated as a known terminal outcome.
// TestTerminalReasonVocabularyIsExhaustive enforces that mechanically, so the
// list cannot silently stop covering the vocabulary the way it did when
// doi_not_registered was added.
var terminalReasonWriters = []TerminalReason{
	TerminalReasonNoLegalCandidates,
	TerminalReasonTemporarySourceFailuresDidNotClear,
	TerminalReasonTemporaryCandidateFailuresDidNotClear,
	TerminalReasonCandidatesExhausted,
	TerminalReasonNoIdentifier,
	TerminalReasonDOINotRegistered,
	TerminalReasonNoEntitlement,
	TerminalReasonBrowserRejected,
	TerminalReasonDocumentDeliveryAvailable,
	TerminalReasonCancelledByUser,
	TerminalReasonBrowserCancelled,
	TerminalReasonUserDismissed,
	TerminalReasonReviewRejected,
}

// TestTerminalReasonVocabularyIsExhaustive guards the guard. A hand-maintained
// list that only ever gets read by tests it is also the input to cannot fail
// when someone forgets it — it just quietly covers less. So derive the truth
// from the declaration itself: every TerminalReason constant in job.go, except
// the Unknown sentinel, must appear above (and therefore in every table below).
func TestTerminalReasonVocabularyIsExhaustive(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "job.go", nil, 0)
	if err != nil {
		t.Fatalf("parse job.go: %v", err)
	}
	listed := make(map[string]bool, len(terminalReasonWriters))
	for _, reason := range terminalReasonWriters {
		listed[string(reason)] = true
	}
	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "TerminalReason" {
				continue
			}
			for _, name := range value.Names {
				if name.Name == "TerminalReasonUnknown" {
					continue
				}
				declared++
				reason := NormalizeTerminalReason(string(reasonValue(t, value)))
				if reason == TerminalReasonUnknown {
					t.Errorf("%s is not returned by NormalizeTerminalReason; add it to the switch", name.Name)
				}
				if !listed[string(reason)] {
					t.Errorf("%s (%q) is missing from terminalReasonWriters", name.Name, reason)
				}
			}
		}
	}
	if declared != len(terminalReasonWriters) {
		t.Errorf("job.go declares %d terminal reasons but terminalReasonWriters lists %d", declared, len(terminalReasonWriters))
	}
}

// reasonValue reads the string literal a TerminalReason constant is declared
// with. The persisted text is the contract, so the test must compare against
// what job.go actually writes rather than re-deriving it from the Go name.
func reasonValue(t *testing.T, spec *ast.ValueSpec) string {
	t.Helper()
	if len(spec.Values) != 1 {
		t.Fatalf("terminal reason spec has %d values, want 1", len(spec.Values))
	}
	lit, ok := spec.Values[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("terminal reason value is %T, want a string literal", spec.Values[0])
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("unquote %s: %v", lit.Value, err)
	}
	return unquoted
}

func TestTerminalReasonVocabularyCoversEveryWriter(t *testing.T) {
	for _, reason := range terminalReasonWriters {
		if got := NormalizeTerminalReason(string(reason)); got != reason {
			t.Errorf("NormalizeTerminalReason(%q) = %q, want %q", reason, got, reason)
		}
	}
}

func TestTerminalReasonRoundTripsAndNormalizesLegacyValues(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	reasons := append([]TerminalReason{TerminalReasonUnknown}, terminalReasonWriters...)
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
