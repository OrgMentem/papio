// Copyright 2026 OrgMentem. Licensed under MIT.

package job

import (
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
