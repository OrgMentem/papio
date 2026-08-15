// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"papio/internal/job"
)

// knownTerminalReasons mirrors job.go's declared TerminalReason vocabulary
// (every constant except the Unknown sentinel).
// TestClassifyCoversEveryDeclaredTerminalReason derives the truth from
// job.go itself, the same technique internal/job/terminal_reason_test.go
// uses against the same risk: a hand-maintained list only a test reads
// cannot fail when someone forgets to update it, it just quietly covers
// less.
var knownTerminalReasons = []job.TerminalReason{
	job.TerminalReasonNoLegalCandidates,
	job.TerminalReasonTemporarySourceFailuresDidNotClear,
	job.TerminalReasonTemporaryCandidateFailuresDidNotClear,
	job.TerminalReasonCandidatesExhausted,
	job.TerminalReasonNoIdentifier,
	job.TerminalReasonDOINotRegistered,
	job.TerminalReasonNoEntitlement,
	job.TerminalReasonBrowserRejected,
	job.TerminalReasonDocumentDeliveryAvailable,
	job.TerminalReasonCancelledByUser,
	job.TerminalReasonBrowserCancelled,
	job.TerminalReasonUserDismissed,
	job.TerminalReasonReviewRejected,
	job.TerminalReasonInsufficientIdentityEvidence,
}

func TestClassifyCoversEveryDeclaredTerminalReason(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../job/job.go", nil, 0)
	if err != nil {
		t.Fatalf("parse job.go: %v", err)
	}
	listed := make(map[string]bool, len(knownTerminalReasons))
	for _, r := range knownTerminalReasons {
		listed[string(r)] = true
	}
	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 {
				continue
			}
			name := vs.Names[0].Name
			if !strings.HasPrefix(name, "TerminalReason") || name == "TerminalReasonUnknown" {
				continue
			}
			if len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", name, err)
			}
			declared++
			if !listed[value] {
				t.Errorf("job.go declares %s = %q, missing from bench.knownTerminalReasons — add it to Classify's mapping and this list", name, value)
			}
		}
	}
	if declared != len(knownTerminalReasons) {
		t.Errorf("job.go declares %d terminal reasons, bench.knownTerminalReasons lists %d", declared, len(knownTerminalReasons))
	}
}

func TestClassifyMapsEveryKnownReasonWithoutError(t *testing.T) {
	for _, reason := range knownTerminalReasons {
		for _, state := range []string{job.StateUnavailable, job.StateCancelled} {
			class, err := Classify(state, reason, 0)
			if err != nil {
				t.Fatalf("Classify(%s, %s) = _, %v, want no error", state, reason, err)
			}
			wantIdentity := reason == job.TerminalReasonReviewRejected
			switch {
			case wantIdentity && class != ClassIdentityReview:
				t.Fatalf("Classify(%s, %s) = %s, want identity_review", state, reason, class)
			case !wantIdentity && class != ClassHonestUnavailable:
				t.Fatalf("Classify(%s, %s) = %s, want honest_unavailable", state, reason, class)
			}
		}
	}
}

func TestClassifyRejectsUnknownTerminalReason(t *testing.T) {
	if _, err := Classify(job.StateUnavailable, job.TerminalReasonUnknown, 0); err == nil {
		t.Fatal("Classify with TerminalReasonUnknown succeeded, want an error")
	}
	if _, err := Classify(job.StateCancelled, job.TerminalReason("made_up_reason"), 0); err == nil {
		t.Fatal("Classify with an unrecognized terminal reason succeeded, want an error")
	}
}

func TestClassifyReadyBucketsByHumanEpisodes(t *testing.T) {
	for _, state := range []string{job.StateReady, job.StateImported} {
		if class, err := Classify(state, "", 0); err != nil || class != ClassAutonomousReady {
			t.Fatalf("Classify(%s, zero episodes) = %s, %v, want autonomous_ready, nil", state, class, err)
		}
		if class, err := Classify(state, "", 3); err != nil || class != ClassReadyAfterHumanBoundary {
			t.Fatalf("Classify(%s, 3 episodes) = %s, %v, want ready_after_human_boundary, nil", state, class, err)
		}
	}
}

func TestClassifyRejectsNonTerminalState(t *testing.T) {
	if _, err := Classify(job.StateAwaitingHuman, "", 0); err == nil {
		t.Fatal("Classify with a non-terminal state succeeded, want an error")
	}
}
