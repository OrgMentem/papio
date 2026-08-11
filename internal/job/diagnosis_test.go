// Copyright 2026 OrgMentem. Licensed under MIT.

package job

import (
	"encoding/json"
	"reflect"
	"testing"

	"papio/internal/work"
)

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
