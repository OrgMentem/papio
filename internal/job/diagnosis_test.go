// Copyright 2026 OrgMentem. Licensed under MIT.

package job

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"

	"papio/internal/work"
)

// TestDiagnosisReasonVocabularyIsExhaustive derives the truth from the
// declaration rather than trusting a hand-maintained set: diagnosisReasons
// gates what may be persisted in human_actions.diagnosis, so a reason missing
// from it fails a producer closed at runtime instead of at compile time.
func TestDiagnosisReasonVocabularyIsExhaustive(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "diagnosis.go", nil, 0)
	if err != nil {
		t.Fatalf("parse diagnosis.go: %v", err)
	}
	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			name := value.Names[0].Name
			if len(name) < len("DiagnosisReason") || name[:len("DiagnosisReason")] != "DiagnosisReason" {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("%s is not declared with a string literal", name)
			}
			reason, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			declared++
			if !ValidDiagnosisReason(reason) {
				t.Errorf("%s (%q) is missing from diagnosisReasons", name, reason)
			}
		}
	}
	if declared != len(diagnosisReasons) {
		t.Errorf("diagnosis.go declares %d reasons but diagnosisReasons holds %d", declared, len(diagnosisReasons))
	}
}

// A diagnosis is a closed enum, so an invented one must fail at the producer
// rather than reach the wire and be guessed at by the family projection.
func TestWithHumanActionDiagnosisRejectsUnknownReason(t *testing.T) {
	js := testStore(t)
	ctx := t.Context()
	id, err := js.CreateRequest(ctx, "wr_diagnosis_closed", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "manual_download", "download it", Access(false, ""),
		WithHumanActionDiagnosis("page_smells_wrong")); err == nil {
		t.Fatal("an unknown diagnosis was accepted")
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("rejected diagnosis still opened an action: %+v", open)
	}
}

// A re-park rewrites detail; diagnosis must move with it or the row's family
// and its prose end up describing different failures.
func TestOpenHumanActionRefreshRewritesDiagnosis(t *testing.T) {
	js := testStore(t)
	ctx := t.Context()
	id, err := js.CreateRequest(ctx, "wr_diagnosis_refresh", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	first, err := js.OpenHumanAction(ctx, id, "manual_download", "no adapter", Access(false, ""),
		WithHumanActionDiagnosis(DiagnosisReasonProviderAdapterMissing))
	if err != nil {
		t.Fatal(err)
	}
	second, err := js.OpenHumanAction(ctx, id, "manual_download", "the file was rejected", Access(false, ""),
		WithHumanActionDiagnosis(DiagnosisReasonAdoptedPDFInvalid))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("refresh created action %d instead of updating %d", second, first)
	}
	var diagnosis string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COALESCE(diagnosis, '') FROM human_actions WHERE id = ?`, first).Scan(&diagnosis); err != nil {
		t.Fatal(err)
	}
	if diagnosis != DiagnosisReasonAdoptedPDFInvalid {
		t.Fatalf("diagnosis = %q, want the refreshed %q", diagnosis, DiagnosisReasonAdoptedPDFInvalid)
	}
}

func diagnosisRow(state string) *Row {
	return &Row{ID: "job_test", State: state, Work: work.Work{DOI: "10.1000/example", Title: "Example work"}}
}

func TestDiagnoseUnknownProviderAdapter(t *testing.T) {
	action := HumanAction{ID: 7, JobID: "job_test", Kind: "manual_download", Status: "open", Detail: "papio has no adapter for this provider yet; download the PDF yourself for now"}
	events := []map[string]any{{
		"kind": "browser.provider_outcome",
		"detail": map[string]any{
			"outcome": "ui_changed", "detail": "No source-controlled adapter matched this provider page.",
		},
	}}
	diagnosis := Diagnose(diagnosisRow(StateAwaitingHuman), []HumanAction{action}, events)
	if diagnosis.Reason != DiagnosisReasonProviderAdapterMissing {
		t.Fatalf("reason = %q, want %q", diagnosis.Reason, DiagnosisReasonProviderAdapterMissing)
	}
	if diagnosis.Action == nil || !diagnosis.Action.CanOpenAction || !diagnosis.NeedsBrowser {
		t.Fatalf("action capabilities = %+v, diagnosis = %+v", diagnosis.Action, diagnosis)
	}
	if diagnosis.ProviderOutcome != "ui_changed" {
		t.Fatalf("provider outcome = %q, want ui_changed", diagnosis.ProviderOutcome)
	}
}

func TestDiagnoseKnownAdapterDrift(t *testing.T) {
	action := HumanAction{ID: 8, JobID: "job_test", Kind: "manual_download", Status: "open", Detail: "papio could not drive the provider page; download the PDF yourself and papio will adopt it"}
	events := []map[string]any{{
		"kind": "browser.provider_outcome",
		"detail": map[string]any{
			"outcome": "ui_changed", "adapter_id": "sciencedirect", "adapter_version": "0.4.0",
		},
	}}
	diagnosis := Diagnose(diagnosisRow(StateAwaitingHuman), []HumanAction{action}, events)
	if diagnosis.Reason != DiagnosisReasonProviderAdapterDrift {
		t.Fatalf("reason = %q, want %q", diagnosis.Reason, DiagnosisReasonProviderAdapterDrift)
	}
	if diagnosis.AdapterID != "sciencedirect" || diagnosis.AdapterVersion != "0.4.0" {
		t.Fatalf("adapter = %s@%s", diagnosis.AdapterID, diagnosis.AdapterVersion)
	}
}

func TestDiagnoseAdoptedPDFValidation(t *testing.T) {
	action := HumanAction{ID: 9, JobID: "job_test", Kind: "manual_download", Status: "open", Detail: "the adopted download failed validation; please supply a different file"}
	diagnosis := Diagnose(diagnosisRow(StateAwaitingHuman), []HumanAction{action}, nil)
	if diagnosis.Reason != DiagnosisReasonAdoptedPDFInvalid {
		t.Fatalf("reason = %q, want %q", diagnosis.Reason, DiagnosisReasonAdoptedPDFInvalid)
	}
	if diagnosis.Action.CanOpenAction != true {
		t.Fatal("validation replacement should remain openable")
	}
}

func TestDiagnoseRetryWait(t *testing.T) {
	diagnosis := Diagnose(diagnosisRow(StateRetryWait), nil, nil)
	if diagnosis.Reason != DiagnosisReasonRetryWait || !diagnosis.CanRetry {
		t.Fatalf("diagnosis = %+v, want retry_wait with retry capability", diagnosis)
	}
	if diagnosis.Action != nil {
		t.Fatalf("action = %+v, want nil", diagnosis.Action)
	}
}

func TestDiagnoseV1ShapeRemainsUnchangedWhileV2AddsNestedCutover(t *testing.T) {
	row := diagnosisRow(StateAwaitingHuman)
	v1 := Diagnose(row, nil, nil)
	v2 := DiagnoseV2(row, nil, []map[string]any{{
		"kind": "job.transition",
		"detail": map[string]any{
			InstitutionCutoverBlockerKey: string(InstitutionCutoverBlockerPolicyGate),
			CanaryReadyRouteExistsKey:    true,
		},
	}})
	v1JSON, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	v1Fields := map[string]any{}
	if err := json.Unmarshal(v1JSON, &v1Fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := v1Fields["institution_cutover"]; ok {
		t.Fatal("v1 diagnosis unexpectedly contains institution_cutover")
	}
	v2JSON, err := json.Marshal(v2)
	if err != nil {
		t.Fatal(err)
	}
	v2Fields := map[string]any{}
	if err := json.Unmarshal(v2JSON, &v2Fields); err != nil {
		t.Fatal(err)
	}
	delete(v2Fields, "institution_cutover")
	if !reflect.DeepEqual(v2Fields, v1Fields) {
		t.Fatalf("v2 base fields changed v1: v2=%v v1=%v", v2Fields, v1Fields)
	}
	if v2.InstitutionCutover == nil || v2.InstitutionCutover.Blocker != InstitutionCutoverBlockerPolicyGate ||
		!v2.InstitutionCutover.CanaryReadyRouteExists {
		t.Fatalf("v2 cutover = %+v", v2.InstitutionCutover)
	}
}

func TestDiagnoseV2IgnoresMalformedNewestCutoverDetail(t *testing.T) {
	events := []map[string]any{
		{"kind": "job.transition", "detail": map[string]any{
			InstitutionCutoverBlockerKey: string(InstitutionCutoverBlockerPolicyGate),
			CanaryReadyRouteExistsKey:    true,
		}},
		{"kind": "job.transition", "detail": map[string]any{
			InstitutionCutoverBlockerKey: "not-a-blocker",
			CanaryReadyRouteExistsKey:    false,
		}},
	}
	diagnosis := DiagnoseV2(diagnosisRow(StateAwaitingHuman), nil, events)
	if diagnosis.InstitutionCutover != nil {
		t.Fatalf("cutover = %+v, want nil for malformed newest transition", diagnosis.InstitutionCutover)
	}
}

func TestDiagnoseV2NewestTransitionWithoutCutoverClearsPrior(t *testing.T) {
	events := []map[string]any{
		{"kind": "job.transition", "detail": map[string]any{
			InstitutionCutoverBlockerKey: string(InstitutionCutoverBlockerPolicyGate),
			CanaryReadyRouteExistsKey:    true,
		}},
		{"kind": "job.transition", "detail": map[string]any{"reason": "complete"}},
	}
	diagnosis := DiagnoseV2(diagnosisRow(StateReady), nil, events)
	if diagnosis.InstitutionCutover != nil {
		t.Fatalf("cutover = %+v, want nil when newest transition has no decision", diagnosis.InstitutionCutover)
	}
}

func TestDiagnoseV2LatestValidTransactionalDecisionWins(t *testing.T) {
	events := []map[string]any{
		{"kind": "job.transition", "detail": map[string]any{
			InstitutionCutoverBlockerKey: string(InstitutionCutoverBlockerSourceGateOnly),
			CanaryReadyRouteExistsKey:    false,
		}},
		{"kind": "diagnostic.noise", "detail": map[string]any{
			InstitutionCutoverBlockerKey: string(InstitutionCutoverBlockerPolicyGate),
			CanaryReadyRouteExistsKey:    true,
		}},
		{"kind": "job.transition", "detail": map[string]any{
			InstitutionCutoverBlockerKey: string(InstitutionCutoverBlockerIdentifierGate),
			CanaryReadyRouteExistsKey:    true,
		}},
	}
	diagnosis := DiagnoseV2(diagnosisRow(StateAwaitingHuman), nil, events)
	if diagnosis.InstitutionCutover == nil || diagnosis.InstitutionCutover.Blocker != InstitutionCutoverBlockerIdentifierGate ||
		!diagnosis.InstitutionCutover.CanaryReadyRouteExists {
		t.Fatalf("cutover = %+v, want identifier_gate/true", diagnosis.InstitutionCutover)
	}
}
